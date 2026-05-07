package worker

import (
	"context"
	"errors"
	"time"

	"github.com/pburkhalter/arrarr/internal/arrclient"
	"github.com/pburkhalter/arrarr/internal/httpx"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/librarian"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// libraryRow re-exported into the worker package for brevity; canonical type
// lives in store.LibraryRow.
type libraryRow = store.LibraryRow

// MaxLibraryAttempts caps how many times the librarian will retry a single
// job before giving up and marking it FAILED. Library writes typically fail
// for transient reasons (TorBox 5xx during requestdl, file system hiccup),
// so we're more patient than the submitter.
const MaxLibraryAttempts = 8

// librarianLoop drains the librarian work queue: jobs in COMPLETED_TORBOX
// whose library_writer_state is still 'pending' (or 'failed' + due for retry).
//
// We tick at VerifyEvery to align with the existing verifier cadence (the two
// stages logically sit next to each other and have similar latency budgets).
func (m *Manager) librarianLoop(ctx context.Context) {
	if m.o.Librarian == nil {
		// Librarian is opt-in. When unset, COMPLETED_TORBOX → READY happens
		// inside verifyOnce (preserving v1 behavior).
		return
	}
	ticker := time.NewTicker(m.o.VerifyEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.librarianTick(ctx)
		}
	}
}

func (m *Manager) librarianTick(ctx context.Context) {
	// Empty origin = all origins. Mirror rows (origin='mirror') get the same
	// library treatment as self rows; the only origin-aware logic lives in the
	// webhook handler (which gates Pushover to 'self').
	rows, err := m.o.Store.ListPendingLibraryWrites(ctx, "", 50)
	if err != nil {
		m.log.Error("librarian: list failed", "err", err)
		return
	}
	for _, r := range rows {
		m.libraryOne(ctx, r)
	}
}

// libraryOne resolves the single TorBox download into an Item and calls
// Writer.Write. On success, transitions the job to READY and persists the
// streaming URL + expiry. On failure, schedules a retry or fails the job
// after MaxLibraryAttempts.
func (m *Manager) libraryOne(ctx context.Context, lr *libraryRow) {
	libPath, primaryURL, expires, err := m.buildAndWriteLibraryEntry(ctx, lr)
	if err != nil {
		m.scheduleLibraryRetry(ctx, lr, err)
		return
	}
	var (
		urlPtr     *string
		expiresPtr *time.Time
	)
	if primaryURL != "" {
		urlPtr = &primaryURL
		expiresPtr = &expires
	}
	if err := m.o.Store.MarkLibraryWritten(ctx, lr.NzoID, libPath, urlPtr, expiresPtr); err != nil {
		m.log.Error("librarian: mark-written failed", "nzo_id", lr.NzoID, "err", err)
		return
	}
	m.log.Info("librarian: written",
		"nzo_id", lr.NzoID,
		"path", libPath,
		"mode", m.o.Librarian.Mode())
}

// buildAndWriteLibraryEntry is the shared core between libraryOne (first
// write) and urlRefreshOne (rewrite before TTL). Fetches files + per-file
// CDN URLs from TorBox, asks Sonarr/Radarr for canonical naming if available,
// invokes the configured Writer, returns the primary library path + URL +
// expiry timestamp.
//
// On any TorBox error the caller decides retry semantics — this function
// surfaces a plain error.
func (m *Manager) buildAndWriteLibraryEntry(ctx context.Context, lr *libraryRow) (libPath, primaryURL string, expires time.Time, err error) {
	id := pickID(lr)
	if id == 0 {
		err = errors.New("no torbox id yet")
		return
	}
	if !lr.TorboxFolderName.Valid {
		err = errors.New("no folder name yet")
		return
	}
	files, err := m.fetchTorboxFiles(ctx, id)
	if err != nil {
		return
	}

	// Per-file streaming URLs. STRM mode requires them; symlink mode tolerates
	// missing URLs (the writer ignores them).
	urls := map[int64]string{}
	expires = time.Now().Add(m.o.StreamingURLRefreshAfter)
	for _, f := range files {
		var u string
		u, err = m.o.Torbox.RequestUsenetDL(ctx, id, f.ID, false)
		if err != nil {
			return
		}
		urls[f.ID] = u
	}

	canonical := m.tryParseCanonical(ctx, lr.Category, lr.TorboxFolderName.String)

	item := librarian.Item{
		NzoID:         lr.NzoID,
		Category:      lr.Category,
		ReleaseName:   lr.TorboxFolderName.String,
		Files:         files,
		StreamingURLs: urls,
		Canonical:     canonical,
		MountBase:     m.o.LocalVerifyBase,
	}
	libPath, err = m.o.Librarian.Write(ctx, item)
	if err != nil {
		return
	}
	primaryURL = pickPrimaryURL(item, files)
	return
}

