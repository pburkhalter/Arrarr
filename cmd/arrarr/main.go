package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pburkhalter/arrarr/internal/config"
	"github.com/pburkhalter/arrarr/internal/downloader"
	"github.com/pburkhalter/arrarr/internal/logger"
	"github.com/pburkhalter/arrarr/internal/pushover"
	"github.com/pburkhalter/arrarr/internal/qbit"
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
		case "verify-webhook":
			os.Exit(runVerifyWebhook())
		case "help", "-h", "--help":
			printHelp()
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Println(`Arrarr — SAB+qBit shim for TorBox with a local puller.

Usage:
  arrarr                  Run the daemon (default).
  arrarr healthcheck      Probe /healthz on the local listener.
  arrarr verify-webhook   Trigger TorBox /notifications/test so you can confirm
                          your webhook URL + secret are wired up correctly.
                          Requires TORBOX_API_KEY in the environment.
                          Server-side rate-limited to 1/minute.
  arrarr version          Print build version.
  arrarr help             This text.

See README.md for full configuration and the architecture overview.`)
}

func runVerifyWebhook() int {
	apiKey := os.Getenv("TORBOX_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "verify-webhook: TORBOX_API_KEY not set")
		return 1
	}
	base := os.Getenv("TORBOX_BASE_URL")
	if base == "" {
		base = "https://api.torbox.app/v1/api"
	}
	client := torbox.NewClient(base, apiKey, 60, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.TestNotification(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "verify-webhook: failed:", err)
		fmt.Fprintln(os.Stderr, "  (TorBox rate-limits this endpoint to 1/min)")
		return 1
	}
	fmt.Println("Test notification fired.")
	return 0
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
		"download_dir", cfg.DownloadDir)

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

	var pushoverClient *pushover.Client
	if cfg.PushoverEnabled() {
		pushoverClient = pushover.New(cfg.PushoverToken, cfg.PushoverUser, 6*time.Second)
		log.Info("pushover enabled", "notify_on", cfg.PushoverNotifyOn)
	}

	var webhookOpts *sab.WebhookOptions
	if cfg.WebhookEnabled() {
		webhookOpts = &sab.WebhookOptions{
			Secret:           cfg.TorboxWebhookSecret,
			RequireSignature: cfg.RequireWebhookSignature,
			ReplayWindow:     cfg.WebhookReplayWindow,
			Pushover:         pushoverClient,
			PushoverNotifyOn: cfg.PushoverNotifyOn,
		}
		log.Info("torbox webhook receiver enabled",
			"replay_window", cfg.WebhookReplayWindow,
			"require_signature", cfg.RequireWebhookSignature)
	}

	wakeCh := make(chan struct{}, 1)

	srv := sab.NewServer(sab.Options{
		APIKey:      cfg.APIKey,
		URLBase:     cfg.URLBase,
		MaxNZBBytes: cfg.MaxNZBBytes,
		Store:       sab.Adapt(st),
		Wake:        wakeCh,
		Logger:      log.With("component", "sab"),
		Webhook:     webhookOpts,
	})

	dl, err := downloader.New(downloader.Options{
		BaseDir:     cfg.DownloadDir,
		Concurrency: cfg.DownloadConcurrency,
		Logger:      log.With("component", "downloader"),
	})
	if err != nil {
		return fmt.Errorf("downloader: %w", err)
	}
	puller := worker.NewPuller(worker.PullerOptions{
		Store:      st,
		Torbox:     tb,
		Downloader: dl,
		BaseDir:    cfg.DownloadDir,
		Logger:     log.With("component", "puller"),
	})
	log.Info("puller enabled", "download_dir", cfg.DownloadDir, "concurrency", cfg.DownloadConcurrency)

	wm := worker.New(worker.Options{
		Store:          st,
		Torbox:         tb,
		Logger:         log.With("component", "worker"),
		Wake:           wakeCh,
		DispatchEvery:  cfg.DispatchInterval,
		PollEvery:      cfg.WorkerPollInterval,
		ReapEvery:      cfg.ReapInterval,
		ReapOlderThan:  time.Duration(cfg.JobRetentionDays) * 24 * time.Hour,
		WorkerPoolSize: cfg.WorkerPoolSize,
		Puller:         puller,
	})

	// Multiplex sab + qbit on one listener. sab.Handler() routes /sabnzbd/*
	// plus /webhook + /healthz at root; qbit.Handler() routes /api/v2/*.
	sabHandler := srv.Handler()
	var root http.Handler = sabHandler
	if cfg.QbitEnabled() {
		qbitSrv := qbit.NewServer(qbit.Options{
			Username:        cfg.QbitUsername,
			Password:        cfg.QbitPassword,
			URLBase:         cfg.QbitURLBase,
			DownloadDir:     cfg.DownloadDir,
			MaxTorrentBytes: cfg.MaxTorrentBytes,
			Store:           qbit.Adapt(st),
			Wake:            wakeCh,
			Logger:          log.With("component", "qbit"),
		})
		qbitHandler := qbitSrv.Handler()
		root = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/v2/") || r.URL.Path == "/api/v2" {
				qbitHandler.ServeHTTP(w, r)
				return
			}
			sabHandler.ServeHTTP(w, r)
		})
		log.Info("qbit shim enabled", "url_base", cfg.QbitURLBase, "max_torrent_bytes", cfg.MaxTorrentBytes)
	}

	g, ctx := errgroup.WithContext(rootCtx)
	g.Go(func() error {
		return sab.Run(ctx, cfg.Listen, root, log.With("component", "http"))
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
