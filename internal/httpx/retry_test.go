package httpx

import (
	"testing"
	"time"
)

func TestBackoffBounds(t *testing.T) {
	base := time.Second
	max := 30 * time.Second
	for attempt := 1; attempt <= 10; attempt++ {
		d := Backoff(attempt, base, max)
		if d < 0 || d > max {
			t.Errorf("attempt %d: %v out of [0,%v]", attempt, d, max)
		}
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	if got := RetryAfter("30"); got != 30*time.Second {
		t.Errorf("RetryAfter(\"30\") = %v want 30s", got)
	}
	if got := RetryAfter(""); got != 0 {
		t.Errorf("RetryAfter empty should be 0, got %v", got)
	}
	if got := RetryAfter("not-a-number-at-all"); got != 0 {
		t.Errorf("RetryAfter garbage should be 0, got %v", got)
	}
}
