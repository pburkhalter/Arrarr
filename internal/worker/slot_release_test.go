package worker

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// frozenTorbox reports a download stuck partway through — non-zero progress and
// a stale non-zero speed, which is how an abandoned usenet job actually looks.
// It records every control call so tests can assert the slot was released.
type frozenTorbox struct {
	mu       sync.Mutex
	item     torbox.MyListItem
	deleted  []int64
	controls []string
}

func (f *frozenTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	return &torbox.CreateResp{}, nil
}
func (f *frozenTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return &torbox.CreateResp{}, nil
}
func (f *frozenTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return &torbox.CreateResp{}, nil
}
func (f *frozenTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []torbox.MyListItem{f.item}, nil
}
func (f *frozenTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (f *frozenTorbox) ControlUsenet(_ context.Context, id int64, op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controls = append(f.controls, op)
	if op == "delete" {
		f.deleted = append(f.deleted, id)
	}
	return nil
}
func (f *frozenTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }
func (f *frozenTorbox) RequestUsenetDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (f *frozenTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/arrarr.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

// insertDownloading creates a DOWNLOADING job whose timestamps are aged by the
// given amounts, mirroring a job that has been in flight for a while.
func insertDownloading(t *testing.T, st *store.Store, nzo string, torboxID int64, age, idle time.Duration) {
	t.Helper()
	ctx := context.Background()
	j := &job.Job{
		NzoID: nzo, Category: "sonarr", Filename: nzo + ".nzb",
		NzbSHA256: nzo, NzbBlob: []byte("nzb"), State: job.StateDownloading,
	}
	if err := st.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	secs := func(d time.Duration) string { return "-" + strconv.Itoa(int(d.Seconds())) + " seconds" }
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE jobs SET state='DOWNLOADING', torbox_active_id=?,
		 created_at=datetime('now', ?), updated_at=datetime('now', ?) WHERE nzo_id=?`,
		torboxID, secs(age), secs(idle), nzo); err != nil {
		t.Fatal(err)
	}
}

// The Sep 2026 outage: a download frozen at 41% held a TorBox slot for 15 days.
// The old rule only recognised a stall at exactly 0% progress and zero speed, so
// this shape survived until the 24h timeout — and even then nothing deleted the
// remote entry, so the slot never came back.
//
// Two passes, because that is the honest minimum: with no earlier reading a
// download at 41% is indistinguishable from one that just got there.
func TestFrozenDownloadIsFailedAndSlotReleased(t *testing.T) {
	st := newStore(t)
	tb := &frozenTorbox{item: torbox.MyListItem{
		ID: 2383391, Name: "arrarr_frozen", DownloadState: "downloading",
		Progress: 0.41, DownloadSpeed: 12345, Size: 1000,
	}}
	mgr := New(Options{Store: st, Torbox: tb, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	ctx := context.Background()
	insertDownloading(t, st, "arrarr_frozen", 2383391, MaxStallDuration+time.Hour, MaxStallDuration+time.Hour)
	mgr.pollOnce(ctx) // first sighting: records 410/1000
	if j, _ := st.Get(ctx, "arrarr_frozen"); j.State != job.StateDownloading {
		t.Fatalf("after first sighting state=%s want DOWNLOADING", j.State)
	}
	// A stall window passes with the same 41% reported back.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE jobs SET updated_at=datetime('now','-2 hours') WHERE nzo_id='arrarr_frozen'`); err != nil {
		t.Fatal(err)
	}
	mgr.pollOnce(ctx)

	j, err := st.Get(ctx, "arrarr_frozen")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != job.StateFailed {
		t.Errorf("state=%s want FAILED (frozen at 41%% must not survive on a stale speed)", j.State)
	}
	if len(tb.deleted) != 1 || tb.deleted[0] != 2383391 {
		t.Errorf("deleted=%v want [2383391] — a local state change does not free the TorBox slot", tb.deleted)
	}
}

// A download that is still moving must survive, however long it has been going.
func TestMovingDownloadIsNotFailed(t *testing.T) {
	st := newStore(t)
	tb := &frozenTorbox{item: torbox.MyListItem{
		ID: 42, Name: "arrarr_moving", DownloadState: "downloading",
		Progress: 0.5, DownloadSpeed: 0, Size: 1000,
	}}
	mgr := New(Options{Store: st, Torbox: tb, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	// Long in flight and idle on paper — but this pass observes new progress.
	insertDownloading(t, st, "arrarr_moving", 42, MaxStallDuration+time.Hour, MaxStallDuration+time.Hour)
	mgr.pollOnce(context.Background())

	j, err := st.Get(context.Background(), "arrarr_moving")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != job.StateDownloading {
		t.Errorf("state=%s want DOWNLOADING — progress moved this pass", j.State)
	}
	if j.BytesDownloaded != 500 {
		t.Errorf("bytes_downloaded=%d want 500 (progress must be persisted to detect freezes)", j.BytesDownloaded)
	}
	if len(tb.deleted) != 0 {
		t.Errorf("deleted=%v want none", tb.deleted)
	}
}

// activeLimitTorbox refuses every create the way a full TorBox account does:
// HTTP 500 with an ACTIVE_LIMIT envelope.
type activeLimitTorbox struct{ calls int }

func (a *activeLimitTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	a.calls++
	return nil, &torbox.APIError{Status: 500, Code: "ACTIVE_LIMIT",
		Detail: "You have reached your active download limit of 10."}
}
func (a *activeLimitTorbox) CreateTorrentFromFile(_ context.Context, _ string, _ []byte, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (a *activeLimitTorbox) CreateTorrentFromMagnet(_ context.Context, _ string, _ torbox.CreateTorrentParams) (*torbox.CreateResp, error) {
	return nil, nil
}
func (a *activeLimitTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (a *activeLimitTorbox) MyListTorrents(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (a *activeLimitTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error  { return nil }
func (a *activeLimitTorbox) ControlTorrent(_ context.Context, _ int64, _ string) error { return nil }
func (a *activeLimitTorbox) RequestUsenetDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (a *activeLimitTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}

// A full account is backpressure, not a bad release. Failing the job reports a
// Failed grab to Sonarr/Radarr, which blocklists a good release and grabs a
// replacement that hits the same wall — 326 such failures in 15 days in Sep 2026.
func TestActiveLimitReschedulesInsteadOfFailing(t *testing.T) {
	st := newStore(t)
	tb := &activeLimitTorbox{}
	mgr := New(Options{Store: st, Torbox: tb, WorkerPoolSize: 4,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()

	j := &job.Job{NzoID: "arrarr_full", Category: "radarr", Filename: "Movie.nzb",
		NzbSHA256: "sha", NzbBlob: []byte("nzb"), State: job.StateNew}
	if err := st.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}

	// Several rounds: attempts must never accumulate towards MaxSubmitAttempts.
	for i := 0; i < MaxSubmitAttempts+2; i++ {
		if _, err := st.DB().ExecContext(ctx,
			`UPDATE jobs SET next_attempt_at=NULL, claimed_at=NULL WHERE nzo_id='arrarr_full'`); err != nil {
			t.Fatal(err)
		}
		mgr.dispatchOnce(ctx)
	}

	got, err := st.Get(ctx, "arrarr_full")
	if err != nil {
		t.Fatal(err)
	}
	if got.State == job.StateFailed {
		t.Errorf("state=FAILED — a full account must not be reported as a failed grab")
	}
	if got.State != job.StateNew {
		t.Errorf("state=%s want NEW (queued for a later slot)", got.State)
	}
	if got.Attempts != 0 {
		t.Errorf("attempts=%d want 0 — backpressure must not burn the job's retry budget", got.Attempts)
	}
}
