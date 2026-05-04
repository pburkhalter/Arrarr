package sab

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Store interface {
	storeReader
	storeWriter
}

type Server struct {
	apiKey  string
	urlBase string
	maxNZB  int64

	store     Store
	wake      chan<- struct{}
	pathMap   pathMapper
	logger    *slog.Logger
	completeDir string
}

type pathMapper interface {
	Visible(folderName string) (string, error)
}

type Options struct {
	APIKey      string
	URLBase     string
	MaxNZBBytes int64
	Store       Store
	Wake        chan<- struct{}
	PathMap     pathMapper
	Logger      *slog.Logger
	CompleteDir string
}

func NewServer(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Server{
		apiKey:      o.APIKey,
		urlBase:     o.URLBase,
		maxNZB:      o.MaxNZBBytes,
		store:       o.Store,
		wake:        o.Wake,
		pathMap:     o.PathMap,
		logger:      o.Logger,
		completeDir: o.CompleteDir,
	}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	api := chi.NewRouter()
	api.HandleFunc("/api", s.handleAPI)
	api.Get("/healthz", s.handleHealthz)

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
	if r.URL.Query().Get("apikey") == s.apiKey {
		return true
	}
	if r.Header.Get("X-Api-Key") == s.apiKey {
		return true
	}
	return false
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
