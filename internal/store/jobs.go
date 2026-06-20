package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
)

var ErrNotFound = errors.New("job not found")
var ErrInvalidTransition = errors.New("invalid state transition")

func (s *Store) Insert(ctx context.Context, j *job.Job) error {
	source := j.Source
	if source == "" {
		source = "usenet"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(
		nzo_id, category, filename, nzb_sha256, nzb_blob, size_bytes, priority,
		state, attempts, source, magnet, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		j.NzoID, j.Category, j.Filename, j.NzbSHA256, j.NzbBlob, j.SizeBytes, j.Priority,
		string(j.State), source, j.Magnet,
	)
	return err
}

// SetLocalProgress updates per-file pull progress without changing state. Cheap
// enough to call from the downloader's OnProgress callback every few seconds.
func (s *Store) SetLocalProgress(ctx context.Context, nzoID string, downloaded, total int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		bytes_downloaded = ?, bytes_total = ?, updated_at = CURRENT_TIMESTAMP
		WHERE nzo_id = ?`, downloaded, total, nzoID)
	return err
}

// MarkLocalReady records the puller's successful local copy and advances the
// job to READY.
func (s *Store) MarkLocalReady(ctx context.Context, nzoID, localPath string, bytesTotal int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET
		local_path = ?, bytes_downloaded = ?, bytes_total = ?,
		updated_at = CURRENT_TIMESTAMP WHERE nzo_id = ?`,
		localPath, bytesTotal, bytesTotal, nzoID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET
		state = 'READY', completed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP,
		next_attempt_at = NULL, last_error = NULL, nzb_blob = NULL, claimed_at = NULL
		WHERE nzo_id = ? AND state = 'COMPLETED_TORBOX'`, nzoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvalidTransition
	}
	return tx.Commit()
}

func (s *Store) Get(ctx context.Context, nzoID string) (*job.Job, error) {
	row := s.db.QueryRowContext(ctx, jobSelectCols+` WHERE nzo_id = ?`, nzoID)
	return scanJob(row)
}

func (s *Store) FindActiveBySHA(ctx context.Context, sha string) (*job.Job, error) {
	row := s.db.QueryRowContext(ctx, jobSelectCols+
		` WHERE nzb_sha256 = ? AND state NOT IN ('READY','FAILED','CANCELED')
		   ORDER BY created_at DESC LIMIT 1`, sha)
	return scanJob(row)
}

func (s *Store) ListByStates(ctx context.Context, states []job.State, limit int) ([]*job.Job, error) {
	if len(states) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	q := jobSelectCols + ` WHERE state IN (` + placeholders(len(states)) + `) ORDER BY created_at ASC LIMIT ?`
	args := make([]any, 0, len(states)+1)
	for _, st := range states {
		args = append(args, string(st))
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *Store) ListReady(ctx context.Context, limit int) ([]*job.Job, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, jobSelectCols+
		` WHERE state IN ('READY','FAILED','CANCELED') ORDER BY completed_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// Claim atomically locks up to limit jobs whose next_attempt_at has elapsed.
// Requires SQLite >= 3.35 for UPDATE ... RETURNING.
func (s *Store) Claim(ctx context.Context, states []job.State, limit int) ([]*job.Job, error) {
	if len(states) == 0 || limit < 1 {
		return nil, nil
	}
	q := `UPDATE jobs SET claimed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
	      WHERE nzo_id IN (
	          SELECT nzo_id FROM jobs
	          WHERE state IN (` + placeholders(len(states)) + `)
	            AND (next_attempt_at IS NULL OR next_attempt_at <= CURRENT_TIMESTAMP)
	            AND (claimed_at IS NULL OR claimed_at <= datetime('now', '-5 minutes'))
	          ORDER BY created_at ASC LIMIT ?
	      )
	      RETURNING ` + jobColsList
	args := make([]any, 0, len(states)+1)
	for _, st := range states {
		args = append(args, string(st))
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *Store) Release(ctx context.Context, nzoID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET claimed_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE nzo_id = ?`, nzoID)
	return err
}

type Transition struct {
	From, To         job.State
	TorboxQueueID    *int64
	TorboxActiveID   *int64
	TorboxFolderName *string
	LastError        *string
	IncAttempts      bool
	NextAttemptAt    *time.Time
	CompletedAt      *time.Time
	ClearBlob        bool
	ClearClaimed     bool
}

func (s *Store) Transition(ctx context.Context, nzoID string, t Transition) error {
	if !job.CanTransition(t.From, t.To) {
		return ErrInvalidTransition
	}
	sets := []string{"state = ?", "updated_at = CURRENT_TIMESTAMP"}
	args := []any{string(t.To)}

	if t.TorboxQueueID != nil {
		sets = append(sets, "torbox_queue_id = ?")
		args = append(args, *t.TorboxQueueID)
	}
	if t.TorboxActiveID != nil {
		sets = append(sets, "torbox_active_id = ?")
		args = append(args, *t.TorboxActiveID)
	}
	if t.TorboxFolderName != nil {
		sets = append(sets, "torbox_folder_name = ?")
		args = append(args, *t.TorboxFolderName)
	}
	if t.LastError != nil {
		sets = append(sets, "last_error = ?")
		args = append(args, *t.LastError)
	} else {
		sets = append(sets, "last_error = NULL")
	}
	if t.IncAttempts {
		sets = append(sets, "attempts = attempts + 1")
	}
	if t.NextAttemptAt != nil {
		sets = append(sets, "next_attempt_at = ?")
		args = append(args, *t.NextAttemptAt)
	} else {
		sets = append(sets, "next_attempt_at = NULL")
	}
	if t.CompletedAt != nil {
		sets = append(sets, "completed_at = ?")
		args = append(args, *t.CompletedAt)
	}
	if t.ClearBlob {
		sets = append(sets, "nzb_blob = NULL")
	}
	if t.ClearClaimed {
		sets = append(sets, "claimed_at = NULL")
	}

	args = append(args, nzoID, string(t.From))
	q := `UPDATE jobs SET ` + strings.Join(sets, ", ") + ` WHERE nzo_id = ? AND state = ?`
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrInvalidTransition
	}
	return nil
}

func (s *Store) AttemptFailure(ctx context.Context, nzoID, errMsg string, nextAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		attempts = attempts + 1,
		last_error = ?,
		next_attempt_at = ?,
		claimed_at = NULL,
		updated_at = CURRENT_TIMESTAMP
		WHERE nzo_id = ?`, errMsg, nextAt, nzoID)
	return err
}

// Reschedule updates next_attempt_at + last_error without bumping the attempt
// counter. Use for transient backpressure (e.g. TorBox 429) that shouldn't
// count against MaxSubmitAttempts.
func (s *Store) Reschedule(ctx context.Context, nzoID, errMsg string, nextAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		last_error = ?,
		next_attempt_at = ?,
		claimed_at = NULL,
		updated_at = CURRENT_TIMESTAMP
		WHERE nzo_id = ?`, errMsg, nextAt, nzoID)
	return err
}

func (s *Store) SetTorboxIDs(ctx context.Context, nzoID string, activeID *int64, folder *string) error {
	sets := []string{"updated_at = CURRENT_TIMESTAMP"}
	args := []any{}
	if activeID != nil {
		sets = append(sets, "torbox_active_id = ?")
		args = append(args, *activeID)
	}
	if folder != nil {
		sets = append(sets, "torbox_folder_name = ?")
		args = append(args, *folder)
	}
	if len(args) == 0 {
		return nil
	}
	args = append(args, nzoID)
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET `+strings.Join(sets, ", ")+` WHERE nzo_id = ?`, args...)
	return err
}

// Stats is the aggregate snapshot the status dashboard renders.
type Stats struct {
	StateCounts map[string]int
	Recent      []*RecentJob
	GeneratedAt time.Time
}

// RecentJob is the trimmed projection used in the dashboard's recent-activity
// table. Avoids leaking nzb_blob and other heavy columns.
type RecentJob struct {
	NzoID      string
	State      string
	Category   string
	FolderName string
	LocalPath  string
	LastError  string
	UpdatedAt  time.Time
}

// FetchStats returns a single snapshot for the dashboard.
func (s *Store) FetchStats(ctx context.Context, recentLimit int) (*Stats, error) {
	if recentLimit <= 0 {
		recentLimit = 20
	}
	out := &Stats{
		StateCounts: map[string]int{},
		GeneratedAt: time.Now().UTC(),
	}

	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var v int
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return nil, err
		}
		out.StateCounts[k] = v
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `
		SELECT nzo_id, state, category,
		       COALESCE(torbox_folder_name, ''),
		       COALESCE(local_path, ''),
		       COALESCE(last_error, ''),
		       updated_at
		FROM jobs
		ORDER BY updated_at DESC
		LIMIT ?`, recentLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		j := &RecentJob{}
		if err := rows.Scan(&j.NzoID, &j.State, &j.Category,
			&j.FolderName, &j.LocalPath, &j.LastError, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out.Recent = append(out.Recent, j)
	}
	return out, rows.Err()
}

