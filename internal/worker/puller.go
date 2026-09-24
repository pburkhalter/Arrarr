package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pburkhalter/arrarr/internal/downloader"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// Puller transitions COMPLETED_TORBOX jobs to READY by pulling the actual file
// bytes from TorBox CDN onto local disk so Sonarr/Radarr can import via a
// normal local path.
type Puller struct {
	store      *store.Store
	tb         torboxPullerClient
	dl         *downloader.Downloader
	baseDir    string
	log        *slog.Logger
	maxRetries int

	// archiveWait is how long a job may list only archives before it fails.
	archiveWait time.Duration
	mu          sync.Mutex
	// archivesSince records when each job was first seen listing only
	// archives. In memory on purpose: a restart merely restarts the wait.
	archivesSince map[string]time.Time
	// inflight holds the jobs currently being pulled. A job stays in
	// COMPLETED_TORBOX for the whole pull, so this set is what keeps a later
	// Tick from starting it a second time.
	inflight map[string]bool
	wg       sync.WaitGroup
	// wake is signalled whenever a pull finishes, so its slot is refilled
	// right away instead of on the next ticker beat.
	wake chan struct{}
}

// errArchivesOnly means TorBox reports the release complete but lists only
// the packed RAR/ZIP parts, not the extracted video yet.
var errArchivesOnly = errors.New("torbox lists only archives, extraction not finished")

// DefaultArchiveWait bounds the wait for TorBox to extract a release.
// Extraction normally finishes within minutes; past this the release is
// treated as broken so the Arr blocklists it and searches another.
const DefaultArchiveWait = 30 * time.Minute

type torboxPullerClient interface {
	MyList(ctx context.Context, bypassCache bool) ([]torbox.MyListItem, error)
	MyListTorrents(ctx context.Context, bypassCache bool) ([]torbox.MyListItem, error)
	RequestUsenetDL(ctx context.Context, usenetID, fileID int64, zipLink bool) (string, error)
	RequestTorrentDL(ctx context.Context, torrentID, fileID int64, zipLink bool) (string, error)
}

type PullerOptions struct {
	Store      *store.Store
	Torbox     torboxPullerClient
	Downloader *downloader.Downloader
	BaseDir    string
	Logger     *slog.Logger
	MaxRetries int
	// ArchiveWait overrides DefaultArchiveWait (tests).
	ArchiveWait time.Duration
}

func NewPuller(opts PullerOptions) *Puller {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxRetries < 1 {
		opts.MaxRetries = 5
	}
	if opts.ArchiveWait <= 0 {
		opts.ArchiveWait = DefaultArchiveWait
	}
	return &Puller{
		store:         opts.Store,
		tb:            opts.Torbox,
		dl:            opts.Downloader,
		baseDir:       strings.TrimRight(opts.BaseDir, "/"),
		log:           opts.Logger,
		maxRetries:    opts.MaxRetries,
		archiveWait:   opts.ArchiveWait,
		archivesSince: map[string]time.Time{},
		inflight:      map[string]bool{},
		wake:          make(chan struct{}, 1),
	}
}

func (m *Manager) pullerLoop(ctx context.Context) {
	ticker := time.NewTicker(m.o.PullEvery)
	defer ticker.Stop()
	for {
		m.o.Puller.Tick(ctx, m.o.WorkerPoolSize)
		select {
		case <-ctx.Done():
			m.o.Puller.Wait()
			return
		case <-ticker.C:
		case <-m.o.Puller.wake:
		}
	}
}

