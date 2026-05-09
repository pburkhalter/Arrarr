package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
)

var ErrNotFound = errors.New("job not found")
var ErrInvalidTransition = errors.New("invalid state transition")

func (s *Store) Insert(ctx context.Context, j *job.Job) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(
		nzo_id, category, filename, nzb_sha256, nzb_blob, size_bytes, priority,
		state, attempts, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		j.NzoID, j.Category, j.Filename, j.NzbSHA256, j.NzbBlob, j.SizeBytes, j.Priority,
		string(j.State),
	)
	return err
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

// MarkTagged records that TorBox accepted our tags+name on this job. Idempotent.
// Failure to tag is not fatal (we have nzb sha + nzo_id as backup identifiers),
// so this just sets a timestamp to deflect re-attempts by future workers.
func (s *Store) MarkTagged(ctx context.Context, nzoID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		tagged_at = CURRENT_TIMESTAMP,
		updated_at = CURRENT_TIMESTAMP
		WHERE nzo_id = ?`, nzoID)
	return err
}

// LibraryRow is the slice of jobs view the librarian worker needs. Distinct
// from job.Job to avoid widening that struct for v2-only fields, and because
// the librarian only needs the columns it can act on.
type LibraryRow struct {
	NzoID                  string
	Category               string
	TorboxFolderName       sql.NullString
	TorboxActiveID         sql.NullInt64
	TorboxQueueID          sql.NullInt64
	LibraryWriterState     string
	LibraryWriterAttempts  int
}

// ListPendingLibraryWrites returns rows ready for the librarian to process:
// state COMPLETED_TORBOX, library_writer_state in ('pending','failed'), and
// next_attempt_at clear or due. Origin filter excludes mirror rows when the
// caller passes 'self'; pass empty string to match all origins.
func (s *Store) ListPendingLibraryWrites(ctx context.Context, origin string, limit int) ([]*LibraryRow, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT nzo_id, category, torbox_folder_name, torbox_active_id, torbox_queue_id,
	             library_writer_state, library_writer_attempts
	      FROM jobs
	      WHERE state = 'COMPLETED_TORBOX'
	        AND library_writer_state IN ('pending','failed')
	        AND torbox_folder_name IS NOT NULL
	        AND (next_attempt_at IS NULL OR next_attempt_at <= CURRENT_TIMESTAMP)`
	args := []any{}
	if origin != "" {
		q += ` AND origin = ?`
		args = append(args, origin)
	}
	q += ` ORDER BY updated_at ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LibraryRow
	for rows.Next() {
		r := &LibraryRow{}
		if err := rows.Scan(&r.NzoID, &r.Category, &r.TorboxFolderName,
			&r.TorboxActiveID, &r.TorboxQueueID,
			&r.LibraryWriterState, &r.LibraryWriterAttempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkLibraryWritten records a successful library write and advances the job
// to READY. streamingURL and expiresAt are optional (only set in STRM modes).
func (s *Store) MarkLibraryWritten(
	ctx context.Context,
	nzoID, libraryPath string,
	streamingURL *string,
	expiresAt *time.Time,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	args := []any{libraryPath}
	q := `UPDATE jobs SET
	        library_writer_state = 'written',
	        library_writer_error = NULL,
	        library_path = ?,
	        updated_at = CURRENT_TIMESTAMP`
	if streamingURL != nil {
		q += `, streaming_url = ?`
		args = append(args, *streamingURL)
	}
	if expiresAt != nil {
		q += `, streaming_url_expires_at = ?`
		args = append(args, *expiresAt)
	}
	q += ` WHERE nzo_id = ?`
	args = append(args, nzoID)
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	// Advance state machine: COMPLETED_TORBOX → READY.
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET
	        state = 'READY',
	        completed_at = CURRENT_TIMESTAMP,
	        updated_at = CURRENT_TIMESTAMP,
	        next_attempt_at = NULL,
	        last_error = NULL,
	        nzb_blob = NULL,
	        claimed_at = NULL
	    WHERE nzo_id = ? AND state = 'COMPLETED_TORBOX'`, nzoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvalidTransition
	}
	return tx.Commit()
}

