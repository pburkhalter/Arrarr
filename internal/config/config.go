package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	Listen   string `env:"ARRARR_LISTEN" envDefault:":8080"`
	URLBase  string `env:"ARRARR_URL_BASE" envDefault:"/sabnzbd"`
	APIKey   string `env:"ARRARR_API_KEY,required"`
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`
	LogFmt   string `env:"LOG_FORMAT" envDefault:"json"`

	TorboxAPIKey         string  `env:"TORBOX_API_KEY,required"`
	TorboxBaseURL        string  `env:"TORBOX_BASE_URL" envDefault:"https://api.torbox.app/v1/api"`
	TorboxRateLimitPerMin float64 `env:"TORBOX_RATE_LIMIT_PER_MIN" envDefault:"200"`

	LocalVerifyBase   string `env:"LOCAL_VERIFY_BASE" envDefault:"/mnt/torbox"`
	SonarrVisibleBase string `env:"SONARR_VISIBLE_BASE" envDefault:"/torbox"`

	DBPath string `env:"DB_PATH" envDefault:"/data/arrarr.db"`

	WorkerPollInterval   time.Duration `env:"WORKER_POLL_INTERVAL" envDefault:"30s"`
	WorkerVerifyInterval time.Duration `env:"WORKER_VERIFY_INTERVAL" envDefault:"60s"`
	DispatchInterval     time.Duration `env:"DISPATCH_INTERVAL" envDefault:"10s"`
	ReapInterval         time.Duration `env:"REAP_INTERVAL" envDefault:"5m"`

	JobRetentionDays int   `env:"JOB_RETENTION_DAYS" envDefault:"7"`
	MaxNZBBytes      int64 `env:"MAX_NZB_BYTES" envDefault:"52428800"`

	WorkerPoolSize int `env:"WORKER_POOL_SIZE" envDefault:"4"`

	// --- v2: tagging ---
	// Used as the `host:` segment in the TorBox tag set, and as a fallback
	// `name=` prefix on the download. Distinguishes our downloads from other
	// users sharing the TorBox account, and from other Arrarr instances.
	InstanceName string `env:"ARRARR_INSTANCE_NAME" envDefault:"arrarr"`

	// --- v2: library writer ---
	// LibraryBase is the root of the structured tree Arrarr writes into.
	// Sonarr/Radarr should be configured to use $LibraryBase/series and
	// $LibraryBase/movies as their Root Folders. Jellyfin scans the same path.
	LibraryBase              string        `env:"ARRARR_LIBRARY_BASE" envDefault:"/library"`
	LibraryMode              string        `env:"ARRARR_LIBRARY_MODE" envDefault:"strm"`
	StreamingURLRefreshAfter time.Duration `env:"ARRARR_STREAMING_URL_REFRESH_AFTER" envDefault:"5h"`

	// --- v2: webhook ---
	// Empty secret => webhook handler returns 503. Setting this enables
	// receiving completion events from TorBox without polling.
	TorboxWebhookSecret string `env:"ARRARR_TORBOX_WEBHOOK_SECRET"`
	WebhookReplayWindow time.Duration `env:"ARRARR_WEBHOOK_REPLAY_WINDOW" envDefault:"5m"`

	// --- v2: pushover ---
	PushoverToken    string `env:"ARRARR_PUSHOVER_TOKEN"`
	PushoverUser     string `env:"ARRARR_PUSHOVER_USER"`
	PushoverNotifyOn string `env:"ARRARR_PUSHOVER_NOTIFY_ON" envDefault:"ready"`

	// --- v2: mirror mode ---
	MirrorMode         string        `env:"ARRARR_MIRROR_MODE" envDefault:"off"`
	MirrorPollInterval time.Duration `env:"ARRARR_MIRROR_POLL_INTERVAL" envDefault:"10m"`

	// --- v2: Sonarr/Radarr API for canonical naming ---
	SonarrURL          string        `env:"ARRARR_SONARR_URL"`
	SonarrAPIKey       string        `env:"ARRARR_SONARR_API_KEY"`
	RadarrURL          string        `env:"ARRARR_RADARR_URL"`
	RadarrAPIKey       string        `env:"ARRARR_RADARR_API_KEY"`
	ArrCallbackTimeout time.Duration `env:"ARRARR_ARR_CALLBACK_TIMEOUT" envDefault:"5s"`
}

// LibraryModeValues are the legal values for ARRARR_LIBRARY_MODE.
var LibraryModeValues = map[string]bool{"strm": true, "webdav": true, "both": true, "off": true}

// PushoverNotifyValues are the legal values for ARRARR_PUSHOVER_NOTIFY_ON.
var PushoverNotifyValues = map[string]bool{"off": true, "ready": true, "failed": true, "both": true}

// MirrorModeValues are the legal values for ARRARR_MIRROR_MODE.
var MirrorModeValues = map[string]bool{"off": true, "on": true}

func Load() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}
	c.URLBase = "/" + strings.Trim(c.URLBase, "/")
	if c.URLBase == "/" {
		c.URLBase = ""
	}
	if c.WorkerPoolSize < 1 {
		return nil, fmt.Errorf("WORKER_POOL_SIZE must be >= 1")
	}
	if c.TorboxRateLimitPerMin < 1 {
		return nil, fmt.Errorf("TORBOX_RATE_LIMIT_PER_MIN must be >= 1")
	}
	if !LibraryModeValues[c.LibraryMode] {
		return nil, fmt.Errorf("ARRARR_LIBRARY_MODE must be one of strm|webdav|both|off, got %q", c.LibraryMode)
	}
	if !PushoverNotifyValues[c.PushoverNotifyOn] {
		return nil, fmt.Errorf("ARRARR_PUSHOVER_NOTIFY_ON must be one of off|ready|failed|both, got %q", c.PushoverNotifyOn)
	}
	if !MirrorModeValues[c.MirrorMode] {
		return nil, fmt.Errorf("ARRARR_MIRROR_MODE must be one of off|on, got %q", c.MirrorMode)
	}
	if c.PushoverNotifyOn != "off" && (c.PushoverToken == "" || c.PushoverUser == "") {
		return nil, fmt.Errorf("ARRARR_PUSHOVER_NOTIFY_ON=%s requires both ARRARR_PUSHOVER_TOKEN and ARRARR_PUSHOVER_USER", c.PushoverNotifyOn)
	}
	c.LibraryBase = strings.TrimRight(c.LibraryBase, "/")
	c.InstanceName = strings.TrimSpace(c.InstanceName)
	if c.InstanceName == "" {
		return nil, fmt.Errorf("ARRARR_INSTANCE_NAME must not be empty")
	}
	return c, nil
}

// PushoverEnabled reports whether Pushover notifications are configured to fire
// on at least one event. Equivalent to: notify-on != "off" AND credentials present.
func (c *Config) PushoverEnabled() bool {
	return c.PushoverNotifyOn != "off" && c.PushoverToken != "" && c.PushoverUser != ""
}

// WebhookEnabled reports whether the TorBox webhook receiver should serve.
// Without a secret we have no way to verify signatures, so the handler 503s.
func (c *Config) WebhookEnabled() bool { return c.TorboxWebhookSecret != "" }

// LibraryEnabled reports whether the librarian should write any files at all.
// LibraryMode=off keeps v1 behavior (raw /mnt/torbox path reported to Sonarr).
func (c *Config) LibraryEnabled() bool { return c.LibraryMode != "off" }

// MirrorEnabled reports whether the mirror worker should run.
func (c *Config) MirrorEnabled() bool { return c.MirrorMode == "on" }
