package qbit

// Version constants. We pretend to be a recent-ish qBittorrent so Sonarr's
// feature gates flip the right way (it sniffs version to decide whether to
// use newer endpoints).
const (
	AppVersion    = "v4.3.2"
	WebAPIVersion = "2.8.3"
)

// TorrentInfo is the per-torrent record /api/v2/torrents/info returns.
// Only the fields Sonarr/Radarr actually read are populated by the shim —
// the rest are emitted as zero values to keep the JSON shape qBit-compatible.
type TorrentInfo struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	Category     string  `json:"category"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	Downloaded   int64   `json:"downloaded"`
	TotalSize    int64   `json:"total_size"`
	Size         int64   `json:"size"`
	DlSpeed      int64   `json:"dlspeed"`
	UpSpeed      int64   `json:"upspeed"`
	ETA          int64   `json:"eta"`
	AddedOn      int64   `json:"added_on"`
	CompletionOn int64   `json:"completion_on"`
	Ratio        float64 `json:"ratio"`
	Tags         string  `json:"tags"`
	Tracker      string  `json:"tracker"`
	MagnetURI    string  `json:"magnet_uri"`

	// Sonarr probes a couple of extra fields; emit them so they're present
	// instead of `null` (qBit always includes them).
	NumSeeds     int    `json:"num_seeds"`
	NumLeechs    int    `json:"num_leechs"`
	Availability float64 `json:"availability"`
	Priority     int    `json:"priority"`
	SeqDL        bool   `json:"seq_dl"`
	FirstLastPiecePrio bool `json:"f_l_piece_prio"`
	AutoTMM      bool   `json:"auto_tmm"`
	ForceStart   bool   `json:"force_start"`
	SuperSeeding bool   `json:"super_seeding"`
}

// Category is the value side of /api/v2/torrents/categories JSON object.
type Category struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

// TorrentFile is one entry in /api/v2/torrents/files. Sonarr really only
// cares about Name; the rest is stub-but-present.
type TorrentFile struct {
	Index        int     `json:"index"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	Priority     int     `json:"priority"`
	IsSeed       bool    `json:"is_seed"`
	PieceRange   []int   `json:"piece_range"`
	Availability float64 `json:"availability"`
}

// TorrentProperties is /api/v2/torrents/properties response. Mostly stub.
type TorrentProperties struct {
	SavePath               string  `json:"save_path"`
	CreationDate           int64   `json:"creation_date"`
	PieceSize              int64   `json:"piece_size"`
	Comment                string  `json:"comment"`
	TotalWasted            int64   `json:"total_wasted"`
	TotalUploaded          int64   `json:"total_uploaded"`
	TotalUploadedSession   int64   `json:"total_uploaded_session"`
	TotalDownloaded        int64   `json:"total_downloaded"`
	TotalDownloadedSession int64   `json:"total_downloaded_session"`
	UpLimit                int64   `json:"up_limit"`
	DlLimit                int64   `json:"dl_limit"`
	TimeElapsed            int64   `json:"time_elapsed"`
	SeedingTime            int64   `json:"seeding_time"`
	NbConnections          int     `json:"nb_connections"`
	NbConnectionsLimit     int     `json:"nb_connections_limit"`
	ShareRatio             float64 `json:"share_ratio"`
	AdditionDate           int64   `json:"addition_date"`
	CompletionDate         int64   `json:"completion_date"`
	CreatedBy              string  `json:"created_by"`
	DlSpeedAvg             int64   `json:"dl_speed_avg"`
	DlSpeed                int64   `json:"dl_speed"`
	Eta                    int64   `json:"eta"`
	LastSeen               int64   `json:"last_seen"`
	Peers                  int     `json:"peers"`
	PeersTotal             int     `json:"peers_total"`
	PiecesHave             int     `json:"pieces_have"`
	PiecesNum              int     `json:"pieces_num"`
	Reannounce             int64   `json:"reannounce"`
	Seeds                  int     `json:"seeds"`
	SeedsTotal             int     `json:"seeds_total"`
	TotalSize              int64   `json:"total_size"`
	UpSpeedAvg             int64   `json:"up_speed_avg"`
	UpSpeed                int64   `json:"up_speed"`
}

// Preferences is /api/v2/app/preferences. qBit's full response is ~120 keys;
// Sonarr/Radarr only inspect a handful (notably save_path + a few capability
// toggles). Anything else is included as defaults to keep the JSON shape
// recognizable.
type Preferences struct {
	SavePath                string `json:"save_path"`
	TempPath                string `json:"temp_path"`
	TempPathEnabled         bool   `json:"temp_path_enabled"`
	DHT                     bool   `json:"dht"`
	PeX                     bool   `json:"pex"`
	LSD                     bool   `json:"lsd"`
	Encryption              int    `json:"encryption"`
	AnonymousMode           bool   `json:"anonymous_mode"`
	QueueingEnabled         bool   `json:"queueing_enabled"`
	MaxActiveDownloads      int    `json:"max_active_downloads"`
	MaxActiveTorrents       int    `json:"max_active_torrents"`
	MaxActiveUploads        int    `json:"max_active_uploads"`
	DontCountSlowTorrents   bool   `json:"dont_count_slow_torrents"`
	IncompleteFilesExt      bool   `json:"incomplete_files_ext"`
	AutoDeleteMode          int    `json:"auto_delete_mode"`
	PreallocateAll          bool   `json:"preallocate_all"`
	ListenPort              int    `json:"listen_port"`
	UPnP                    bool   `json:"upnp"`
	UseHTTPS                bool   `json:"use_https"`
	WebUIUsername           string `json:"web_ui_username"`
	WebUIPort               int    `json:"web_ui_port"`
	CreateSubfolderEnabled  bool   `json:"create_subfolder_enabled"`
	StartPausedEnabled      bool   `json:"start_paused_enabled"`
	AutoTMMEnabled          bool   `json:"auto_tmm_enabled"`
	TorrentChangedTMMEnabled bool  `json:"torrent_changed_tmm_enabled"`
	SavePathChangedTMMEnabled bool `json:"save_path_changed_tmm_enabled"`
	CategoryChangedTMMEnabled bool `json:"category_changed_tmm_enabled"`
}
