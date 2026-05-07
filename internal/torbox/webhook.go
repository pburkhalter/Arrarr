package torbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Webhook signature verification per Standard Webhooks
// (https://www.standardwebhooks.com/). TorBox claims compliance.
//
// Signed message format: "<webhook-id>.<webhook-timestamp>.<body>" hashed with
// HMAC-SHA256 keyed by the per-webhook secret. The header carries one or more
// space-separated "v1,<base64>" entries; we accept any matching v1 entry.

// ErrSignatureMissing means the request had no webhook-signature header.
var ErrSignatureMissing = errors.New("torbox webhook: missing signature header")

// ErrSignatureMismatch means none of the v1 signatures in the header matched
// our HMAC of the body.
var ErrSignatureMismatch = errors.New("torbox webhook: signature mismatch")

// ErrTimestampStale means the signed timestamp is outside the configured replay
// window — drop the request to avoid replay attacks.
var ErrTimestampStale = errors.New("torbox webhook: timestamp outside replay window")

// VerifyWebhook returns nil if the supplied headers + body verify against the
// shared secret. The secret is the value the user pasted into TorBox's
// settings UI when configuring the webhook.
//
// id, timestamp, signatureHeader come from the standard headers:
//
//	webhook-id        → id
//	webhook-timestamp → timestamp (unix seconds, decimal string)
//	webhook-signature → signatureHeader (one or more "v1,<b64>" entries, space-separated)
//
// replayWindow caps how far in the past or future we'll accept the signed
// timestamp; 5 minutes is the spec recommendation.
func VerifyWebhook(secret, id, timestamp, signatureHeader string, body []byte, replayWindow time.Duration, now time.Time) error {
	if secret == "" {
		return errors.New("torbox webhook: empty secret")
	}
	if id == "" {
		return errors.New("torbox webhook: missing id")
	}
	if signatureHeader == "" {
		return ErrSignatureMissing
	}
	tsSecs, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return fmt.Errorf("torbox webhook: bad timestamp: %w", err)
	}
	signedAt := time.Unix(tsSecs, 0)
	skew := now.Sub(signedAt)
	if skew < 0 {
		skew = -skew
	}
	if replayWindow > 0 && skew > replayWindow {
		return ErrTimestampStale
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	want := mac.Sum(nil)

	// signatureHeader: "v1,<b64> v1,<b64> ..." (space-separated).
	for _, entry := range strings.Fields(signatureHeader) {
		ver, sig, ok := strings.Cut(entry, ",")
		if !ok || ver != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrSignatureMismatch
}
