package qbit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

// fakeStore is a thin in-memory JobStore impl. Lets the handler tests run
// without spinning up SQLite for every case.
type fakeStore struct {
	mu   sync.Mutex
	jobs map[string]*job.Job // keyed by nzo_id
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: map[string]*job.Job{}} }

func (f *fakeStore) AddTorrent(_ context.Context, j *job.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[j.NzoID] = j
	return nil
}
func (f *fakeStore) GetByHash(_ context.Context, hash string) (*job.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hash = strings.ToLower(hash)
	for _, j := range f.jobs {
		if strings.ToLower(j.NzbSHA256) == hash {
			return j, nil
		}
	}
	return nil, store.ErrNotFound
}
func (f *fakeStore) ListByCategory(_ context.Context, category string) ([]*job.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*job.Job, 0, len(f.jobs))
	for _, j := range f.jobs {
		if category == "" || j.Category == category {
			out = append(out, j)
		}
	}
	return out, nil
}
func (f *fakeStore) Delete(_ context.Context, nzoID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jobs, nzoID)
	return nil
}
func (f *fakeStore) UpdateState(_ context.Context, nzoID, state, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[nzoID]
	if !ok {
		return store.ErrNotFound
	}
	j.State = job.State(state)
	if lastError != "" {
		j.LastError.Valid = true
		j.LastError.String = lastError
	}
	return nil
}

func newTestServer(t *testing.T) (*Server, *fakeStore) {
	t.Helper()
	fs := newFakeStore()
	srv := NewServer(Options{
		Username:        "admin",
		Password:        "adminadmin",
		URLBase:         "",
		DownloadDir:     "/downloads",
		MaxTorrentBytes: 1 << 20,
		Store:           fs,
		Wake:            make(chan struct{}, 1),
		Logger:          slog.Default(),
	})
	return srv, fs
}

func TestAppVersionUnauthenticated(t *testing.T) {
	srv, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/api/v2/app/version", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != AppVersion {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestLoginIssuesCookieAndGuardsEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)

	// 1. Hitting an authed endpoint without a cookie → 403.
	r := httptest.NewRequest("GET", "/api/v2/torrents/info", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}

	// 2. Wrong creds → 200 "Fails." (qBit quirk).
	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	r = httptest.NewRequest("POST", "/api/v2/auth/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "Fails." {
		t.Fatalf("bad creds: status=%d body=%q", w.Code, w.Body.String())
	}

	// 3. Correct creds → 200 "Ok." + SID cookie.
	form = url.Values{"username": {"admin"}, "password": {"adminadmin"}}
	r = httptest.NewRequest("POST", "/api/v2/auth/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "Ok." {
		t.Fatalf("login: status=%d body=%q", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	var sid string
	for _, c := range cookies {
		if c.Name == sessionCookie {
			sid = c.Value
		}
	}
	if sid == "" {
		t.Fatal("no SID cookie issued")
	}

	// 4. Same cookie reused → 200 on authed endpoint.
	r = httptest.NewRequest("GET", "/api/v2/torrents/info", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("expected 200 with cookie, got %d", w.Code)
	}
}

func TestAddMagnetCreatesJob(t *testing.T) {
	srv, fs := newTestServer(t)

	hash := "0123456789abcdef0123456789abcdef01234567"
	magnet := "magnet:?xt=urn:btih:" + hash + "&dn=Test.Show.S01E01.mkv"

	body, ct := buildAddMultipart(t, map[string]string{
		"urls":     magnet,
		"category": "sonarr",
	}, nil)
	r := httptest.NewRequest("POST", "/api/v2/torrents/add", body)
	r.Header.Set("Content-Type", ct)
	r.AddCookie(loginCookie(t, srv))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "Ok." {
		t.Fatalf("add status=%d body=%q", w.Code, w.Body.String())
	}
	if len(fs.jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(fs.jobs))
	}
	var stored *job.Job
	for _, j := range fs.jobs {
		stored = j
	}
	if stored.NzbSHA256 != hash {
		t.Errorf("hash=%s want %s", stored.NzbSHA256, hash)
	}
	if stored.Category != "sonarr" {
		t.Errorf("category=%s want sonarr", stored.Category)
	}
	if stored.State != job.StateNew {
		t.Errorf("state=%s want NEW", stored.State)
	}
}

func TestAddTorrentFileExtractsInfoHash(t *testing.T) {
	srv, fs := newTestServer(t)
	// Minimal valid bencoded torrent: d4:infod4:name4:Test6:lengthi100eee
	// The "info" dict here is d4:name4:Test6:lengthi100ee — we don't need
	// real piece hashes for the parser to find it.
	torrent := []byte("d4:infod4:name4:Test6:lengthi100eee")

	body, ct := buildAddMultipart(t, map[string]string{"category": "sonarr"}, map[string][]byte{
		"torrents": torrent,
	})
	r := httptest.NewRequest("POST", "/api/v2/torrents/add", body)
	r.Header.Set("Content-Type", ct)
	r.AddCookie(loginCookie(t, srv))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "Ok." {
		t.Fatalf("add: status=%d body=%q", w.Code, w.Body.String())
	}
	if len(fs.jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(fs.jobs))
	}
	var stored *job.Job
	for _, j := range fs.jobs {
		stored = j
	}
	if len(stored.NzbSHA256) != 40 {
		t.Errorf("info-hash should be 40 hex chars, got %q", stored.NzbSHA256)
	}
	if stored.Filename != "Test" {
		t.Errorf("filename=%q want Test (from torrent info.name)", stored.Filename)
	}
}