// MarkLibraryFailed records a library-write failure. If terminal=true, also
// transitions the job to FAILED. Otherwise only the librarian fields move.
func (s *Store) MarkLibraryFailed(
	ctx context.Context,
	nzoID, errMsg string,
	nextAttempt *time.Time,
	terminal bool,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	args := []any{errMsg}
	q := `UPDATE jobs SET
	        library_writer_state    = 'failed',
	        library_writer_error    = ?,
	        library_writer_attempts = library_writer_attempts + 1,
	        updated_at = CURRENT_TIMESTAMP`
	if nextAttempt != nil {
		q += `, next_attempt_at = ?`
		args = append(args, *nextAttempt)
	}
	q += ` WHERE nzo_id = ?`
	args = append(args, nzoID)
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	if terminal {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET
		        state = 'FAILED',
		        completed_at = CURRENT_TIMESTAMP,
		        last_error = ?,
		        next_attempt_at = NULL,
		        claimed_at = NULL,
		        updated_at = CURRENT_TIMESTAMP
		    WHERE nzo_id = ? AND state = 'COMPLETED_TORBOX'`, errMsg, nzoID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkLibraryPending resets a job's library state to 'pending' (used when a
// transient retry succeeds and we want subsequent ticks to try again cleanly).
// Mostly useful for the URL refresh worker in Phase B-3.
func (s *Store) MarkLibraryPending(ctx context.Context, nzoID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET
	        library_writer_state = 'pending',
	        library_writer_error = NULL,
	        next_attempt_at = NULL,
	        updated_at = CURRENT_TIMESTAMP
	    WHERE nzo_id = ?`, nzoID)
	return err
}

