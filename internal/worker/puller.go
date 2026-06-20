package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/pburkhalter/arrarr/internal/downloader"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// Puller transitions COMPLETED_TORBOX jobs to READY by pulling the actual file
// bytes from TorBox CDN onto local disk. This is the v3 replacement for the v2
// librarian (which wrote STRMs/symlinks pointing into a TorBox WebDAV mount —
// fragile when TorBox purges old items).
//
// The puller only runs when DOWNLOAD_DIR is configured. When LibraryMode is
// "off" and no DOWNLOAD_DIR is set, jobs stay in COMPLETED_TORBOX indefinitely
// (no-op). When the v2 librarian is also enabled, only one of them will pick
// up a given job — the librarian filters on library_writer_state, the puller
// filters on local_path being NULL.
type Puller struct {
	store      *store.Store
	tb         torboxPullerClient
	dl         *downloader.Downloader
	baseDir    string
	log        *slog.Logger
	maxRetries int
}

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
}

func NewPuller(opts PullerOptions) *Puller {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxRetries < 1 {
		opts.MaxRetries = 5
	}
	return &Puller{
		store:      opts.Store,
		tb:         opts.Torbox,
		dl:         opts.Downloader,
		baseDir:    strings.TrimRight(opts.BaseDir, "/"),
		log:        opts.Logger,
		maxRetries: opts.MaxRetries,
	}
}

func (m *Manager) pullerLoop(ctx context.Context) {
	if m.o.Puller == nil {
		return
	}
	ticker := time.NewTicker(m.o.PullEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.o.Puller.Tick(ctx, m.o.WorkerPoolSize)
		}
	}
}

func (p *Puller) Tick(ctx context.Context, limit int) {
	if limit <= 0 {
		limit = 8
	}
	jobs, err := p.store.ListByStates(ctx, []job.State{job.StateCompletedTorbox}, limit)
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
		if j.LibraryPath.Valid {
			// v2 librarian already handled this one; skip.
			continue
		}
		p.pullOne(ctx, j)
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

// buildFileList resolves a presigned CDN URL for every video-ish file in the
// item, skipping noise (NFOs, samples, txt). Returns the FileDownload list and
// the aggregate byte total for progress reporting.
func (p *Puller) buildFileList(ctx context.Context, j *job.Job, item *torbox.MyListItem) ([]downloader.FileDownload, int64, error) {
	src := jobSource(j)
	tbid := item.ID
	if tbid == 0 {
		tbid = item.QueueID
	}
	out := make([]downloader.FileDownload, 0, len(item.Files))
	var total int64
	for _, f := range item.Files {
		if isNoiseFile(f.Name) {
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
			LastError:   strPtrLocal("puller exhausted: " + reason),
			CompletedAt: nowPtrLocal(),
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

func strPtrLocal(s string) *string { return &s }
func nowPtrLocal() *time.Time      { t := time.Now(); return &t }
