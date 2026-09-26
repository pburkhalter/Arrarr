package sab

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

// Sonarr/Radarr send del_files=1 when a download is removed with its data.
// It used to be ignored: the job was canceled, but its files and its running
// TorBox download stayed.
func TestDeleteWithDelFilesDiscardsTheJob(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	_ = st.Migrate(ctx)
	insert := func(nzo string, state job.State) {
		t.Helper()
		if err := st.Insert(ctx, &job.Job{NzoID: nzo, Category: "sonarr", Filename: nzo + ".nzb",
			NzbSHA256: nzo, NzbBlob: []byte("nzb"), State: job.StateNew}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE jobs SET state=? WHERE nzo_id=?`, state, nzo); err != nil {
			t.Fatal(err)
		}
	}
	insert("q1", job.StateDownloading)
	insert("q2", job.StateDownloading)
	insert("h1", job.StateReady)

	var got []string
	var states []job.State
	h := NewServer(Options{APIKey: "k", Store: Adapt(st), Logger: slog.Default(),
		Discard: func(_ context.Context, j *job.Job) {
			got = append(got, j.NzoID)
			states = append(states, j.State)
		}}).Handler()
	call := func(q string) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api?apikey=k&"+q, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
		}
	}
	call("mode=queue&name=delete&value=q1&del_files=1")
	call("mode=queue&name=delete&value=q2")
	call("mode=history&name=delete&value=h1&del_files=1")

	if len(got) != 2 || got[0] != "q1" || got[1] != "h1" {
		t.Fatalf("discarded %v, want [q1 h1] (q2 had no del_files)", got)
	}
	// Discard sees the state before the delete: that is what decides whether
	// TorBox still holds a running download.
	if states[0] != job.StateDownloading || states[1] != job.StateReady {
		t.Fatalf("states %v, want [DOWNLOADING READY]", states)
	}
	if j, _ := st.Get(ctx, "q1"); j == nil || j.State != job.StateCanceled {
		t.Fatalf("q1 not canceled: %+v", j)
	}
}
