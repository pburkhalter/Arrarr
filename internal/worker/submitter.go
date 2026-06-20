package worker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pburkhalter/arrarr/internal/httpx"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

func (m *Manager) submitterLoop(ctx context.Context) {
	ticker := time.NewTicker(m.o.DispatchEvery)
	defer ticker.Stop()

	wake := m.o.Wake

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.dispatchOnce(ctx)
		case <-wake:
			m.dispatchOnce(ctx)
		}
	}
}

func (m *Manager) dispatchOnce(ctx context.Context) {
	jobs, err := m.o.Store.Claim(ctx, []job.State{job.StateNew}, m.o.WorkerPoolSize)
	if err != nil {
		m.log.Error("claim failed", "err", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j *job.Job) {
			defer wg.Done()
			m.submitOne(ctx, j)
		}(j)
	}
	wg.Wait()
}

func (m *Manager) submitOne(ctx context.Context, j *job.Job) {
	resp, err := m.callCreate(ctx, j)
	if err != nil {
		if errors.Is(err, errEmptyPayload) {
			m.log.Error("submit: empty payload", "nzo_id", j.NzoID, "source", j.Source)
			_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
				From:         job.StateNew,
				To:           job.StateFailed,
				LastError:    strPtr("empty payload"),
				CompletedAt:  nowPtr(),
				ClearClaimed: true,
			})
			return
		}
		m.handleSubmitFailure(ctx, j, err)
		return
	}
	queueID := resp.QueueID
	activeID := resp.ID
	// TorBox /torrents/createtorrent returns torrent_id (not id/queue_id);
	// /usenet/createusenetdownload may also return usenet_id on duplicates.
	// Treat any non-zero "operational" id as activeID so the poller can match.
	if activeID == 0 {
		if resp.TorrentID != 0 {
			activeID = resp.TorrentID
		} else if resp.UsenetID != 0 {
			activeID = resp.UsenetID
		}
	}
	// 0/0 means TorBox accepted upload but returned a "duplicate"-style response
	// without ids (often after a previous timeout where the NZB was still
	// processed server-side). Don't transition to SUBMITTED — the poller would
	// have nothing to match against. The poller's name-fallback recovers the
	// id later, but here we mark the submission so the job stays SUBMITTED only
	// if at least one id is set.
	if queueID == 0 && activeID == 0 {
		m.log.Warn("submit returned 0/0 ids — letting poller name-match recover", "nzo_id", j.NzoID, "filename", j.Filename)
	}
	t := store.Transition{
		From:         job.StateNew,
		To:           job.StateSubmitted,
		ClearClaimed: true,
	}
	if queueID != 0 {
		t.TorboxQueueID = &queueID
	}
	if activeID != 0 {
		t.TorboxActiveID = &activeID
	}
	if err := m.o.Store.Transition(ctx, j.NzoID, t); err != nil {
		m.log.Error("submit: transition failed", "nzo_id", j.NzoID, "err", err)
		_ = m.o.Store.Release(ctx, j.NzoID)
		return
	}
	m.log.Info("submitted", "nzo_id", j.NzoID, "queue_id", queueID, "active_id", activeID, "filename", j.Filename)
}

func (m *Manager) handleSubmitFailure(ctx context.Context, j *job.Job, err error) {
	// 429 from TorBox is transient backpressure, not a job-specific failure.
	// Don't count it against MaxSubmitAttempts and back off well past the
	// 60/hour createusenetdownload limit so retries don't burn the budget.
	if is429(err) {
		delay := 5*time.Minute + httpx.Backoff(1, time.Minute, 5*time.Minute)
		if d := torboxRetryAfter(err); d > delay {
			delay = d
		}
		next := time.Now().UTC().Add(delay)
		m.log.Warn("submit rate-limited", "nzo_id", j.NzoID, "next_in", delay)
		if e := m.o.Store.Reschedule(ctx, j.NzoID, describe(err), next); e != nil {
			m.log.Error("reschedule write", "nzo_id", j.NzoID, "err", e)
		}
		return
	}

	attempts := j.Attempts + 1
	m.log.Warn("submit failed", "nzo_id", j.NzoID, "attempt", attempts, "err", describe(err))

	if attempts >= MaxSubmitAttempts {
		_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
			From:         job.StateNew,
			To:           job.StateFailed,
			LastError:    strPtr(describe(err)),
			CompletedAt:  nowPtr(),
			ClearClaimed: true,
			IncAttempts:  true,
		})
		return
	}
	delay := httpx.Backoff(attempts, 30*time.Second, time.Hour)
	if d := torboxRetryAfter(err); d > 0 && d > delay {
		delay = d
	}
	next := time.Now().UTC().Add(delay)
	if e := m.o.Store.AttemptFailure(ctx, j.NzoID, describe(err), next); e != nil {
		m.log.Error("attempt-failure write", "nzo_id", j.NzoID, "err", e)
	}
}

func is429(err error) bool {
	var apiErr *torbox.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == 429
	}
	return false
}

func torboxRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	type retryAfterer interface{ RetryAfter() time.Duration }
	if ra, ok := err.(retryAfterer); ok { //nolint:errorlint
		return ra.RetryAfter()
	}
	return 0
}

func strPtr(s string) *string { return &s }
func nowPtr() *time.Time      { t := time.Now().UTC(); return &t }

// errEmptyPayload is returned when a NEW job has neither nzb blob nor torrent
// blob nor magnet — nothing to send to TorBox. Caller fails the job.
var errEmptyPayload = errors.New("empty payload")

// callCreate dispatches the right TorBox create-call for this job's source.
// "" or "usenet" → CreateUsenetDownload with NzbBlob.
// "torrent"      → CreateTorrentFromMagnet (if Magnet set) else CreateTorrentFromFile.
func (m *Manager) callCreate(ctx context.Context, j *job.Job) (*torbox.CreateResp, error) {
	switch j.Source {
	case "torrent":
		params := torbox.CreateTorrentParams{Name: j.Filename}
		if j.Magnet.Valid && j.Magnet.String != "" {
			return m.o.Torbox.CreateTorrentFromMagnet(ctx, j.Magnet.String, params)
		}
		if len(j.NzbBlob) == 0 {
			return nil, errEmptyPayload
		}
		return m.o.Torbox.CreateTorrentFromFile(ctx, j.Filename, j.NzbBlob, params)
	default:
		if len(j.NzbBlob) == 0 {
			return nil, errEmptyPayload
		}
		return m.o.Torbox.CreateUsenetDownload(ctx, j.Filename, j.NzbBlob, "")
	}
}
