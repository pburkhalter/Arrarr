package librarian

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pburkhalter/arrarr/internal/arrclient"
)

func TestSTRMWriterSeriesWithCanonical(t *testing.T) {
	dir := t.TempDir()
	w, err := New("strm", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	item := Item{
		NzoID:       "arrarr_xyz",
		Category:    "sonarr",
		ReleaseName: "Twisted.Metal.S02E01.GERMAN.DL.1080p.WEB.H264-WAYNE",
		Files: []FileInfo{
			{ID: 7, Name: "Twisted.Metal.S02E01.WAYNE.mkv", ShortName: "Twisted.Metal.S02E01.WAYNE.mkv", Size: 1234, MimeType: "video/x-matroska"},
		},
		StreamingURLs: map[int64]string{7: "https://cdn.torbox.app/abc/file.mkv?sig=xyz"},
		Canonical: &arrclient.ParseResult{
			MediaType:   arrclient.MediaTypeSeries,
			Title:       "Twisted Metal",
			Season:      2,
			Episodes:    []int{1},
			EpisodeName: "Pilot",
			Quality:     "WEBDL-1080p",
		},
	}
	got, err := w.Write(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "series", "Twisted Metal", "Season 02", "Twisted Metal - S02E01 - Pilot.strm")
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
	body, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "https://cdn.torbox.app/abc/file.mkv") {
		t.Errorf("unexpected STRM contents: %q", body)
	}
}

func TestSTRMWriterMovieWithCanonical(t *testing.T) {
	dir := t.TempDir()
	w, _ := New("strm", dir, "")
	item := Item{
		NzoID:       "arrarr_mov",
		Category:    "radarr",
		ReleaseName: "Hoppers.2026.GERMAN.DL.1080p.WEB.H264-MGE",
		Files: []FileInfo{
			{ID: 11, Name: "Hoppers.2026.MGE.mkv", ShortName: "Hoppers.2026.MGE.mkv"},
		},
		StreamingURLs: map[int64]string{11: "https://cdn.torbox.app/h/h.mkv?sig=q"},
		Canonical: &arrclient.ParseResult{
			MediaType: arrclient.MediaTypeMovie,
			Title:     "Hoppers",
			Year:      2026,
			Quality:   "WEBDL-1080p",
		},
	}
	got, err := w.Write(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "movies", "Hoppers (2026)", "Hoppers (2026) - WEBDL-1080p.strm")
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestSTRMWriterFallbackWithoutCanonical(t *testing.T) {
	dir := t.TempDir()
	w, _ := New("strm", dir, "")
	item := Item{
		NzoID:       "arrarr_fb",
		Category:    "sonarr",
		ReleaseName: "Some.Weird.Release.NAME-XX",
		Files: []FileInfo{
			{ID: 1, Name: "ep01.mkv", ShortName: "ep01.mkv"},
		},
		StreamingURLs: map[int64]string{1: "u"},
		// Canonical is nil
	}
	got, err := w.Write(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "series", "Some.Weird.Release.NAME-XX", "ep01.strm")
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

func TestSTRMWriterMissingURLErrors(t *testing.T) {
	dir := t.TempDir()
	w, _ := New("strm", dir, "")
	item := Item{
		NzoID:    "arrarr_n",
		Category: "sonarr",
		Files: []FileInfo{
			{ID: 99, Name: "x.mkv", ShortName: "x.mkv"},
		},
		// no StreamingURLs
	}
	_, err := w.Write(context.Background(), item)
	if err == nil {
		t.Fatal("expected error when URL is missing")
	}
}

func TestSTRMWriterIsAtomic(t *testing.T) {
	// Verify that intermediate .tmp files are gone after a successful write.
	dir := t.TempDir()
	w, _ := New("strm", dir, "")
	item := Item{
		NzoID:       "arrarr_a",
		Category:    "sonarr",
		ReleaseName: "X",
		Files:       []FileInfo{{ID: 1, Name: "x.mkv", ShortName: "x.mkv"}},
		StreamingURLs: map[int64]string{1: "u"},
	}
	if _, err := w.Write(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	count := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ".tmp") {
			t.Errorf("found leftover tmp: %s", p)
		}
		count++
		return nil
	})
	if count == 0 {
		t.Error("expected at least one written file")
	}
}

func TestSTRMWriterIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, _ := New("strm", dir, "")
	item := Item{
		NzoID:       "arrarr_i",
		Category:    "sonarr",
		ReleaseName: "X",
		Files:       []FileInfo{{ID: 1, Name: "x.mkv", ShortName: "x.mkv"}},
		StreamingURLs: map[int64]string{1: "u"},
	}
	first, err := w.Write(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Write(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("path differs across writes: %s vs %s", first, second)
	}
	body, _ := os.ReadFile(first)
	if string(body) != "u\n" {
		t.Errorf("body=%q", body)
	}
}

