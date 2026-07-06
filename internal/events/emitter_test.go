package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
)

func TestDeliverPayloadShape(t *testing.T) {
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Webhook-Token") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		got <- m
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	getJob := func(ctx context.Context, nzoID string) (*job.Job, error) {
		return &job.Job{
			NzoID:           nzoID,
			NzbSHA256:       "abc123",
			Source:          "usenet",
			Filename:        "Show.S01E01.mkv",
			SizeBytes:       sql.NullInt64{Int64: 500, Valid: true},
			BytesDownloaded: 250,
			BytesTotal:      500,
		}, nil
	}
	e := New(srv.URL, "secret", 2*time.Second, getJob, slog.Default())
	e.deliver(transition{nzoID: "arrarr_deadbeef", from: "SUBMITTED", to: "DOWNLOADING"})

	select {
	case m := <-got:
		// Field names must match Journarr's arrarrEvent receiver exactly.
		for k, want := range map[string]any{
			"event": "job.transition", "nzo_id": "arrarr_deadbeef",
			"from": "SUBMITTED", "to": "DOWNLOADING",
			"nzb_sha256": "abc123", "source": "usenet", "filename": "Show.S01E01.mkv",
		} {
			if m[k] != want {
				t.Errorf("field %q = %v, want %v", k, m[k], want)
			}
		}
		if m["event_id"] == "" || m["ts"] == "" {
			t.Error("event_id/ts missing")
		}
		if m["bytes_total"].(float64) != 500 {
			t.Errorf("bytes_total = %v, want 500", m["bytes_total"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery received")
	}
}

func TestEnqueueDoesNotBlockWhenFull(t *testing.T) {
	e := New("http://127.0.0.1:1/never", "", time.Second, nil, slog.Default())
	// Overfill the buffer well past cap; must never block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			e.Enqueue("x", "A", "B")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked")
	}
}
