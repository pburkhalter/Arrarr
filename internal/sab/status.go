package sab

import (
	_ "embed"
	"html/template"
	"net/http"
	"time"

	"github.com/pburkhalter/arrarr/internal/store"
)

//go:embed status.html
var statusHTML string

var statusTmpl = template.Must(
	template.New("status").
		Funcs(template.FuncMap{
			"shortID": func(s string) string {
				if len(s) > 16 {
					return s[:16] + "…"
				}
				return s
			},
			"ago": func(t time.Time) string {
				d := time.Since(t)
				switch {
				case d < time.Minute:
					return "just now"
				case d < time.Hour:
					return d.Round(time.Second).String() + " ago"
				case d < 24*time.Hour:
					return d.Round(time.Minute).String() + " ago"
				default:
					return d.Round(time.Hour).String() + " ago"
				}
			},
			"ifEmpty": func(s, fallback string) string {
				if s == "" {
					return fallback
				}
				return s
			},
		}).
		Parse(statusHTML))

type statusPageData struct {
	Stats              *store.Stats
	WebhookEnabled     bool
	PushoverConfigured bool
	PushoverNotifyOn   string
	GeneratedAt        time.Time
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.FetchStats(r.Context(), 25)
	if err != nil {
		http.Error(w, "stats unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := statusPageData{
		Stats:       stats,
		GeneratedAt: time.Now().UTC(),
	}
	if s.webhook != nil && s.webhook.Secret != "" {
		data.WebhookEnabled = true
		if s.webhook.Pushover != nil {
			data.PushoverConfigured = true
			data.PushoverNotifyOn = s.webhook.PushoverNotifyOn
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusTmpl.Execute(w, data); err != nil {
		s.logger.Warn("status: template execute failed", "err", err)
	}
}
