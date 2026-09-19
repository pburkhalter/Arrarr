package torbox

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

	"golang.org/x/time/rate"

	"github.com/pburkhalter/arrarr/internal/httpx"
)

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client

	// createLimiter is a separate, much stricter bucket for the
	// createusenetdownload/createtorrent endpoints. TorBox enforces a 60-per-
	// hour ceiling there (returns 429 with "60 per 1 hour") independent of the
	// general API rate limit. Without a dedicated bucket we burst dozens of
	// NZBs from Sonarr's MissingEpisodeSearch into the same hour, hit 429s,
	// and the submitter has to back off 5+ min per job — slow recovery and
	// noisy logs. With the bucket the submitter naturally paces itself.
	createLimiter *rate.Limiter
}

func NewClient(baseURL, apiKey string, perMin float64, createPerHour float64, timeout time.Duration) *Client {
	if timeout <= 0 {
		// 90s — TorBox's createusenetdownload regularly takes 20-60s under load,
		// and a too-short timeout causes the response to be lost while the upload
		// still completes server-side, leaving arrarr unable to track the job.
		timeout = 90 * time.Second
	}
	transport := httpx.NewRateLimitTransport(http.DefaultTransport, perMin, 10)
	if createPerHour <= 0 {
		// TorBox's documented createusenetdownload limit is 60/hour. Default a
		// touch under so a slow clock or shared-account scenario still has
		// headroom before the server-side 429 fires.
		createPerHour = 55
	}
	return &Client{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		APIKey:        apiKey,
		HTTP:          &http.Client{Transport: transport, Timeout: timeout},
		createLimiter: rate.NewLimiter(rate.Limit(createPerHour/3600.0), int(createPerHour)),
	}
}

type APIError struct {
	Status     int
	Code       string
	Detail     string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("torbox %d: %s (%s)", e.Status, e.Code, e.Detail)
	}
	return fmt.Sprintf("torbox %d: %s", e.Status, e.Detail)
}

func (e *APIError) Retryable() bool {
	if e.Status == 429 || e.Status >= 500 {
		return true
	}
	return false
}

