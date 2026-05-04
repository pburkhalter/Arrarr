package job

import (
	"database/sql"
	"time"
)

type Job struct {
	NzoID            string
	Category         string
	Filename         string
	NzbSHA256        string
	NzbBlob          []byte
	SizeBytes        sql.NullInt64
	Priority         int
	TorboxQueueID    sql.NullInt64
	TorboxActiveID   sql.NullInt64
	TorboxFolderName sql.NullString
	State            State
	Attempts         int
	LastError        sql.NullString
	ClaimedAt        sql.NullTime
	NextAttemptAt    sql.NullTime
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      sql.NullTime
}

func (j *Job) EffectiveTorboxID() int64 {
	if j.TorboxActiveID.Valid {
		return j.TorboxActiveID.Int64
	}
	if j.TorboxQueueID.Valid {
		return j.TorboxQueueID.Int64
	}
	return 0
}