// ListUpcomingURLRefreshes returns READY jobs whose streaming URL is due for
// refresh: streaming_url_expires_at < before. The caller passes
// "now + buffer" as before, so STRMs get rewritten while the existing URL is
// still valid (avoiding playback breakage during the swap).
func (s *Store) ListUpcomingURLRefreshes(ctx context.Context, before time.Time, limit int) ([]*LibraryRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT nzo_id, category, torbox_folder_name, torbox_active_id, torbox_queue_id,
		       library_writer_state, library_writer_attempts
		FROM jobs
		WHERE state = 'READY'
		  AND library_writer_state = 'written'
		  AND streaming_url IS NOT NULL
		  AND streaming_url_expires_at IS NOT NULL
		  AND streaming_url_expires_at < ?
		  AND origin = 'self'
		ORDER BY streaming_url_expires_at ASC
		LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LibraryRow
	for rows.Next() {
		r := &LibraryRow{}
		if err := rows.Scan(&r.NzoID, &r.Category, &r.TorboxFolderName,
			&r.TorboxActiveID, &r.TorboxQueueID,
			&r.LibraryWriterState, &r.LibraryWriterAttempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateStreamingURL rewrites the cached streaming URL + expiry on a READY
// row without changing state. Used by the URL refresh worker after it has
// re-fetched fresh CDN URLs and rewritten the STRM file(s).
func (s *Store) UpdateStreamingURL(ctx context.Context, nzoID, libraryPath, streamingURL string, expiresAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET
	        library_path = ?,
	        streaming_url = ?,
	        streaming_url_expires_at = ?,
	        updated_at = CURRENT_TIMESTAMP
	    WHERE nzo_id = ? AND state = 'READY'`, libraryPath, streamingURL, expiresAt, nzoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Stats is the aggregate snapshot the status dashboard renders.
type Stats struct {
	StateCounts        map[string]int
	LibraryStateCounts map[string]int
	OriginCounts       map[string]int
	Recent             []*RecentJob
	// HiddenFailedMirror is the count of FAILED+origin=mirror jobs excluded
	// from Recent. They're nearly always TorBox-side errors on stale items in
	// the shared account (DATABASE_ERROR / 500 on requestdl) — actionable by
	// nobody, so we suppress them from the table and surface only the count.
	HiddenFailedMirror int
	UntaggedCount      int
	UpcomingRefreshes  int
	GeneratedAt        time.Time
}

// RecentJob is the trimmed projection used in the dashboard's recent-activity
// table. Avoids leaking nzb_blob and other heavy columns.
type RecentJob struct {
	NzoID              string
	State              string
	Origin             string
	Category           string
	FolderName         string
	LibraryPath        string
	LibraryWriterState string
	LibraryAttempts    int
	LastError          string
	UpdatedAt          time.Time
	StreamingExpires   *time.Time
}

// FetchStats returns a single snapshot for the dashboard. Cheaper than five
// round trips because each query is small and SQLite is in-process anyway.
func (s *Store) FetchStats(ctx context.Context, recentLimit int) (*Stats, error) {
	if recentLimit <= 0 {
		recentLimit = 20
	}
	out := &Stats{
		StateCounts:        map[string]int{},
		LibraryStateCounts: map[string]int{},
		OriginCounts:       map[string]int{},
		GeneratedAt:        time.Now().UTC(),
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

	rows, err = s.db.QueryContext(ctx, `SELECT library_writer_state, COUNT(*) FROM jobs GROUP BY library_writer_state`)
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
		out.LibraryStateCounts[k] = v
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT origin, COUNT(*) FROM jobs GROUP BY origin`)
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
		out.OriginCounts[k] = v
	}
	rows.Close()

	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE tagged_at IS NULL
		  AND state IN ('SUBMITTED','DOWNLOADING','COMPLETED_TORBOX','READY')
		  AND origin = 'self'
		  AND (torbox_active_id IS NOT NULL OR torbox_queue_id IS NOT NULL)`).
		Scan(&out.UntaggedCount); err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(URLRefreshHorizon)
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE state = 'READY'
		  AND library_writer_state = 'written'
		  AND streaming_url_expires_at IS NOT NULL
		  AND streaming_url_expires_at < ?`, cutoff).
		Scan(&out.UpcomingRefreshes); err != nil {
		return nil, err
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state='FAILED' AND origin='mirror'`).
		Scan(&out.HiddenFailedMirror); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `
		SELECT nzo_id, state, origin, category,
		       COALESCE(torbox_folder_name, ''),
		       COALESCE(library_path, ''),
		       library_writer_state, library_writer_attempts,
		       COALESCE(last_error, ''),
		       updated_at, streaming_url_expires_at
		FROM jobs
		WHERE NOT (state='FAILED' AND origin='mirror')
		ORDER BY updated_at DESC
		LIMIT ?`, recentLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		j := &RecentJob{}
		if err := rows.Scan(&j.NzoID, &j.State, &j.Origin, &j.Category,
			&j.FolderName, &j.LibraryPath, &j.LibraryWriterState, &j.LibraryAttempts,
			&j.LastError, &j.UpdatedAt, &j.StreamingExpires); err != nil {
			return nil, err
		}
		out.Recent = append(out.Recent, j)
	}
	return out, rows.Err()
}

// URLRefreshHorizon is the look-ahead used by the dashboard's "upcoming URL
// refreshes" counter. Mirrors the worker's URLRefreshBuffer.
const URLRefreshHorizon = time.Hour

// MirrorJobSpec describes a TorBox download we want to materialize as a mirror
// row. Used by the mirror worker when ARRARR_MIRROR_MODE=on so Jellyfin sees
// the entire household's downloads, not just Arrarr-initiated ones.
type MirrorJobSpec struct {
	TorboxID    int64  // active id from /usenet/mylist (or queue id if no active yet)
	QueueID     int64  // optional, when distinct from TorboxID
	FolderName  string // name TorBox stores; becomes torbox_folder_name
	Category    string // "tv-sonarr", "movies", or "other" (sniffed by caller)
	SizeBytes   int64
}

// UpsertMirrorJob inserts a mirror row if it doesn't already exist for this
// TorBox id, and is a no-op if it does. Idempotent under repeated mirror
// sweeps. Returns the synthetic nzo_id and whether it was a new insert.
func (s *Store) UpsertMirrorJob(ctx context.Context, spec MirrorJobSpec) (string, bool, error) {
	if spec.TorboxID == 0 {
		return "", false, errors.New("UpsertMirrorJob: torbox id required")
	}
	// Deterministic nzo_id so multiple sweeps converge on the same row even
	// without an explicit existence check. Prefix is distinct from arrarr_*
	// (the SAB layer's UUID-style ids) so the two namespaces never collide.
	nzoID := fmt.Sprintf("mirror_%d", spec.TorboxID)
	sha := fmt.Sprintf("mirror:%d", spec.TorboxID)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs(
			nzo_id, category, filename, nzb_sha256, nzb_blob, size_bytes, priority,
			state, attempts, origin,
			torbox_active_id, torbox_queue_id, torbox_folder_name,
			library_writer_state, created_at, updated_at
		) VALUES (?, ?, ?, ?, NULL, ?, 0,
		          'COMPLETED_TORBOX', 0, 'mirror',
		          ?, ?, ?,
		          'pending', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT (nzo_id) DO NOTHING`,
		nzoID, spec.Category, spec.FolderName, sha, spec.SizeBytes,
		spec.TorboxID, spec.QueueID, spec.FolderName)
	if err != nil {
		return "", false, err
	}
	n, _ := res.RowsAffected()
	return nzoID, n > 0, nil
}

