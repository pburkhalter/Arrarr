package job

import (
	"database/sql"
	"strings"
	"time"
)

// nzoIDPrefix is the prefix the SAB layer applies to nzo_ids (see internal/sab/addfile.go).
// Stripping it gives a clean hex UUID suitable as a tag identifier on TorBox.
const nzoIDPrefix = "arrarr_"

// TagID returns the UUID portion of a job's nzo_id, suitable for use as a
// stable identifier in TorBox tags (e.g. "job:<TagID>") and as a lookup key
// in webhook payloads. Returns the input unchanged if it lacks the prefix
// (forward-compatibility for hand-inserted rows or future ID schemes).
func TagID(nzoID string) string {
	return strings.TrimPrefix(nzoID, nzoIDPrefix)
}

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

	// Source distinguishes which TorBox API the submitter calls
	// (CreateUsenetDownload vs CreateTorrentFromFile/Magnet) and which mylist
	// endpoint the poller queries. "" is treated as "usenet" for backward compat.
	Source string

	// Magnet holds the original magnet URI for torrent jobs submitted that way.
	// Used by the submitter to re-call CreateTorrentFromMagnet on retry without
	// keeping a .torrent blob around. Empty for usenet jobs and for torrent
	// jobs that came in as a .torrent file (those use NzbBlob).
	Magnet sql.NullString

	// LocalPath is the absolute directory under DOWNLOAD_DIR where the puller
	// wrote the files for this job. Reported to Sonarr/Radarr as the import
	// path via the qbit/sab shim.
	LocalPath sql.NullString

	// BytesDownloaded/BytesTotal are puller progress, surfaced via the qbit
	// shim's torrents/info so Sonarr/Radarr's UI progress reflects the local
	// pull rather than just TorBox's cloud download.
	BytesDownloaded int64
	BytesTotal      int64
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
