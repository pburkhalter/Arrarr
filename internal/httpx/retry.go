package httpx

import (
	"errors"
	"math/rand/v2"
	"time"
)

func Backoff(attempt int, base, maxDelay time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base << (attempt - 1)
	if d <= 0 || d > maxDelay {
		d = maxDelay
	}
	return time.Duration(rand.Int64N(int64(d)))
}

func RetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if d, err := time.ParseDuration(h + "s"); err == nil && d > 0 {
		return d
	}
	if t, err := time.Parse(time.RFC1123, h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

var ErrRetryable = errors.New("retryable")
