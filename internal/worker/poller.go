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

	usenetItems, err := m.o.Torbox.MyList(ctx, true)
	if err != nil {
		m.log.Error("poll: mylist (usenet) failed", "err", describe(err))
		return
	}
	torrentItems, terr := m.o.Torbox.MyListTorrents(ctx, true)
	if terr != nil {
		// soft-fail — usenet polling still works without torrent visibility
		m.log.Warn("poll: mylist (torrents) failed", "err", describe(terr))
	}

	usenetByID := indexByID(usenetItems)
	torrentByID := indexByID(torrentItems)

	for _, j := range inflight {
		var pool []torbox.MyListItem
		var idx map[int64]*torbox.MyListItem
		switch j.Source {
		case "torrent":
			pool, idx = torrentItems, torrentByID
		default:
			pool, idx = usenetItems, usenetByID
		}

		var item *torbox.MyListItem
		if j.TorboxActiveID.Valid {
			item = idx[j.TorboxActiveID.Int64]
		}
		if item == nil && j.TorboxQueueID.Valid {
			item = idx[j.TorboxQueueID.Int64]
		}
		// Fallback: when the submit response had 0/0 ids (TorBox returned a
		// duplicate-style response without ids), match by name. TorBox's
		// `name` field is the folder name, which equals filename minus .nzb.
		if item == nil {
			searchName := strings.TrimSuffix(j.Filename, ".nzb")
			for i := range pool {
				if pool[i].Name == searchName {
					item = &pool[i]
					break
				}
			}
		}
		if item == nil {
			m.handlePollMissing(ctx, j)
			continue
		}
		m.applyMyListItem(ctx, j, item)
	}
}

func indexByID(items []torbox.MyListItem) map[int64]*torbox.MyListItem {
	out := make(map[int64]*torbox.MyListItem, len(items)*2)
	for i := range items {
		it := &items[i]
		if it.ID != 0 {
			out[it.ID] = it
		}
		if it.QueueID != 0 {
			out[it.QueueID] = it
		}
	}
	return out
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
	if strings.EqualFold(it.DownloadState, "downloading") &&
		it.Progress == 0 && it.DownloadSpeed == 0 &&
		time.Since(j.CreatedAt) > MaxStallDuration {
		m.log.Warn("poll: torbox stalled at 0%",
			"nzo_id", j.NzoID, "age", time.Since(j.CreatedAt))
		_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
			From:        j.State,
			To:          job.StateFailed,
			LastError:   strPtr("torbox stalled: no progress (missing usenet articles)"),
			CompletedAt: nowPtr(),
		})
		return
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
	if time.Since(j.UpdatedAt) > MaxMissingDuration {
		m.log.Warn("poll: vanished from torbox",
			"nzo_id", j.NzoID, "age", time.Since(j.CreatedAt), "idle", time.Since(j.UpdatedAt))
		_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
			From:        j.State,
			To:          job.StateFailed,
			LastError:   strPtr("torbox lost the download (absent from mylist)"),
			CompletedAt: nowPtr(),
		})
	}
}