// ListMirrorOrphans returns mirror rows whose torbox_active_id is NOT in the
// supplied liveIDs set. Caller passes the set of currently-on-TorBox ids from
// /usenet/mylist; orphans are mirror rows the user (or another household
// member) has since deleted from TorBox, so we should reciprocally clean them
// up + remove their library files.
//
// Empty liveIDs = treat ALL mirror rows as orphans (caller usually doesn't
// want this — guard against an empty/transient mylist response).
func (s *Store) ListMirrorOrphans(ctx context.Context, liveIDs []int64) ([]*MirrorOrphan, error) {
	q := `SELECT nzo_id, library_path, torbox_active_id FROM jobs
	      WHERE origin = 'mirror'`
	var args []any
	if len(liveIDs) > 0 {
		q += ` AND torbox_active_id NOT IN (` + placeholders(len(liveIDs)) + `)`
		for _, id := range liveIDs {
			args = append(args, id)
		}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MirrorOrphan
	for rows.Next() {
		o := &MirrorOrphan{}
		if err := rows.Scan(&o.NzoID, &o.LibraryPath, &o.TorboxID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MirrorOrphan is a stale mirror row + its library file path (if any) for
// cleanup.
type MirrorOrphan struct {
	NzoID       string
	LibraryPath sql.NullString
	TorboxID    sql.NullInt64
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

// JobOrigin returns 'self' or 'mirror' for a job. Used by the webhook handler
// to gate Pushover notifications on origin (only fire for our own jobs).
func (s *Store) JobOrigin(ctx context.Context, nzoID string) (string, error) {
	var o string
	err := s.db.QueryRowContext(ctx,
		`SELECT origin FROM jobs WHERE nzo_id = ?`, nzoID).Scan(&o)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return o, nil
}

// ListPendingTagWrites returns jobs that haven't been successfully tagged yet
// but DO have a TorBox id (so EditUsenet has something to act on). Used by the
// tagger retry loop — TorBox rejects edit calls until the item is "cached"
// (past initial processing), so the submitter's eager attempt often fails and
// this loop catches up.
func (s *Store) ListPendingTagWrites(ctx context.Context, limit int) ([]*LibraryRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT nzo_id, category, torbox_folder_name, torbox_active_id, torbox_queue_id,
		       library_writer_state, library_writer_attempts
		FROM jobs
		WHERE tagged_at IS NULL
		  AND state IN ('SUBMITTED','DOWNLOADING','COMPLETED_TORBOX','READY')
		  AND (torbox_active_id IS NOT NULL OR torbox_queue_id IS NOT NULL)
		  AND origin = 'self'
		ORDER BY updated_at ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LibraryRow
	for rows.Next() {
		r := &LibraryRow{}
		if err := rows.Scan(&r.NzoID, &r.Category, &r.TorboxFolderName,
			&r.TorboxActiveID, &r.TorboxQueueID,
			&r.LibraryWriterState, &r.LibraryWriterAttempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
	claimed_at, next_attempt_at, created_at, updated_at, completed_at, library_path`

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
		&j.ClaimedAt, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &j.CompletedAt, &j.LibraryPath,
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
