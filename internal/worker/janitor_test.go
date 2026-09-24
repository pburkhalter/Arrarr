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

func newJanitorManager(t *testing.T) (*store.Store, *frozenTorbox) {
	t.Helper()
	st := newStore(t)
	tb := &frozenTorbox{}
	New(Options{Store: st, Torbox: tb, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return st, tb
}

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

// Until Sep 2026 only stalled and timed-out jobs deleted their TorBox entry.
// Every normally finished download stayed in the account's list forever:
// 1 001 entries, 941 of them dead, fetched in full on every poll and pull.
func TestReadyJobReleasesTorboxEntry(t *testing.T) {
	st, tb := newJanitorManager(t)
	insertWithTorboxID(t, st, "arrarr_done", "COMPLETED_TORBOX", 501)

	if err := st.MarkLocalReady(context.Background(), "arrarr_done", "/downloads/sonarr/x", 7); err != nil {
		t.Fatal(err)
	}
	if len(tb.deleted) != 1 || tb.deleted[0] != 501 {
		t.Fatalf("deleted=%v want [501] after READY", tb.deleted)
	}
}

// A cancel from Sonarr (SAB queue delete) ends the TorBox download too.
func TestCanceledJobReleasesTorboxEntry(t *testing.T) {
	st, tb := newJanitorManager(t)
	insertWithTorboxID(t, st, "arrarr_cancel", "DOWNLOADING", 502)

	if err := st.Transition(context.Background(), "arrarr_cancel", store.Transition{
		From: job.StateDownloading, To: job.StateCanceled, LastError: strPtr("canceled by client"), CompletedAt: nowPtr(),
	}); err != nil {
		t.Fatal(err)
	}
	if len(tb.deleted) != 1 || tb.deleted[0] != 502 {
		t.Fatalf("deleted=%v want [502] after CANCELED", tb.deleted)
	}
}

// A job TorBox never accepted has nothing to release; a non-terminal
// transition must not touch TorBox either.
func TestJanitorSkipsJobsWithoutEntry(t *testing.T) {
	st, tb := newJanitorManager(t)
	insertWithTorboxID(t, st, "arrarr_noid", "NEW", 0)
	insertWithTorboxID(t, st, "arrarr_live", "SUBMITTED", 503)
	ctx := context.Background()

	if err := st.Transition(ctx, "arrarr_noid", store.Transition{From: job.StateNew, To: job.StateFailed,
		LastError: strPtr("submit exhausted"), CompletedAt: nowPtr()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Transition(ctx, "arrarr_live", store.Transition{From: job.StateSubmitted, To: job.StateDownloading}); err != nil {
		t.Fatal(err)
	}
	if len(tb.deleted) != 0 {
		t.Fatalf("deleted=%v want none", tb.deleted)
	}
}

var _ = torbox.MyListItem{}
