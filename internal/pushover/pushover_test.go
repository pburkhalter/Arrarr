package pushover

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNotifySendsExpectedForm(t *testing.T) {
	var captured struct {
		method      string
		contentType string
		token       string
		user        string
		title       string
		message     string
		priority    string
		sound       string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.contentType = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		captured.token = r.PostForm.Get("token")
		captured.user = r.PostForm.Get("user")
		captured.title = r.PostForm.Get("title")
		captured.message = r.PostForm.Get("message")
		captured.priority = r.PostForm.Get("priority")
		captured.sound = r.PostForm.Get("sound")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer srv.Close()

	c := New("tok", "usr", time.Second)
	c.HTTP = srv.Client() // route to test server
	// Override apiURL by routing through the request constructor — easiest is
	// to swap c.HTTP.Transport's base. Here we just rebuild a client that hits
	// the test server directly via DoRequest pattern: we substitute the URL by
	// constructing the request ourselves. But since pushover.go has the URL
	// hardcoded, the cleanest way is to route via the transport. Use a
	// rewriting RoundTripper.
	c.HTTP.Transport = rewrite(srv.URL)

	if err := c.Notify(context.Background(), Notification{
		Title:    "All set",
		Message:  "Twisted Metal S02E01 is ready",
		Priority: 1,
		Sound:    "magic",
	}); err != nil {
		t.Fatal(err)
	}
	if captured.method != http.MethodPost {
		t.Errorf("method=%q", captured.method)
	}
	if captured.contentType != "application/x-www-form-urlencoded" {
		t.Errorf("content-type=%q", captured.contentType)
	}
	if captured.token != "tok" || captured.user != "usr" {
		t.Errorf("token/user mismatch: %s / %s", captured.token, captured.user)
	}
	if captured.title != "All set" {
		t.Errorf("title=%q", captured.title)
	}
	if captured.message != "Twisted Metal S02E01 is ready" {
		t.Errorf("message=%q", captured.message)
	}
	if captured.priority != "1" {
		t.Errorf("priority=%q want 1", captured.priority)
	}
	if captured.sound != "magic" {
		t.Errorf("sound=%q", captured.sound)
	}
}

func TestNotifyMissingTokenReturnsError(t *testing.T) {
	c := New("", "u", time.Second)
	if err := c.Notify(context.Background(), Notification{Message: "x"}); err == nil {
		t.Error("expected error on empty token")
	}
}

func TestNotifyMissingUserReturnsError(t *testing.T) {
	c := New("t", "", time.Second)
	if err := c.Notify(context.Background(), Notification{Message: "x"}); err == nil {
		t.Error("expected error on empty user")
	}
}

func TestNotifyTrimsLongFields(t *testing.T) {
	var capturedTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		capturedTitle = r.PostForm.Get("title")
		_, _ = io.WriteString(w, `{"status":1}`)
	}))
	defer srv.Close()

	c := New("t", "u", time.Second)
	c.HTTP.Transport = rewrite(srv.URL)
	long := strings.Repeat("x", 500)
	if err := c.Notify(context.Background(), Notification{Title: long, Message: "m"}); err != nil {
		t.Fatal(err)
	}
	if len(capturedTitle) != 250 {
		t.Errorf("title len=%d want 250 (trimmed)", len(capturedTitle))
	}
}

func TestNotifySurfacesPushoverErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"status":0,"errors":["application token is invalid"]}`)
	}))
	defer srv.Close()

	c := New("bad", "u", time.Second)
	c.HTTP.Transport = rewrite(srv.URL)
	err := c.Notify(context.Background(), Notification{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "application token is invalid") {
		t.Errorf("err=%v should surface pushover error message", err)
	}
}

// rewrite is a tiny RoundTripper that replaces the request URL host/scheme
// with the test server's, so we can hit our own test server while leaving the
// production URL hardcoded in the client.
type rewriteTransport struct {
	target string
}

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target := r.target
	// Rebuild the URL with target scheme+host.
	u, err := req.URL.Parse(target)
	if err != nil {
		return nil, err
	}
	u.Path = req.URL.Path
	u.RawQuery = req.URL.RawQuery
	req.URL = u
	req.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func rewrite(target string) http.RoundTripper {
	return &rewriteTransport{target: target}
}
