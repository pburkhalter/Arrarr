package torbox

import "encoding/json"

type envelope struct {
	Success bool            `json:"success"`
	Detail  string          `json:"detail"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type CreateResp struct {
	QueueID int64 `json:"queue_id"`
	ID      int64 `json:"id"`
	// hash + auth_id are deliberately not parsed: TorBox sometimes returns
	// them as strings, which broke createusenetdownload. We don't use them.
}

func (c *CreateResp) EffectiveID() int64 {
	if c.ID != 0 {
		return c.ID
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
	switch i.DownloadState {
	case "completed", "cached", "uploading":
		return true
	}
	return false
}

func (i *MyListItem) IsFailure() bool {
	switch i.DownloadState {
	case "failed", "error", "missing_files":
		return true
	}
	return false
}
