package torbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateUsenetMIMEandAuth(t *testing.T) {
	var captured struct {
		auth        string
		contentType string
		fileCT      string
		fileBody    string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.auth = r.Header.Get("Authorization")
		captured.contentType = r.Header.Get("Content-Type")
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.FormName() == "file" {
				captured.fileCT = p.Header.Get("Content-Type")
				body, _ := io.ReadAll(p)
				captured.fileBody = string(body)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"detail":"ok","data":{"queue_id":4242}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "supersecret", 1000, 5*time.Second)
	resp, err := c.CreateUsenetDownload(context.Background(), "Evil.S03E01.nzb", []byte("<nzb>"), "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.QueueID != 4242 {
		t.Errorf("queue_id=%d want 4242", resp.QueueID)
	}
	if captured.auth != "Bearer supersecret" {
		t.Errorf("auth=%q", captured.auth)
	}
	if !strings.HasPrefix(captured.contentType, "multipart/form-data") {
		t.Errorf("content-type=%q", captured.contentType)
	}
	// THE bug we have to avoid: anything other than plain "application/x-nzb"
	// (TorBox rejects ;charset=utf-8).
	if captured.fileCT != "application/x-nzb" {
		t.Errorf("file Content-Type=%q want exactly 'application/x-nzb'", captured.fileCT)
	}
	if captured.fileBody != "<nzb>" {
		t.Errorf("file body=%q", captured.fileBody)
	}
}

func TestMyListDualIDSwap(t *testing.T) {
	// First call: queue_id present, no id yet.
	// Second call: id assigned (active), queue_id may be 0.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			_, _ = io.WriteString(w, `{"success":true,"data":[{"queue_id":11,"download_state":"queued","name":"Evil"}]}`)
		default:
			_, _ = io.WriteString(w, `{"success":true,"data":[{"id":99,"queue_id":11,"download_state":"downloading","name":"Evil","progress":0.5}]}`)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", 1000, 5*time.Second)
	got, err := c.MyList(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].QueueID != 11 || got[0].ID != 0 {
		t.Errorf("first call wrong: %+v", got)
	}
	got, err = c.MyList(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != 99 || got[0].QueueID != 11 {
		t.Errorf("second call wrong: %+v", got)
	}
	if !strings.EqualFold(got[0].DownloadState, "downloading") {
		t.Errorf("state=%q", got[0].DownloadState)
	}
}

func TestRateLimit429SurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"success":false,"error":"rate_limited","detail":"too fast"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", 1000, 5*time.Second)
	_, err := c.MyList(context.Background(), false)
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T", err)
	}
	if apiErr.Status != 429 {
		t.Errorf("status=%d want 429", apiErr.Status)
	}
	if apiErr.RetryAfter != 5*time.Second {
		t.Errorf("retry-after=%v want 5s", apiErr.RetryAfter)
	}
	if !apiErr.Retryable() {
		t.Errorf("429 should be retryable")
	}
}

func TestEnvelopeDecode(t *testing.T) {
	var raw = `{"success":true,"detail":"ok","data":{"queue_id":7,"id":42}}`
	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	if !env.Success {
		t.Error("success false")
	}
	var c CreateResp
	if err := json.Unmarshal(env.Data, &c); err != nil {
		t.Fatal(err)
	}
	if c.ID != 42 || c.QueueID != 7 {
		t.Errorf("decoded=%+v", c)
	}
}

// TorBox sometimes returns auth_id and hash as strings; older versions of
// CreateResp typed auth_id as int64 and broke decoding for every grab.
func TestCreateRespToleratesUnusedStringFields(t *testing.T) {
	raw := `{"queue_id":42,"id":99,"hash":"abc123","auth_id":"some-string-value"}`
	var c CreateResp
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("decode should not fail on unknown-type fields: %v", err)
	}
	if c.QueueID != 42 || c.ID != 99 {
		t.Errorf("got %+v", c)
	}
}