func (c *Client) CreateUsenetDownload(ctx context.Context, filename string, nzb []byte, password string) (*CreateResp, error) {
	if err := c.waitCreate(ctx); err != nil {
		return nil, err
	}
	body, contentType, err := buildNZBMultipart(filename, nzb, password)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/usenet/createusenetdownload", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	c.auth(req)

	var out CreateResp
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// waitCreate blocks until the create-endpoint token bucket has capacity, or
// the context is cancelled. Returns the context error so callers can stop
// shutting down gracefully. nil createLimiter means "no per-endpoint limit"
// (test fakes leave it unset).
func (c *Client) waitCreate(ctx context.Context) error {
	if c.createLimiter == nil {
		return nil
	}
	return c.createLimiter.Wait(ctx)
}

// CreateHeadroom reports the createusenetdownload token bucket's currently
// available tokens and its burst ceiling, for status/monitoring surfaces.
// avail is -1 when no limiter is configured (test fakes). It is a float
// because tokens refill continuously; callers rendering a count should floor it.
func (c *Client) CreateHeadroom() (avail float64, burst int) {
	if c.createLimiter == nil {
		return -1, 0
	}
	return c.createLimiter.Tokens(), c.createLimiter.Burst()
}

func (c *Client) MyList(ctx context.Context, bypassCache bool) ([]MyListItem, error) {
	q := url.Values{}
	if bypassCache {
		q.Set("bypass_cache", "true")
	}
	u := c.BaseURL + "/usenet/mylist"
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	var out []MyListItem
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EditUsenetParams is the request body for EditUsenet. Pointer + omitempty so
// we only overwrite fields we explicitly set — TorBox's edit endpoint replaces
// pre-existing data for whatever fields are present in the body.
type EditUsenetParams struct {
	Name              *string  `json:"name,omitempty"`
	Tags              []string `json:"tags,omitempty"`
	AlternativeHashes []string `json:"alternative_hashes,omitempty"`
}

// EditUsenet sets metadata on an existing usenet download. The download must
// already be "cached" (TorBox-internal: past initial processing). Calling this
// immediately after CreateUsenetDownload may fail with 4xx until the row is
// cached; the caller should treat such failures as soft (retry later) rather
// than failing the whole job.
func (c *Client) EditUsenet(ctx context.Context, id int64, p EditUsenetParams) error {
	type editBody struct {
		UsenetDownloadID int64 `json:"usenet_download_id"`
		EditUsenetParams
	}
	raw, err := json.Marshal(editBody{UsenetDownloadID: id, EditUsenetParams: p})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.BaseURL+"/usenet/editusenetdownload", strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	return c.do(req, nil)
}

// RequestUsenetDL returns a CDN download URL for a single file inside a usenet
// download. URL is presigned and time-limited (typically ~6 hours). Callers
// should refresh STRMs well before that window. fileID 0 returns a zip-link.
func (c *Client) RequestUsenetDL(ctx context.Context, usenetID, fileID int64, zipLink bool) (string, error) {
	q := url.Values{}
	q.Set("token", c.APIKey)
	q.Set("usenet_id", fmt.Sprintf("%d", usenetID))
	if fileID > 0 {
		q.Set("file_id", fmt.Sprintf("%d", fileID))
	}
	if zipLink {
		q.Set("zip_link", "true")
	}
	q.Set("redirect", "false")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/usenet/requestdl?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	c.auth(req)

	var dlURL string
	if err := c.do(req, &dlURL); err != nil {
		return "", err
	}
	return dlURL, nil
}

// TestNotification asks TorBox to fire one notification through every
// configured channel (email, webhook, telegram, etc.). Useful for verifying a
// just-configured webhook URL+secret without waiting for a real download.
//
// Server-side rate limited to 1/min — running this repeatedly returns 429.
func (c *Client) TestNotification(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/notifications/test", nil)
	if err != nil {
		return err
	}
	c.auth(req)
	return c.do(req, nil)
}

func (c *Client) ControlUsenet(ctx context.Context, id int64, operation string) error {
	body := strings.NewReader(fmt.Sprintf(`{"usenet_id":%d,"operation":%q}`, id, operation))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/usenet/controlusenetdownload", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	return c.do(req, nil)
}

func (c *Client) auth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
}

// maxBodyBytes caps how much of a TorBox response we buffer. /usenet/mylist
// grows with account history — every completed, expired and failed download
// stays in the list with its full file array (~5 KB per entry) — so this has to
// clear the whole history, not just the active downloads. It crossed the
// previous 4 MiB cap in Aug 2026 at ~1000 entries, which silently truncated the
// body mid-JSON and killed the poller for six days.
const maxBodyBytes = 32 << 20

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Read one byte past the cap so a body that fills it exactly is
	// distinguishable from one that overflows it.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return err
	}
	// Fail loudly on truncation. Silently handing a clipped body to the JSON
	// decoder yields "unexpected end of JSON input", which reads like a TorBox
	// API fault rather than our own limit — exactly the misdirection that made
	// the Aug 2026 poller outage take days to pin down.
	if len(raw) > maxBodyBytes {
		return fmt.Errorf("torbox: response body exceeds %d MiB limit (url=%s) — raise maxBodyBytes",
			maxBodyBytes>>20, req.URL.Path)
	}
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		code, detail := errEnvelope(raw)
		return &APIError{
			Status:     resp.StatusCode,
			Code:       code,
			Detail:     detail,
			RetryAfter: httpx.RetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	if resp.StatusCode >= 400 {
		code, detail := errEnvelope(raw)
		return &APIError{Status: resp.StatusCode, Code: code, Detail: detail}
	}
	if out == nil {
		return nil
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode envelope: %w (body=%s)", err, truncate(string(raw), 200))
	}
	if !env.Success {
		return &APIError{Status: resp.StatusCode, Code: env.Error, Detail: env.Detail}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode data: %w", err)
	}
	return nil
}

// errEnvelope pulls the machine-readable code out of an error response. TorBox
// answers quota problems with HTTP 500 and a normal envelope
// ({"success":false,"error":"ACTIVE_LIMIT",…}), so without this the code is lost
// and callers are left substring-matching the raw body to tell a hard failure
// from backpressure. Falls back to the raw body for responses that carry no
// envelope (gateway errors and the like).
func errEnvelope(raw []byte) (code, detail string) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil && !env.Success && env.Error != "" {
		return env.Error, truncate(env.Detail, 200)
	}
	return "", truncate(string(raw), 200)
}

func IsRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	return err != nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