// Tick tops the pool up to `limit` concurrent pulls and returns without
// waiting for them. Wait blocks until every started pull has finished.
//
// The concurrency is across jobs on purpose. Downloader.Concurrency only
// parallelises the files *within* one job, and a typical episode release is a
// single mkv — so that knob does nothing for a queue of episodes. Pulling one
// release at a time meant every job also waited out all its predecessors:
// measured over a 22-episode batch in Sep 2026, TorBox itself finished in a
// median of 28s while COMPLETED_TORBOX → READY took a median of 60 minutes,
// nearly all of it queueing.
//
// Slots are refilled as each pull finishes rather than per batch. Waiting for
// a whole batch let one large release idle the other slots: in Sep 2026 a
// 13 GB film held three slots empty while eleven jobs queued behind it.
func (p *Puller) Tick(ctx context.Context, limit int) {
	if limit <= 0 {
		limit = 8
	}
	p.mu.Lock()
	busy := len(p.inflight)
	p.mu.Unlock()
	if busy >= limit {
		return
	}
	// List past the in-flight jobs so there are enough idle ones to start.
	jobs, err := p.store.ListByStates(ctx, []job.State{job.StateCompletedTorbox}, limit+busy)
	if err != nil {
		p.log.Error("puller: list failed", "err", err)
		return
	}

	for _, j := range jobs {
		if j.LocalPath.Valid && j.LocalPath.String != "" {
			// already pulled by an earlier tick that crashed before transition;
			// transition explicitly so we don't spin on it forever.
			if err := p.store.MarkLocalReady(ctx, j.NzoID, j.LocalPath.String, j.BytesTotal); err != nil &&
				!errors.Is(err, store.ErrInvalidTransition) {
				p.log.Warn("puller: stale-localpath transition failed", "nzo_id", j.NzoID, "err", err)
			}
			continue
		}
		p.mu.Lock()
		if p.inflight[j.NzoID] || len(p.inflight) >= limit {
			p.mu.Unlock()
			continue
		}
		p.inflight[j.NzoID] = true
		p.mu.Unlock()

		p.wg.Add(1)
		go func(j *job.Job) {
			defer p.wg.Done()
			defer p.finish(j.NzoID)
			p.pullOne(ctx, j)
		}(j)
	}
}

// Wait blocks until every pull started by Tick has finished.
func (p *Puller) Wait() { p.wg.Wait() }

func (p *Puller) finish(nzoID string) {
	p.mu.Lock()
	delete(p.inflight, nzoID)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Puller) pullOne(ctx context.Context, j *job.Job) {
	logger := p.log.With("nzo_id", j.NzoID, "source", jobSource(j))

	item, err := p.findItem(ctx, j)
	if err != nil {
		logger.Warn("puller: lookup failed", "err", describe(err))
		p.scheduleRetry(ctx, j, "lookup: "+err.Error())
		return
	}
	if len(item.Files) == 0 {
		logger.Warn("puller: torbox item has no files")
		p.scheduleRetry(ctx, j, "torbox item has no files yet")
		return
	}

	subdir := p.subdir(j, item)
	files, total, err := p.buildFileList(ctx, j, item)
	if errors.Is(err, errArchivesOnly) {
		p.awaitExtraction(ctx, j, logger)
		return
	}
	p.clearArchiveWait(j.NzoID)
	if err != nil {
		logger.Warn("puller: build file list failed", "err", describe(err))
		p.scheduleRetry(ctx, j, "build file list: "+err.Error())
		return
	}

	dlJob := downloader.Job{
		JobID:  j.NzoID,
		Subdir: subdir,
		Files:  files,
	}
	logger.Info("puller: starting download",
		"subdir", subdir, "files", len(files), "bytes_total", total)

	abs, err := p.dl.Run(ctx, dlJob)
	if err != nil {
		logger.Error("puller: download failed", "err", err.Error())
		p.scheduleRetry(ctx, j, "download: "+err.Error())
		return
	}

	if err := p.store.MarkLocalReady(ctx, j.NzoID, abs, total); err != nil {
		logger.Error("puller: state transition failed", "err", err)
		return
	}
	logger.Info("puller: ready", "local_path", abs, "bytes", total)
}

