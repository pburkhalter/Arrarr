package worker

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

// requestdl carries the TorBox api key as token=…; a transport error quotes
// the full URL, and that text used to land verbatim in last_error, on the
// status page and in the outbound event payload.
func TestDescribeMasksCredentialsInURLs(t *testing.T) {
	err := fmt.Errorf("requestdl file_id=3: %w", &url.Error{
		Op:  "Get",
		URL: "https://api.torbox.app/v1/api/usenet/requestdl?token=tb-secret-123&usenet_id=9&redirect=false",
		Err: errors.New("dial tcp: i/o timeout"),
	})
	got := describe(err)
	if strings.Contains(got, "tb-secret-123") {
		t.Fatalf("api key leaked: %s", got)
	}
	if !strings.Contains(got, "token=***") || !strings.Contains(got, "usenet_id=9") || !strings.Contains(got, "i/o timeout") {
		t.Fatalf("redaction mangled the message: %s", got)
	}
	for in, want := range map[string]string{
		"apikey=abc&x=1":        "apikey=***&x=1",
		"X api_key=abc def":     "X api_key=*** def",
		"API-KEY=zz":            "API-KEY=***",
		"password=hunter2":      "password=***",
		"no secrets here":       "no secrets here",
		"token=abc\"; quoted":   "token=***\"; quoted",
		"plain torbox 429 text": "plain torbox 429 text",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func transitionTo(from, to job.State) store.Transition {
	return store.Transition{From: from, To: to, ClearClaimed: true}
}
