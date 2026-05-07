package sab

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/pathmap"
	"github.com/pburkhalter/arrarr/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pm := pathmap.New("/mnt/torbox", "/torbox")
	srv := NewServer(Options{
		APIKey:      "secret",
		URLBase:     "/sabnzbd",
		MaxNZBBytes: 1 << 20,
		Store:       Adapt(st),
		Wake:        make(chan struct{}, 1),
		PathMap:     pm,
		Logger:      slog.Default(),
		CompleteDir: "/torbox",
	})
	return srv, st
}

// Sonarr/Radarr crash with "Unable to retrieve queue and history items" when
// SAB returns "slots": null. Empty must serialize as "slots": [].
func TestEmptyQueueAndHistorySlotsAreEmptyArrays(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, mode := range []string{"queue", "history"} {
		r := httptest.NewRequest("GET", "/sabnzbd/api?mode="+mode+"&apikey=secret&output=json", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("mode=%s status=%d body=%s", mode, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if strings.Contains(body, `"slots":null`) {
			t.Errorf("mode=%s body contains slots:null — Sonarr will reject. body=%s", mode, body)
		}
		if !strings.Contains(body, `"slots":[]`) {
			t.Errorf("mode=%s body should contain slots:[] when empty. body=%s", mode, body)
		}
	}
}

func TestVersionUnauthenticated(t *testing.T) {
	srv, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/sabnzbd/api?mode=version", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var v VersionResp
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Version == "" {
		t.Error("empty version")
	}
}

func TestAddFileToQueueToHistory(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	// 1. Sonarr POSTs an NZB to /sabnzbd/api?mode=addfile.
	body, contentType := buildMultipart(t, "Evil.S03E01.nzb", []byte("<?xml?><nzb/>"))
	r := httptest.NewRequest("POST", "/sabnzbd/api?mode=addfile&apikey=secret&cat=sonarr", body)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("addfile status=%d body=%s", w.Code, w.Body.String())
	}
	var addResp AddResp
	if err := json.Unmarshal(w.Body.Bytes(), &addResp); err != nil {
		t.Fatal(err)
	}
	if !addResp.Status || len(addResp.NzoIDs) != 1 {
		t.Fatalf("bad addResp: %+v", addResp)
	}
	nzoID := addResp.NzoIDs[0]

	// 2. Sonarr immediately polls mode=queue — slot must be visible at status=Queued.
	r = httptest.NewRequest("GET", "/sabnzbd/api?mode=queue&apikey=secret&output=json", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var qr QueueResp
	if err := json.Unmarshal(w.Body.Bytes(), &qr); err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	if len(qr.Queue.Slots) != 1 || qr.Queue.Slots[0].NzoID != nzoID {
		t.Fatalf("expected 1 slot with nzo_id=%s, got %+v", nzoID, qr.Queue.Slots)
	}
	if qr.Queue.Slots[0].Status != "Queued" {
		t.Errorf("status=%q want Queued", qr.Queue.Slots[0].Status)
	}
	if qr.Queue.Slots[0].Cat != "sonarr" {
		t.Errorf("cat=%q want sonarr", qr.Queue.Slots[0].Cat)
	}

	// 3. Idempotency: same NZB POSTed again returns the same nzo_id.
	body, contentType = buildMultipart(t, "Evil.S03E01.nzb", []byte("<?xml?><nzb/>"))
	r = httptest.NewRequest("POST", "/sabnzbd/api?mode=addfile&apikey=secret&cat=sonarr", body)
	r.Header.Set("Content-Type", contentType)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var addResp2 AddResp
	if err := json.Unmarshal(w.Body.Bytes(), &addResp2); err != nil {
		t.Fatal(err)
	}
	if addResp2.NzoIDs[0] != nzoID {
		t.Errorf("dedup failed: got %s want %s", addResp2.NzoIDs[0], nzoID)
	}

	// 4. Drive job to READY and check history reports the right storage path.
	folder := "Evil.S03E01.GERMAN.DUBBED.DL.1080p.BDRiP.x264.V2-TSCC"
	if err := st.Transition(ctx, nzoID, store.Transition{From: job.StateNew, To: job.StateSubmitted}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTorboxIDs(ctx, nzoID, intP(99), &folder); err != nil {
		t.Fatal(err)
	}
	if err := st.Transition(ctx, nzoID, store.Transition{From: job.StateSubmitted, To: job.StateCompletedTorbox}); err != nil {
		t.Fatal(err)
	}
	if err := st.Transition(ctx, nzoID, store.Transition{
		From: job.StateCompletedTorbox, To: job.StateReady, CompletedAt: tNow(),
	}); err != nil {
		t.Fatal(err)
	}

	r = httptest.NewRequest("GET", "/sabnzbd/api?mode=history&apikey=secret&output=json", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var hr HistoryResp
	if err := json.Unmarshal(w.Body.Bytes(), &hr); err != nil {
		t.Fatal(err)
	}
	if len(hr.History.Slots) != 1 {
		t.Fatalf("history slots=%d want 1", len(hr.History.Slots))
	}
	got := hr.History.Slots[0]
	if got.Status != "Completed" {
		t.Errorf("status=%q want Completed", got.Status)
	}
	want := "/torbox/" + folder
	if got.Storage != want {
		t.Errorf("storage=%q want %q", got.Storage, want)
	}
	if got.Path != want {
		t.Errorf("path=%q want %q", got.Path, want)
	}
}

// TestHistoryStoragePrefersLibraryPathWhenSet — v2 librarian writes a STRM
// (or symlink) under /library/... and reports that path so Sonarr/Radarr do
// an in-place register. Without this, Sonarr would try to import from the raw
// /torbox/<folder> path and fight the read-only WebDAV mount.
func TestHistoryStoragePrefersLibraryPathWhenSet(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	body := []byte("<nzb/>")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("name", "Twisted.Metal.S02E01.nzb")
	_, _ = fw.Write(body)
	mw.Close()
	r := httptest.NewRequest("POST", "/sabnzbd/api?mode=addfile&apikey=secret&cat=sonarr", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var ar AddResp
	if err := json.Unmarshal(w.Body.Bytes(), &ar); err != nil {
		t.Fatal(err)
	}
	nzoID := ar.NzoIDs[0]

	// Drive to READY via the librarian path: COMPLETED_TORBOX → MarkLibraryWritten
	folder := "Twisted.Metal.S02E01.WEB.h264-WAYNE"
	if err := st.Transition(ctx, nzoID, store.Transition{From: job.StateNew, To: job.StateSubmitted}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTorboxIDs(ctx, nzoID, intP(99), &folder); err != nil {
		t.Fatal(err)
	}
	if err := st.Transition(ctx, nzoID, store.Transition{From: job.StateSubmitted, To: job.StateCompletedTorbox}); err != nil {
		t.Fatal(err)
	}
	libPath := "/library/series/Twisted Metal/Season 02/Twisted Metal - S02E01.strm"
	streamURL := "https://cdn.torbox.app/abc"
	expires := time.Now().Add(5 * time.Hour)
	if err := st.MarkLibraryWritten(ctx, nzoID, libPath, &streamURL, &expires); err != nil {
		t.Fatal(err)
	}

	r = httptest.NewRequest("GET", "/sabnzbd/api?mode=history&apikey=secret&output=json", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var hr HistoryResp
	if err := json.Unmarshal(w.Body.Bytes(), &hr); err != nil {
		t.Fatal(err)
	}
	if len(hr.History.Slots) != 1 {
		t.Fatalf("slots=%d want 1", len(hr.History.Slots))
	}
	got := hr.History.Slots[0]
	if got.Storage != libPath {
		t.Errorf("storage=%q want %q (librarian's library_path, not the v1 /torbox/ path)", got.Storage, libPath)
	}
	if got.Path != libPath {
		t.Errorf("path=%q want %q", got.Path, libPath)
	}
}

func TestAuthRequiredForQueue(t *testing.T) {
	srv, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/sabnzbd/api?mode=queue", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func TestQueueDeleteCancelsJob(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	body, ct := buildMultipart(t, "x.nzb", []byte("data"))
	r := httptest.NewRequest("POST", "/sabnzbd/api?mode=addfile&apikey=secret&cat=sonarr", body)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var add AddResp
	_ = json.Unmarshal(w.Body.Bytes(), &add)
	nzoID := add.NzoIDs[0]

	r = httptest.NewRequest("POST", "/sabnzbd/api?mode=queue&name=delete&value="+nzoID+"&apikey=secret", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got, err := st.Get(ctx, nzoID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateCanceled {
		t.Errorf("state=%s want CANCELED", got.State)
	}
}

func buildMultipart(t *testing.T, filename string, body []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("name", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(body)
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func intP(n int64) *int64 { return &n }
func tNow() *time.Time { t := time.Now(); return &t }
