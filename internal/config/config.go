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

	TorboxAPIKey          string  `env:"TORBOX_API_KEY,required"`
	TorboxBaseURL         string  `env:"TORBOX_BASE_URL" envDefault:"https://api.torbox.app/v1/api"`
	TorboxRateLimitPerMin float64 `env:"TORBOX_RATE_LIMIT_PER_MIN" envDefault:"200"`
	// TorboxCreatePerHour throttles createusenetdownload + createtorrent
	// requests independently of the global limiter. TorBox's documented ceiling
	// is 60/hour; defaulting a touch under to leave headroom for clock skew
	// and Recyclarr's separate calls.
	TorboxCreatePerHour float64 `env:"TORBOX_CREATE_PER_HOUR" envDefault:"55"`

	DBPath string `env:"DB_PATH" envDefault:"/data/arrarr.db"`

	WorkerPollInterval time.Duration `env:"WORKER_POLL_INTERVAL" envDefault:"30s"`
	DispatchInterval   time.Duration `env:"DISPATCH_INTERVAL" envDefault:"10s"`
	ReapInterval       time.Duration `env:"REAP_INTERVAL" envDefault:"5m"`

	JobRetentionDays int   `env:"JOB_RETENTION_DAYS" envDefault:"7"`
	MaxNZBBytes      int64 `env:"MAX_NZB_BYTES" envDefault:"52428800"`

	WorkerPoolSize int `env:"WORKER_POOL_SIZE" envDefault:"4"`

	// Puller (always on): copies completed TorBox items to a local directory so
	// Sonarr/Radarr import via the qbit/sab shim from a normal local path.
	DownloadDir         string `env:"ARRARR_DOWNLOAD_DIR" envDefault:"/downloads"`
	DownloadConcurrency int    `env:"ARRARR_DOWNLOAD_CONCURRENCY" envDefault:"4"`

	// qBittorrent v2 shim. Conventionally rootless — leave QbitURLBase empty.
	QbitURLBase     string `env:"ARRARR_QBIT_URL_BASE"`
	QbitUsername    string `env:"ARRARR_QBIT_USERNAME" envDefault:"admin"`
	QbitPassword    string `env:"ARRARR_QBIT_PASSWORD"`
	MaxTorrentBytes int64  `env:"ARRARR_MAX_TORRENT_BYTES" envDefault:"4194304"`

	// Webhook: empty secret leaves the receiver disabled (handler returns 503).
	TorboxWebhookSecret string `env:"ARRARR_TORBOX_WEBHOOK_SECRET"`
	// TorBox does not currently sign webhooks despite claiming Standard Webhooks
	// compliance, so set this to false when targeting TorBox. Compensate with
	// CF rate limit + WAF rules at the edge.
	RequireWebhookSignature bool          `env:"ARRARR_TORBOX_WEBHOOK_REQUIRE_SIGNATURE" envDefault:"true"`
	WebhookReplayWindow     time.Duration `env:"ARRARR_WEBHOOK_REPLAY_WINDOW" envDefault:"5m"`

	// Pushover notifications.
	PushoverToken    string `env:"ARRARR_PUSHOVER_TOKEN"`
	PushoverUser     string `env:"ARRARR_PUSHOVER_USER"`
	PushoverNotifyOn string `env:"ARRARR_PUSHOVER_NOTIFY_ON" envDefault:"off"`
}

// PushoverNotifyValues are the legal values for ARRARR_PUSHOVER_NOTIFY_ON.
var PushoverNotifyValues = map[string]bool{"off": true, "ready": true, "failed": true, "both": true}

func Load() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}
	c.URLBase = "/" + strings.Trim(c.URLBase, "/")
	if c.URLBase == "/" {
		c.URLBase = ""
	}
	c.QbitURLBase = "/" + strings.Trim(c.QbitURLBase, "/")
	if c.QbitURLBase == "/" {
		c.QbitURLBase = ""
	}
	c.DownloadDir = strings.TrimRight(c.DownloadDir, "/")

	if c.WorkerPoolSize < 1 {
		return nil, fmt.Errorf("WORKER_POOL_SIZE must be >= 1")
	}
	if c.TorboxRateLimitPerMin < 1 {
		return nil, fmt.Errorf("TORBOX_RATE_LIMIT_PER_MIN must be >= 1")
	}
	if !PushoverNotifyValues[c.PushoverNotifyOn] {
		return nil, fmt.Errorf("ARRARR_PUSHOVER_NOTIFY_ON must be one of off|ready|failed|both, got %q", c.PushoverNotifyOn)
	}
	if c.PushoverNotifyOn != "off" && (c.PushoverToken == "" || c.PushoverUser == "") {
		return nil, fmt.Errorf("ARRARR_PUSHOVER_NOTIFY_ON=%s requires both ARRARR_PUSHOVER_TOKEN and ARRARR_PUSHOVER_USER", c.PushoverNotifyOn)
	}
	if c.DownloadDir == "" {
		return nil, fmt.Errorf("ARRARR_DOWNLOAD_DIR must be set")
	}
	if c.DownloadConcurrency < 1 {
		return nil, fmt.Errorf("ARRARR_DOWNLOAD_CONCURRENCY must be >= 1")
	}
	if c.QbitEnabled() && c.QbitPassword == "" {
		return nil, fmt.Errorf("ARRARR_QBIT_USERNAME set without ARRARR_QBIT_PASSWORD")
	}
	return c, nil
}

// QbitEnabled reports whether the qBittorrent v2 shim should serve. Treated as
// enabled when the user has set credentials — Sonarr/Radarr won't authenticate
// without them anyway, so absent credentials = absent qbit endpoint.
func (c *Config) QbitEnabled() bool {
	return c.QbitUsername != "" && c.QbitPassword != ""
}

// PushoverEnabled reports whether Pushover notifications are configured to fire
// on at least one event.
func (c *Config) PushoverEnabled() bool {
	return c.PushoverNotifyOn != "off" && c.PushoverToken != "" && c.PushoverUser != ""
}

// WebhookEnabled reports whether the TorBox webhook receiver should serve.
// Without a secret we have no way to verify signatures, so the handler 503s.
func (c *Config) WebhookEnabled() bool { return c.TorboxWebhookSecret != "" }
