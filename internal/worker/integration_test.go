package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/librarian"
	"github.com/pburkhalter/arrarr/internal/pathmap"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// fakeTorbox simulates the queued→active transition: first call to MyList
// returns queue_id only; subsequent calls return id + folder + completed.
type fakeTorbox struct {
	createCalls atomic.Int32
	mylistCalls atomic.Int32
	editCalls   atomic.Int32
	folder      string
	files       []torbox.MyListFile // attached to "completed" responses (for librarian)
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
	return []torbox.MyListItem{{
		ID: 99, QueueID: 11,
		DownloadState:    "completed",
		DownloadFinished: true,
		Name:             f.folder,
		Files:            f.files,
	}}, nil
}

func (f *fakeTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error {
	return nil
}

func (f *fakeTorbox) EditUsenet(_ context.Context, _ int64, _ torbox.EditUsenetParams) error {
	f.editCalls.Add(1)
	return nil
}

func (f *fakeTorbox) RequestUsenetDL(_ context.Context, _, fileID int64, _ bool) (string, error) {
	return "https://cdn.torbox.app/test/file?id=" + strconv.FormatInt(fileID, 10), nil
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
		InstanceName:   "testhost",
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
			if tb.editCalls.Load() < 1 {
				t.Errorf("expected at least one EditUsenet call (tagging), got %d", tb.editCalls.Load())
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
func (r *rateLimitedTorbox) EditUsenet(_ context.Context, _ int64, _ torbox.EditUsenetParams) error {
	return nil
}
func (r *rateLimitedTorbox) RequestUsenetDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}

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

// fakeFSAlwaysMissing simulates the case where arrarr's mount diverged from
// Sonarr/Jellyfin's: the folder name is correct but stat() returns false
// because arrarr captured a different namespace. Verifier must NOT block on
// this — it should trust TorBox's API state.
type fakeFSAlwaysMissing struct{}

func (f *fakeFSAlwaysMissing) Exists(_ string) (bool, error) { return false, nil }

func TestVerifierTrustsTorboxStateWhenLocalStatFails(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	folder := "Some.Movie.GERMAN.DL.1080p.WEB.H264-XYZ"
	mgr := New(Options{
		Store:   st,
		Torbox:  &fakeTorbox{folder: folder},
		PathMap: pathmap.New("/m", "/v"),
		FS:      &fakeFSAlwaysMissing{}, // arrarr's mount can't see the folder
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})

	// Insert a job in COMPLETED_TORBOX with a folder name set.
	j := &job.Job{
		NzoID: "arrarr_diverged", Category: "sonarr",
		Filename: folder + ".nzb", NzbSHA256: "h", NzbBlob: []byte("nzb"),
		State: job.StateNew,
	}
	if err := st.Insert(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	// Walk through legal transitions to land in COMPLETED_TORBOX with folder set.
	for _, step := range []store.Transition{
		{From: job.StateNew, To: job.StateSubmitted},
		{From: job.StateSubmitted, To: job.StateCompletedTorbox, TorboxFolderName: ptrString(folder)},
	} {
		if err := st.Transition(context.Background(), j.NzoID, step); err != nil {
			t.Fatal(err)
		}
	}

	mgr.verifyOnce(context.Background())

	got, err := st.Get(context.Background(), j.NzoID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Errorf("state=%s want READY (verifier should trust TorBox API even when local stat fails)", got.State)
	}
}

func ptrString(s string) *string { return &s }

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

// When a submit response gives queue_id=0 AND id=0 (TorBox sometimes does this
// after the upload TCP read times out but the server still accepts the NZB),
// the poller must still find the job via name match, not leave it stuck
// SUBMITTED forever.
type nameFallbackTorbox struct {
	folderName string
}

func (t *nameFallbackTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	return &torbox.CreateResp{}, nil // 0/0 IDs
}
func (t *nameFallbackTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return []torbox.MyListItem{
		{ID: 999, QueueID: 999, DownloadState: "completed", DownloadFinished: true, Name: t.folderName},
	}, nil
}
func (t *nameFallbackTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error { return nil }
func (t *nameFallbackTorbox) EditUsenet(_ context.Context, _ int64, _ torbox.EditUsenetParams) error {
	return nil
}
func (t *nameFallbackTorbox) RequestUsenetDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}

func TestPollerNameFallbackForZeroIDs(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	folder := "Twisted.Metal.S02E01.GERMAN.DL.1080P.WEB.H264-WAYNE"
	tb := &nameFallbackTorbox{folderName: folder}
	mgr := New(Options{
		Store:   st,
		Torbox:  tb,
		PathMap: pathmap.New("/m", "/v"),
		FS:      &fakeFS{folder: folder},
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})

	// Insert a job in SUBMITTED state with NO TorBox ids set — simulating the
	// 0/0 response scenario after the rescue script wouldn't have populated them.
	j := &job.Job{
		NzoID:     "arrarr_zero",
		Category:  "sonarr",
		Filename:  folder + ".nzb",
		NzbSHA256: "h",
		NzbBlob:   []byte("nzb"),
		State:     job.StateSubmitted,
	}
	if err := st.Insert(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	// Force state to SUBMITTED via direct SQL (Insert always uses j.State).
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE jobs SET state='SUBMITTED' WHERE nzo_id=?`, j.NzoID); err != nil {
		t.Fatal(err)
	}

	mgr.pollOnce(context.Background())

	got, err := st.Get(context.Background(), j.NzoID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateCompletedTorbox {
		t.Errorf("state=%s want COMPLETED_TORBOX (poller should've name-matched and advanced)", got.State)
	}
	if !got.TorboxFolderName.Valid || got.TorboxFolderName.String != folder {
		t.Errorf("folder_name=%v want %q", got.TorboxFolderName, folder)
	}
}

// TestEndToEndPipelineWithLibrarian exercises the v2 path: a librarian writer
// is wired, the verifier should defer the COMPLETED_TORBOX → READY transition
// to the librarian, and the librarian must write a STRM file under
// /library/series/... before flipping the job to READY.
func TestEndToEndPipelineWithLibrarian(t *testing.T) {
	dir := t.TempDir()
	libDir := t.TempDir()

	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	folder := "Twisted.Metal.S02E01.GERMAN.DL.1080p.WEB.H264-WAYNE"
	tb := &fakeTorbox{
		folder: folder,
		files: []torbox.MyListFile{
			{ID: 1, Name: folder + "/Twisted.Metal.S02E01.WAYNE.mkv", ShortName: "Twisted.Metal.S02E01.WAYNE.mkv", Size: 1024, MimeType: "video/x-matroska"},
		},
	}
	fs := &fakeFS{folder: folder}

	pm := pathmap.New("/mnt/torbox", "/torbox")
	wake := make(chan struct{}, 1)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// LibraryMode "strm" with no Sonarr/Radarr → fallback layout under
	// /library/series/<release>/<file>.strm.
	libWriter, err := librarian.New("strm", libDir, "/mnt/torbox")
	if err != nil {
		t.Fatal(err)
	}

	mgr := New(Options{
		Store:                    st,
		Torbox:                   tb,
		PathMap:                  pm,
		FS:                       fs,
		Logger:                   logger,
		Wake:                     wake,
		DispatchEvery:            20 * time.Millisecond,
		PollEvery:                20 * time.Millisecond,
		VerifyEvery:              20 * time.Millisecond,
		ReapEvery:                time.Hour,
		ReapOlderThan:            time.Hour,
		WorkerPoolSize:           2,
		InstanceName:             "testhost",
		Librarian:                libWriter,
		LocalVerifyBase:          "/mnt/torbox",
		StreamingURLRefreshAfter: 5 * time.Hour,
	})

	j := &job.Job{
		NzoID:     "arrarr_v2int",
		Category:  "sonarr",
		Filename:  folder + ".nzb",
		NzbSHA256: "deadbeef-v2",
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
		got, err := st.Get(context.Background(), "arrarr_v2int")
		if err != nil {
			t.Fatal(err)
		}
		if got.State == job.StateReady {
			cancel()
			<-done
			// STRM file must exist at the fallback path (no canonical naming
			// because we didn't wire a Sonarr client).
			expectedSTRM := filepath.Join(libDir, "series", folder, "Twisted.Metal.S02E01.WAYNE.strm")
			if _, err := os.Stat(expectedSTRM); err != nil {
				t.Errorf("expected STRM at %s: %v", expectedSTRM, err)
			}
			body, _ := os.ReadFile(expectedSTRM)
			if !bytesContains(body, "cdn.torbox.app") {
				t.Errorf("STRM body should contain CDN URL, got: %q", body)
			}
			// library_path + streaming_url should be populated.
			row := st.DB().QueryRowContext(context.Background(),
				`SELECT library_path, library_writer_state, streaming_url FROM jobs WHERE nzo_id=?`,
				"arrarr_v2int")
			var libPath, writerState, streamURL *string
			if err := row.Scan(&libPath, &writerState, &streamURL); err != nil {
				t.Fatal(err)
			}
			if libPath == nil || *libPath != expectedSTRM {
				t.Errorf("library_path=%v want %s", libPath, expectedSTRM)
			}
			if writerState == nil || *writerState != "written" {
				t.Errorf("library_writer_state=%v want 'written'", writerState)
			}
			if streamURL == nil || !bytesContains([]byte(*streamURL), "cdn.torbox.app") {
				t.Errorf("streaming_url=%v want CDN URL", streamURL)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	final, _ := st.Get(context.Background(), "arrarr_v2int")
	t.Fatalf("job did not reach READY (librarian path) in time. state=%s last_error=%v library_writer_state-via-extra-query: see jobs row", final.State, final.LastError)
}

// bytesContains avoids dragging "bytes" into imports just for testing.
func bytesContains(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}

