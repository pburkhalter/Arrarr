package sab

const SabnzbdVersion = "4.4.0"

type AuthResp struct {
	Auth string `json:"auth"`
}

type VersionResp struct {
	Version string `json:"version"`
}

type GetConfigResp struct {
	Config GetConfig `json:"config"`
}
type GetConfig struct {
	Misc       MiscConfig   `json:"misc"`
	Categories []CategoryCfg `json:"categories"`
	Servers    []any        `json:"servers"`
	Sorters    []any        `json:"sorters"`
}
type MiscConfig struct {
	CompleteDir string `json:"complete_dir"`
	HistoryRetention string `json:"history_retention"`
	HistoryRetentionOption string `json:"history_retention_option"`
	HistoryRetentionNumber int `json:"history_retention_number"`
	PreCheck      bool   `json:"pre_check"`
}
type CategoryCfg struct {
	Name     string `json:"name"`
	Order    int    `json:"order"`
	PP       string `json:"pp"`
	Script   string `json:"script"`
	Dir      string `json:"dir"`
	Priority int    `json:"priority"`
}

type AddResp struct {
	Status bool     `json:"status"`
	NzoIDs []string `json:"nzo_ids"`
}

type QueueResp struct {
	Queue Queue `json:"queue"`
}
type Queue struct {
	Status        string     `json:"status"`
	Speedlimit    string     `json:"speedlimit"`
	SpeedlimitAbs string     `json:"speedlimit_abs"`
	Paused        bool       `json:"paused"`
	NoofSlotsTotal int       `json:"noofslots_total"`
	NoofSlots      int       `json:"noofslots"`
	Limit          int       `json:"limit"`
	Start          int       `json:"start"`
	Speed          string    `json:"speed"`
	Kbpersec       string    `json:"kbpersec"`
	Size           string    `json:"size"`
	Sizeleft       string    `json:"sizeleft"`
	Mb             string    `json:"mb"`
	Mbleft         string    `json:"mbleft"`
	DiskSpaceTotal1 string   `json:"diskspacetotal1"`
	DiskSpace1      string   `json:"diskspace1"`
	Slots          []QueueSlot `json:"slots"`
}
type QueueSlot struct {
	Index      int    `json:"index"`
	NzoID      string `json:"nzo_id"`
	Filename   string `json:"filename"`
	Cat        string `json:"cat"`
	Status     string `json:"status"`
	Priority   string `json:"priority"`
	Percentage string `json:"percentage"`
	Size       string `json:"size"`
	Sizeleft   string `json:"sizeleft"`
	Mb         string `json:"mb"`
	Mbleft     string `json:"mbleft"`
	Timeleft   string `json:"timeleft"`
	ETA        string `json:"eta"`
	Script     string `json:"script"`
}

type HistoryResp struct {
	History History `json:"history"`
}
type History struct {
	NoofSlots int          `json:"noofslots"`
	Slots     []HistorySlot `json:"slots"`
}
type HistorySlot struct {
	NzoID         string `json:"nzo_id"`
	Name          string `json:"name"`
	Category      string `json:"category"`
	PP            string `json:"pp"`
	Script        string `json:"script"`
	Status        string `json:"status"`
	Storage       string `json:"storage"`
	Path          string `json:"path"`
	Size          string `json:"size"`
	FailMessage   string `json:"fail_message"`
	DownloadTime  int64  `json:"download_time"`
	Completed     int64  `json:"completed"`
	Bytes         int64  `json:"bytes"`
	NzbName       string `json:"nzb_name"`
}

type ErrorResp struct {
	Status bool   `json:"status"`
	Error  string `json:"error"`
}

type SimpleStatus struct {
	Status bool `json:"status"`
}
