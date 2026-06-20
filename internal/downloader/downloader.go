// Package downloader pulls files from TorBox (or any HTTP source) to a local
// directory. It is the piece that makes arrarr behave like qBit/SAB to Sonarr
// and Radarr: once TorBox finishes a job, downloader fetches the resulting
// files to /downloads so the *arrs can import them locally.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type Options struct {
	BaseDir     string
	Concurrency int
	HTTP        *http.Client
	Logger      Logger
	OnProgress  func(jobID string, fileID int64, bytes int64, total int64)
}

type Downloader struct {
	baseDir     string
	concurrency int
	http        *http.Client
	log         Logger
	onProgress  func(jobID string, fileID int64, bytes int64, total int64)
}

type Job struct {
	JobID  string
	Subdir string
	Files  []FileDownload
}

type FileDownload struct {
	FileID   int64
	URL      string
	DestName string
	Size     int64
}

func New(opts Options) (*Downloader, error) {
	if opts.BaseDir == "" {
		return nil, errors.New("downloader: BaseDir required")
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	httpClient := opts.HTTP
	if httpClient == nil {
		// Cap redirects at 10 — TorBox CDN typically hops once to S3, but we
		// don't want a misconfigured redirect loop to hang a download.
		httpClient = &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				return nil
			},
		}
	}
	log := opts.Logger
	if log == nil {
		log = noopLogger{}
	}
	return &Downloader{
		baseDir:     opts.BaseDir,
		concurrency: opts.Concurrency,
		http:        httpClient,
		log:         log,
		onProgress:  opts.OnProgress,
	}, nil
}

func (d *Downloader) Run(ctx context.Context, job Job) (string, error) {
	target, err := safeJoin(d.baseDir, job.Subdir)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", job.JobID, err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", fmt.Errorf("download %s: mkdir: %w", job.JobID, err)
	}

	// Cancel sibling goroutines as soon as one file fails — caller retries the
	// whole job, so finishing the rest is wasted bandwidth.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, d.concurrency)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		firstErr error
	)

	for _, f := range job.Files {
		f := f
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			if err := d.downloadFile(runCtx, job, target, f); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return "", firstErr
	}
	return target, nil
}

func (d *Downloader) downloadFile(ctx context.Context, job Job, target string, f FileDownload) error {
	dest, err := safeJoin(target, f.DestName)
	if err != nil {
		return fmt.Errorf("download %s file %d: %w", job.JobID, f.FileID, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("download %s file %d: mkdir: %w", job.JobID, f.FileID, err)
	}

	// Skip atomic rename if file already at expected size — saves a write+rename
	// cycle on re-runs after a partial job retry.
	if f.Size > 0 {
		if st, err := os.Stat(dest); err == nil && st.Size() == f.Size {
			d.log.Info("downloader: skip existing", "job", job.JobID, "file", f.FileID, "path", dest)
			return nil
		}
	}

	partial := dest + ".partial"
	// Remove stale .partial from a previous crashed run so we always start clean.
	_ = os.Remove(partial)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return fmt.Errorf("download %s file %d: %w", job.JobID, f.FileID, err)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("download %s file %d: %w", job.JobID, f.FileID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download %s file %d: http %d", job.JobID, f.FileID, resp.StatusCode)
	}

	out, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("download %s file %d: open: %w", job.JobID, f.FileID, err)
	}

	written, copyErr := d.copyWithProgress(ctx, out, resp.Body, job.JobID, f)
	if copyErr != nil {
		out.Close()
		_ = os.Remove(partial)
		return fmt.Errorf("download %s file %d: copy: %w", job.JobID, f.FileID, copyErr)
	}

	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(partial)
		return fmt.Errorf("download %s file %d: sync: %w", job.JobID, f.FileID, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("download %s file %d: close: %w", job.JobID, f.FileID, err)
	}

	if f.Size > 0 && written != f.Size {
		_ = os.Remove(partial)
		return fmt.Errorf("download %s file %d: size mismatch got=%d want=%d", job.JobID, f.FileID, written, f.Size)
	}

	if err := os.Rename(partial, dest); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("download %s file %d: rename: %w", job.JobID, f.FileID, err)
	}
	d.log.Info("downloader: wrote", "job", job.JobID, "file", f.FileID, "path", dest, "bytes", written)
	return nil
}

func (d *Downloader) copyWithProgress(ctx context.Context, dst io.Writer, src io.Reader, jobID string, f FileDownload) (int64, error) {
	buf := make([]byte, 256*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
			if w != n {
				return total, io.ErrShortWrite
			}
			if d.onProgress != nil {
				d.onProgress(jobID, f.FileID, total, f.Size)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return total, nil
			}
			return total, rerr
		}
	}
}

// safeJoin returns base+rel, rejecting paths that would escape base via "..",
// absolute paths, or backslash separators on POSIX (which librarian also blocks).
func safeJoin(base, rel string) (string, error) {
	if rel == "" {
		return base, nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q is absolute", rel)
	}
	if strings.Contains(rel, "\x00") {
		return "", fmt.Errorf("path %q contains null byte", rel)
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == "." {
		if cleaned != "." {
			return "", fmt.Errorf("path %q escapes base", rel)
		}
	}
	joined := filepath.Join(base, cleaned)
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	rel2, err := filepath.Rel(absBase, absJoined)
	if err != nil {
		return "", err
	}
	if rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes base", rel)
	}
	return joined, nil
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}
