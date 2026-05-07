package sab

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/pathmap"
	"github.com/pburkhalter/arrarr/internal/pushover"
	"github.com/pburkhalter/arrarr/internal/store"
)

// newWebhookServer is like newTestServer but wires webhook options. Returns
// the server, the store (for direct row inspection), the pushover request
// counter, and the pushover test server (so the test can shut it down).
func newWebhookServer(t *testing.T, secret string, notifyOn string) (*Server, *store.Store, *atomic.Int32, *httptest.Server) {
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

	var pushCalls atomic.Int32
	pushSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pushCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	t.Cleanup(pushSrv.Close)

	pc := pushover.New("tok", "user", time.Second)
	pc.HTTP.Transport = pushoverRewrite(pushSrv.URL)

	srv := NewServer(Options{
		APIKey:      "secret",
		URLBase:     "/sabnzbd",
		MaxNZBBytes: 1 << 20,
		Store:       Adapt(st),
		Wake:        make(chan struct{}, 1),
		PathMap:     pathmap.New("/mnt/torbox", "/torbox"),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		CompleteDir: "/torbox",
		Webhook: &WebhookOptions{
			Secret:           secret,
			ReplayWindow:     5 * time.Minute,
			Pushover:         pc,
			PushoverNotifyOn: notifyOn,
		},
	})
	return srv, st, &pushCalls, pushSrv
}

// pushoverRewrite mirrors the helper in the pushover_test package — needed
// because the pushover client hardcodes its API URL.
type pushoverRewriteTransport struct{ target string }

func (r *pushoverRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := req.URL.Parse(r.target)
	if err != nil {
		return nil, err
	}
	u.Path = req.URL.Path
	u.RawQuery = req.URL.RawQuery
	req.URL = u
	req.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}
func pushoverRewrite(target string) http.RoundTripper { return &pushoverRewriteTransport{target: target} }

