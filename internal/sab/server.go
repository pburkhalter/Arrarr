package sab

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Store interface {
	storeReader
	storeWriter
}

type Server struct {
	apiKey      string
	urlBase     string
	maxNZB      int64
	downloadDir string

	store   Store
	wake    chan<- struct{}
	logger  *slog.Logger
	version string

	// webhook is set when ARRARR_TORBOX_WEBHOOK_SECRET is configured. nil
	// means the webhook receiver is disabled and returns 503.
	webhook *WebhookOptions

	// torboxQuota reports the createusenetdownload token bucket's available
	// tokens and burst ceiling, for /status.json. nil → omitted from the
	// payload (e.g. test fakes with no TorBox client).
	torboxQuota func() (avail float64, burst int)

	// pollHealth reports the worker poller's last successful pass and last
	// error, for /status.json. nil → omitted from the payload.
	pollHealth func() (lastOK time.Time, lastErr string)
}

type Options struct {
	APIKey      string
	URLBase     string
	MaxNZBBytes int64
	// DownloadDir is the puller's DOWNLOAD_DIR, surfaced via SAB's
	// /api?mode=get_config as `misc.complete_dir`. Sonarr/Radarr use this
	// (with the per-category `dir`) to predict where a download will land,
	// and run a docker-mount sanity check against it — so it must reflect
	// the actual puller output, not a stale default.
	DownloadDir string
	Store       Store
	Wake        chan<- struct{}
	Logger      *slog.Logger
	Webhook     *WebhookOptions
	// Version is the running arrarr build (main.versionStr), surfaced on
	// /status.json so an external monitor can display it and check for updates.
	Version string
	// TorboxQuota, when set, exposes the create-endpoint rate-limiter
	// headroom on /status.json. Wire it to (*torbox.Client).CreateHeadroom.
	TorboxQuota func() (avail float64, burst int)
	// PollHealth, when set, exposes poller liveness on /status.json. Wire it to
	// (*worker.Manager).PollHealth.
	PollHealth func() (lastOK time.Time, lastErr string)
}

func NewServer(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Server{
		apiKey:      o.APIKey,
		urlBase:     o.URLBase,
		maxNZB:      o.MaxNZBBytes,
		downloadDir: o.DownloadDir,
		store:       o.Store,
		wake:        o.Wake,
		logger:      o.Logger,
		webhook:     o.Webhook,
		torboxQuota: o.TorboxQuota,
		pollHealth:  o.PollHealth,
		version:     o.Version,
	}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	api := chi.NewRouter()
	api.HandleFunc("/api", s.handleAPI)
	api.Get("/healthz", s.handleHealthz)
	api.Post("/webhook", s.handleTorBoxWebhook)
	api.Get("/status.json", s.handleStatusJSON)
	// The HTML page lists job paths, nzo ids and error texts — key required.
	// /status.json stays open: aggregates only, and the NAS healthcheck reads it.
	api.Get("/", s.requireKey(s.handleStatus))

	if s.urlBase == "" {
		r.Mount("/", api)
	} else {
		r.Mount(s.urlBase, api)
		// Also mount at root: a probe with empty URL Base still gets a valid version reply.
		r.Mount("/", api)
	}
	return r
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) signalWake() {
	if s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Server) authenticate(r *http.Request, mode string) bool {
	if mode == "version" || mode == "auth" {
		return true
	}
	if s.apiKey == "" {
		return true
	}
	key := []byte(s.apiKey)
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("apikey")), key) == 1 {
		return true
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Api-Key")), key) == 1 {
		return true
	}
	return false
}

// requireKey gates a plain GET (no SAB mode) behind the api key.
func (s *Server) requireKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticate(r, "") {
			http.Error(w, "API Key Incorrect", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResp{Status: false, Error: msg})
}

func Run(ctx context.Context, addr string, h http.Handler, logger *slog.Logger) error {
	srv := &http.Server{Addr: addr, Handler: h}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := contextWithTimeoutSafe(ctx, 30)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		if err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
