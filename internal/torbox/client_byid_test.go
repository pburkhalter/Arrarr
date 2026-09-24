package torbox

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// main.go passed 30s for years while the client's own default says 90s and
// explains why: creates take 20-60s under load.
func TestNewClientDefaultTimeoutIsNinetySeconds(t *testing.T) {
	c := NewClient("http://x", "k", 100, 0, 0)
	if c.HTTP.Timeout != 90*time.Second {
		t.Fatalf("timeout = %s, want 90s", c.HTTP.Timeout)
	}
}

func TestMyListByIDDecodesObjectAndArray(t *testing.T) {
	var gotQuery string
	shape := "object"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		switch shape {
		case "object":
			_, _ = io.WriteString(w, `{"success":true,"data":{"id":42,"name":"Rel","download_state":"completed"}}`)
		case "array":
			_, _ = io.WriteString(w, `{"success":true,"data":[{"id":41,"name":"Other"},{"id":42,"name":"Rel"}]}`)
		default:
			_, _ = io.WriteString(w, `{"success":true,"data":null}`)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "k", 1000, 0, 5*time.Second)
	ctx := context.Background()

	it, err := c.MyListByID(ctx, 42, true)
	if err != nil || it == nil || it.Name != "Rel" {
		t.Fatalf("object shape: %+v, %v", it, err)
	}
	if gotQuery != "bypass_cache=true&id=42" {
		t.Fatalf("query = %q, want id + bypass_cache", gotQuery)
	}
	shape = "array"
	if it, err = c.MyListByID(ctx, 42, false); err != nil || it == nil || it.ID != 42 {
		t.Fatalf("array shape: %+v, %v", it, err)
	}
	shape = "null"
	if it, err = c.MyListByID(ctx, 42, false); err != nil || it != nil {
		t.Fatalf("missing entry: %+v, %v — want (nil, nil)", it, err)
	}
}
