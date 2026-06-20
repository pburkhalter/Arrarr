package downloader

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestRunSingleFile(t *testing.T) {
	payload := bytes.Repeat([]byte("hello-arrarr-"), 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(200)
		w.Write(payload)
	}))
	defer srv.Close()

	base := t.TempDir()
	d, err := New(Options{BaseDir: base, Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	job := Job{
		JobID:  "job1",
		Subdir: "sonarr/Family.Guy.S01.GERMAN.WEB",
		Files: []FileDownload{
			{FileID: 1, URL: srv.URL + "/a", DestName: "Family Guy/Season 1/ep1.mkv", Size: int64(len(payload))},
		},
	}
	got, err := d.Run(context.Background(), job)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := filepath.Join(base, "sonarr/Family.Guy.S01.GERMAN.WEB")
	if got != want {
		t.Fatalf("path: got %q want %q", got, want)
	}
	wrote, err := os.ReadFile(filepath.Join(want, "Family Guy/Season 1/ep1.mkv"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(wrote, payload) {
		t.Fatalf("bytes mismatch: got %d want %d", len(wrote), len(payload))
	}
	if _, err := os.Stat(filepath.Join(want, "Family Guy/Season 1/ep1.mkv.partial")); !os.IsNotExist(err) {
		t.Fatalf("partial file not cleaned up: %v", err)
	}
}

func TestRunMultiFileOneFailure(t *testing.T) {
	good := bytes.Repeat([]byte("ok"), 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			http.Error(w, "boom", 500)
			return
		}
		w.Write(good)
	}))
	defer srv.Close()

	base := t.TempDir()
	d, err := New(Options{BaseDir: base, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	job := Job{
		JobID:  "jobX",
		Subdir: "out",
		Files: []FileDownload{
			{FileID: 1, URL: srv.URL + "/good1", DestName: "a.bin", Size: int64(len(good))},
			{FileID: 2, URL: srv.URL + "/bad", DestName: "b.bin", Size: int64(len(good))},
			{FileID: 3, URL: srv.URL + "/good3", DestName: "c.bin", Size: int64(len(good))},
		},
	}
	_, err = d.Run(context.Background(), job)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// At least one successful file should remain on disk; concurrency=1 means
	// "a.bin" completes before "b.bin" fails and cancels the rest.
	if _, statErr := os.Stat(filepath.Join(base, "out", "a.bin")); statErr != nil {
		t.Fatalf("expected a.bin on disk: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(base, "out", "b.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("b.bin should not exist after failure: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(base, "out", "b.bin.partial")); !os.IsNotExist(statErr) {
		t.Fatalf("b.bin.partial should be cleaned up: %v", statErr)
	}
}

func TestRunSkipIfExists(t *testing.T) {
	payload := []byte("already-here")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(payload)
	}))
	defer srv.Close()

	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "skip"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(base, "skip", "x.bin")
	if err := os.WriteFile(existing, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	d, _ := New(Options{BaseDir: base, Concurrency: 1})
	_, err := d.Run(context.Background(), Job{
		JobID:  "j",
		Subdir: "skip",
		Files: []FileDownload{
			{FileID: 1, URL: srv.URL, DestName: "x.bin", Size: int64(len(payload))},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("server should not have been hit, got %d", hits)
	}
}

func TestRunRejectsPathEscape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("nope"))
	}))
	defer srv.Close()

	base := t.TempDir()
	d, _ := New(Options{BaseDir: base, Concurrency: 1})
	_, err := d.Run(context.Background(), Job{
		JobID:  "evil",
		Subdir: "safe",
		Files: []FileDownload{
			{FileID: 1, URL: srv.URL, DestName: "../escape.bin", Size: 4},
		},
	})
	if err == nil {
		t.Fatal("expected error for path escape")
	}
	if _, statErr := os.Stat(filepath.Join(base, "escape.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("escape.bin must not exist: %v", statErr)
	}
}

func TestRunRejectsAbsolutePath(t *testing.T) {
	base := t.TempDir()
	d, _ := New(Options{BaseDir: base, Concurrency: 1})
	_, err := d.Run(context.Background(), Job{
		JobID:  "evil2",
		Subdir: "safe",
		Files: []FileDownload{
			{FileID: 1, URL: "http://example.invalid", DestName: "/etc/passwd", Size: 4},
		},
	})
	if err == nil {
		t.Fatal("expected error for absolute path")
	}
}
