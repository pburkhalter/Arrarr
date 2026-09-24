-- The poller's 24h ceiling and stall window counted from created_at, which
-- includes the time a job queues in NEW behind TorBox's active limit. Stamp
-- the SUBMITTED transition so both measure from when TorBox got the job.
ALTER TABLE jobs ADD COLUMN submitted_at DATETIME;
