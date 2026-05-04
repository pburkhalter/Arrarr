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
}

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
	return c, nil
}
