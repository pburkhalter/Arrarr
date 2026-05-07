package sab

import (
	"context"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

type storeReader interface {
	Get(ctx context.Context, nzoID string) (*job.Job, error)
	FindActiveBySHA(ctx context.Context, sha string) (*job.Job, error)
	FindByTorboxID(ctx context.Context, id int64) (*job.Job, error)
	FindByFolderName(ctx context.Context, name string) (*job.Job, error)
	ListByStates(ctx context.Context, states []job.State, limit int) ([]*job.Job, error)
	ListReady(ctx context.Context, limit int) ([]*job.Job, error)
	PushoverAlreadySent(ctx context.Context, nzoID string) (bool, error)
	JobOrigin(ctx context.Context, nzoID string) (string, error)
	FetchStats(ctx context.Context, recentLimit int) (*store.Stats, error)
}

type storeWriter interface {
	Insert(ctx context.Context, j *job.Job) error
	Delete(ctx context.Context, nzoID string) error
	Transition(ctx context.Context, nzoID string, t store.Transition) error
	MarkPushoverSent(ctx context.Context, nzoID string) error
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
func (s storeAdapter) FindByTorboxID(ctx context.Context, id int64) (*job.Job, error) {
	return s.Store.FindByTorboxID(ctx, id)
}
func (s storeAdapter) FindByFolderName(ctx context.Context, name string) (*job.Job, error) {
	return s.Store.FindByFolderName(ctx, name)
}
func (s storeAdapter) PushoverAlreadySent(ctx context.Context, nzoID string) (bool, error) {
	return s.Store.PushoverAlreadySent(ctx, nzoID)
}
func (s storeAdapter) JobOrigin(ctx context.Context, nzoID string) (string, error) {
	return s.Store.JobOrigin(ctx, nzoID)
}
func (s storeAdapter) MarkPushoverSent(ctx context.Context, nzoID string) error {
	return s.Store.MarkPushoverSent(ctx, nzoID)
}
func (s storeAdapter) FetchStats(ctx context.Context, recentLimit int) (*store.Stats, error) {
	return s.Store.FetchStats(ctx, recentLimit)
}

func Adapt(s *store.Store) Store { return storeAdapter{s} }