func sign(secret, id, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(id))
	m.Write([]byte("."))
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func sendWebhook(t *testing.T, srv *Server, secret string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	ts := fmt.Sprintf("%d", time.Now().Unix())
	req := httptest.NewRequest("POST", "/sabnzbd/webhook", bytes.NewReader(body))
	req.Header.Set("Webhook-Id", id)
	req.Header.Set("Webhook-Timestamp", ts)
	req.Header.Set("Webhook-Signature", sign(secret, id, ts, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func insertJobAtState(t *testing.T, st *store.Store, nzoID, category, folder string, queueID, activeID int64, state job.State, origin string) {
	t.Helper()
	j := &job.Job{
		NzoID:     nzoID,
		Category:  category,
		Filename:  folder + ".nzb",
		NzbSHA256: nzoID,
		NzbBlob:   []byte("<n/>"),
		State:     job.StateNew,
	}
	if err := st.Insert(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	_, err := st.DB().ExecContext(context.Background(), `UPDATE jobs SET
		state = ?, torbox_queue_id = ?, torbox_active_id = ?, torbox_folder_name = ?, origin = ?
		WHERE nzo_id = ?`, string(state), queueID, activeID, folder, origin, nzoID)
	if err != nil {
		t.Fatal(err)
	}
}

// --- tests ---

func TestWebhookDisabledReturns503(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	// No Webhook in Options → receiver disabled.
	srv := NewServer(Options{
		APIKey:      "secret",
		URLBase:     "/sabnzbd",
		MaxNZBBytes: 1 << 20,
		Store:       Adapt(st),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/sabnzbd/webhook", bytes.NewReader([]byte(`{}`)))
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want 503", w.Code)
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	srv, _, _, _ := newWebhookServer(t, "shh", "ready")
	body := []byte(`{"event":"download.ready","data":{"id":1}}`)
	req := httptest.NewRequest("POST", "/sabnzbd/webhook", bytes.NewReader(body))
	req.Header.Set("Webhook-Id", "msg")
	req.Header.Set("Webhook-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("Webhook-Signature", "v1,deadbeef")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d want 401", w.Code)
	}
}

func TestWebhookValidUnknownIDReturns204AndDoesNotNotify(t *testing.T) {
	srv, _, push, _ := newWebhookServer(t, "shh", "ready")
	w := sendWebhook(t, srv, "shh", map[string]any{
		"event":     "download.ready",
		"timestamp": "2026-05-08T12:00:00Z",
		"data":      map[string]any{"id": 999, "title": "Some Other User's DL"},
	})
	if w.Code != http.StatusNoContent {
		t.Errorf("status=%d want 204 (unknown ID = drop)", w.Code)
	}
	// Pushover must not fire — ownership lookup missed.
	time.Sleep(50 * time.Millisecond) // notify is async
	if push.Load() != 0 {
		t.Errorf("pushover fired %d times for foreign download", push.Load())
	}
}

func TestWebhookMatchesByStructuredIDAndFiresPushover(t *testing.T) {
	srv, st, push, _ := newWebhookServer(t, "shh", "ready")
	insertJobAtState(t, st, "arrarr_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"sonarr", "Twisted.Metal.S02E01.WEB.h264-WAYNE",
		11, 99, job.StateCompletedTorbox, "self")

	w := sendWebhook(t, srv, "shh", map[string]any{
		"event": "download.ready",
		"data":  map[string]any{"id": 99, "name": "Twisted.Metal.S02E01.WEB.h264-WAYNE"},
	})
	if w.Code != http.StatusNoContent {
		t.Errorf("status=%d want 204", w.Code)
	}
	// Pushover is fired in a goroutine; wait for it.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && push.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if push.Load() != 1 {
		t.Errorf("pushover calls=%d want 1", push.Load())
	}
	// pushover_sent_at must be populated for idempotency on duplicate delivery.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var ts *time.Time
		_ = st.DB().QueryRowContext(context.Background(),
			`SELECT pushover_sent_at FROM jobs WHERE nzo_id=?`,
			"arrarr_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").Scan(&ts)
		if ts != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("pushover_sent_at never populated")
}

func TestWebhookDuplicateDeliveryDoesNotDoubleNotify(t *testing.T) {
	srv, st, push, _ := newWebhookServer(t, "shh", "ready")
	insertJobAtState(t, st, "arrarr_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"sonarr", "X", 0, 99, job.StateCompletedTorbox, "self")

	for i := 0; i < 3; i++ {
		w := sendWebhook(t, srv, "shh", map[string]any{
			"event": "download.ready",
			"data":  map[string]any{"id": 99},
		})
		if w.Code != http.StatusNoContent {
			t.Errorf("attempt %d status=%d want 204", i, w.Code)
		}
	}
	// Wait for any in-flight pushover goroutine.
	time.Sleep(200 * time.Millisecond)
	if push.Load() != 1 {
		t.Errorf("pushover calls=%d want 1 (duplicate webhook delivery should be idempotent)", push.Load())
	}
}

func TestWebhookMirrorOriginDoesNotFirePushover(t *testing.T) {
	srv, st, push, _ := newWebhookServer(t, "shh", "ready")
	insertJobAtState(t, st, "arrarr_cccccccccccccccccccccccccccccccc",
		"sonarr", "X", 0, 77, job.StateCompletedTorbox, "mirror")

	w := sendWebhook(t, srv, "shh", map[string]any{
		"event": "download.ready",
		"data":  map[string]any{"id": 77},
	})
	if w.Code != http.StatusNoContent {
		t.Errorf("status=%d want 204", w.Code)
	}
	time.Sleep(150 * time.Millisecond)
	if push.Load() != 0 {
		t.Errorf("pushover fired %d times for mirror-origin job", push.Load())
	}
}

func TestWebhookFailedEventNotifiesWhenNotifyOnBoth(t *testing.T) {
	srv, st, push, _ := newWebhookServer(t, "shh", "both")
	insertJobAtState(t, st, "arrarr_dddddddddddddddddddddddddddddddd",
		"sonarr", "X", 0, 22, job.StateCompletedTorbox, "self")

	w := sendWebhook(t, srv, "shh", map[string]any{
		"event": "download.failed",
		"data":  map[string]any{"id": 22},
	})
	if w.Code != http.StatusNoContent {
		t.Errorf("status=%d want 204", w.Code)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && push.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if push.Load() != 1 {
		t.Errorf("pushover calls=%d want 1 (failed event with notify_on=both)", push.Load())
	}
}

func TestWebhookFailedEventNotificationGatedByNotifyOn(t *testing.T) {
	srv, st, push, _ := newWebhookServer(t, "shh", "ready") // ready-only
	insertJobAtState(t, st, "arrarr_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		"sonarr", "X", 0, 33, job.StateCompletedTorbox, "self")

	w := sendWebhook(t, srv, "shh", map[string]any{
		"event": "download.failed",
		"data":  map[string]any{"id": 33},
	})
	if w.Code != http.StatusNoContent {
		t.Errorf("status=%d want 204", w.Code)
	}
	time.Sleep(150 * time.Millisecond)
	if push.Load() != 0 {
		t.Errorf("pushover fired %d times when notify_on=ready and event=failed", push.Load())
	}
}

func TestWebhookMatchesByFolderName(t *testing.T) {
	srv, st, push, _ := newWebhookServer(t, "shh", "ready")
	folder := "Twisted.Metal.S02E03.WEB.h264-WAYNE"
	insertJobAtState(t, st, "arrarr_ffffffffffffffffffffffffffffffff",
		"sonarr", folder, 0, 0, job.StateCompletedTorbox, "self")

	// No structured id in the payload — only the name.
	w := sendWebhook(t, srv, "shh", map[string]any{
		"event": "download.ready",
		"data":  map[string]any{"name": folder},
	})
	if w.Code != http.StatusNoContent {
		t.Errorf("status=%d want 204", w.Code)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && push.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if push.Load() != 1 {
		t.Errorf("pushover calls=%d want 1 (folder-name match)", push.Load())
	}
}

func TestWebhookOversizedBodyRejected(t *testing.T) {
	srv, _, _, _ := newWebhookServer(t, "shh", "ready")
	big := make([]byte, maxWebhookBody+10)
	for i := range big {
		big[i] = 'x'
	}
	req := httptest.NewRequest("POST", "/sabnzbd/webhook", bytes.NewReader(big))
	req.Header.Set("Webhook-Id", "msg")
	req.Header.Set("Webhook-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("Webhook-Signature", sign("shh", "msg", fmt.Sprintf("%d", time.Now().Unix()), big))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d want 413", w.Code)
	}
}

func TestWebhookGetReturnsMethodNotAllowed(t *testing.T) {
	// Webhook is registered POST-only; chi should respond 405 to GET.
	srv, _, _, _ := newWebhookServer(t, "shh", "ready")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/sabnzbd/webhook", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d want 405", w.Code)
	}
}
