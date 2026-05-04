CREATE TABLE jobs (
  nzo_id              TEXT PRIMARY KEY,
  category            TEXT NOT NULL,
  filename            TEXT NOT NULL,
  nzb_sha256          TEXT NOT NULL,
  nzb_blob            BLOB,
  size_bytes          INTEGER,
  priority            INTEGER NOT NULL DEFAULT 0,
  torbox_queue_id     INTEGER,
  torbox_active_id    INTEGER,
  torbox_folder_name  TEXT,
  state               TEXT NOT NULL,
  attempts            INTEGER NOT NULL DEFAULT 0,
  last_error          TEXT,
  claimed_at          DATETIME,
  next_attempt_at     DATETIME,
  created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  completed_at        DATETIME
);
CREATE INDEX idx_jobs_state_next ON jobs(state, next_attempt_at);
CREATE INDEX idx_jobs_sha        ON jobs(nzb_sha256);
CREATE INDEX idx_jobs_torbox_ids ON jobs(torbox_queue_id, torbox_active_id);
CREATE INDEX idx_jobs_completed  ON jobs(completed_at);
