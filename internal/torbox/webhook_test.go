package torbox

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"
)

func sign(secret, id, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(id))
	m.Write([]byte("."))
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func TestVerifyWebhookHappyPath(t *testing.T) {
	secret := "shh"
	id := "msg_abc"
	ts := fmt.Sprintf("%d", time.Now().Unix())
	body := []byte(`{"event":"download.ready","data":{"title":"x"}}`)
	sig := sign(secret, id, ts, body)
	if err := VerifyWebhook(secret, id, ts, sig, body, 5*time.Minute, time.Now()); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestVerifyWebhookMultipleSignaturesAcceptsMatching(t *testing.T) {
	secret := "shh"
	id := "msg_x"
	ts := fmt.Sprintf("%d", time.Now().Unix())
	body := []byte(`{}`)
	good := sign(secret, id, ts, body)
	bad := "v1," + base64.StdEncoding.EncodeToString([]byte("garbage"))
	header := bad + " " + good
	if err := VerifyWebhook(secret, id, ts, header, body, 5*time.Minute, time.Now()); err != nil {
		t.Errorf("expected accept, got %v", err)
	}
}

func TestVerifyWebhookRejectsTamperedBody(t *testing.T) {
	secret := "shh"
	id := "id1"
	ts := fmt.Sprintf("%d", time.Now().Unix())
	body := []byte(`{"foo":1}`)
	sig := sign(secret, id, ts, body)
	tampered := []byte(`{"foo":2}`)
	err := VerifyWebhook(secret, id, ts, sig, tampered, 5*time.Minute, time.Now())
	if !errors.Is(err, ErrSignatureMismatch) {
		t.Errorf("err=%v want ErrSignatureMismatch", err)
	}
}

func TestVerifyWebhookRejectsStaleTimestamp(t *testing.T) {
	secret := "shh"
	id := "id"
	body := []byte(`{}`)
	old := time.Now().Add(-1 * time.Hour)
	ts := fmt.Sprintf("%d", old.Unix())
	sig := sign(secret, id, ts, body)
	err := VerifyWebhook(secret, id, ts, sig, body, 5*time.Minute, time.Now())
	if !errors.Is(err, ErrTimestampStale) {
		t.Errorf("err=%v want ErrTimestampStale", err)
	}
}

func TestVerifyWebhookRejectsFuturedTimestamp(t *testing.T) {
	// Clock skew protection works in both directions.
	secret := "shh"
	id := "id"
	body := []byte(`{}`)
	future := time.Now().Add(1 * time.Hour)
	ts := fmt.Sprintf("%d", future.Unix())
	sig := sign(secret, id, ts, body)
	err := VerifyWebhook(secret, id, ts, sig, body, 5*time.Minute, time.Now())
	if !errors.Is(err, ErrTimestampStale) {
		t.Errorf("err=%v want ErrTimestampStale", err)
	}
}

func TestVerifyWebhookEmptySecretRejects(t *testing.T) {
	err := VerifyWebhook("", "id", "0", "v1,xxxx", []byte{}, 5*time.Minute, time.Now())
	if err == nil {
		t.Error("expected error on empty secret")
	}
}

func TestVerifyWebhookMissingHeaderRejects(t *testing.T) {
	err := VerifyWebhook("shh", "id", "0", "", []byte{}, 5*time.Minute, time.Now())
	if !errors.Is(err, ErrSignatureMissing) {
		t.Errorf("err=%v want ErrSignatureMissing", err)
	}
}

func TestVerifyWebhookIgnoresUnknownVersionPrefix(t *testing.T) {
	// Per spec, unknown prefixes (v2, v0, etc.) should be ignored, not error.
	secret := "shh"
	id := "id"
	ts := fmt.Sprintf("%d", time.Now().Unix())
	body := []byte(`{}`)
	good := sign(secret, id, ts, body)
	header := "v2,abc def " + good
	if err := VerifyWebhook(secret, id, ts, header, body, 5*time.Minute, time.Now()); err != nil {
		t.Errorf("should accept when at least one v1 entry matches; got %v", err)
	}
}

func TestVerifyWebhookZeroReplayWindowDisablesCheck(t *testing.T) {
	// replayWindow=0 should accept any timestamp (caller-opt-out).
	secret := "shh"
	id := "id"
	body := []byte(`{}`)
	old := time.Now().Add(-24 * time.Hour)
	ts := fmt.Sprintf("%d", old.Unix())
	sig := sign(secret, id, ts, body)
	if err := VerifyWebhook(secret, id, ts, sig, body, 0, time.Now()); err != nil {
		t.Errorf("expected accept with replayWindow=0, got %v", err)
	}
}
