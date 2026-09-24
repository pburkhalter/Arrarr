package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// The 24h ceiling counted from created_at, so a job that queued in NEW for a
// day behind TorBox's active limit was failed minutes after TorBox finally
// started it. It must count from the SUBMITTED stamp.
func TestPollTimeoutCountsFromSubmission(t *testing.T) {
	st := newStore(t)
	tb := &frozenTorbox{item: torbox.MyListItem{
		ID: 61, Name: "arrarr_late", DownloadState: "downloading", Progress: 0.2, Size: 1000,
	}}
	mgr := New(Options{Store: st, Torbox: tb, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()

	// Created 30h ago, submitted an hour ago, moving right now.
	insertDownloading(t, st, "arrarr_late", 61, 30*time.Hour, 0)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE jobs SET submitted_at=datetime('now','-1 hour') WHERE nzo_id='arrarr_late'`); err != nil {
		t.Fatal(err)
	}
	mgr.pollOnce(ctx)

	j, err := st.Get(ctx, "arrarr_late")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != job.StateDownloading {
		t.Fatalf("state=%s want DOWNLOADING — only one hour in flight", j.State)
	}
	if len(tb.deleted) != 0 {
		t.Fatalf("deleted=%v want none", tb.deleted)
	}
}

func TestSubmittedTransitionStampsSubmittedAt(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	j := &job.Job{NzoID: "arrarr_stamp", Category: "sonarr", Filename: "x.nzb",
		NzbSHA256: "stamp", NzbBlob: []byte("nzb"), State: job.StateNew}
	if err := st.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, "arrarr_stamp")
	if got.SubmittedAt.Valid {
		t.Fatal("submitted_at set before submission")
	}
	if err := st.Transition(ctx, "arrarr_stamp", transitionTo(job.StateNew, job.StateSubmitted)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Get(ctx, "arrarr_stamp")
	if !got.SubmittedAt.Valid || time.Since(got.SubmittedAt.Time) > time.Minute {
		t.Fatalf("submitted_at = %v, want just now", got.SubmittedAt)
	}
	if got.InFlightSince() != got.SubmittedAt.Time {
		t.Fatal("InFlightSince must prefer the submission stamp")
	}
}
