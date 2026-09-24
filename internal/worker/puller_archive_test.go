package worker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/downloader"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// newArchivePuller sets up one COMPLETED_TORBOX job whose TorBox item lists files.
func newArchivePuller(t *testing.T, wait time.Duration, files []torbox.MyListFile) (*Puller, *slowTorbox, string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "payload")
	}))
	t.Cleanup(srv.Close)

	st := newStore(t)
	ctx := context.Background()
	const nzo, folder = "arrarr_arch", "Station.Eleven.S01E01"
	tb := &slowTorbox{base: srv.URL, list: []torbox.MyListItem{{
		ID: 100, Name: folder, DownloadState: "completed", Files: files}}}
	j := &job.Job{NzoID: nzo, Category: "sonarr", Filename: folder + ".nzb",
		NzbSHA256: nzo, NzbBlob: []byte("nzb"), State: job.StateCompletedTorbox}
	if err := st.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE jobs SET state='COMPLETED_TORBOX', torbox_active_id=100 WHERE nzo_id=?`, nzo); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	dl, err := downloader.New(downloader.Options{BaseDir: base, Concurrency: 1, Logger: noopLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	p := NewPuller(PullerOptions{Store: st, Torbox: tb, Downloader: dl, BaseDir: base,
		ArchiveWait: wait,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil))})
	return p, tb, nzo, base
}

// elapseBackoff simulates the backoff running out.
func elapseBackoff(t *testing.T, p *Puller, nzo string) {
	t.Helper()
	if _, err := p.store.DB().Exec(`UPDATE jobs SET next_attempt_at=NULL WHERE nzo_id=?`, nzo); err != nil {
		t.Fatal(err)
	}
}

func file(id int64, name string) torbox.MyListFile {
	return torbox.MyListFile{ID: id, Name: "Station.Eleven.S01E01/" + name, ShortName: name, Size: 7}
}

func TestPullerSkipsArchivesNextToVideo(t *testing.T) {
	p, _, nzo, base := newArchivePuller(t, time.Hour, []torbox.MyListFile{
		file(1, "se.s01e01.mkv"), file(2, "se.s01e01.rar"), file(3, "se.s01e01.r00"), file(4, "se.s01e01.part01.rar"),
	})
	ctx := context.Background()
	p.Tick(ctx, 1)
	p.Wait()

	j, err := p.store.Get(ctx, nzo)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != job.StateReady {
		t.Fatalf("state = %s, want READY", j.State)
	}
	var names []string
	_ = filepath.Walk(base, func(path string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			names = append(names, info.Name())
		}
		return nil
	})
	if len(names) != 1 || names[0] != "se.s01e01.mkv" {
		t.Fatalf("pulled %v, want only the mkv", names)
	}
}

func TestPullerWaitsWhileOnlyArchivesListed(t *testing.T) {
	p, tb, nzo, _ := newArchivePuller(t, time.Hour, []torbox.MyListFile{
		file(1, "se.s01e01.rar"), file(2, "se.s01e01.r00"), file(3, "se.nfo"),
	})
	ctx := context.Background()
	for i := 0; i < 10; i++ { // more ticks than maxRetries
		p.Tick(ctx, 1)
		p.Wait()
		elapseBackoff(t, p, nzo)
	}
	j, _ := p.store.Get(ctx, nzo)
	if j.State != job.StateCompletedTorbox {
		t.Fatalf("state = %s, want COMPLETED_TORBOX while archives are unpacked", j.State)
	}
	if j.Attempts != 0 {
		t.Fatalf("attempts = %d, waiting must not burn attempts", j.Attempts)
	}

	// TorBox finishes extracting: the video appears next to the archives.
	tb.mu.Lock()
	tb.list[0].Files = append(tb.list[0].Files, file(9, "se.s01e01.mkv"))
	tb.mu.Unlock()
	p.Tick(ctx, 1)
	p.Wait()
	if j, _ = p.store.Get(ctx, nzo); j.State != job.StateReady {
		t.Fatalf("state = %s after extraction, want READY", j.State)
	}
}

func TestPullerFailsWhenExtractionNeverHappens(t *testing.T) {
	p, _, nzo, _ := newArchivePuller(t, 50*time.Millisecond, []torbox.MyListFile{
		file(1, "se.s01e01.rar"), file(2, "se.s01e01.r01"),
	})
	ctx := context.Background()
	p.Tick(ctx, 1)
	p.Wait()
	time.Sleep(80 * time.Millisecond)
	elapseBackoff(t, p, nzo)
	p.Tick(ctx, 1)
	p.Wait()
	j, _ := p.store.Get(ctx, nzo)
	if j.State != job.StateFailed {
		t.Fatalf("state = %s, want FAILED once the wait is over", j.State)
	}
}

func TestArchiveAndVideoClassification(t *testing.T) {
	for name, want := range map[string]bool{
		"a.rar": true, "a.R00": true, "a.r123": true, "a.part01.rar": true, "a.zip": true,
		"a.7z": true, "a.001": true, "a.mkv": false, "a.mp4": false, "a.r": false, "Show.2160p.mkv": false,
	} {
		if got := isArchiveFile(name); got != want {
			t.Errorf("isArchiveFile(%q) = %v, want %v", name, got, want)
		}
	}
	if !isVideoFile("A.MKV") || isVideoFile("a.rar") {
		t.Error("isVideoFile misclassifies")
	}
}

// A finished pull wakes the loop at once. A job in backoff must not be picked
// up again by that — it would poll TorBox back to back and hold a slot.
func TestPullerHonoursBackoff(t *testing.T) {
	p, tb, nzo, _ := newArchivePuller(t, time.Hour, []torbox.MyListFile{file(1, "se.s01e01.rar")})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		p.Tick(ctx, 1)
		p.Wait()
	}
	if got := tb.myLists.Load(); got != 1 {
		t.Fatalf("TorBox polled %d times during backoff, want 1", got)
	}
	elapseBackoff(t, p, nzo)
	p.Tick(ctx, 1)
	p.Wait()
	if got := tb.myLists.Load(); got != 2 {
		t.Fatalf("TorBox polled %d times after backoff, want 2", got)
	}
}
