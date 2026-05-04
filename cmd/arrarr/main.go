package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pburkhalter/arrarr/internal/config"
	"github.com/pburkhalter/arrarr/internal/logger"
	"github.com/pburkhalter/arrarr/internal/pathmap"
	"github.com/pburkhalter/arrarr/internal/sab"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
	"github.com/pburkhalter/arrarr/internal/worker"
	"golang.org/x/sync/errgroup"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(runHealthcheck())
		case "version":
			fmt.Println("arrarr", versionStr)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := logger.New(cfg.LogLevel, cfg.LogFmt)
	log.Info("starting arrarr",
		"version", versionStr,
		"listen", cfg.Listen,
		"url_base", cfg.URLBase,
		"db", cfg.DBPath,
		"local_verify_base", cfg.LocalVerifyBase,
		"sonarr_visible_base", cfg.SonarrVisibleBase)

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	st, err := store.Open(rootCtx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if err := st.Migrate(rootCtx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	tb := torbox.NewClient(cfg.TorboxBaseURL, cfg.TorboxAPIKey, cfg.TorboxRateLimitPerMin, 30*time.Second)
	pm := pathmap.New(cfg.LocalVerifyBase, cfg.SonarrVisibleBase)

	wakeCh := make(chan struct{}, 1)

	srv := sab.NewServer(sab.Options{
		APIKey:      cfg.APIKey,
		URLBase:     cfg.URLBase,
		MaxNZBBytes: cfg.MaxNZBBytes,
		Store:       sab.Adapt(st),
		Wake:        wakeCh,
		PathMap:     pm,
		Logger:      log.With("component", "sab"),
		CompleteDir: cfg.SonarrVisibleBase,
	})

	wm := worker.New(worker.Options{
		Store:          st,
		Torbox:         tb,
		PathMap:        pm,
		Logger:         log.With("component", "worker"),
		Wake:           wakeCh,
		DispatchEvery:  cfg.DispatchInterval,
		PollEvery:      cfg.WorkerPollInterval,
		VerifyEvery:    cfg.WorkerVerifyInterval,
		ReapEvery:      cfg.ReapInterval,
		ReapOlderThan:  time.Duration(cfg.JobRetentionDays) * 24 * time.Hour,
		WorkerPoolSize: cfg.WorkerPoolSize,
	})

	g, ctx := errgroup.WithContext(rootCtx)
	g.Go(func() error {
		return sab.Run(ctx, cfg.Listen, srv.Handler(), log.With("component", "http"))
	})
	g.Go(func() error {
		return wm.Run(ctx)
	})

	err = g.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

func runHealthcheck() int {
	addr := os.Getenv("ARRARR_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	urlBase := os.Getenv("ARRARR_URL_BASE")
	if urlBase == "" {
		urlBase = "/sabnzbd"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + urlBase + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

var versionStr = "dev"