func pickID(lr *libraryRow) int64 {
	if lr.TorboxActiveID.Valid && lr.TorboxActiveID.Int64 != 0 {
		return lr.TorboxActiveID.Int64
	}
	if lr.TorboxQueueID.Valid && lr.TorboxQueueID.Int64 != 0 {
		return lr.TorboxQueueID.Int64
	}
	return 0
}

// fetchTorboxFiles retrieves the file list for a single download by querying
// the same /usenet/mylist endpoint the poller uses, server-side filtered by id.
// (TorBox supports id= as a query param; some clients ignore it but our
// torbox.Client will send it via MyList(true) where true forces bypass-cache.)
func (m *Manager) fetchTorboxFiles(ctx context.Context, id int64) ([]librarian.FileInfo, error) {
	items, err := m.o.Torbox.MyList(ctx, true)
	if err != nil {
		return nil, err
	}
	var matched *torbox.MyListItem
	for i := range items {
		if items[i].ID == id || items[i].QueueID == id {
			matched = &items[i]
			break
		}
	}
	if matched == nil {
		return nil, errors.New("librarian: no matching mylist item")
	}
	out := make([]librarian.FileInfo, 0, len(matched.Files))
	for _, f := range matched.Files {
		out = append(out, librarian.FileInfo{
			ID:        f.ID,
			Name:      f.Name,
			ShortName: f.ShortName,
			Size:      f.Size,
			MimeType:  f.MimeType,
		})
	}
	return out, nil
}

// tryParseCanonical asks the appropriate arr (Sonarr for series-ish, Radarr
// for movie-ish) to identify the release. Best-effort — returns nil on any
// failure (no arr configured, network error, ErrNoMatch). Caller treats nil
// as "use release-name fallback" via the librarian writer.
func (m *Manager) tryParseCanonical(ctx context.Context, category, releaseName string) *arrclient.ParseResult {
	if releaseName == "" {
		return nil
	}
	var c *arrclient.Client
	switch {
	case isSeriesCategory(category) && m.o.Sonarr != nil:
		c = m.o.Sonarr
	case isMovieCategory(category) && m.o.Radarr != nil:
		c = m.o.Radarr
	default:
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, m.o.ArrCallbackTimeout)
	defer cancel()
	res, err := c.Parse(cctx, releaseName)
	if err != nil {
		// Soft fail: log at debug so we don't spam, library write proceeds with
		// release-name fallback.
		m.log.Debug("librarian: arr parse miss",
			"err", err, "category", category, "release", releaseName)
		return nil
	}
	return res
}

// pickPrimaryURL returns the streaming URL we want to remember on the row for
// the URL-refresh worker. Picking the first playable file's URL is fine; the
// refresh worker re-resolves all of them when due.
func pickPrimaryURL(item librarian.Item, files []librarian.FileInfo) string {
	for _, f := range files {
		if u, ok := item.StreamingURLs[f.ID]; ok && u != "" {
			return u
		}
	}
	return ""
}

func (m *Manager) scheduleLibraryRetry(ctx context.Context, lr *libraryRow, cause error) {
	attempts := lr.LibraryWriterAttempts + 1
	terminal := attempts >= MaxLibraryAttempts
	if terminal {
		m.log.Warn("librarian: giving up",
			"nzo_id", lr.NzoID, "attempts", attempts, "err", describe(cause))
		_ = m.o.Store.MarkLibraryFailed(ctx, lr.NzoID, describe(cause), nil, true)
		return
	}
	delay := httpx.Backoff(attempts, time.Minute, 30*time.Minute)
	next := time.Now().UTC().Add(delay)
	m.log.Info("librarian: deferring",
		"nzo_id", lr.NzoID, "attempt", attempts, "next_in", delay, "err", describe(cause))
	_ = m.o.Store.MarkLibraryFailed(ctx, lr.NzoID, describe(cause), &next, false)
}

func isSeriesCategory(c string) bool {
	switch c {
	case "sonarr", "tv", "tv-sonarr", "anime":
		return true
	}
	return false
}

func isMovieCategory(c string) bool {
	switch c {
	case "radarr", "movies":
		return true
	}
	return false
}

// --- URL refresh sweeper ---