func TestSymlinkWriterCreatesRelativeLink(t *testing.T) {
	libBase := t.TempDir()
	mountBase := t.TempDir()
	// Pre-create the source the symlink will target so test asserts a real
	// relative path resolution.
	srcDir := filepath.Join(mountBase, "RelName")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "ep.mkv")
	if err := os.WriteFile(srcFile, []byte("\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := New("webdav", libBase, mountBase)
	if err != nil {
		t.Fatal(err)
	}
	item := Item{
		NzoID:       "arrarr_s",
		Category:    "sonarr",
		ReleaseName: "RelName",
		Files: []FileInfo{
			{ID: 1, Name: "ep.mkv", ShortName: "ep.mkv"},
		},
		MountBase: mountBase,
	}
	got, err := w.Write(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "ep.mkv") {
		t.Errorf("path=%q want suffix .mkv", got)
	}
	target, err := os.Readlink(got)
	if err != nil {
		t.Fatal(err)
	}
	// Should be relative.
	if filepath.IsAbs(target) {
		t.Errorf("symlink target should be relative: %q", target)
	}
	// Resolve relative to the symlink's dir; must point at our source file.
	abs := filepath.Join(filepath.Dir(got), target)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(srcFile)
	if resolved != wantResolved {
		t.Errorf("symlink resolves to %q want %q", resolved, wantResolved)
	}
}

// TestSymlinkWriterHandlesReleasePrefixedNames exercises the bug where TorBox
// returns f.Name with the release name already as the first path component
// (e.g. "Rick.S09E01/file.mkv"). Pre-fix the writer concatenated MountBase +
// ReleaseName + Name → double release-name in the target. Now the writer
// strips any leading "<ReleaseName>/" before prepending, idempotent for all
// three shapes: flat name, release-prefixed name, release/episodeDir name.
func TestSymlinkWriterHandlesReleasePrefixedNames(t *testing.T) {
	cases := []struct {
		desc         string
		releaseName  string
		fileName     string // what TorBox returns as f.Name
		srcRelPath   string // path of real file relative to mountBase
	}{
		{
			desc:        "name without release prefix (flat)",
			releaseName: "RelName",
			fileName:    "ep.mkv",
			srcRelPath:  "RelName/ep.mkv",
		},
		{
			desc:        "name with release prefix (single-episode shape)",
			releaseName: "RelName",
			fileName:    "RelName/ep.mkv",
			srcRelPath:  "RelName/ep.mkv",
		},
		{
			desc:        "name with release prefix + episode subdir (season pack shape)",
			releaseName: "RelName.S01",
			fileName:    "RelName.S01/RelName.S01E01/ep.mkv",
			srcRelPath:  "RelName.S01/RelName.S01E01/ep.mkv",
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			libBase := t.TempDir()
			mountBase := t.TempDir()
			srcAbs := filepath.Join(mountBase, tc.srcRelPath)
			if err := os.MkdirAll(filepath.Dir(srcAbs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(srcAbs, []byte("\x00\x00"), 0o644); err != nil {
				t.Fatal(err)
			}

			w, _ := New("webdav", libBase, mountBase)
			item := Item{
				NzoID:       "arrarr_p",
				Category:    "sonarr",
				ReleaseName: tc.releaseName,
				Files:       []FileInfo{{ID: 1, Name: tc.fileName, ShortName: filepath.Base(tc.fileName)}},
				MountBase:   mountBase,
			}
			got, err := w.Write(context.Background(), item)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := filepath.EvalSymlinks(got)
			if err != nil {
				t.Fatalf("symlink should resolve, got error: %v", err)
			}
			wantResolved, _ := filepath.EvalSymlinks(srcAbs)
			if resolved != wantResolved {
				t.Errorf("symlink resolved to %q, want %q", resolved, wantResolved)
			}
		})
	}
}

func TestSymlinkWriterIsIdempotent(t *testing.T) {
	libBase := t.TempDir()
	mountBase := t.TempDir()
	w, _ := New("webdav", libBase, mountBase)
	item := Item{
		NzoID:       "arrarr_si",
		Category:    "radarr",
		ReleaseName: "Mov",
		Files:       []FileInfo{{ID: 1, Name: "f.mkv", ShortName: "f.mkv"}},
		MountBase:   mountBase,
	}
	if _, err := w.Write(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(context.Background(), item); err != nil {
		t.Fatalf("second write should succeed: %v", err)
	}
}

func TestNewModeOff(t *testing.T) {
	w, err := New("off", "/anywhere", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Write(context.Background(), Item{})
	if err != nil || got != "" {
		t.Errorf("nopWriter should return empty path nil err, got %q %v", got, err)
	}
	if w.Mode() != "off" {
		t.Errorf("mode=%q", w.Mode())
	}
}

func TestNewRejectsUnknownMode(t *testing.T) {
	_, err := New("garbage", "/x", "")
	if err == nil {
		t.Error("expected error")
	}
}

func TestNewRejectsWebdavWithoutMountBase(t *testing.T) {
	_, err := New("webdav", "/x", "")
	if err == nil {
		t.Error("expected error")
	}
}

func TestSanitizeRemovesUnsafeChars(t *testing.T) {
	cases := map[string]string{
		"Twisted Metal":      "Twisted Metal",
		"Foo: Bar":           "Foo - Bar",
		"Some/path":          "Some-path",
		`bad"chars*?<>|`:     "badchars",
		"":                   "untitled",
		"...":                "untitled",
		"trailing.":          "trailing",
		" double..dot ":      "double.dot",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlayableExtFilters(t *testing.T) {
	files := []FileInfo{
		{ID: 1, ShortName: "ep.mkv"},
		{ID: 2, ShortName: "subs.srt"},
		{ID: 3, ShortName: "info.nfo"},
		{ID: 4, ShortName: "preview.mp4"},
	}
	got := filterPlayable(files)
	if len(got) != 2 {
		t.Errorf("expected 2 playable, got %d", len(got))
	}
	for _, f := range got {
		ext := strings.ToLower(filepath.Ext(f.ShortName))
		if !playableExt[ext] {
			t.Errorf("non-playable file leaked through: %s", f.ShortName)
		}
	}
}
