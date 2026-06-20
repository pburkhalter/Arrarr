package qbit

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Logger is the small slog-shaped interface the shim uses. *slog.Logger
// satisfies it. Keeping our own interface lets test code substitute a no-op.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type Options struct {
	Username        string
	Password        string
	URLBase         string
	DownloadDir     string
	MaxTorrentBytes int64
	Store           JobStore
	Wake            chan<- struct{}
	Logger          Logger
}

type Server struct {
	username        string
	password        string
	urlBase         string
	downloadDir     string
	maxTorrentBytes int64

	store  JobStore
	wake   chan<- struct{}
	logger Logger

	sessions *sessionStore
}

func NewServer(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MaxTorrentBytes <= 0 {
		// qBit clients commonly upload < 1 MiB .torrent files. Cap to keep
		// abusive uploads from filling memory.
		o.MaxTorrentBytes = 5 << 20
	}
	return &Server{
		username:        o.Username,
		password:        o.Password,
		urlBase:         o.URLBase,
		downloadDir:     o.DownloadDir,
		maxTorrentBytes: o.MaxTorrentBytes,
		store:           o.Store,
		wake:            o.Wake,
		logger:          o.Logger,
		sessions:        newSessionStore(time.Hour),
	}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	api := chi.NewRouter()
	// Public endpoints — auth/login obviously, and app/version is what Sonarr
	// probes before sending credentials to confirm we're a qBit endpoint.
	api.Get("/api/v2/app/version", s.handleAppVersion)
	api.Get("/api/v2/app/webapiVersion", s.handleWebAPIVersion)
	api.Post("/api/v2/auth/login", s.handleLogin)
	api.Post("/api/v2/auth/logout", s.handleLogout)

	// Authenticated endpoints.
	auth := chi.NewRouter()
	auth.Use(s.requireAuth)
	auth.Get("/api/v2/app/preferences", s.handlePreferences)
	auth.Get("/api/v2/torrents/info", s.handleTorrentsInfo)
	auth.Post("/api/v2/torrents/add", s.handleTorrentsAdd)
	auth.Post("/api/v2/torrents/delete", s.handleTorrentsDelete)
	auth.Post("/api/v2/torrents/pause", s.handleTorrentsPause)
	auth.Post("/api/v2/torrents/resume", s.handleTorrentsResume)
	auth.Get("/api/v2/torrents/categories", s.handleCategoriesGet)
	auth.Post("/api/v2/torrents/createCategory", s.handleCategoriesCreate)
	// qBit accepts hash via either query or form on these — chi.HandleFunc lets
	// us handle GET/POST/HEAD uniformly. Sonarr v4 uses GET; older builds POST.
	auth.HandleFunc("/api/v2/torrents/files", s.handleTorrentsFiles)
	auth.HandleFunc("/api/v2/torrents/properties", s.handleTorrentsProperties)

	api.Mount("/", auth)

	if s.urlBase == "" {
		r.Mount("/", api)
	} else {
		r.Mount(s.urlBase, api)
		// Also expose at root so probes hitting /api/v2/app/version without
		// the prefix still get a version reply. Same trick as internal/sab.
		r.Mount("/", api)
	}
	return r
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

func Run(ctx context.Context, addr string, h http.Handler, logger *slog.Logger) error {
	srv := &http.Server{Addr: addr, Handler: h}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("qbit http listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func nowUTC() time.Time { return time.Now().UTC() }
