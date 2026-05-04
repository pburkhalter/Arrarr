package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newJob(id string) *job.Job {
	return &job.Job{
		NzoID:     id,
		Category:  "sonarr",
		Filename:  id + ".nzb",
		NzbSHA256: id + "-sha",
		NzbBlob:   []byte("dummy"),
		State:     job.StateNew,
	}
}

func TestInsertGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	j := newJob("arrarr_a")
	if err := s.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "arrarr_a")
	if err != nil {
		t.Fatal(err)
	}
	if got.NzoID != "arrarr_a" || got.State != job.StateNew {
		t.Errorf("got=%+v", got)
	}
}

func TestFindActiveBySHA(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := newJob("arrarr_a")
	a.NzbSHA256 = "shared-sha"
	if err := s.Insert(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.FindActiveBySHA(ctx, "shared-sha")
	if err != nil || got.NzoID != "arrarr_a" {
		t.Fatalf("expected dedup hit, got %v %v", got, err)
	}

	// Once terminal, no longer found.
	if err := s.Transition(ctx, "arrarr_a", Transition{
		From: job.StateNew, To: job.StateFailed,
		LastError:    strP("done"),
		CompletedAt:  timeP(time.Now()),
		ClearClaimed: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindActiveBySHA(ctx, "shared-sha"); err == nil {
		t.Error("expected ErrNotFound after terminal transition")
	}
}

func TestTransitionGuardsRace(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	j := newJob("arrarr_g")
	if err := s.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	// First transition wins.
	if err := s.Transition(ctx, "arrarr_g", Transition{From: job.StateNew, To: job.StateSubmitted}); err != nil {
		t.Fatal(err)
	}
	// Second loses — wrong from-state.
	if err := s.Transition(ctx, "arrarr_g", Transition{From: job.StateNew, To: job.StateSubmitted}); err == nil {
		t.Error("expected ErrInvalidTransition on stale from-state")
	}
}

func TestClaimRespectsNextAttempt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	j := newJob("arrarr_c")
	if err := s.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	// Mark a future next_attempt_at; claim should skip it.
	future := time.Now().Add(time.Hour)
	if err := s.AttemptFailure(ctx, j.NzoID, "later", future); err != nil {
		t.Fatal(err)
	}
	got, err := s.Claim(ctx, []job.State{job.StateNew}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 claims, got %d", len(got))
	}
}

func TestReap(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	j := newJob("arrarr_r")
	if err := s.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	// Walk through the legal transitions.
	for _, step := range []Transition{
		{From: job.StateNew, To: job.StateSubmitted},
		{From: job.StateSubmitted, To: job.StateCompletedTorbox},
	} {
		if err := s.Transition(ctx, j.NzoID, step); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := s.Transition(ctx, j.NzoID, Transition{
		From:        job.StateCompletedTorbox,
		To:          job.StateReady,
		CompletedAt: &past,
	}); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	n, err := s.Reap(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reaped %d want 1", n)
	}
}

func strP(s string) *string  { return &s }
func timeP(t time.Time) *time.Time { return &t }