func TestInfoMapsStateAndSavePath(t *testing.T) {
	srv, fs := newTestServer(t)

	hash := "abcdef0123456789abcdef0123456789abcdef01"
	fs.jobs["nz_1"] = &job.Job{
		NzoID:     "nz_1",
		Category:  "sonarr",
		Filename:  "MyShow.S01E01",
		NzbSHA256: hash,
		State:     job.StateReady,
	}
	fs.jobs["nz_1"].SizeBytes.Valid = true
	fs.jobs["nz_1"].SizeBytes.Int64 = 1024
	fs.jobs["nz_1"].LocalPath.Valid = true
	fs.jobs["nz_1"].LocalPath.String = "/downloads/sonarr/MyShow.S01E01"

	r := httptest.NewRequest("GET", "/api/v2/torrents/info?category=sonarr", nil)
	r.AddCookie(loginCookie(t, srv))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("info status=%d body=%s", w.Code, w.Body.String())
	}
	var infos []TorrentInfo
	if err := json.Unmarshal(w.Body.Bytes(), &infos); err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("got %d infos, want 1", len(infos))
	}
	got := infos[0]
	if got.Hash != hash {
		t.Errorf("hash=%s want %s", got.Hash, hash)
	}
	if got.State != "completed" {
		t.Errorf("state=%s want completed", got.State)
	}
	if got.Progress != 1.0 {
		t.Errorf("progress=%v want 1.0", got.Progress)
	}
	if got.SavePath != filepath.Join("/downloads", "sonarr") {
		t.Errorf("save_path=%q want /downloads/sonarr", got.SavePath)
	}
	wantCP := "/downloads/sonarr/MyShow.S01E01"
	if got.ContentPath != wantCP {
		t.Errorf("content_path=%q want %q (LocalPath wins)", got.ContentPath, wantCP)
	}
}

func TestCategoriesEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	r := httptest.NewRequest("GET", "/api/v2/torrents/categories", nil)
	r.AddCookie(loginCookie(t, srv))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	var cats map[string]Category
	if err := json.Unmarshal(w.Body.Bytes(), &cats); err != nil {
		t.Fatal(err)
	}
	if cats["sonarr"].Name != "sonarr" || cats["radarr"].Name != "radarr" {
		t.Errorf("missing default categories: %+v", cats)
	}
}

func TestDeleteByHashRemovesJob(t *testing.T) {
	srv, fs := newTestServer(t)
	hash := "00000000000000000000000000000000deadbeef"
	fs.jobs["nz_1"] = &job.Job{
		NzoID: "nz_1", Category: "sonarr", NzbSHA256: hash, State: job.StateReady,
	}

	form := url.Values{"hashes": {hash}, "deleteFiles": {"false"}}
	r := httptest.NewRequest("POST", "/api/v2/torrents/delete", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(loginCookie(t, srv))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if len(fs.jobs) != 0 {
		t.Fatalf("expected job deleted, got %d remaining", len(fs.jobs))
	}
}

// TestStoreAdapterRoundtripsAJob is a smoke test against the real *store.Store
// to confirm the adapter compiles + runs the basic insert/get cycle.
func TestStoreAdapterRoundtripsAJob(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := Adapt(st)
	hash := "11111111111111111111111111111111deadbeef"
	j := &job.Job{
		NzoID:     "arrarr_test1",
		Category:  "sonarr",
		Filename:  "Foo.S01E01",
		NzbSHA256: hash,
		NzbBlob:   []byte("magnet:..."),
		State:     job.StateNew,
	}
	if err := a.AddTorrent(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	got, err := a.GetByHash(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.NzoID != j.NzoID {
		t.Errorf("nzo_id=%s want %s", got.NzoID, j.NzoID)
	}
}

// loginCookie shortcuts the login dance for tests that need an authed
// session — call once per test, get a cookie ready to attach to the
// real request under test.
func loginCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	form := url.Values{"username": {"admin"}, "password": {"adminadmin"}}
	r := httptest.NewRequest("POST", "/api/v2/auth/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("login did not set SID cookie")
	return nil
}

func buildAddMultipart(t *testing.T, fields map[string]string, files map[string][]byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	for field, body := range files {
		fw, err := w.CreateFormFile(field, "upload.torrent")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(body)
	}
	_ = w.Close()
	return &buf, w.FormDataContentType()
}
