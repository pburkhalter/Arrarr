package worker

import (
	"context"
	"strings"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

func (m *Manager) pollerLoop(ctx context.Context) {
	ticker := time.NewTicker(m.o.PollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollOnce(ctx)
		}
	}
}

func (m *Manager) pollOnce(ctx context.Context) {
	inflight, err := m.o.Store.ListByStates(ctx,
		[]job.State{job.StateSubmitted, job.StateDownloading}, 1000)
	if err != nil {
		m.log.Error("poll: list failed", "err", err)
		return
	}
	if len(inflight) == 0 {
		return
	}
	items, err := m.o.Torbox.MyList(ctx, true)
	if err != nil {
		m.log.Error("poll: mylist failed", "err", describe(err))
		return
	}

	// Index by both ids — TorBox swaps queue_id for id mid-flight.
	byID := make(map[int64]*torbox.MyListItem, len(items)*2)
	for i := range items {
		it := &items[i]
		if it.ID != 0 {
			byID[it.ID] = it
		}
		if it.QueueID != 0 {
			byID[it.QueueID] = it
		}
	}

	for _, j := range inflight {
		var item *torbox.MyListItem
		if j.TorboxActiveID.Valid {
			item = byID[j.TorboxActiveID.Int64]
		}
		if item == nil && j.TorboxQueueID.Valid {
			item = byID[j.TorboxQueueID.Int64]
		}
		if item == nil {
			m.handlePollMissing(ctx, j)
			continue
		}
		m.applyMyListItem(ctx, j, item)
	}
}

func (m *Manager) applyMyListItem(ctx context.Context, j *job.Job, it *torbox.MyListItem) {
	var activeIDPtr *int64
	if it.ID != 0 && (!j.TorboxActiveID.Valid || j.TorboxActiveID.Int64 != it.ID) {
		id := it.ID
		activeIDPtr = &id
	}
	var folderPtr *string
	if name := it.FolderName(); name != "" && (!j.TorboxFolderName.Valid || j.TorboxFolderName.String != name) {
		s := name
		folderPtr = &s
	}
	if activeIDPtr != nil || folderPtr != nil {
		if err := m.o.Store.SetTorboxIDs(ctx, j.NzoID, activeIDPtr, folderPtr); err != nil {
			m.log.Warn("poll: setIDs failed", "nzo_id", j.NzoID, "err", err)
		}
	}

	if it.IsFailure() {
		m.log.Warn("poll: torbox failure", "nzo_id", j.NzoID, "state", it.DownloadState)
		_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
			From:        j.State,
			To:          job.StateFailed,
			LastError:   strPtr("torbox state: " + it.DownloadState),
			CompletedAt: nowPtr(),
		})
		return
	}
	if it.IsTerminal() {
		m.log.Info("poll: torbox complete", "nzo_id", j.NzoID, "folder", it.FolderName())
		_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
			From: j.State,
			To:   job.StateCompletedTorbox,
		})
		return
	}
	if j.State == job.StateSubmitted && (strings.EqualFold(it.DownloadState, "downloading") ||
		it.DownloadSpeed > 0 || it.Progress > 0) {
		_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
			From: job.StateSubmitted,
			To:   job.StateDownloading,
		})
	}
	if time.Since(j.CreatedAt) > MaxPollDuration {
		m.log.Warn("poll: timeout reached", "nzo_id", j.NzoID, "age", time.Since(j.CreatedAt))
		_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
			From:        j.State,
			To:          job.StateFailed,
			LastError:   strPtr("polling timeout: torbox did not complete in 24h"),
			CompletedAt: nowPtr(),
		})
	}
}

func (m *Manager) handlePollMissing(ctx context.Context, j *job.Job) {
	const grace = 5 * time.Minute
	if time.Since(j.CreatedAt) < grace {
		return
	}
	if time.Since(j.UpdatedAt) > MaxPollDuration {
		m.log.Warn("poll: vanished from torbox", "nzo_id", j.NzoID, "age", time.Since(j.CreatedAt))
		_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
			From:        j.State,
			To:          job.StateFailed,
			LastError:   strPtr("torbox lost the download"),
			CompletedAt: nowPtr(),
		})
	}
}
