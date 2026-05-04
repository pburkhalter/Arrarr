package sab

import "net/http"

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, VersionResp{Version: SabnzbdVersion})
}

func (s *Server) handleAuth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, AuthResp{Auth: "apikey"})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	cats := []CategoryCfg{
		{Name: "*", Order: 0, PP: "3", Script: "None", Dir: "", Priority: -100},
		{Name: "tv", Order: 1, PP: "3", Script: "None", Dir: "tv", Priority: -100},
		{Name: "sonarr", Order: 2, PP: "3", Script: "None", Dir: "tv", Priority: -100},
		{Name: "movies", Order: 3, PP: "3", Script: "None", Dir: "movies", Priority: -100},
		{Name: "radarr", Order: 4, PP: "3", Script: "None", Dir: "movies", Priority: -100},
	}
	resp := GetConfigResp{
		Config: GetConfig{
			Misc: MiscConfig{
				CompleteDir:            s.completeDir,
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
