package sab

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/store"
)

// The poller block is what makes a frozen pipeline visible to Journarr and the
// NAS healthcheck: when the TorBox poll loop breaks, jobs stop transitioning
// but every state count and container healthcheck still looks normal.
func TestStatusJSONReportsPollerHealth(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	lastOK := time.Now().UTC().Add(-90 * time.Minute)
	srv := NewServer(Options{
		APIKey: "k",
		Store:  Adapt(st),
		Logger: slog.Default(),
		PollHealth: func() (time.Time, string) {
			return lastOK, "torbox: response body exceeds 32 MiB limit"
		},
	})

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/status.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Poller *struct {
			LastOK         *time.Time `json:"last_ok"`
			SecondsSinceOK *int64     `json:"seconds_since_ok"`
			LastError      string     `json:"last_error"`
		} `json:"poller"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Poller == nil {
		t.Fatal("poller block missing from /status.json")
	}
	if got.Poller.SecondsSinceOK == nil || *got.Poller.SecondsSinceOK < 5300 {
		t.Errorf("seconds_since_ok=%v, want ~5400 so a monitor can threshold on staleness", got.Poller.SecondsSinceOK)
	}
	if !strings.Contains(got.Poller.LastError, "32 MiB") {
		t.Errorf("last_error=%q should carry the underlying failure", got.Poller.LastError)
	}
}

// With no PollHealth wired (test fakes, or a build without the worker) the
// block is omitted rather than reported as a zero-valued healthy poller.
func TestStatusJSONOmitsPollerWhenUnwired(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	srv := NewServer(Options{APIKey: "k", Store: Adapt(st), Logger: slog.Default()})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/status.json", nil))
	if strings.Contains(w.Body.String(), "poller") {
		t.Errorf("body should omit poller: %s", w.Body.String())
	}
}

func TestStatusPageRenders(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	srv := NewServer(Options{
		APIKey:      "k",
		URLBase:     "/sabnzbd",
		MaxNZBBytes: 1 << 20,
		Store:       Adapt(st),
		Logger:      slog.Default(),
		Webhook:     &WebhookOptions{Secret: "shh"},
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Api-Key", "k") // the page lists job paths and errors — key required
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("content-type=%q", got)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Arrarr",
		"Pipeline",
		"Recent activity",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}
