// Package librarian writes structured library entries into a tree shaped for
// Sonarr/Radarr (their Root Folder) and Jellyfin (its library scan path).
//
// Two output modes:
//
//   - STRM:    <base>/<category>/<show or movie path>/<canonical name>.strm
//              File is a UTF-8 text file containing a TorBox CDN URL. Jellyfin
//              and Kodi follow STRM natively; the URL has a TTL so the caller
//              must refresh stale STRMs (URL refresh sweeper, Phase B-3).
//
//   - Symlink: <base>/<category>/<show or movie path>/<canonical name><.ext>
//              File is a relative symlink targeting the WebDAV mount path
//              (<mountBase>/<release>/<file>). Read-through via fuse mount.
//
// Filenames come from the ParseResult when supplied (Sonarr/Radarr's canonical
// metadata). Without it, we fall back to the original release folder + raw
// filename — Sonarr/Radarr will reparse on their own scan, so we never lose
// the file, only the prettiness of the tree.
//
// Writes are atomic: tmp file + fsync + rename for STRM, target-dir-create +
// os.Symlink for symlinks (which is atomic on POSIX).
package librarian

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pburkhalter/arrarr/internal/arrclient"
)

// FileInfo identifies a single playable file inside a TorBox download.
type FileInfo struct {
	ID        int64
	Name      string // raw filename inside the folder, possibly path-like
	ShortName string // basename
	Size      int64
	MimeType  string
}

// Item is everything the writer needs to lay down a library entry.
type Item struct {
	NzoID         string
	Category      string                 // "sonarr" / "radarr" / fallback "default"
	ReleaseName   string                 // TorBox folder name (original)
	Files         []FileInfo             // playable files inside the folder
	StreamingURLs map[int64]string       // file_id -> CDN URL (STRM mode only)
	Canonical     *arrclient.ParseResult // optional canonical naming oracle
	MountBase     string                 // for symlink mode: where the WebDAV is mounted
}

// Writer lays down library entries. Implementations must be idempotent — the
// same Item written twice yields the same on-disk result with no errors.
type Writer interface {
	// Write produces zero or more on-disk entries for the supplied Item.
	// Returns the primary library path (the first entry written) for caller
	// bookkeeping.
	Write(ctx context.Context, item Item) (string, error)
	// Mode returns the writer's library mode tag, e.g. "strm" or "webdav".
	Mode() string
}

// New returns a Writer for the given mode. base is the absolute root of the
// library tree. mountBase is required for symlink/both modes — it's the path
// inside the Arrarr container where /mnt/torbox is mounted.
//
// mode "off" returns a no-op writer (used to keep the worker loop simple even
// when v2 library output is disabled).
func New(mode, base, mountBase string) (Writer, error) {
	if base == "" {
		return nil, errors.New("librarian: base path required")
	}
	switch mode {
	case "off":
		return &nopWriter{}, nil
	case "strm":
		return &STRMWriter{Base: base}, nil
	case "webdav":
		if mountBase == "" {
			return nil, errors.New("librarian: webdav mode requires mountBase")
		}
		return &SymlinkWriter{Base: base, MountBase: mountBase}, nil
	case "both":
		if mountBase == "" {
			return nil, errors.New("librarian: both mode requires mountBase")
		}
		return &DualWriter{
			STRM:    &STRMWriter{Base: base},
			Symlink: &SymlinkWriter{Base: base, MountBase: mountBase},
		}, nil
	default:
		return nil, fmt.Errorf("librarian: unknown mode %q", mode)
	}
}

// --- nopWriter ---

type nopWriter struct{}

func (*nopWriter) Mode() string { return "off" }
func (*nopWriter) Write(_ context.Context, _ Item) (string, error) {
	return "", nil
}

// --- STRMWriter ---

// STRMWriter writes <file>.strm text files containing the streaming URL.
type STRMWriter struct {
	Base string
}

func (*STRMWriter) Mode() string { return "strm" }

