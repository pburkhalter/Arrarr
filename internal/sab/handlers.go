package sab

import (
	"net/http"
	"strings"
)

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := strings.ToLower(q.Get("mode"))

	if !s.authenticate(r, mode) {
		s.writeError(w, http.StatusUnauthorized, "API Key Incorrect")
		return
	}

	switch mode {
	case "version":
		s.handleVersion(w, r)
	case "auth":
		s.handleAuth(w, r)
	case "get_config":
		s.handleGetConfig(w, r)
	case "fullstatus":
		writeJSON(w, http.StatusOK, map[string]any{"status": map[string]any{"paused": false}})
	case "addfile":
		s.handleAddFile(w, r)
	case "addurl":
		s.handleAddURL(w, r)
	case "queue":
		s.handleQueue(w, r)
	case "history":
		s.handleHistory(w, r)
	default:
		s.writeError(w, http.StatusBadRequest, "Unknown mode: "+mode)
	}
}
