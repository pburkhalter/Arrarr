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

	"github.com/pburkhalter/arrarr/internal/arrclient"
	"github.com/pburkhalter/arrarr/internal/config"
	"github.com/pburkhalter/arrarr/internal/librarian"
	"github.com/pburkhalter/arrarr/internal/logger"
	"github.com/pburkhalter/arrarr/internal/pathmap"
	"github.com/pburkhalter/arrarr/internal/pushover"
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
	fmt.Println(`Arrarr — SAB-compatible TorBox usenet shim with library writer.

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
	fmt.Println()
	fmt.Println("Now check:")
	fmt.Println("  • TorBox Settings → Webhooks lists your URL + secret")
	fmt.Println("  • Arrarr is reachable at that URL from TorBox's servers")
	fmt.Println("    (public URL via Caddy/Cloudflare-tunnel; *.home.* won't work)")
	fmt.Println("  • Arrarr logs show one of:")
	fmt.Println("       webhook: matched         (event resolved to a job)")
	fmt.Println("       webhook: not ours        (lookup miss — also expected for some test events)")
	fmt.Println("       webhook: signature verify failed   (URL or secret wrong)")
	fmt.Println()
	fmt.Println("If nothing logs at all, the URL isn't reachable from TorBox.")
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

	// v2 librarian: structured /library tree writer (STRM, symlink, or both).
	// Mode "off" returns a no-op writer so the v1 verifier path stays in charge
	// of COMPLETED_TORBOX → READY.
	var libWriter librarian.Writer
	if cfg.LibraryEnabled() {
		libWriter, err = librarian.New(cfg.LibraryMode, cfg.LibraryBase, cfg.LocalVerifyBase)
		if err != nil {
			return fmt.Errorf("librarian: %w", err)
		}
		log.Info("librarian enabled",
			"mode", cfg.LibraryMode,
			"base", cfg.LibraryBase,
			"mount_base", cfg.LocalVerifyBase)
	}

	// v2 arrclients: optional canonical-naming oracles. Either may be unset.
	var sonarrClient, radarrClient *arrclient.Client
	if cfg.SonarrURL != "" && cfg.SonarrAPIKey != "" {
		sonarrClient = arrclient.New(cfg.SonarrURL, cfg.SonarrAPIKey, arrclient.MediaTypeSeries, cfg.ArrCallbackTimeout)
		log.Info("sonarr canonical naming enabled", "url", cfg.SonarrURL)
	}
	if cfg.RadarrURL != "" && cfg.RadarrAPIKey != "" {
		radarrClient = arrclient.New(cfg.RadarrURL, cfg.RadarrAPIKey, arrclient.MediaTypeMovie, cfg.ArrCallbackTimeout)
		log.Info("radarr canonical naming enabled", "url", cfg.RadarrURL)
	}

	// v2 pushover: optional notification sink for ready/failed events.
	var pushoverClient *pushover.Client
	if cfg.PushoverEnabled() {
		pushoverClient = pushover.New(cfg.PushoverToken, cfg.PushoverUser, 6*time.Second)
		log.Info("pushover enabled", "notify_on", cfg.PushoverNotifyOn)
	}

	// v2 webhook: TorBox-side push channel. Only enabled when a secret is set
	// — without a secret we'd have no way to verify signatures, so we'd rather
	// 503 the receiver than silently accept unsigned payloads.
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
		PathMap:     pm,
		Logger:      log.With("component", "sab"),
		CompleteDir: cfg.SonarrVisibleBase,
		Webhook:     webhookOpts,
		Dashboard: &sab.DashboardConfig{
			LibraryEnabled: cfg.LibraryEnabled(),
			LibraryMode:    cfg.LibraryMode,
			MirrorEnabled:  cfg.MirrorEnabled(),
			InstanceName:   cfg.InstanceName,
		},
	})

	wm := worker.New(worker.Options{
		Store:                    st,
		Torbox:                   tb,
		PathMap:                  pm,
		Logger:                   log.With("component", "worker"),
		Wake:                     wakeCh,
		DispatchEvery:            cfg.DispatchInterval,
		PollEvery:                cfg.WorkerPollInterval,
		VerifyEvery:              cfg.WorkerVerifyInterval,
		ReapEvery:                cfg.ReapInterval,
		ReapOlderThan:            time.Duration(cfg.JobRetentionDays) * 24 * time.Hour,
		WorkerPoolSize:           cfg.WorkerPoolSize,
		InstanceName:             cfg.InstanceName,
		Librarian:                libWriter,
		LocalVerifyBase:          cfg.LocalVerifyBase,
		StreamingURLRefreshAfter: cfg.StreamingURLRefreshAfter,
		Sonarr:                   sonarrClient,
		Radarr:                   radarrClient,
		ArrCallbackTimeout:       cfg.ArrCallbackTimeout,
		MirrorEnabled:            cfg.MirrorEnabled(),
		MirrorPollInterval:       cfg.MirrorPollInterval,
	})
	if cfg.MirrorEnabled() {
		log.Info("mirror mode enabled — TorBox account-wide downloads will populate /library",
			"poll_interval", cfg.MirrorPollInterval)
	}

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
