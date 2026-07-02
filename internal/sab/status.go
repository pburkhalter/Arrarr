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

// statusJSON is the machine-readable sibling of handleStatus, consumed by the
// streaming dashboard's customapi widget. Unauthenticated, like handleStatus
// and healthz — it exposes only aggregate counts, no secrets.
type statusJSON struct {
	GeneratedAt  time.Time         `json:"generated_at"`
	States       map[string]int    `json:"states"`
	TorboxCreate *torboxCreateJSON `json:"torbox_create,omitempty"`
}

// torboxCreateJSON reports the createusenetdownload rate-limiter headroom.
// Available is floored to a whole token (the bucket refills continuously).
type torboxCreateJSON struct {
	Available int `json:"available"`
	Capacity  int `json:"capacity"`
}

func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.FetchStats(r.Context(), 0)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "stats unavailable: "+err.Error())
		return
	}
	out := statusJSON{
		GeneratedAt: time.Now().UTC(),
		States:      stats.StateCounts,
	}
	if s.torboxQuota != nil {
		avail, burst := s.torboxQuota()
		// burst == 0 signals "no limiter configured" (CreateHeadroom returns
		// -1, 0). When a limiter exists, Tokens() can go negative under
		// sustained create load — Wait() borrows against future refill — so
		// floor Available at 0 instead of dropping the field, which would
		// render as NaN in the dashboard widget.
		if burst > 0 {
			a := int(avail)
			if a < 0 {
				a = 0
			}
			out.TorboxCreate = &torboxCreateJSON{Available: a, Capacity: burst}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
