package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/librarian"
	"github.com/pburkhalter/arrarr/internal/pathmap"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// urlRefreshTorbox returns ever-changing CDN URLs so each refresh produces a
// different streaming_url, which the test asserts on.
type urlRefreshTorbox struct {
	mu       sync.Mutex
	requests atomic.Int32
	folder   string
}

func (u *urlRefreshTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	return nil, errors.New("not used")
}
func (u *urlRefreshTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return []torbox.MyListItem{{
		ID: 99, QueueID: 11,
		DownloadFinished: true,
		DownloadState:    "completed",
		Name:             u.folder,
		Files: []torbox.MyListFile{
			{ID: 1, Name: u.folder + "/x.mkv", ShortName: "x.mkv", MimeType: "video/x-matroska"},
		},
	}}, nil
}
func (u *urlRefreshTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error { return nil }
func (u *urlRefreshTorbox) EditUsenet(_ context.Context, _ int64, _ torbox.EditUsenetParams) error {
	return nil
}

// Each call returns a unique URL so the test can prove the refresh actually happened.
func (u *urlRefreshTorbox) RequestUsenetDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	n := u.requests.Add(1)
	return "https://cdn.torbox.app/v1/" + strconv.FormatInt(int64(n), 10), nil
}

func TestURLRefreshTickRewritesSTRM(t *testing.T) {
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

	// Insert a READY job that's already been librarian-written, with an expiry
	// in the past so the sweeper will pick it up.
	folder := "Sample.Show.S01E01.WEB-DL"
	j := &job.Job{
		NzoID:     "arrarr_refresh1",
		Category:  "sonarr",
		Filename:  folder + ".nzb",
		NzbSHA256: "x",
		NzbBlob:   []byte("<n/>"),
		State:     job.StateNew,
	}
	if err := st.Insert(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	// Force-advance directly to READY w/ stale streaming_url + library entries.
	pastExpiry := time.Now().Add(-30 * time.Minute) // already expired-ish
	originalSTRM := filepath.Join(libDir, "series", folder, "x.strm")
	if err := os.MkdirAll(filepath.Dir(originalSTRM), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(originalSTRM, []byte("https://cdn.torbox.app/old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(context.Background(), `UPDATE jobs SET
		state = 'READY',
		library_writer_state = 'written',
		library_path = ?,
		streaming_url = 'https://cdn.torbox.app/old',
		streaming_url_expires_at = ?,
		torbox_active_id = 99,
		torbox_queue_id = 11,
		torbox_folder_name = ?,
		completed_at = CURRENT_TIMESTAMP,
		nzb_blob = NULL
		WHERE nzo_id = ?`, originalSTRM, pastExpiry, folder, j.NzoID); err != nil {
		t.Fatal(err)
	}

	tb := &urlRefreshTorbox{folder: folder}
	libWriter, err := librarian.New("strm", libDir, "/mnt/torbox")
	if err != nil {
		t.Fatal(err)
	}
	mgr := New(Options{
		Store:                    st,
		Torbox:                   tb,
		PathMap:                  pathmap.New("/mnt/torbox", "/torbox"),
		Logger:                   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		WorkerPoolSize:           1,
		InstanceName:             "test",
		Librarian:                libWriter,
		LocalVerifyBase:          "/mnt/torbox",
		StreamingURLRefreshAfter: 5 * time.Hour,
	})

	mgr.urlRefreshTick(context.Background())

	// STRM should now contain a NEW url that came from RequestUsenetDL.
	body, err := os.ReadFile(originalSTRM)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got == "https://cdn.torbox.app/old\n" || !startsWith(got, "https://cdn.torbox.app/v1/") {
		t.Errorf("STRM not refreshed: %q", got)
	}

	// streaming_url + expires_at on the row should advance.
	var streamURL *string
	var expires *time.Time
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT streaming_url, streaming_url_expires_at FROM jobs WHERE nzo_id = ?`,
		j.NzoID).Scan(&streamURL, &expires); err != nil {
		t.Fatal(err)
	}
	if streamURL == nil || *streamURL == "https://cdn.torbox.app/old" {
		t.Errorf("streaming_url not updated: %v", streamURL)
	}
	if expires == nil || !expires.After(time.Now()) {
		t.Errorf("streaming_url_expires_at not pushed forward: %v", expires)
	}

	// State must remain READY (refresh doesn't transition).
	got, _ := st.Get(context.Background(), j.NzoID)
	if got.State != job.StateReady {
		t.Errorf("state changed by refresh: %s", got.State)
	}
}

func TestURLRefreshTickSkipsRowsNotYetDue(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	j := &job.Job{
		NzoID:     "arrarr_notdue",
		Category:  "sonarr",
		Filename:  "x.nzb",
		NzbSHA256: "y",
		NzbBlob:   []byte("<n/>"),
		State:     job.StateNew,
	}
	_ = st.Insert(context.Background(), j)
	// Far-future expiry → sweeper should not touch this row.
	farFuture := time.Now().Add(48 * time.Hour)
	_, _ = st.DB().ExecContext(context.Background(), `UPDATE jobs SET
		state = 'READY',
		library_writer_state = 'written',
		library_path = '/lib/x.strm',
		streaming_url = 'https://still-fresh',
		streaming_url_expires_at = ?
		WHERE nzo_id = ?`, farFuture, j.NzoID)

	tb := &urlRefreshTorbox{folder: "X"}
	libWriter, _ := librarian.New("strm", t.TempDir(), "/mnt/torbox")
	mgr := New(Options{
		Store:                    st,
		Torbox:                   tb,
		Logger:                   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize:           1,
		InstanceName:             "test",
		Librarian:                libWriter,
		LocalVerifyBase:          "/mnt/torbox",
		StreamingURLRefreshAfter: 5 * time.Hour,
	})

	mgr.urlRefreshTick(context.Background())
	if got := tb.requests.Load(); got != 0 {
		t.Errorf("RequestUsenetDL was called %d times for a row that's not due", got)
	}
}

func TestWriterNeedsURLRefresh(t *testing.T) {
	cases := map[string]bool{"strm": true, "both": true, "webdav": false, "off": false, "unknown": false}
	for mode, want := range cases {
		if got := writerNeedsURLRefresh(mode); got != want {
			t.Errorf("writerNeedsURLRefresh(%q)=%v want %v", mode, got, want)
		}
	}
}

// --- Tagger retry ---

type taggerFakeTorbox struct {
	editCalls atomic.Int32
	editFail  atomic.Bool
}

func (t *taggerFakeTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	return nil, errors.New("unused")
}
func (t *taggerFakeTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (t *taggerFakeTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error { return nil }
func (t *taggerFakeTorbox) EditUsenet(_ context.Context, _ int64, _ torbox.EditUsenetParams) error {
	t.editCalls.Add(1)
	if t.editFail.Load() {
		return errors.New("not cached yet")
	}
	return nil
}
func (t *taggerFakeTorbox) RequestUsenetDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}

func TestTaggerRetryMarksTaggedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	// Submitted, untagged, has a torbox id → tagger should pick it up.
	j := &job.Job{
		NzoID:     "arrarr_lateTag",
		Category:  "sonarr",
		Filename:  "x.nzb",
		NzbSHA256: "z",
		NzbBlob:   []byte("<n/>"),
		State:     job.StateNew,
	}
	_ = st.Insert(context.Background(), j)
	_, _ = st.DB().ExecContext(context.Background(), `UPDATE jobs SET
		state = 'SUBMITTED',
		torbox_queue_id = 42
		WHERE nzo_id = ?`, j.NzoID)

	tb := &taggerFakeTorbox{}
	mgr := New(Options{
		Store:          st,
		Torbox:         tb,
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize: 1,
		InstanceName:   "patrik",
	})

	mgr.taggerRetryTick(context.Background())

	if got := tb.editCalls.Load(); got != 1 {
		t.Errorf("EditUsenet calls=%d want 1", got)
	}
	var taggedAt *time.Time
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT tagged_at FROM jobs WHERE nzo_id=?`, j.NzoID).Scan(&taggedAt); err != nil {
		t.Fatal(err)
	}
	if taggedAt == nil {
		t.Error("tagged_at should be non-NULL after successful retry")
	}

	// Second tick: row is now tagged, must NOT be picked up again.
	mgr.taggerRetryTick(context.Background())
	if got := tb.editCalls.Load(); got != 1 {
		t.Errorf("EditUsenet calls=%d after second tick — row was retagged unexpectedly", got)
	}
}

func TestTaggerRetryLeavesUntaggedOnFailure(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	j := &job.Job{
		NzoID:     "arrarr_failtag",
		Category:  "radarr",
		Filename:  "m.nzb",
		NzbSHA256: "w",
		NzbBlob:   []byte("<n/>"),
		State:     job.StateNew,
	}
	_ = st.Insert(context.Background(), j)
	_, _ = st.DB().ExecContext(context.Background(), `UPDATE jobs SET
		state = 'DOWNLOADING',
		torbox_active_id = 7
		WHERE nzo_id = ?`, j.NzoID)

	tb := &taggerFakeTorbox{}
	tb.editFail.Store(true) // TorBox keeps rejecting (item not cached yet)
	mgr := New(Options{
		Store:          st,
		Torbox:         tb,
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize: 1,
		InstanceName:   "patrik",
	})

	mgr.taggerRetryTick(context.Background())
	if got := tb.editCalls.Load(); got != 1 {
		t.Errorf("EditUsenet calls=%d want 1", got)
	}

	// tagged_at must remain NULL so future ticks try again.
	var taggedAt *time.Time
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT tagged_at FROM jobs WHERE nzo_id=?`, j.NzoID).Scan(&taggedAt); err != nil {
		t.Fatal(err)
	}
	if taggedAt != nil {
		t.Errorf("tagged_at should remain NULL on failure; got %v", taggedAt)
	}

	// Flip success and re-tick — should now mark tagged.
	tb.editFail.Store(false)
	mgr.taggerRetryTick(context.Background())
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT tagged_at FROM jobs WHERE nzo_id=?`, j.NzoID).Scan(&taggedAt); err != nil {
		t.Fatal(err)
	}
	if taggedAt == nil {
		t.Error("tagged_at should populate after edit success")
	}
}

func TestTaggerRetrySkipsAlreadyTagged(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	j := &job.Job{
		NzoID:     "arrarr_done",
		Category:  "sonarr",
		Filename:  "x.nzb",
		NzbSHA256: "v",
		NzbBlob:   []byte("<n/>"),
		State:     job.StateNew,
	}
	_ = st.Insert(context.Background(), j)
	_, _ = st.DB().ExecContext(context.Background(), `UPDATE jobs SET
		state = 'SUBMITTED',
		torbox_queue_id = 42,
		tagged_at = CURRENT_TIMESTAMP
		WHERE nzo_id = ?`, j.NzoID)

	tb := &taggerFakeTorbox{}
	mgr := New(Options{
		Store:          st,
		Torbox:         tb,
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize: 1,
		InstanceName:   "patrik",
	})

	mgr.taggerRetryTick(context.Background())
	if got := tb.editCalls.Load(); got != 0 {
		t.Errorf("already-tagged row should be ignored; EditUsenet calls=%d", got)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
