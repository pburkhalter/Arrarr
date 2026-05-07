package sab

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pburkhalter/arrarr/internal/store"
)

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
		Dashboard: &DashboardConfig{
			LibraryEnabled: true,
			LibraryMode:    "strm",
			MirrorEnabled:  false,
			InstanceName:   "patrik",
		},
		Webhook: &WebhookOptions{Secret: "shh"},
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
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
		"instance <strong>patrik</strong>",
		"Pipeline",
		"Library writer",
		"Recent activity",
		"strm",     // library mode pill
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}
