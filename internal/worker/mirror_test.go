package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/librarian"
	"github.com/pburkhalter/arrarr/internal/pathmap"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// mirrorTorbox is a programmable mylist source for the mirror worker. The
// items field can be swapped between sweeps to simulate downloads coming and
// going on the TorBox account.
type mirrorTorbox struct {
	mu          sync.Mutex
	items       []torbox.MyListItem
	requests    atomic.Int32
	mylistCalls atomic.Int32
}

func (mt *mirrorTorbox) setItems(items []torbox.MyListItem) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.items = items
}

func (mt *mirrorTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	return nil, errors.New("not used")
}
func (mt *mirrorTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	mt.mylistCalls.Add(1)
	mt.mu.Lock()
	defer mt.mu.Unlock()
	out := make([]torbox.MyListItem, len(mt.items))
	copy(out, mt.items)
	return out, nil
}
func (mt *mirrorTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error { return nil }
func (mt *mirrorTorbox) EditUsenet(_ context.Context, _ int64, _ torbox.EditUsenetParams) error {
	return nil
}
func (mt *mirrorTorbox) RequestUsenetDL(_ context.Context, _, fileID int64, _ bool) (string, error) {
	n := mt.requests.Add(1)
	return "https://cdn.torbox.app/m/" + strconv.FormatInt(int64(n), 10) + "/" + strconv.FormatInt(fileID, 10), nil
}

func TestSniffCategoryHeuristics(t *testing.T) {
	cases := map[string]string{
		"Twisted.Metal.S02E01.GERMAN.DL.1080p.WEB.H264-WAYNE":     "tv-sonarr",
		"The.Curse.of.Oak.Island.S13E08.1080p.WEB.h264-EDITH":     "tv-sonarr",
		"Bluey.S01.COMPLETE.720p.DSNP.WEBRip.x264-GalaxyTV":       "tv-sonarr", // season pack
		"The.Americans (2013) Season 1-6 S01-S06":                 "tv-sonarr", // multi-season
		"Goof Troop S01-S02 (1992-)":                              "tv-sonarr",
		"Hoppers.2026.GERMAN.DL.1080p.WEB.H264-MGE":               "movies",
		"Pokemon Detective Pikachu (2019)":                        "movies",
		"weird-no-pattern-name":                                   "other",
		"":                                                        "other",
	}
	for in, want := range cases {
		if got := sniffCategory(in); got != want {
			t.Errorf("sniffCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMirrorTickInsertsTerminalItemsOnly(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	tb := &mirrorTorbox{}
	tb.setItems([]torbox.MyListItem{
		// Terminal — should be mirrored.
		{ID: 100, Name: "Twisted.Metal.S02E01.WAYNE", DownloadFinished: true,
			DownloadState: "completed",
			Files: []torbox.MyListFile{{ID: 1, ShortName: "Twisted.Metal.S02E01.WAYNE.mkv", Name: "Twisted.Metal.S02E01.WAYNE/file.mkv"}}},
		// Still downloading — should be skipped this sweep.
		{ID: 101, Name: "Hoppers.2026.MGE", DownloadState: "downloading",
			DownloadFinished: false},
		// No id at all — should be skipped.
		{Name: "ghost.release", DownloadFinished: true, DownloadState: "completed"},
		// No name — should be skipped (we use name as folder).
		{ID: 102, DownloadFinished: true, DownloadState: "completed"},
	})

	libDir := t.TempDir()
	libWriter, _ := librarian.New("strm", libDir, "/mnt/torbox")
	mgr := New(Options{
		Store:                    st,
		Torbox:                   tb,
		PathMap:                  pathmap.New("/mnt/torbox", "/torbox"),
		Logger:                   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize:           1,
		Librarian:                libWriter,
		LocalVerifyBase:          "/mnt/torbox",
		StreamingURLRefreshAfter: 5 * time.Hour,
		MirrorEnabled:            true,
		MirrorPollInterval:       time.Minute,
	})

	mgr.mirrorTick(context.Background())

	// Only one row should have been inserted (id=100, terminal+named).
	var count int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM jobs WHERE origin = 'mirror'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("mirror row count=%d want 1", count)
	}

	// Inspect the row we expect.
	var nzoID, category, folder, state, libState string
	var torboxID int64
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT nzo_id, category, torbox_folder_name, state, library_writer_state, torbox_active_id
		 FROM jobs WHERE origin = 'mirror'`).Scan(
		&nzoID, &category, &folder, &state, &libState, &torboxID); err != nil {
		t.Fatal(err)
	}
	if nzoID != "mirror_100" {
		t.Errorf("nzo_id=%q want mirror_100", nzoID)
	}
	if category != "tv-sonarr" {
		t.Errorf("category=%q want tv-sonarr (S02E01 in name)", category)
	}
	if folder != "Twisted.Metal.S02E01.WAYNE" {
		t.Errorf("folder=%q", folder)
	}
	if state != "COMPLETED_TORBOX" {
		t.Errorf("state=%q want COMPLETED_TORBOX", state)
	}
	if libState != "pending" {
		t.Errorf("library_writer_state=%q want pending", libState)
	}
	if torboxID != 100 {
		t.Errorf("torbox_active_id=%d want 100", torboxID)
	}
}

func TestMirrorTickIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	tb := &mirrorTorbox{}
	tb.setItems([]torbox.MyListItem{
		{ID: 200, Name: "Show.S01E01", DownloadFinished: true, DownloadState: "completed"},
	})
	libWriter, _ := librarian.New("strm", t.TempDir(), "/mnt/torbox")
	mgr := New(Options{
		Store:           st,
		Torbox:          tb,
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize:  1,
		Librarian:       libWriter,
		LocalVerifyBase: "/mnt/torbox",
		MirrorEnabled:   true,
	})

	for i := 0; i < 3; i++ {
		mgr.mirrorTick(context.Background())
	}
	var count int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM jobs WHERE origin = 'mirror'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("mirror rows after 3 sweeps = %d, want 1 (idempotent upsert)", count)
	}
}

func TestMirrorOrphanSweepDeletesStaleRowsAndFiles(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	libDir := t.TempDir()
	tb := &mirrorTorbox{}
	libWriter, _ := librarian.New("strm", libDir, "/mnt/torbox")
	mgr := New(Options{
		Store:           st,
		Torbox:          tb,
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize:  1,
		Librarian:       libWriter,
		LocalVerifyBase: "/mnt/torbox",
		MirrorEnabled:   true,
	})

	// Seed two mirror rows — one will become orphaned, one stays live.
	for _, id := range []int64{300, 301} {
		spec := store.MirrorJobSpec{
			TorboxID:   id,
			FolderName: "X",
			Category:   "movies",
		}
		_, _, err := st.UpsertMirrorJob(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		// Pretend the librarian has already written each one's library file.
		libPath := filepath.Join(libDir, "movies", "X-"+strconv.FormatInt(id, 10), "x.strm")
		if err := os.MkdirAll(filepath.Dir(libPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(libPath, []byte("u"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _ = st.DB().ExecContext(context.Background(),
			`UPDATE jobs SET library_path = ?, library_writer_state = 'written', state = 'READY'
			 WHERE nzo_id = ?`, libPath, "mirror_"+strconv.FormatInt(id, 10))
	}

	// TorBox now reports only id=301 — so 300 is orphaned.
	tb.setItems([]torbox.MyListItem{
		{ID: 301, Name: "Y.2026", DownloadFinished: true, DownloadState: "completed"},
	})

	mgr.mirrorTick(context.Background())

	// 300's row + library file gone; 301 still present.
	var n int
	_ = st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM jobs WHERE nzo_id = 'mirror_300'`).Scan(&n)
	if n != 0 {
		t.Errorf("orphaned row should be deleted, count=%d", n)
	}
	if _, err := os.Stat(filepath.Join(libDir, "movies", "X-300", "x.strm")); !os.IsNotExist(err) {
		t.Errorf("orphan library file should be removed: %v", err)
	}

	_ = st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM jobs WHERE nzo_id = 'mirror_301'`).Scan(&n)
	if n != 1 {
		t.Errorf("live mirror row should survive, count=%d", n)
	}
	if _, err := os.Stat(filepath.Join(libDir, "movies", "X-301", "x.strm")); err != nil {
		t.Errorf("live mirror file should remain: %v", err)
	}
}

func TestMirrorOrphanSweepSkipsOnEmptyMyList(t *testing.T) {
	// If TorBox returns empty mylist (transient API blip), we MUST NOT
	// nuke every mirror row — that'd mass-delete the user's library on a
	// network hiccup.
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	tb := &mirrorTorbox{} // empty items
	libWriter, _ := librarian.New("strm", t.TempDir(), "/mnt/torbox")
	mgr := New(Options{
		Store:           st,
		Torbox:          tb,
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize:  1,
		Librarian:       libWriter,
		LocalVerifyBase: "/mnt/torbox",
		MirrorEnabled:   true,
	})

	// Pre-seed a mirror row that would be "orphaned" if we naively swept.
	_, _, _ = st.UpsertMirrorJob(context.Background(), store.MirrorJobSpec{
		TorboxID: 400, FolderName: "Z", Category: "movies",
	})

	mgr.mirrorTick(context.Background())

	var n int
	_ = st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM jobs WHERE origin = 'mirror'`).Scan(&n)
	if n != 1 {
		t.Errorf("mirror row should survive empty mylist response; count=%d", n)
	}
}

func TestMirrorRowsRoutedThroughLibrarian(t *testing.T) {
	// End-to-end: a mirror row inserted with library_writer_state='pending'
	// must be picked up by the librarian (which now scans all origins).
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	tb := &mirrorTorbox{}
	tb.setItems([]torbox.MyListItem{
		{
			ID: 500, Name: "Mirrored.S01E01",
			DownloadFinished: true, DownloadState: "completed",
			Files: []torbox.MyListFile{{ID: 7, ShortName: "ep.mkv", Name: "Mirrored.S01E01/ep.mkv", MimeType: "video/x-matroska"}},
		},
	})

	libDir := t.TempDir()
	libWriter, _ := librarian.New("strm", libDir, "/mnt/torbox")
	mgr := New(Options{
		Store:                    st,
		Torbox:                   tb,
		Logger:                   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize:           1,
		Librarian:                libWriter,
		LocalVerifyBase:          "/mnt/torbox",
		StreamingURLRefreshAfter: 5 * time.Hour,
		MirrorEnabled:            true,
	})

	mgr.mirrorTick(context.Background())   // inserts mirror_500 in COMPLETED_TORBOX/pending
	mgr.librarianTick(context.Background()) // librarian writes file, transitions to READY

	var state, libState string
	var libPath *string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT state, library_writer_state, library_path FROM jobs WHERE nzo_id = 'mirror_500'`).
		Scan(&state, &libState, &libPath); err != nil {
		t.Fatal(err)
	}
	if state != string(job.StateReady) {
		t.Errorf("state=%q want READY", state)
	}
	if libState != "written" {
		t.Errorf("library_writer_state=%q want written", libState)
	}
	if libPath == nil || *libPath == "" {
		t.Error("library_path should be populated")
	}
	if _, err := os.Stat(*libPath); err != nil {
		t.Errorf("library file should exist on disk: %v", err)
	}
}

func TestMirrorLoopDisabledWhenLibrarianMissing(t *testing.T) {
	// Mirror without librarian is meaningless — the worker should bail.
	dir := t.TempDir()
	st, _ := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	defer st.Close()
	_ = st.Migrate(context.Background())

	tb := &mirrorTorbox{}
	tb.setItems([]torbox.MyListItem{{ID: 600, Name: "x", DownloadFinished: true, DownloadState: "completed"}})

	mgr := New(Options{
		Store:          st,
		Torbox:         tb,
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize: 1,
		MirrorEnabled:  true,
		// Librarian: nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	mgr.mirrorLoop(ctx) // should return immediately

	var n int
	_ = st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM jobs WHERE origin = 'mirror'`).Scan(&n)
	if n != 0 {
		t.Errorf("loop should have bailed without librarian; rows=%d", n)
	}
}
