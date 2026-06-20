-- v3 cleanup: drop columns the v2 librarian / mirror / tagger wrote that the
-- v3 puller-only path no longer reads or writes. Requires SQLite >= 3.35
-- (already required by Claim's UPDATE ... RETURNING).

DROP INDEX IF EXISTS idx_jobs_origin;
DROP INDEX IF EXISTS idx_jobs_library_state;
DROP INDEX IF EXISTS idx_jobs_streaming_expires;
DROP INDEX IF EXISTS idx_jobs_tagged;

ALTER TABLE jobs DROP COLUMN library_path;
ALTER TABLE jobs DROP COLUMN library_writer_state;
ALTER TABLE jobs DROP COLUMN library_writer_attempts;
ALTER TABLE jobs DROP COLUMN library_writer_error;
ALTER TABLE jobs DROP COLUMN streaming_url;
ALTER TABLE jobs DROP COLUMN streaming_url_expires_at;
ALTER TABLE jobs DROP COLUMN tagged_at;
ALTER TABLE jobs DROP COLUMN origin;
ALTER TABLE jobs DROP COLUMN canonical_title;
ALTER TABLE jobs DROP COLUMN canonical_season;
ALTER TABLE jobs DROP COLUMN canonical_episode;
ALTER TABLE jobs DROP COLUMN canonical_year;
ALTER TABLE jobs DROP COLUMN media_type;
