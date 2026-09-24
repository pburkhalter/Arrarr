package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/downloader"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// slowTorbox serves one single-file release per job — the shape of a normal
// episode grab, where Downloader.Concurrency has nothing to parallelise.
type slowTorbox struct {
	base    string
	mu      sync.Mutex
	list    []torbox.MyListItem
	myLists atomic.Int64
}

func (s *slowTorbox) MyList(context.Context, bool) ([]torbox.MyListItem, error) {
	s.myLists.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list, nil
}
func (s *slowTorbox) MyListTorrents(context.Context, bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (s *slowTorbox) RequestUsenetDL(_ context.Context, id, fileID int64, _ bool) (string, error) {
	return fmt.Sprintf("%s/dl/%d/%d", s.base, id, fileID), nil
}
func (s *slowTorbox) RequestTorrentDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}

// Pulling releases one after another makes every job wait out its predecessors.
// Measured on a 22-episode batch in Sep 2026: TorBox finished in a median of
// 28s, the local pull in a median of 60 minutes — almost entirely queueing.
func TestPullerPullsJobsConcurrently(t *testing.T) {
	const jobs = 4
	const perFileDelay = 300 * time.Millisecond

	var inFlight, maxInFlight atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cur := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(perFileDelay)
		inFlight.Add(-1)
		_, _ = io.WriteString(w, "payload")
	}))
	defer srv.Close()

	st := newStore(t)
	ctx := context.Background()

	tb := &slowTorbox{base: srv.URL}
	for i := 0; i < jobs; i++ {
		nzo := "arrarr_p" + strconv.Itoa(i)
		folder := "Show.S01E0" + strconv.Itoa(i)
		tb.list = append(tb.list, torbox.MyListItem{
			ID: int64(100 + i), Name: folder, DownloadState: "completed",
			Files: []torbox.MyListFile{{ID: 1, Name: folder + "/file.mkv",
				ShortName: "file.mkv", Size: 7, MimeType: "video/x-matroska"}},
		})
		j := &job.Job{NzoID: nzo, Category: "sonarr", Filename: folder + ".nzb",
			NzbSHA256: nzo, NzbBlob: []byte("nzb"), State: job.StateCompletedTorbox}
		if err := st.Insert(ctx, j); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx,
			`UPDATE jobs SET state='COMPLETED_TORBOX', torbox_active_id=? WHERE nzo_id=?`,
			100+i, nzo); err != nil {
			t.Fatal(err)
		}
	}

	dl, err := downloader.New(downloader.Options{
		BaseDir: t.TempDir(), Concurrency: 1, Logger: noopLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	p := NewPuller(PullerOptions{Store: st, Torbox: tb, Downloader: dl,
		BaseDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	start := time.Now()
	p.Tick(ctx, jobs)
	p.Wait()
	elapsed := time.Since(start)

	if got := maxInFlight.Load(); got < 2 {
		t.Errorf("hoechstens %d Abruf(e) gleichzeitig — der Puller arbeitet die Jobs seriell ab", got)
	}
	// Seriell braeuchte es jobs*perFileDelay; nebenlaeufig deutlich weniger.
	if elapsed > time.Duration(jobs)*perFileDelay {
		t.Errorf("Dauer %v >= serielle Untergrenze %v", elapsed, time.Duration(jobs)*perFileDelay)
	}
	for i := 0; i < jobs; i++ {
		got, err := st.Get(ctx, "arrarr_p"+strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		if got.State != job.StateReady {
			t.Errorf("Job %d: state=%s want READY", i, got.State)
		}
	}
	t.Logf("gleichzeitig=%d Dauer=%v (seriell waeren >=%v)", maxInFlight.Load(), elapsed,
		time.Duration(jobs)*perFileDelay)
}

// A large release used to hold its whole batch: Tick waited for every job it
// started, so the other slots idled until the slowest pull finished.
func TestPullerRefillsSlotsWithoutWaitingForSlowPull(t *testing.T) {
	const slow = 1500 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/100/") {
			time.Sleep(slow)
		} else {
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "payload")
	}))
	defer srv.Close()

	st := newStore(t)
	ctx := context.Background()
	tb := &slowTorbox{base: srv.URL}
	var nzos []string
	for i := 0; i < 5; i++ { // job 0 is the big film, 1–4 are episodes
		nzo := "arrarr_r" + strconv.Itoa(i)
		folder := "Rel" + strconv.Itoa(i)
		nzos = append(nzos, nzo)
		tb.list = append(tb.list, torbox.MyListItem{
			ID: int64(100 + i), Name: folder, DownloadState: "completed",
			Files: []torbox.MyListFile{{ID: 1, Name: folder + "/file.mkv", ShortName: "file.mkv", Size: 7}},
		})
		j := &job.Job{NzoID: nzo, Category: "radarr", Filename: folder + ".nzb",
			NzbSHA256: nzo, NzbBlob: []byte("nzb"), State: job.StateCompletedTorbox}
		if err := st.Insert(ctx, j); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx,
			`UPDATE jobs SET state='COMPLETED_TORBOX', torbox_active_id=? WHERE nzo_id=?`, 100+i, nzo); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond) // keep created_at order: the film is listed first
	}
	dl, err := downloader.New(downloader.Options{BaseDir: t.TempDir(), Concurrency: 1, Logger: noopLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	p := NewPuller(PullerOptions{Store: st, Torbox: tb, Downloader: dl,
		BaseDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	defer p.Wait()

	start := time.Now()
	for time.Since(start) < 3*slow {
		p.Tick(ctx, 2) // two slots: the film blocks one, episodes cycle through the other
		allEpisodes := true
		for _, nzo := range nzos[1:] {
			if j, _ := st.Get(ctx, nzo); j.State != job.StateReady {
				allEpisodes = false
			}
		}
		if allEpisodes {
			if el := time.Since(start); el >= slow {
				t.Fatalf("episodes done after %s — they waited for the %s film", el, slow)
			}
			return
		}
		select {
		case <-p.wake:
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("episodes never finished")
}