// FindByTorboxID looks up a job by its TorBox queue id or active id. Used by
// the webhook handler to map an incoming TorBox event to one of our jobs.
// Returns ErrNotFound if neither column matches.
func (s *Store) FindByTorboxID(ctx context.Context, id int64) (*job.Job, error) {
	if id == 0 {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx,
		jobSelectCols+` WHERE torbox_active_id = ? OR torbox_queue_id = ? LIMIT 1`,
		id, id)
	return scanJob(row)
}

// FindByFolderName looks up a job by its TorBox folder name. Used as a webhook
// fallback when the payload only includes the human-readable name. Match is
// case-insensitive to accommodate TorBox normalization differences.
func (s *Store) FindByFolderName(ctx context.Context, name string) (*job.Job, error) {
	if name == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx,
		jobSelectCols+` WHERE LOWER(torbox_folder_name) = LOWER(?) ORDER BY created_at DESC LIMIT 1`,
		name)
	return scanJob(row)
}

// MarkPushoverSent records that a Pushover notification fired for this job.
// Idempotency guard: the webhook handler refuses to send a second notification
// if pushover_sent_at is already populated.
func (s *Store) MarkPushoverSent(ctx context.Context, nzoID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET
	        pushover_sent_at = CURRENT_TIMESTAMP,
	        updated_at = CURRENT_TIMESTAMP
	    WHERE nzo_id = ? AND pushover_sent_at IS NULL`, nzoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound // either no job, or already sent (idempotent miss)
	}
	return nil
}

// PushoverAlreadySent reports whether the row's pushover_sent_at is set. Cheap
// pre-check before constructing a notification.
func (s *Store) PushoverAlreadySent(ctx context.Context, nzoID string) (bool, error) {
	var ts *time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT pushover_sent_at FROM jobs WHERE nzo_id = ?`, nzoID).Scan(&ts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	return ts != nil, nil
}

