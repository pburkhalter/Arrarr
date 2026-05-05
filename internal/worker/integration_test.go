package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/pathmap"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// fakeTorbox simulates the queued→active transition: first call to MyList
// returns queue_id only; subsequent calls return id + folder + completed.
type fakeTorbox struct {
	createCalls atomic.Int32
	mylistCalls atomic.Int32
	folder      string
}

func (f *fakeTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	f.createCalls.Add(1)
	return &torbox.CreateResp{QueueID: 11}, nil
}

func (f *fakeTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	n := f.mylistCalls.Add(1)
	if n <= 1 {
		return []torbox.MyListItem{{QueueID: 11, DownloadState: "queued"}}, nil
	}
	if n == 2 {
		return []torbox.MyListItem{{ID: 99, QueueID: 11, DownloadState: "downloading", Progress: 0.5, Name: f.folder}}, nil
	}
	return []torbox.MyListItem{{ID: 99, QueueID: 11, DownloadState: "completed", DownloadFinished: true, Name: f.folder}}, nil
}

func (f *fakeTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error {
	return nil
}

// fakeFS pretends a folder appears after N misses, simulating WebDAV cache lag.
type fakeFS struct {
	folder string
	misses atomic.Int32
}

func (f *fakeFS) Exists(path string) (bool, error) {
	if filepath.Base(path) != f.folder {
		return false, nil
	}
	if f.misses.Load() > 0 {
		f.misses.Add(-1)
		return false, nil
	}
	return true, nil
}

func TestEndToEndPipeline(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	folder := "Evil.S03E01.NICE-RIP"
	tb := &fakeTorbox{folder: folder}
	fs := &fakeFS{folder: folder}
	fs.misses.Store(1) // one verifier tick will see "not yet", second will see it

	pm := pathmap.New("/mnt/torbox", "/torbox")
	wake := make(chan struct{}, 1)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mgr := New(Options{
		Store:          st,
		Torbox:         tb,
		PathMap:        pm,
		FS:             fs,
		Logger:         logger,
		Wake:           wake,
		DispatchEvery:  20 * time.Millisecond,
		PollEvery:      20 * time.Millisecond,
		VerifyEvery:    20 * time.Millisecond,
		ReapEvery:      time.Hour,
		ReapOlderThan:  time.Hour,
		WorkerPoolSize: 2,
	})

	// Insert a NEW job directly.
	j := &job.Job{
		NzoID:     "arrarr_int1",
		Category:  "sonarr",
		Filename:  "Evil.S03E01.nzb",
		NzbSHA256: "deadbeef",
		NzbBlob:   []byte("<nzb/>"),
		State:     job.StateNew,
	}
	if err := st.Insert(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	wake <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		got, err := st.Get(context.Background(), "arrarr_int1")
		if err != nil {
			t.Fatal(err)
		}
		if got.State == job.StateReady {
			cancel()
			<-done
			if !got.TorboxActiveID.Valid || got.TorboxActiveID.Int64 != 99 {
				t.Errorf("expected active_id=99, got %+v", got.TorboxActiveID)
			}
			if !got.TorboxFolderName.Valid || got.TorboxFolderName.String != folder {
				t.Errorf("folder_name=%v want %s", got.TorboxFolderName, folder)
			}
			if got.TorboxQueueID.Valid && got.TorboxQueueID.Int64 != 11 {
				t.Errorf("queue_id=%v want 11", got.TorboxQueueID)
			}
			if len(got.NzbBlob) != 0 {
				t.Errorf("nzb_blob should be cleared on success")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	final, _ := st.Get(context.Background(), "arrarr_int1")
	t.Fatalf("job did not reach READY in time. state=%s last_error=%v", final.State, final.LastError)
}

// TorBox returns 429 with body "60 per 1 hour" when its createusenetdownload
// per-key cap is exceeded. That's transient backpressure, not a job failure —
// the job must NOT advance toward MaxSubmitAttempts and must be rescheduled
// far enough out that the retry doesn't immediately hit 429 again.
type rateLimitedTorbox struct {
	calls atomic.Int32
}

func (r *rateLimitedTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	r.calls.Add(1)
	return nil, &torbox.APIError{Status: 429, Detail: "60 per 1 hour"}
}
func (r *rateLimitedTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (r *rateLimitedTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error { return nil }

func TestSubmitter429DoesNotIncrementAttempts(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	mgr := New(Options{
		Store:          st,
		Torbox:         &rateLimitedTorbox{},
		PathMap:        pathmap.New("/m", "/v"),
		FS:             &fakeFS{folder: "x"},
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize: 1,
	})

	j := &job.Job{
		NzoID:     "arrarr_429",
		Category:  "sonarr",
		Filename:  "test.nzb",
		NzbSHA256: "h",
		NzbBlob:   []byte("nzb"),
		State:     job.StateNew,
	}
	if err := st.Insert(context.Background(), j); err != nil {
		t.Fatal(err)
	}

	// Run dispatchOnce repeatedly with claim windows reset between calls so
	// the same job gets re-claimed each time. We expect: state stays NEW,
	// attempts stays 0, regardless of how many times we hit 429.
	for i := 0; i < 7; i++ {
		// Open a new claim window
		if _, err := st.DB().ExecContext(context.Background(),
			`UPDATE jobs SET claimed_at = NULL, next_attempt_at = NULL WHERE nzo_id = ?`, j.NzoID); err != nil {
			t.Fatal(err)
		}
		mgr.dispatchOnce(context.Background())
	}

	got, err := st.Get(context.Background(), j.NzoID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateNew {
		t.Errorf("state=%s want NEW (429 must not advance to FAILED)", got.State)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts=%d want 0 (429 must not count against MaxSubmitAttempts)", got.Attempts)
	}
	if !got.NextAttemptAt.Valid {
		t.Errorf("expected next_attempt_at to be set after 429")
	}
	if got.LastError.String == "" {
		t.Errorf("expected last_error to be set after 429")
	}
}

// Verify the sentinel returned for unmatched MyListItem in poller doesn't blow up.
func TestPollerHandlesEmpty(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.Migrate(context.Background())
	tb := &fakeTorbox{folder: "x"}
	mgr := New(Options{
		Store: st, Torbox: tb,
		PathMap: pathmap.New("/m", "/v"),
		FS:      &fakeFS{folder: "x"},
		Logger:  slog.Default(),
		PollEvery: time.Hour, DispatchEvery: time.Hour,
		VerifyEvery: time.Hour, ReapEvery: time.Hour,
	})
	mgr.pollOnce(context.Background())
	if errors.Is(nil, errors.New("never")) {
		// keep imports tidy
	}
}
