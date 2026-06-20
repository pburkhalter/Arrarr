package qbit

import (
	"context"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

// JobStore is the subset of internal/store the qBit shim needs. Keeping this
// narrow (vs. importing *store.Store directly) makes the handlers trivial to
// fake in tests and decouples the shim from columns it doesn't read.
type JobStore interface {
	// AddTorrent inserts a fresh job whose NzbSHA256 doubles as the BT info-hash
	// (uppercase hex) so /torrents/info?hashes=... lookups are deterministic.
	AddTorrent(ctx context.Context, j *job.Job) error

	// GetByHash looks a job up by BT info-hash (case-insensitive hex). qBit's
	// pause/resume/delete take comma-separated hashes, not nzo_ids.
	GetByHash(ctx context.Context, hash string) (*job.Job, error)

	// ListByCategory returns active+terminal jobs for the category filter qBit
	// clients pass (e.g. ?category=sonarr). Empty string = all.
	ListByCategory(ctx context.Context, category string) ([]*job.Job, error)

	// Delete removes a job. Caller decides whether to also nuke local files.
	Delete(ctx context.Context, nzoID string) error

	// UpdateState is a coarse-grained state setter used by pause/resume/cancel.
	// The qBit shim doesn't reach for the full Transition struct — it only
	// needs to flag jobs as canceled / record a fail reason.
	UpdateState(ctx context.Context, nzoID, state, lastError string) error
}

// storeAdapter wraps *store.Store to satisfy JobStore. Same pattern as
// internal/sab/store_iface.go's storeAdapter.
type storeAdapter struct{ s *store.Store }

// Adapt returns a JobStore backed by the real *store.Store.
func Adapt(s *store.Store) JobStore { return &storeAdapter{s: s} }

func (a *storeAdapter) AddTorrent(ctx context.Context, j *job.Job) error {
	return a.s.Insert(ctx, j)
}

func (a *storeAdapter) GetByHash(ctx context.Context, hash string) (*job.Job, error) {
	// We store the info-hash in nzb_sha256; FindActiveBySHA covers the common
	// case (job still mid-flight). The fallback to listing terminal rows is
	// done by the caller via ListByCategory when needed.
	return a.s.FindActiveBySHA(ctx, hash)
}

func (a *storeAdapter) ListByCategory(ctx context.Context, category string) ([]*job.Job, error) {
	// Pull every non-terminal + terminal row, then filter in-process. Cheaper
	// than two queries when the shim is the only reader; the dataset is small
	// (single user, < 1k rows in practice).
	active, err := a.s.ListByStates(ctx, job.ActiveStates(), 1000)
	if err != nil {
		return nil, err
	}
	done, err := a.s.ListReady(ctx, 1000)
	if err != nil {
		return nil, err
	}
	all := append(active, done...)
	if category == "" {
		return all, nil
	}
	out := all[:0]
	for _, j := range all {
		if j.Category == category {
			out = append(out, j)
		}
	}
	return out, nil
}

func (a *storeAdapter) Delete(ctx context.Context, nzoID string) error {
	return a.s.Delete(ctx, nzoID)
}

func (a *storeAdapter) UpdateState(ctx context.Context, nzoID, state, lastError string) error {
	cur, err := a.s.Get(ctx, nzoID)
	if err != nil {
		return err
	}
	to := job.State(state)
	if !job.CanTransition(cur.State, to) {
		// No-op when the requested state isn't reachable — qBit clients spam
		// pause/resume even on terminal rows.
		return nil
	}
	t := store.Transition{From: cur.State, To: to}
	if lastError != "" {
		t.LastError = &lastError
	}
	if to == job.StateCanceled || to == job.StateFailed || to == job.StateReady {
		now := nowUTC()
		t.CompletedAt = &now
	}
	return a.s.Transition(ctx, nzoID, t)
}