func (s *Store) Delete(ctx context.Context, nzoID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE nzo_id = ?`, nzoID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Reap(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM jobs WHERE state IN ('READY','FAILED','CANCELED') AND completed_at IS NOT NULL AND completed_at < ?`,
		before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const jobColsList = `nzo_id, category, filename, nzb_sha256, nzb_blob, size_bytes, priority,
	torbox_queue_id, torbox_active_id, torbox_folder_name, state, attempts, last_error,
	claimed_at, next_attempt_at, created_at, updated_at, completed_at,
	source, magnet, local_path, bytes_downloaded, bytes_total`

const jobSelectCols = `SELECT ` + jobColsList + ` FROM jobs`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(r rowScanner) (*job.Job, error) {
	j := &job.Job{}
	var state string
	err := r.Scan(
		&j.NzoID, &j.Category, &j.Filename, &j.NzbSHA256, &j.NzbBlob, &j.SizeBytes, &j.Priority,
		&j.TorboxQueueID, &j.TorboxActiveID, &j.TorboxFolderName, &state, &j.Attempts, &j.LastError,
		&j.ClaimedAt, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.CompletedAt,
		&j.Source, &j.Magnet, &j.LocalPath, &j.BytesDownloaded, &j.BytesTotal,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.State = job.State(state)
	return j, nil
}

func scanJobs(rows *sql.Rows) ([]*job.Job, error) {
	var out []*job.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