// URLRefreshBuffer is how far ahead of the streaming URL's expiry we proactively
// rewrite the STRM. With ~6h TorBox CDN URL TTL, an hour of headroom comfortably
// covers a sweeper tick miss + a slow refresh.
const URLRefreshBuffer = 1 * time.Hour

// urlRefreshLoop rewrites STRM files before their underlying TorBox CDN URLs
// expire. Only runs when the librarian is configured AND the writer mode
// produces STRM files (strm or both); for webdav-only mode this is a no-op
// because symlinks don't carry URL TTLs.
func (m *Manager) urlRefreshLoop(ctx context.Context) {
	if m.o.Librarian == nil || !writerNeedsURLRefresh(m.o.Librarian.Mode()) {
		return
	}
	// Tick more sparsely than the librarian — most rows aren't due for hours.
	interval := m.o.StreamingURLRefreshAfter / 4
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.urlRefreshTick(ctx)
		}
	}
}

func writerNeedsURLRefresh(mode string) bool {
	switch mode {
	case "strm", "both":
		return true
	}
	return false
}

func (m *Manager) urlRefreshTick(ctx context.Context) {
	cutoff := time.Now().Add(URLRefreshBuffer)
	rows, err := m.o.Store.ListUpcomingURLRefreshes(ctx, cutoff, 25)
	if err != nil {
		m.log.Error("urlrefresh: list failed", "err", err)
		return
	}
	for _, r := range rows {
		m.urlRefreshOne(ctx, r)
	}
}

// urlRefreshOne re-runs the librarian's write step against a READY row,
// updating the cached streaming URL + expiry without changing job state. If
// TorBox is briefly unavailable we just log and try again next tick — the
// existing STRM keeps working until its actual TTL elapses.
func (m *Manager) urlRefreshOne(ctx context.Context, lr *libraryRow) {
	libPath, primaryURL, expires, err := m.buildAndWriteLibraryEntry(ctx, lr)
	if err != nil {
		m.log.Warn("urlrefresh: skip (will retry)",
			"nzo_id", lr.NzoID, "err", describe(err))
		return
	}
	if primaryURL == "" {
		// No playable files (rare — e.g. all metadata). Nothing to refresh.
		return
	}
	if err := m.o.Store.UpdateStreamingURL(ctx, lr.NzoID, libPath, primaryURL, expires); err != nil {
		m.log.Warn("urlrefresh: update failed", "nzo_id", lr.NzoID, "err", err)
		return
	}
	m.log.Info("urlrefresh: rewrote",
		"nzo_id", lr.NzoID, "path", libPath, "expires_at", expires.UTC().Format(time.RFC3339))
}

// --- Tagger retry loop ---

// taggerRetryInterval is how often the tagger sweeper picks up untagged rows.
// TorBox's editusenetdownload requires the item to be "cached" (past initial
// processing), so the submitter's eager attempt typically fails. This loop
// retries gently — the cost of a failed PUT is negligible.
const taggerRetryInterval = 90 * time.Second

// taggerRetryLoop catches up tags on rows the submitter couldn't tag because
// TorBox hadn't finished caching them yet. Best-effort — failures are logged
// and the row stays untagged for the next tick.
func (m *Manager) taggerRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(taggerRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.taggerRetryTick(ctx)
		}
	}
}

func (m *Manager) taggerRetryTick(ctx context.Context) {
	rows, err := m.o.Store.ListPendingTagWrites(ctx, 25)
	if err != nil {
		m.log.Error("tagger: list failed", "err", err)
		return
	}
	for _, r := range rows {
		m.taggerRetryOne(ctx, r)
	}
}

func (m *Manager) taggerRetryOne(ctx context.Context, lr *libraryRow) {
	id := pickID(lr)
	if id == 0 {
		return
	}
	tags := []string{
		"arrarr",
		"host:" + m.o.InstanceName,
		"job:" + job.TagID(lr.NzoID),
		"cat:" + lr.Category,
	}
	tagCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := m.o.Torbox.EditUsenet(tagCtx, id, torbox.EditUsenetParams{Tags: tags}); err != nil {
		// Quiet — failure here is expected until TorBox caches the item.
		m.log.Debug("tagger: still failing (will retry)",
			"nzo_id", lr.NzoID, "id", id, "err", describe(err))
		return
	}
	if err := m.o.Store.MarkTagged(ctx, lr.NzoID); err != nil {
		m.log.Warn("tagger: mark-tagged failed", "nzo_id", lr.NzoID, "err", err)
		return
	}
	m.log.Info("tagger: tagged on retry", "nzo_id", lr.NzoID, "id", id)
}

