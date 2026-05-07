package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/pathmap"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// recordingTorbox captures EditUsenet args for assertion.
type recordingTorbox struct {
	mu        sync.Mutex
	editID    int64
	editTags  []string
	editError error
}

func (r *recordingTorbox) CreateUsenetDownload(_ context.Context, _ string, _ []byte, _ string) (*torbox.CreateResp, error) {
	return &torbox.CreateResp{QueueID: 42}, nil
}
func (r *recordingTorbox) MyList(_ context.Context, _ bool) ([]torbox.MyListItem, error) {
	return nil, nil
}
func (r *recordingTorbox) ControlUsenet(_ context.Context, _ int64, _ string) error { return nil }
func (r *recordingTorbox) EditUsenet(_ context.Context, id int64, p torbox.EditUsenetParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.editID = id
	r.editTags = append([]string{}, p.Tags...)
	return r.editError
}
func (r *recordingTorbox) RequestUsenetDL(_ context.Context, _, _ int64, _ bool) (string, error) {
	return "", nil
}
func (r *recordingTorbox) snapshot() (int64, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string{}, r.editTags...)
	return r.editID, out
}

func TestSubmitterTagsAfterCreate(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	tb := &recordingTorbox{}
	mgr := New(Options{
		Store:          st,
		Torbox:         tb,
		PathMap:        pathmap.New("/mnt/torbox", "/torbox"),
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize: 1,
		InstanceName:   "patrik",
	})

	j := &job.Job{
		NzoID:     "arrarr_abc123def456",
		Category:  "sonarr",
		Filename:  "Twisted.Metal.S02E01.nzb",
		NzbSHA256: "sha",
		NzbBlob:   []byte("<nzb/>"),
		State:     job.StateNew,
	}
	if err := st.Insert(context.Background(), j); err != nil {
		t.Fatal(err)
	}

	mgr.dispatchOnce(context.Background())

	// Verify EditUsenet was called with the right id + tag shape.
	id, tags := tb.snapshot()
	if id != 42 {
		t.Errorf("EditUsenet id=%d want 42 (the queue_id from CreateResp)", id)
	}
	wantTags := []string{"arrarr", "host:patrik", "job:abc123def456", "cat:sonarr"}
	if len(tags) != len(wantTags) {
		t.Fatalf("tag count: got %d want %d (got=%v)", len(tags), len(wantTags), tags)
	}
	for i, w := range wantTags {
		if tags[i] != w {
			t.Errorf("tags[%d]=%q want %q", i, tags[i], w)
		}
	}

	// Verify tagged_at was recorded.
	row := st.DB().QueryRowContext(context.Background(),
		`SELECT tagged_at FROM jobs WHERE nzo_id = ?`, j.NzoID)
	var taggedAt *time.Time
	if err := row.Scan(&taggedAt); err != nil {
		t.Fatal(err)
	}
	if taggedAt == nil {
		t.Error("tagged_at should be non-NULL after successful EditUsenet")
	}
}

func TestSubmitterDoesNotFailJobOnTagError(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	tb := &recordingTorbox{editError: errors.New("not cached yet")}
	mgr := New(Options{
		Store:          st,
		Torbox:         tb,
		PathMap:        pathmap.New("/mnt/torbox", "/torbox"),
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		WorkerPoolSize: 1,
		InstanceName:   "patrik",
	})
	j := &job.Job{
		NzoID:     "arrarr_xyz",
		Category:  "radarr",
		Filename:  "Movie.2026.nzb",
		NzbSHA256: "sha2",
		NzbBlob:   []byte("<nzb/>"),
		State:     job.StateNew,
	}
	if err := st.Insert(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	mgr.dispatchOnce(context.Background())

	got, err := st.Get(context.Background(), j.NzoID)
	if err != nil {
		t.Fatal(err)
	}
	// Tag failure must not affect the state machine.
	if got.State != job.StateSubmitted {
		t.Errorf("state=%s want SUBMITTED (tag failure should not block submit transition)", got.State)
	}
	// And tagged_at must remain NULL (so a future tagger pass can retry).
	row := st.DB().QueryRowContext(context.Background(),
		`SELECT tagged_at FROM jobs WHERE nzo_id = ?`, j.NzoID)
	var taggedAt *time.Time
	if err := row.Scan(&taggedAt); err != nil {
		t.Fatal(err)
	}
	if taggedAt != nil {
		t.Error("tagged_at should remain NULL after EditUsenet failure")
	}
}
