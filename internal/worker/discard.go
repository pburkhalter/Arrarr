package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/pburkhalter/arrarr/internal/job"
)

// Discard throws away what arrarr holds for a job the client deleted with
// del_files=1: Sonarr/Radarr send that when a download is removed together
// with its data (Journarr's "remove everywhere", or the Arr cleaning up after
// an import). The caller has already moved the job out of its active state.
//
//   - A running pull is stopped before its directory is deleted, so nothing
//     writes into it afterwards.
//   - The TorBox entry is deleted only while TorBox is still downloading it,
//     by the id this job received on submit. Finished entries stay: the
//     account is shared (see releaseTorboxSlot).
//   - Local files are deleted only strictly below DOWNLOAD_DIR/<category>.
func (m *Manager) Discard(ctx context.Context, j *job.Job) {
	var dirs []string
	if m.o.Puller != nil {
		if d := m.o.Puller.Abort(ctx, j.NzoID); d != "" {
			dirs = append(dirs, d)
		}
	}
	switch j.State {
	case job.StateSubmitted, job.StateDownloading:
		m.releaseTorboxSlot(ctx, j)
	}
	if j.LocalPath.Valid && j.LocalPath.String != "" {
		dirs = append(dirs, j.LocalPath.String)
	}
	base := ""
	if m.o.Puller != nil {
		base = m.o.Puller.baseDir
	}
	seen := map[string]bool{}
	for _, d := range dirs {
		if seen[d] {
			continue
		}
		seen[d] = true
		if !jobDirUnder(base, d) {
			m.log.Warn("discard: not deleting a path outside the download dir",
				"nzo_id", j.NzoID, "path", d)
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			m.log.Warn("discard: deleting local files failed", "nzo_id", j.NzoID, "path", d, "err", err)
			continue
		}
		m.log.Info("discard: deleted local files", "nzo_id", j.NzoID, "path", d)
	}
}

// jobDirUnder reports whether dir is a job directory: at least two levels
// (<category>/<release>) below base. Never the base or a category folder.
func jobDirUnder(base, dir string) bool {
	if base == "" || dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(dir))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return len(strings.Split(rel, string(filepath.Separator))) >= 2
}