func (w *STRMWriter) Write(_ context.Context, item Item) (string, error) {
	entries, err := layout(item)
	if err != nil {
		return "", err
	}
	var firstPath string
	for _, e := range entries {
		dst := filepath.Join(w.Base, e.RelPath+".strm")
		// Resolve URL: prefer per-file streaming URL, fall back to a single shared
		// URL keyed at 0 (treats the whole download as a single playable item).
		urlStr := item.StreamingURLs[e.FileID]
		if urlStr == "" {
			urlStr = item.StreamingURLs[0]
		}
		if urlStr == "" {
			return "", fmt.Errorf("librarian/strm: no streaming URL for file_id=%d (item=%s)", e.FileID, item.NzoID)
		}
		if err := writeFileAtomic(dst, []byte(urlStr+"\n"), 0o644); err != nil {
			return "", fmt.Errorf("librarian/strm: write %s: %w", dst, err)
		}
		if firstPath == "" {
			firstPath = dst
		}
	}
	return firstPath, nil
}

// --- SymlinkWriter ---

// SymlinkWriter creates relative symlinks pointing into the WebDAV mount.
type SymlinkWriter struct {
	Base      string
	MountBase string
}

func (*SymlinkWriter) Mode() string { return "webdav" }

func (w *SymlinkWriter) Write(_ context.Context, item Item) (string, error) {
	entries, err := layout(item)
	if err != nil {
		return "", err
	}
	var firstPath string
	for _, e := range entries {
		dst := filepath.Join(w.Base, e.RelPath+e.ExtFromSource())
		// Source absolute path for the file inside the WebDAV mount.
		srcAbs := filepath.Join(w.MountBase, item.ReleaseName, e.SourceName)
		// Make it relative to dst's directory so the library tree stays
		// relocatable (if you move /library, the symlinks still resolve as long
		// as MountBase is reachable from the new location).
		rel, err := filepath.Rel(filepath.Dir(dst), srcAbs)
		if err != nil {
			rel = srcAbs
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		// Idempotent: replace existing link if its target differs.
		if existing, err := os.Readlink(dst); err == nil {
			if existing == rel {
				if firstPath == "" {
					firstPath = dst
				}
				continue
			}
			_ = os.Remove(dst)
		}
		if err := os.Symlink(rel, dst); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("librarian/symlink: %s -> %s: %w", dst, rel, err)
		}
		if firstPath == "" {
			firstPath = dst
		}
	}
	return firstPath, nil
}

// --- DualWriter ---

// DualWriter writes both STRM and symlink entries (for A/B comparison or for
// gradual migration between modes).
type DualWriter struct {
	STRM    Writer
	Symlink Writer
}

func (*DualWriter) Mode() string { return "both" }

func (w *DualWriter) Write(ctx context.Context, item Item) (string, error) {
	first, err := w.STRM.Write(ctx, item)
	if err != nil {
		return "", err
	}
	if _, err := w.Symlink.Write(ctx, item); err != nil {
		return "", err
	}
	return first, nil
}

// --- layout: derive on-disk paths for an Item's files ---

type entry struct {
	FileID     int64
	RelPath    string // relative to base, NO extension
	SourceName string // raw file name inside the TorBox folder (used by symlink)
	SourceExt  string // including dot, e.g. ".mkv"
}

func (e entry) ExtFromSource() string {
	if e.SourceExt != "" {
		return e.SourceExt
	}
	return ""
}

// layout decides the relative paths for each playable file in an Item.
// Non-video files are skipped. If no playable files are present, an empty
// slice is returned (the worker treats this as a successful no-op).
func layout(item Item) ([]entry, error) {
	playable := filterPlayable(item.Files)
	if len(playable) == 0 {
		// No playable files (e.g. all .nfo / metadata). Still write a single
		// placeholder entry using the release name so the library reflects the
		// download exists. This is what Sonarr does with extras-only releases.
		return []entry{{
			FileID:     0,
			RelPath:    filepath.Join(topLevel(item.Category), sanitize(item.ReleaseName), sanitize(item.ReleaseName)),
			SourceName: item.ReleaseName,
		}}, nil
	}
	if item.Canonical != nil && item.Canonical.HasSeasonEpisode() {
		return seriesLayout(item, playable), nil
	}
	if item.Canonical != nil && item.Canonical.HasMovie() {
		return movieLayout(item, playable), nil
	}
	// Fallback: no canonical info → use release folder + raw filenames.
	return fallbackLayout(item, playable), nil
}

