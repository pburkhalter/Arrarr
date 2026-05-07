package worker

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/pburkhalter/arrarr/internal/store"
)

// mirrorLoop synthesizes "mirror" jobs from /usenet/mylist so Jellyfin sees
// every download on the shared TorBox account, not just Arrarr-initiated ones.
// Pushover stays gated to origin='self' (the webhook handler), so mirroring
// doesn't generate notifications for other users' downloads.
func (m *Manager) mirrorLoop(ctx context.Context) {
	if !m.o.MirrorEnabled {
		return
	}
	if m.o.Librarian == nil {
		// Mirror rows are useless without a librarian: their entire purpose
		// is to populate the structured /library tree.
		m.log.Warn("mirror: enabled but librarian is nil — disabling")
		return
	}
	interval := m.o.MirrorPollInterval
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	// First sweep on startup, then on a ticker. The librarian will catch up
	// with any rows we insert; no need to wait a full interval for the first
	// pass.
	m.mirrorTick(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mirrorTick(ctx)
		}
	}
}

func (m *Manager) mirrorTick(ctx context.Context) {
	items, err := m.o.Torbox.MyList(ctx, false)
	if err != nil {
		m.log.Warn("mirror: mylist failed (will retry)", "err", describe(err))
		return
	}
	// Only mirror downloads that are actually playable. Queued/downloading
	// rows would create empty library entries; we wait until they finish.
	liveIDs := make([]int64, 0, len(items))
	inserted, skipped := 0, 0
	for _, it := range items {
		id := it.ID
		if id == 0 {
			id = it.QueueID
		}
		if id == 0 {
			continue
		}
		liveIDs = append(liveIDs, id)
		if !it.IsTerminal() || it.Name == "" {
			continue
		}
		spec := store.MirrorJobSpec{
			TorboxID:   id,
			QueueID:    it.QueueID,
			FolderName: it.Name,
			Category:   sniffCategory(it.Name),
			SizeBytes:  it.Size,
		}
		_, isNew, err := m.o.Store.UpsertMirrorJob(ctx, spec)
		if err != nil {
			m.log.Warn("mirror: upsert failed", "id", id, "name", it.Name, "err", err)
			continue
		}
		if isNew {
			inserted++
		} else {
			skipped++
		}
	}

	// Orphan cleanup. Skip when the API returned no live IDs at all — that's
	// almost always a transient blip and we don't want to nuke every mirror
	// row on a blank response.
	if len(liveIDs) > 0 {
		m.mirrorOrphanSweep(ctx, liveIDs)
	}

	if inserted > 0 || skipped > 0 {
		m.log.Info("mirror: sweep complete",
			"inserted", inserted, "already_present", skipped, "live_ids", len(liveIDs))
	}
}

// mirrorOrphanSweep deletes mirror rows whose TorBox id is no longer present
// in mylist, plus the corresponding library file (if any). Self-origin rows
// are never touched.
func (m *Manager) mirrorOrphanSweep(ctx context.Context, liveIDs []int64) {
	orphans, err := m.o.Store.ListMirrorOrphans(ctx, liveIDs)
	if err != nil {
		m.log.Warn("mirror: orphan list failed", "err", err)
		return
	}
	for _, o := range orphans {
		// Remove the on-disk library entry first (best-effort). Even if we
		// can't (file moved, perms), still remove the DB row so the next
		// sweep doesn't report it again.
		if o.LibraryPath.Valid && o.LibraryPath.String != "" {
			if err := os.Remove(o.LibraryPath.String); err != nil && !os.IsNotExist(err) {
				m.log.Warn("mirror: cleanup file failed",
					"nzo_id", o.NzoID, "path", o.LibraryPath.String, "err", err)
			}
			// Also try to prune the parent dir if it's now empty (cosmetic —
			// don't recurse; if it's not empty, leave it alone).
			parent := filepath.Dir(o.LibraryPath.String)
			_ = os.Remove(parent) // ignores ENOTEMPTY
		}
		if err := m.o.Store.Delete(ctx, o.NzoID); err != nil {
			m.log.Warn("mirror: delete row failed", "nzo_id", o.NzoID, "err", err)
			continue
		}
		m.log.Info("mirror: orphan cleaned", "nzo_id", o.NzoID, "torbox_id", o.TorboxID.Int64)
	}
}

// sniffCategory derives a category from a release name when we don't have one
// (mirror rows weren't grabbed by Sonarr/Radarr so they have no category from
// the original request). Used to route mirrors to the right top-level dir
// (`series/` vs `movies/` vs `other/`) inside the librarian writer.
//
// Heuristics, in order:
//
//  1. SxxExx pattern → tv-sonarr
//  2. " 1234 " or ".1234." (4-digit year, 1900..2099) → movies
//  3. otherwise → other
func sniffCategory(name string) string {
	if reSeasonEpisode.MatchString(name) {
		return "tv-sonarr"
	}
	if reYear.MatchString(name) {
		return "movies"
	}
	return "other"
}

var (
	// SxxExx (episode), or season-pack patterns: SxxEnn, Sxx (alone),
	// "Season N", "S01-S06". Generously matched and case-insensitive.
	reSeasonEpisode = regexp.MustCompile(
		`(?i)(\bS\d{1,3}(E\d{1,3})?\b|\bSeason\s+\d+\b|\bS\d{1,2}-S\d{1,2}\b)`)
	// 4-digit year bounded by typical separators (.- ()) — excludes runs of
	// digits inside larger numbers.
	reYear = regexp.MustCompile(`(?:^|[\s.\-(])(19|20)\d{2}(?:[\s.\-)]|$)`)
)
