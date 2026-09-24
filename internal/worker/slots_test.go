package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

func insertWithTorboxID(t *testing.T, st *store.Store, nzo, state string, torboxID int64) {
	t.Helper()
	ctx := context.Background()
	j := &job.Job{NzoID: nzo, Category: "sonarr", Filename: nzo + ".nzb",
		NzbSHA256: nzo, NzbBlob: []byte("nzb"), State: job.StateNew}
	if err := st.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE jobs SET state=?, torbox_active_id=NULLIF(?, 0) WHERE nzo_id=?`, state, torboxID, nzo); err != nil {
		t.Fatal(err)
	}
}

// The TorBox account is shared. A download that finished, failed on TorBox's
// side or was canceled by Sonarr holds no slot and stays in the account —
// only a hung download (stall, 24h ceiling) is deleted, see
// TestFrozenDownloadIsFailedAndSlotReleased.
func TestFinishedDownloadsKeepTheirTorboxEntry(t *testing.T) {
	st := newStore(t)
	tb := &frozenTorbox{item: torbox.MyListItem{ID: 503, Name: "arrarr_failed", DownloadState: "failed (Aborted, cannot be completed)"}}
	mgr := New(Options{Store: st, Torbox: tb, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()

	insertWithTorboxID(t, st, "arrarr_done", "COMPLETED_TORBOX", 501)
	if err := st.MarkLocalReady(ctx, "arrarr_done", "/downloads/sonarr/x", 7); err != nil {
		t.Fatal(err)
	}
	insertWithTorboxID(t, st, "arrarr_cancel", "DOWNLOADING", 502)
	if err := st.Transition(ctx, "arrarr_cancel", store.Transition{
		From: job.StateDownloading, To: job.StateCanceled, LastError: strPtr("canceled by client"), CompletedAt: nowPtr(),
	}); err != nil {
		t.Fatal(err)
	}
	insertWithTorboxID(t, st, "arrarr_failed", "DOWNLOADING", 503)
	mgr.pollOnce(ctx) // TorBox reports the download as failed

	if j, _ := st.Get(ctx, "arrarr_failed"); j.State != job.StateFailed {
		t.Fatalf("state=%s want FAILED", j.State)
	}
	if len(tb.deleted) != 0 {
		t.Fatalf("deleted=%v — finished, canceled and failed downloads must stay in the shared account", tb.deleted)
	}
}
