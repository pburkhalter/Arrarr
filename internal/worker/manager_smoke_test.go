package worker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/downloader"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// fakeTorbox is the all-in-one in-memory TorBox stand-in for end-to-end loop
// tests. Tracks call counts and lets each MyList call observe a different
// "lifecycle stage" so the poller transitions through queued → downloading →
// completed without us needing to wire fancy stateful behaviour.
type fakeTorbox struct {
	createCalls atomic.Int32
	mylistCalls atomic.Int32
	folder      string
	files       []torbox.MyListFile
}

func (f *fakeTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	f.createCalls.Add(1)
	return &torbox.CreateResp{QueueID: 11}, nil
}
func (f *fakeTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	f.createCalls.Add(1)
	return &torbox.CreateResp{TorrentID: 21}, nil
}
func (f *fakeTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	f.createCalls.Add(1)
	return &torbox.CreateResp{TorrentID: 31}, nil
}

func (f *fakeTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	n := f.mylistCalls.Add(1)
	switch {
	case n <= 1:
		return []torbox.MyListItem{{QueueID: 11, DownloadState: "queued"}}, nil
	case n == 2:
		return []torbox.MyListItem{{ID: 99, QueueID: 11, DownloadState: "downloading", Progress: 0.5, Name: f.folder}}, nil
	default:
		return []torbox.MyListItem{{
			ID: 99, QueueID: 11,
			DownloadState:    "completed",
			DownloadFinished: true,
			Name:             f.folder,
			Files:            f.files,
		}}, nil
	}
}

func (f *fakeTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}

func (f *fakeTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error  { return nil }
func (f *fakeTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }

func (f *fakeTorbox) RequestUsenetDL(_ context.Context, _, fileID int64, _ bool) (string, error) {
	return "http://cdn.example/" + strconv.FormatInt(fileID, 10), nil
}
func (f *fakeTorbox) RequestTorrentDL(_ context.Context, _, fileID int64, _ bool) (string, error) {
	return "http://cdn.example/" + strconv.FormatInt(fileID, 10), nil
}

// TestManagerEndToEndUsenet runs the four manager loops against a fake TorBox +
// real on-disk downloader, and asserts that a NEW usenet job reaches READY
// with a populated local_path.
func TestManagerEndToEndUsenet(t *testing.T) {
	dir := t.TempDir()
	dlDir := filepath.Join(dir, "downloads")

	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	folder := "Evil.S03E01.NICE-RIP"

	// Tiny HTTP server returns "hi" for every file URL the puller resolves.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hi")
	}))
	defer srv.Close()

	tb := &fakeTorbox{
		folder: folder,
		files: []torbox.MyListFile{
			{ID: 1, Name: folder + "/file.mkv", ShortName: "file.mkv", Size: 2, MimeType: "video/x-matroska"},
		},
	}
	// Wrap the fake torbox so the RequestUsenetDL points at the test server.
	tb2 := &rewriteTorbox{fakeTorbox: tb, base: srv.URL}

	dl, err := downloader.New(downloader.Options{
		BaseDir:     dlDir,
		Concurrency: 1,
		Logger:      noopLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	puller := NewPuller(PullerOptions{
		Store:      st,
		Torbox:     tb2,
		Downloader: dl,
		BaseDir:    dlDir,
		Logger:     slog.Default(),
	})

	wake := make(chan struct{}, 1)
	mgr := New(Options{
		Store:          st,
		Torbox:         tb2,
		Logger:         slog.Default(),
		Wake:           wake,
		DispatchEvery:  20 * time.Millisecond,
		PollEvery:      20 * time.Millisecond,
		PullEvery:      20 * time.Millisecond,
		ReapEvery:      time.Hour,
		ReapOlderThan:  time.Hour,
		WorkerPoolSize: 2,
		Puller:         puller,
	})

	j := &job.Job{
		NzoID:     "arrarr_smoke1",
		Category:  "sonarr",
		Filename:  "Evil.S03E01.nzb",
		NzbSHA256: "deadbeef-smoke",
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
		got, err := st.Get(context.Background(), j.NzoID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == job.StateReady {
			cancel()
			<-done
			if !got.LocalPath.Valid || got.LocalPath.String == "" {
				t.Errorf("expected local_path to be set on READY, got %v", got.LocalPath)
			}
			if got.BytesTotal == 0 {
				t.Errorf("expected bytes_total > 0 on READY")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	final, _ := st.Get(context.Background(), j.NzoID)
	t.Fatalf("job did not reach READY. state=%s last_error=%v", final.State, final.LastError)
}

// TestSubmitter429DoesNotIncrementAttempts confirms TorBox 429 backpressure is
// treated as transient and does NOT count against MaxSubmitAttempts.
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
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
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

	for i := 0; i < 7; i++ {
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
}

// TestPollerNameFallbackForZeroIDs covers the recovery path when TorBox returns
// queue_id=0 AND id=0 from createusenetdownload — the poller must match by
// folder name and advance the job rather than leaving it stuck SUBMITTED.
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
		Store:  st,
		Torbox: tb,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

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
		t.Errorf("state=%s want COMPLETED_TORBOX (poller should've name-matched)", got.State)
	}
	if !got.TorboxFolderName.Valid || got.TorboxFolderName.String != folder {
		t.Errorf("folder_name=%v want %q", got.TorboxFolderName, folder)
	}
}

// rateLimitedTorbox always returns 429 on create.
type rateLimitedTorbox struct{}

func (r *rateLimitedTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	return nil, &torbox.APIError{Status: 429, Detail: "60 per 1 hour"}
}
func (r *rateLimitedTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, &torbox.APIError{Status: 429}
}
func (r *rateLimitedTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, &torbox.APIError{Status: 429}
}
func (r *rateLimitedTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (r *rateLimitedTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (r *rateLimitedTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error  { return nil }
func (r *rateLimitedTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }
func (r *rateLimitedTorbox) RequestUsenetDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (r *rateLimitedTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}

// nameFallbackTorbox returns 0/0 ids from create + a completed item under the
// given folder name on subsequent mylist calls.
type nameFallbackTorbox struct {
	folderName string
}

func (t *nameFallbackTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	return &torbox.CreateResp{}, nil
}
func (t *nameFallbackTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return &torbox.CreateResp{}, nil
}
func (t *nameFallbackTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return &torbox.CreateResp{}, nil
}
func (t *nameFallbackTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return []torbox.MyListItem{
		{ID: 999, QueueID: 999, DownloadState: "completed", DownloadFinished: true, Name: t.folderName},
	}, nil
}
func (t *nameFallbackTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (t *nameFallbackTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error  { return nil }
func (t *nameFallbackTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }
func (t *nameFallbackTorbox) RequestUsenetDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (t *nameFallbackTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}

// rewriteTorbox wraps fakeTorbox so the request-dl URL points at a per-test
// httptest server. Avoids hardcoding a real CDN URL into the puller test.
type rewriteTorbox struct {
	*fakeTorbox
	base string
}

func (r *rewriteTorbox) RequestUsenetDL(_ context.Context, _, fileID int64, _ bool) (string, error) {
	return r.base + "/" + strconv.FormatInt(fileID, 10), nil
}
func (r *rewriteTorbox) RequestTorrentDL(_ context.Context, _, fileID int64, _ bool) (string, error) {
	return r.base + "/" + strconv.FormatInt(fileID, 10), nil
}

// noopLogger silences the downloader inside tests.
type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

