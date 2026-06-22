package sab

import "net/http"

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, VersionResp{Version: SabnzbdVersion})
}

func (s *Server) handleAuth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, AuthResp{Auth: "apikey"})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	// Category `Dir` must match what the puller writes: subdir = path.Join(
	// j.Category, sanitized name). Sonarr/Radarr predict the completed path
	// as <complete_dir>/<category.dir>/<name>; if Dir disagrees with the
	// puller, every download gets marked failed because Sonarr can't find
	// the file at the predicted location.
	cats := []CategoryCfg{
		{Name: "*", Order: 0, PP: "3", Script: "None", Dir: "", Priority: -100},
		{Name: "tv", Order: 1, PP: "3", Script: "None", Dir: "tv", Priority: -100},
		{Name: "sonarr", Order: 2, PP: "3", Script: "None", Dir: "sonarr", Priority: -100},
		{Name: "movies", Order: 3, PP: "3", Script: "None", Dir: "movies", Priority: -100},
		{Name: "radarr", Order: 4, PP: "3", Script: "None", Dir: "radarr", Priority: -100},
	}
	resp := GetConfigResp{
		Config: GetConfig{
			Misc: MiscConfig{
				CompleteDir:            s.downloadDir,
				HistoryRetention:       "0",
				HistoryRetentionOption: "all",
				HistoryRetentionNumber: 0,
				PreCheck:               false,
			},
			Categories: cats,
			Servers:    []any{},
			Sorters:    []any{},
		},
	}
	writeJSON(w, http.StatusOK, resp)
}
