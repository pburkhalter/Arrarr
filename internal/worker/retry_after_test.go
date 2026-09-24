package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// The Retry-After TorBox sends with a 429 was parsed into APIError and then
// looked up as a method that does not exist — never honoured.
func TestTorboxRetryAfterReadsTheField(t *testing.T) {
	err := fmt.Errorf("submit: %w", &torbox.APIError{Status: 429, RetryAfter: 2 * time.Hour})
	if got := torboxRetryAfter(err); got != 2*time.Hour {
		t.Fatalf("retry-after = %s, want 2h", got)
	}
	if got := torboxRetryAfter(fmt.Errorf("plain")); got != 0 {
		t.Fatalf("retry-after for a plain error = %s, want 0", got)
	}
}

func TestRateLimitedSubmitWaitsForRetryAfter(t *testing.T) {
	st := newStore(t)
	m := New(Options{Store: st, Torbox: &frozenTorbox{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx := context.Background()
	j := &job.Job{NzoID: "arrarr_429", Category: "sonarr", Filename: "x.nzb",
		NzbSHA256: "429", NzbBlob: []byte("nzb"), State: job.StateNew}
	if err := st.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}

	m.handleSubmitFailure(ctx, j, &torbox.APIError{Status: 429, Code: "RATE_LIMIT_EXCEEDED", RetryAfter: 2 * time.Hour})

	got, err := st.Get(ctx, "arrarr_429")
	if err != nil {
		t.Fatal(err)
	}
	if !got.NextAttemptAt.Valid || time.Until(got.NextAttemptAt.Time) < 119*time.Minute {
		t.Fatalf("next attempt %v, want at least 2h out (Retry-After)", got.NextAttemptAt)
	}
	if got.Attempts != 0 {
		t.Fatalf("attempts = %d, a rate limit must not count", got.Attempts)
	}
}