// findItem returns the MyListItem for this job from the appropriate TorBox
// list (usenet or torrent). Matches by TorBox id first, falls back to folder
// name (TorBox swaps queue_id for id mid-flight).
func (p *Puller) findItem(ctx context.Context, j *job.Job) (*torbox.MyListItem, error) {
	var items []torbox.MyListItem
	var err error
	switch jobSource(j) {
	case "torrent":
		items, err = p.tb.MyListTorrents(ctx, true)
	default:
		items, err = p.tb.MyList(ctx, true)
	}
	if err != nil {
		return nil, err
	}
	wantID := j.EffectiveTorboxID()
	for i := range items {
		it := &items[i]
		if wantID != 0 && (it.ID == wantID || it.QueueID == wantID) {
			return it, nil
		}
	}
	if j.TorboxFolderName.Valid {
		for i := range items {
			if items[i].Name == j.TorboxFolderName.String {
				return &items[i], nil
			}
		}
	}
	return nil, fmt.Errorf("torbox item not found (id=%d folder=%q)", wantID, j.TorboxFolderName.String)
}

// awaitExtraction leaves a job whose release is still packed in
// COMPLETED_TORBOX so the next tick polls TorBox again, without spending an
// attempt. Pulling the archives instead would hand the Arr RAR parts it
// cannot import (Station Eleven, Sep 2026). Past archiveWait the job fails.
func (p *Puller) awaitExtraction(ctx context.Context, j *job.Job, logger *slog.Logger) {
	p.mu.Lock()
	since, seen := p.archivesSince[j.NzoID]
	if !seen {
		since = time.Now()
		p.archivesSince[j.NzoID] = since
	}
	p.mu.Unlock()

	waited := time.Since(since)
	if waited < p.archiveWait {
		logger.Info("puller: only archives listed, waiting for torbox extraction",
			"waited", waited.Round(time.Second))
		if err := p.store.Reschedule(ctx, j.NzoID, errArchivesOnly.Error(), time.Now().Add(time.Minute)); err != nil {
			logger.Warn("puller: reschedule failed", "err", err)
		}
		return
	}
	p.clearArchiveWait(j.NzoID)
	logger.Warn("puller: torbox never extracted the release, failing job", "waited", waited.Round(time.Second))
	_ = p.store.Transition(ctx, j.NzoID, store.Transition{
		From:        j.State,
		To:          job.StateFailed,
		LastError:   strPtr(fmt.Sprintf("%s after %s", errArchivesOnly, waited.Round(time.Minute))),
		CompletedAt: nowPtr(),
	})
}

func (p *Puller) clearArchiveWait(nzoID string) {
	p.mu.Lock()
	delete(p.archivesSince, nzoID)
	p.mu.Unlock()
}

// buildFileList resolves a presigned CDN URL for every video-ish file in the
// item, skipping noise (NFOs, samples, txt). Archives are skipped whenever a
// video is present — they are the leftovers of an extraction. With archives
// but no video it returns errArchivesOnly. Returns the FileDownload list and
// the aggregate byte total for progress reporting.
func (p *Puller) buildFileList(ctx context.Context, j *job.Job, item *torbox.MyListItem) ([]downloader.FileDownload, int64, error) {
	src := jobSource(j)
	tbid := item.ID
	if tbid == 0 {
		tbid = item.QueueID
	}
	var hasVideo, hasArchive bool
	for _, f := range item.Files {
		if isNoiseFile(f.Name) {
			continue
		}
		hasVideo = hasVideo || isVideoFile(f.Name)
		hasArchive = hasArchive || isArchiveFile(f.Name)
	}
	if hasArchive && !hasVideo {
		return nil, 0, errArchivesOnly
	}
	out := make([]downloader.FileDownload, 0, len(item.Files))
	var total int64
	for _, f := range item.Files {
		if isNoiseFile(f.Name) || isArchiveFile(f.Name) {
			continue
		}
		var url string
		var err error
		if src == "torrent" {
			url, err = p.tb.RequestTorrentDL(ctx, tbid, f.ID, false)
		} else {
			url, err = p.tb.RequestUsenetDL(ctx, tbid, f.ID, false)
		}
		if err != nil {
			return nil, 0, fmt.Errorf("requestdl file_id=%d: %w", f.ID, err)
		}
		out = append(out, downloader.FileDownload{
			FileID:   f.ID,
			URL:      url,
			DestName: relativeFileName(item.Name, f.Name, f.ShortName),
			Size:     f.Size,
		})
		total += f.Size
	}
	if len(out) == 0 {
		return nil, 0, errors.New("no playable files after filter")
	}
	return out, total, nil
}

