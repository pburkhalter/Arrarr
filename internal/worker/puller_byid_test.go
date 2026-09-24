package worker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pburkhalter/arrarr/internal/downloader"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// byIDTorbox serves the single-entry lookup and counts full-list fetches.
type byIDTorbox struct {
	*slowTorbox
	fullLists atomic.Int32
	byIDCalls atomic.Int32
}

func (b *byIDTorbox) MyList(ctx context.Context, bypass bool) ([]torbox.MyListItem, error) {
	b.fullLists.Add(1)
	return b.slowTorbox.MyList(ctx, bypass)
}

func (b *byIDTorbox) MyListByID(_ context.Context, id int64, _ bool) (*torbox.MyListItem, error) {
	b.byIDCalls.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.list {
		if b.list[i].ID == id || b.list[i].QueueID == id {
			it := b.list[i]
			return &it, nil
		}
	}
	return nil, nil
}

func (b *byIDTorbox) MyListTorrentByID(context.Context, int64, bool) (*torbox.MyListItem, error) {
	return nil, nil
}

// The puller fetched the account's whole TorBox history to find one entry —
// 5.6 MB per job by Sep 2026, on top of the poller doing the same every 30s.
func TestPullerLooksUpItemByIDInsteadOfTheFullList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "payload")
	}))
	defer srv.Close()

	st := newStore(t)
	ctx := context.Background()
	tb := &byIDTorbox{slowTorbox: &slowTorbox{base: srv.URL}}
	tb.list = []torbox.MyListItem{{ID: 700, Name: "Rel", DownloadState: "completed",
		Files: []torbox.MyListFile{{ID: 1, Name: "Rel/file.mkv", ShortName: "file.mkv", Size: 7}}}}
	j := &job.Job{NzoID: "arrarr_byid", Category: "sonarr", Filename: "Rel.nzb",
		NzbSHA256: "byid", NzbBlob: []byte("nzb"), State: job.StateCompletedTorbox}
	if err := st.Insert(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE jobs SET state='COMPLETED_TORBOX', torbox_active_id=700 WHERE nzo_id=?`, "arrarr_byid"); err != nil {
		t.Fatal(err)
	}
	dl, err := downloader.New(downloader.Options{BaseDir: t.TempDir(), Concurrency: 1, Logger: noopLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	p := NewPuller(PullerOptions{Store: st, Torbox: tb, Downloader: dl, BaseDir: t.TempDir(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	p.Tick(ctx, 1)
	p.Wait()

	if got, _ := st.Get(ctx, "arrarr_byid"); got.State != job.StateReady {
		t.Fatalf("state = %s, want READY", got.State)
	}
	if tb.byIDCalls.Load() == 0 || tb.fullLists.Load() != 0 {
		t.Fatalf("by-id calls=%d full lists=%d — want the single-entry lookup, no full list",
			tb.byIDCalls.Load(), tb.fullLists.Load())
	}
}
