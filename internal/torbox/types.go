package torbox

import (
	"encoding/json"
	"strings"
)

type envelope struct {
	Success bool            `json:"success"`
	Detail  string          `json:"detail"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type CreateResp struct {
	QueueID   int64 `json:"queue_id"`
	ID        int64 `json:"id"`
	TorrentID int64 `json:"torrent_id"` // torrent endpoint returns this instead of id/queue_id
	UsenetID  int64 `json:"usenet_id"`  // some legacy paths return this
	// hash + auth_id are deliberately not parsed: TorBox sometimes returns
	// them as strings, which broke createusenetdownload. We don't use them.
}

func (c *CreateResp) EffectiveID() int64 {
	if c.ID != 0 {
		return c.ID
	}
	if c.TorrentID != 0 {
		return c.TorrentID
	}
	if c.UsenetID != 0 {
		return c.UsenetID
	}
	return c.QueueID
}

// MyListItem rows may have queue_id only (queued), then later gain id (active);
// queue_id may stay or disappear after activation. Match by either.
type MyListItem struct {
	ID            int64    `json:"id"`
	QueueID       int64    `json:"queue_id"`
	Hash          string   `json:"hash,omitempty"`
	Name          string   `json:"name,omitempty"`
	DownloadState string   `json:"download_state,omitempty"`
	DownloadFinished bool  `json:"download_finished,omitempty"`
	DownloadPresent  bool  `json:"download_present,omitempty"`
	Active        bool     `json:"active,omitempty"`
	Progress      float64  `json:"progress,omitempty"`
	Size          int64    `json:"size,omitempty"`
	ETA           int64    `json:"eta,omitempty"`
	DownloadSpeed int64    `json:"download_speed,omitempty"`
	Files         []MyListFile `json:"files,omitempty"`
}

type MyListFile struct {
	ID        int64  `json:"id"`
	ShortName string `json:"short_name"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MimeType  string `json:"mime_type"`
}

func (i *MyListItem) FolderName() string {
	return i.Name
}

func (i *MyListItem) IsTerminal() bool {
	if i.DownloadFinished {
		return true
	}
	return hasAnyPrefix(i.DownloadState, "completed", "cached", "uploading")
}

// TorBox decorates terminal states with parenthesised detail
// (e.g. "failed (Aborted, cannot be completed - https://sabnzbd.org/not-complete)"),
// so match by prefix, not equality — exact match misses every annotated failure
// and leaves the job stuck in DOWNLOADING forever.
func (i *MyListItem) IsFailure() bool {
	return hasAnyPrefix(i.DownloadState, "failed", "error", "missing_files")
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	s = strings.ToLower(s)
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
