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
			"untilExpires": func(t *time.Time) string {
				if t == nil {
					return "—"
				}
				d := time.Until(*t)
				if d < 0 {
					return "expired"
				}
				return "in " + d.Round(time.Minute).String()
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
	Stats             *store.Stats
	WebhookEnabled    bool
	PushoverConfigured bool
	PushoverNotifyOn  string
	LibraryEnabled    bool
	LibraryMode       string
	MirrorEnabled     bool
	InstanceName      string
	GeneratedAt       time.Time
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
	if s.dashCfg != nil {
		data.LibraryEnabled = s.dashCfg.LibraryEnabled
		data.LibraryMode = s.dashCfg.LibraryMode
		data.MirrorEnabled = s.dashCfg.MirrorEnabled
		data.InstanceName = s.dashCfg.InstanceName
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusTmpl.Execute(w, data); err != nil {
		// Headers already sent — best we can do is log via the chi recoverer.
		s.logger.Warn("status: template execute failed", "err", err)
	}
}

// DashboardConfig is the read-only view of v2 settings the dashboard needs.
// Held by reference on Server so main.go can fill it without burdening the
// hot path of webhook/SAB handlers.
type DashboardConfig struct {
	LibraryEnabled bool
	LibraryMode    string
	MirrorEnabled  bool
	InstanceName   string
}
