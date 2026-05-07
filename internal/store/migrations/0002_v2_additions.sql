-- v2 additions: tagging on TorBox (no new column — we tag using nzo_id directly,
-- stripping the "arrarr_" prefix), library writer state, streaming URL refresh,
-- pushover idempotency, mirror mode origin, canonical naming cache.
-- See PLAN.md for column semantics.

ALTER TABLE jobs ADD COLUMN origin                   TEXT NOT NULL DEFAULT 'self';
ALTER TABLE jobs ADD COLUMN library_path             TEXT;
ALTER TABLE jobs ADD COLUMN library_writer_state     TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE jobs ADD COLUMN library_writer_attempts  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN library_writer_error     TEXT;
ALTER TABLE jobs ADD COLUMN streaming_url            TEXT;
ALTER TABLE jobs ADD COLUMN streaming_url_expires_at DATETIME;
ALTER TABLE jobs ADD COLUMN pushover_sent_at         DATETIME;
ALTER TABLE jobs ADD COLUMN tagged_at                DATETIME;
ALTER TABLE jobs ADD COLUMN canonical_title          TEXT;
ALTER TABLE jobs ADD COLUMN canonical_season         INTEGER;
ALTER TABLE jobs ADD COLUMN canonical_episode        INTEGER;
ALTER TABLE jobs ADD COLUMN canonical_year           INTEGER;
ALTER TABLE jobs ADD COLUMN media_type               TEXT;

CREATE INDEX idx_jobs_origin            ON jobs(origin);
CREATE INDEX idx_jobs_library_state     ON jobs(library_writer_state);
CREATE INDEX idx_jobs_streaming_expires ON jobs(streaming_url_expires_at) WHERE streaming_url_expires_at IS NOT NULL;
CREATE INDEX idx_jobs_tagged            ON jobs(tagged_at)                WHERE tagged_at IS NULL;