func seriesLayout(item Item, files []FileInfo) []entry {
	c := item.Canonical
	showDir := sanitize(c.Title)
	seasonDir := fmt.Sprintf("Season %02d", c.Season)
	out := make([]entry, 0, len(files))
	for i, f := range files {
		ep := 0
		if i < len(c.Episodes) {
			ep = c.Episodes[i]
		} else if len(c.Episodes) > 0 {
			ep = c.Episodes[0]
		}
		base := fmt.Sprintf("%s - S%02dE%02d", c.Title, c.Season, ep)
		if c.EpisodeName != "" && i == 0 {
			base = fmt.Sprintf("%s - %s", base, c.EpisodeName)
		}
		out = append(out, entry{
			FileID:     f.ID,
			RelPath:    filepath.Join(topLevel(item.Category), showDir, seasonDir, sanitize(base)),
			SourceName: f.Name,
			SourceExt:  ext(f.ShortName, f.Name),
		})
	}
	return out
}

func movieLayout(item Item, files []FileInfo) []entry {
	c := item.Canonical
	movieDir := sanitize(fmt.Sprintf("%s (%d)", c.Title, c.Year))
	out := make([]entry, 0, len(files))
	for i, f := range files {
		base := movieDir
		if c.Quality != "" {
			base = fmt.Sprintf("%s - %s", movieDir, c.Quality)
		}
		// In multi-file movie releases (extras, alt cuts), append index.
		if i > 0 {
			base = fmt.Sprintf("%s - part%d", base, i+1)
		}
		out = append(out, entry{
			FileID:     f.ID,
			RelPath:    filepath.Join(topLevel(item.Category), movieDir, sanitize(base)),
			SourceName: f.Name,
			SourceExt:  ext(f.ShortName, f.Name),
		})
	}
	return out
}

func fallbackLayout(item Item, files []FileInfo) []entry {
	relDir := sanitize(item.ReleaseName)
	out := make([]entry, 0, len(files))
	for _, f := range files {
		baseName := f.ShortName
		if baseName == "" {
			baseName = filepath.Base(f.Name)
		}
		baseName = strings.TrimSuffix(baseName, ext("", f.Name))
		out = append(out, entry{
			FileID:     f.ID,
			RelPath:    filepath.Join(topLevel(item.Category), relDir, sanitize(baseName)),
			SourceName: f.Name,
			SourceExt:  ext(f.ShortName, f.Name),
		})
	}
	return out
}

// topLevel maps a category to a stable top-level dir.
func topLevel(category string) string {
	switch strings.ToLower(category) {
	case "sonarr", "tv", "tv-sonarr", "anime":
		return "series"
	case "radarr", "movies":
		return "movies"
	default:
		return "other"
	}
}

// playable file extensions Jellyfin/Kodi understand. Stays conservative; if
// you grab esoteric containers add them here.
var playableExt = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true,
	".wmv": true, ".ts":  true, ".m2ts": true, ".webm": true, ".flv": true,
}

func filterPlayable(files []FileInfo) []FileInfo {
	out := make([]FileInfo, 0, len(files))
	for _, f := range files {
		e := strings.ToLower(filepath.Ext(f.ShortName))
		if e == "" {
			e = strings.ToLower(filepath.Ext(f.Name))
		}
		if playableExt[e] {
			out = append(out, f)
		}
	}
	return out
}

func ext(short, full string) string {
	if e := filepath.Ext(short); e != "" {
		return e
	}
	return filepath.Ext(full)
}

// sanitize strips characters that misbehave in filenames across the platforms
// Jellyfin runs on. Kept conservative; we want libraries to roundtrip across
// Linux/macOS/SMB-shared volumes.
func sanitize(name string) string {
	if name == "" {
		return "untitled"
	}
	r := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", " -",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
		"\x00", "",
	)
	out := r.Replace(name)
	// Collapse repeated dots (a few release groups produce "..")
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	out = strings.TrimSpace(out)
	out = strings.Trim(out, ".")
	if out == "" {
		return "untitled"
	}
	return out
}

// writeFileAtomic writes data to path via tmp + rename, fsynced. Caller-side
// directory creation is implicit (we MkdirAll first).
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
