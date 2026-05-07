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

func TestEditUsenetSendsJSONBody(t *testing.T) {
	var captured struct {
		method string
		auth   string
		ct     string
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.auth = r.Header.Get("Authorization")
		captured.ct = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"detail":"ok"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", 1000, 5*time.Second)
	name := "abc__release"
	err := c.EditUsenet(context.Background(), 7, EditUsenetParams{
		Name: &name,
		Tags: []string{"arrarr", "host:patrik", "job:xxxxxxxx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.method != http.MethodPut {
		t.Errorf("method=%q want PUT", captured.method)
	}
	if captured.auth != "Bearer k" {
		t.Errorf("auth=%q", captured.auth)
	}
	if captured.ct != "application/json" {
		t.Errorf("ct=%q", captured.ct)
	}
	if got := captured.body["usenet_download_id"]; got != float64(7) {
		t.Errorf("usenet_download_id=%v", got)
	}
	if got := captured.body["name"]; got != "abc__release" {
		t.Errorf("name=%v", got)
	}
	tags, ok := captured.body["tags"].([]any)
	if !ok || len(tags) != 3 {
		t.Fatalf("tags=%v", captured.body["tags"])
	}
	if tags[0] != "arrarr" || tags[2] != "job:xxxxxxxx" {
		t.Errorf("tags content wrong: %v", tags)
	}
	// alternative_hashes was unset; must be absent (not null) so we don't clobber
	if _, present := captured.body["alternative_hashes"]; present {
		t.Errorf("alternative_hashes should be omitted when unset, got: %v", captured.body["alternative_hashes"])
	}
}

func TestEditUsenetOmitsUnsetFields(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", 1000, 5*time.Second)
	if err := c.EditUsenet(context.Background(), 11, EditUsenetParams{Tags: []string{"only-tags"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := captured["name"]; ok {
		t.Errorf("name must be absent when unset; body=%v", captured)
	}
	if _, ok := captured["alternative_hashes"]; ok {
		t.Errorf("alternative_hashes must be absent when unset; body=%v", captured)
	}
	if got := captured["tags"]; got == nil {
		t.Errorf("tags must be present; body=%v", captured)
	}
}

func TestRequestUsenetDLReturnsURL(t *testing.T) {
	var captured struct {
		path     string
		token    string
		usenetID string
		fileID   string
		zipLink  string
		redirect string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		q := r.URL.Query()
		captured.token = q.Get("token")
		captured.usenetID = q.Get("usenet_id")
		captured.fileID = q.Get("file_id")
		captured.zipLink = q.Get("zip_link")
		captured.redirect = q.Get("redirect")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"detail":"ok","data":"https://cdn.torbox.app/abc/file.mkv?sig=xyz"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok", 1000, 5*time.Second)
	got, err := c.RequestUsenetDL(context.Background(), 42, 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://cdn.torbox.app/abc/file.mkv?sig=xyz" {
		t.Errorf("url=%q", got)
	}
	if captured.path != "/usenet/requestdl" {
		t.Errorf("path=%q", captured.path)
	}
	if captured.token != "tok" {
		t.Errorf("token=%q want %q (must be in query, not just header)", captured.token, "tok")
	}
	if captured.usenetID != "42" || captured.fileID != "7" {
		t.Errorf("ids: usenet=%q file=%q", captured.usenetID, captured.fileID)
	}
	if captured.zipLink != "" {
		t.Errorf("zip_link=%q want empty (default false omitted)", captured.zipLink)
	}
	if captured.redirect != "false" {
		t.Errorf("redirect=%q want false (we want JSON not 302)", captured.redirect)
	}
}

func TestTestNotificationFiresPOST(t *testing.T) {
	var captured struct {
		method string
		path   string
		auth   string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"success":true,"detail":"sent"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", 1000, 5*time.Second)
	if err := c.TestNotification(context.Background()); err != nil {
		t.Fatal(err)
	}
	if captured.method != http.MethodPost {
		t.Errorf("method=%q want POST", captured.method)
	}
	if captured.path != "/notifications/test" {
		t.Errorf("path=%q want /notifications/test", captured.path)
	}
	if captured.auth != "Bearer k" {
		t.Errorf("auth=%q", captured.auth)
	}
}

func TestRequestUsenetDLZipLinkSetsParam(t *testing.T) {
	var zipLink string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zipLink = r.URL.Query().Get("zip_link")
		_, _ = io.WriteString(w, `{"success":true,"data":"u"}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", 1000, 5*time.Second)
	_, _ = c.RequestUsenetDL(context.Background(), 1, 0, true)
	if zipLink != "true" {
		t.Errorf("zip_link=%q want true", zipLink)
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