func (p *Puller) subdir(j *job.Job, item *torbox.MyListItem) string {
	cat := j.Category
	if cat == "" {
		cat = "other"
	}
	name := item.Name
	if name == "" && j.TorboxFolderName.Valid {
		name = j.TorboxFolderName.String
	}
	if name == "" {
		name = strings.TrimSuffix(j.Filename, ".nzb")
	}
	return path.Join(cat, sanitizeForFS(name))
}

func (p *Puller) scheduleRetry(ctx context.Context, j *job.Job, reason string) {
	const backoff = 2 * time.Minute
	if j.Attempts+1 >= p.maxRetries {
		p.log.Warn("puller: max retries reached, failing job", "nzo_id", j.NzoID, "reason", reason)
		_ = p.store.Transition(ctx, j.NzoID, store.Transition{
			From:        j.State,
			To:          job.StateFailed,
			LastError:   strPtr("puller exhausted: " + reason),
			CompletedAt: nowPtr(),
		})
		return
	}
	next := time.Now().Add(backoff)
	if err := p.store.AttemptFailure(ctx, j.NzoID, reason, next); err != nil {
		p.log.Warn("puller: schedule-retry failed", "nzo_id", j.NzoID, "err", err)
	}
}

func jobSource(j *job.Job) string {
	if j.Source == "" {
		return "usenet"
	}
	return j.Source
}

// isNoiseFile filters TorBox listings down to playable media. The Arrs do
// their own filtering on import, but skipping NFOs etc. avoids wasted CDN
// requests + writes.
func isNoiseFile(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".nfo", ".sfv", ".txt", ".jpg", ".png", ".srr", ".par2", ".sample"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return strings.Contains(lower, "/sample/") || strings.Contains(lower, "sample.")
}

var (
	videoExts = []string{".mkv", ".mp4", ".m4v", ".avi", ".ts", ".m2ts", ".mov", ".wmv", ".mpg", ".mpeg", ".webm"}
	// .rar, .r00–.r999, .zip, .7z and split parts like .001.
	archiveRE = regexp.MustCompile(`\.(rar|r\d{2,3}|zip|7z|\d{3})$`)
)

func isVideoFile(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range videoExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func isArchiveFile(name string) bool {
	return archiveRE.MatchString(strings.ToLower(name))
}

// relativeFileName strips the release-folder prefix from MyListFile.Name so
// the downloader writes file paths relative to its Subdir, not double-nested.
// MyListFile.Name typically looks like "ReleaseName/Subdir/file.mkv" and our
// Subdir already includes the release name. Falls back to ShortName if the
// strip would produce an empty path.
func relativeFileName(releaseName, fullName, shortName string) string {
	trimmed := strings.TrimPrefix(fullName, releaseName+"/")
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" || trimmed == fullName {
		// fullName didn't carry the release prefix (some single-file releases
		// don't); just use the short name to avoid writing a path with the
		// release name nested inside itself.
		if shortName != "" {
			return shortName
		}
		return path.Base(fullName)
	}
	return trimmed
}

// sanitizeForFS makes a TorBox name safe for use as a directory component on
// ext4/zfs/apfs. TorBox names sometimes carry characters that aren't strictly
// problematic but break shell globbing during import (square brackets, colons).
func sanitizeForFS(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.TrimSpace(name)
	if name == "" {
		return "untitled"
	}
	return name
}
