package sab

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pburkhalter/arrarr/internal/store"
)

// The HTML status page lists nzo ids, local paths and error texts of recent
// jobs; it was reachable without a key. /status.json (aggregates only) stays
// open because the NAS healthcheck reads it unauthenticated.
func TestStatusPageRequiresAPIKey(t *testing.T) {
	st, _ := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())
	h := NewServer(Options{APIKey: "k", Store: Adapt(st), Logger: slog.Default()}).Handler()

	get := func(path, key string) int {
		req := httptest.NewRequest("GET", path, nil)
		if key != "" {
			req.Header.Set("X-Api-Key", key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if got := get("/", ""); got != http.StatusUnauthorized {
		t.Fatalf("GET / without key = %d, want 401", got)
	}
	if got := get("/", "wrong"); got != http.StatusUnauthorized {
		t.Fatalf("GET / with wrong key = %d, want 401", got)
	}
	if got := get("/", "k"); got != http.StatusOK {
		t.Fatalf("GET / with key = %d, want 200", got)
	}
	if got := get("/?apikey=k", ""); got != http.StatusOK {
		t.Fatalf("GET /?apikey= = %d, want 200", got)
	}
	if got := get("/status.json", ""); got != http.StatusOK {
		t.Fatalf("GET /status.json without key = %d, want 200 (aggregates only)", got)
	}
}
