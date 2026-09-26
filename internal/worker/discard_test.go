package worker

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/downloader"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// Journarr's "remove everywhere" reaches arrarr as a SAB delete with
// del_files=1. A job TorBox is still downloading gives up its own entry (it
// holds one of the account's slots); a finished job keeps it — the account is
// shared — but its local files go.
func TestDiscardReleasesOnlyRunningEntriesAndDeletesJobDirs(t *testing.T) {
	st := newStore(t)
	tb := &frozenTorbox{}
	base := t.TempDir()
	p := NewPuller(PullerOptions{Store: st, Torbox: tb, BaseDir: base,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	mgr := New(Options{Store: st, Torbox: tb, Puller: p,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()

	running := &job.Job{NzoID: "a", State: job.StateDownloading, TorboxActiveID: nullInt(701)}
	mgr.Discard(ctx, running)

	done := filepath.Join(base, "sonarr", "Show.S01E01")
	if err := os.MkdirAll(done, 0o755); err != nil {
		t.Fatal(err)
	}
	finished := &job.Job{NzoID: "b", State: job.StateReady, TorboxActiveID: nullInt(702), LocalPath: nullStr(done)}
	mgr.Discard(ctx, finished)

	outside := t.TempDir()
	stray := &job.Job{NzoID: "c", State: job.StateReady, LocalPath: nullStr(outside)}
	mgr.Discard(ctx, stray)
	category := &job.Job{NzoID: "d", State: job.StateReady, LocalPath: nullStr(filepath.Join(base, "sonarr"))}
	mgr.Discard(ctx, category)

	if len(tb.deleted) != 1 || tb.deleted[0] != 701 {
		t.Fatalf("torbox deletes = %v, want only the running download 701", tb.deleted)
	}
	if _, err := os.Stat(done); !os.IsNotExist(err) {
		t.Fatalf("job dir still there: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a path outside DOWNLOAD_DIR was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "sonarr")); err != nil {
		t.Fatalf("the category folder was deleted: %v", err)
	}
}

func TestJobDirUnder(t *testing.T) {
	for _, c := range []struct {
		dir  string
		want bool
	}{
		{"/downloads/sonarr/Rel", true},
		{"/downloads/sonarr/Rel/Sub", true},
		{"/downloads/sonarr", false},
		{"/downloads", false},
		{"/downloads/sonarr/../../etc", false},
		{"/other/sonarr/Rel", false},
		{"sonarr/Rel", false},
		{"", false},
	} {
		if got := jobDirUnder("/downloads", c.dir); got != c.want {
			t.Errorf("jobDirUnder(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
	if jobDirUnder("", "/downloads/sonarr/Rel") {
		t.Error("an empty base must never allow a delete")
	}
}

// Abort stops a running pull and waits for it; the pull ends without a retry
// and the caller learns its directory.
func TestAbortStopsPullWithoutRetry(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	st := newStore(t)
	ctx := context.Background()
	tb := &slowTorbox{base: srv.URL, list: []torbox.MyListItem{{ID: 900, Name: "Movie.2026", DownloadState: "completed",
		Files: []torbox.MyListFile{{ID: 1, Name: "Movie.2026/movie.mkv", ShortName: "movie.mkv", Size: 1000000}}}}}
	insertWithTorboxID(t, st, "arrarr_abort", "COMPLETED_TORBOX", 900)

	base := t.TempDir()
	dl, err := downloader.New(downloader.Options{BaseDir: base, Concurrency: 1, Logger: noopLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	p := NewPuller(PullerOptions{Store: st, Torbox: tb, Downloader: dl, BaseDir: base,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	p.Tick(ctx, 2)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}

	dir := p.Abort(ctx, "arrarr_abort")
	if want := filepath.Join(base, "sonarr", "Movie.2026"); dir != want {
		t.Fatalf("Abort dir = %q, want %q", dir, want)
	}
	p.Wait()
	j, err := st.Get(ctx, "arrarr_abort")
	if err != nil {
		t.Fatal(err)
	}
	if j.Attempts != 0 || j.LastError.Valid {
		t.Fatalf("aborted pull was retried: attempts=%d last_error=%q", j.Attempts, j.LastError.String)
	}
	if p.Abort(ctx, "arrarr_abort") != "" {
		t.Fatal("second Abort should find nothing running")
	}
}

func nullInt(v int64) sql.NullInt64   { return sql.NullInt64{Int64: v, Valid: true} }
func nullStr(v string) sql.NullString { return sql.NullString{String: v, Valid: true} }
