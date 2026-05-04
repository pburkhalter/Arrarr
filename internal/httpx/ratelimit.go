package httpx

import (
	"context"
	"net/http"

	"golang.org/x/time/rate"
)

type RateLimitTransport struct {
	Base    http.RoundTripper
	Limiter *rate.Limiter
}

func NewRateLimitTransport(base http.RoundTripper, perMinute float64, burst int) *RateLimitTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	if burst < 1 {
		burst = 1
	}
	return &RateLimitTransport{
		Base:    base,
		Limiter: rate.NewLimiter(rate.Limit(perMinute/60.0), burst),
	}
}

func (t *RateLimitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx := r.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := t.Limiter.Wait(ctx); err != nil {
		return nil, err
	}
	return t.Base.RoundTrip(r)
}
