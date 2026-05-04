package sab

import (
	"context"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

type storeReader interface {
	Get(ctx context.Context, nzoID string) (*job.Job, error)
	FindActiveBySHA(ctx context.Context, sha string) (*job.Job, error)
	ListByStates(ctx context.Context, states []job.State, limit int) ([]*job.Job, error)
	ListReady(ctx context.Context, limit int) ([]*job.Job, error)
}

type storeWriter interface {
	Insert(ctx context.Context, j *job.Job) error
	Delete(ctx context.Context, nzoID string) error
	Transition(ctx context.Context, nzoID string, t store.Transition) error
}

var _ Store = (*storeAdapter)(nil)

type storeAdapter struct{ *store.Store }

func (s storeAdapter) FindActiveBySHA(ctx context.Context, sha string) (*job.Job, error) {
	return s.Store.FindActiveBySHA(ctx, sha)
}
func (s storeAdapter) Get(ctx context.Context, nzoID string) (*job.Job, error) {
	return s.Store.Get(ctx, nzoID)
}
func (s storeAdapter) ListByStates(ctx context.Context, states []job.State, limit int) ([]*job.Job, error) {
	return s.Store.ListByStates(ctx, states, limit)
}
func (s storeAdapter) ListReady(ctx context.Context, limit int) ([]*job.Job, error) {
	return s.Store.ListReady(ctx, limit)
}
func (s storeAdapter) Insert(ctx context.Context, j *job.Job) error { return s.Store.Insert(ctx, j) }
func (s storeAdapter) Delete(ctx context.Context, nzoID string) error {
	return s.Store.Delete(ctx, nzoID)
}
func (s storeAdapter) Transition(ctx context.Context, nzoID string, t store.Transition) error {
	return s.Store.Transition(ctx, nzoID, t)
}

func Adapt(s *store.Store) Store { return storeAdapter{s} }
