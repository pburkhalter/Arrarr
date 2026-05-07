// Package pushover is a minimal Pushover client. We use it as the
// notification sink on download.ready / download.failed webhook events.
//
// Single endpoint, single method, no retry beyond the stdlib client timeout.
// Pushover's failure here is decoupled from the state machine — if the
// notification can't be delivered, the job has already been written to the
// library and the state has advanced; we just lose the user's heads-up.
package pushover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiURL = "https://api.pushover.net/1/messages.json"

// Notification is the subset of Pushover's API we expose.
type Notification struct {
	Title    string
	Message  string
	Priority int    // -2..2; 0 normal, 1 high, 2 emergency (latter requires retry+expire)
	Sound    string // optional, e.g. "pushover", "magic", "siren"
	URL      string // optional, e.g. link to Jellyfin
	URLTitle string // optional
}

// Client wraps a configured Pushover app token + user key.
type Client struct {
	Token string
	User  string
	HTTP  *http.Client
}

// New builds a client. Both token and user are required by Pushover.
func New(token, user string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	return &Client{
		Token: token,
		User:  user,
		HTTP:  &http.Client{Timeout: timeout},
	}
}

// Notify delivers one message. Returns nil on Pushover-side success, or an
// error on transport / 4xx / 5xx. Caller decides whether to log+drop.
func (c *Client) Notify(ctx context.Context, n Notification) error {
	if c.Token == "" || c.User == "" {
		return errors.New("pushover: missing token or user")
	}
	form := url.Values{}
	form.Set("token", c.Token)
	form.Set("user", c.User)
	form.Set("title", trim(n.Title, 250))
	form.Set("message", trim(n.Message, 1024))
	if n.Priority != 0 {
		form.Set("priority", fmt.Sprintf("%d", n.Priority))
	}
	if n.Sound != "" {
		form.Set("sound", n.Sound)
	}
	if n.URL != "" {
		form.Set("url", n.URL)
	}
	if n.URLTitle != "" {
		form.Set("url_title", trim(n.URLTitle, 100))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	// Pushover returns JSON with "errors":[...] on validation failures.
	var p struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
		Info   string   `json:"info"`
	}
	_ = json.Unmarshal(body, &p)
	if len(p.Errors) > 0 {
		return fmt.Errorf("pushover: HTTP %d: %s", resp.StatusCode, strings.Join(p.Errors, "; "))
	}
	return fmt.Errorf("pushover: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
