// Package events emits an outbound webhook on job state transitions so an
// external tracker (Journarr) can mirror the TorBox substages. It is
// fire-and-forget and fully decoupled from the state machine: a delivery
// failure never blocks or fails a transition — the tracker's own pollers
// reconcile any gaps.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pburkhalter/arrarr/internal/job"
)

// GetJobFunc loads the current job row for enrichment (store.Get).
type GetJobFunc func(ctx context.Context, nzoID string) (*job.Job, error)

type transition struct {
	nzoID    string
	from, to string
}

type Emitter struct {
	url    string
	token  string
	getJob GetJobFunc
	log    *slog.Logger
	http   *http.Client
	ch     chan transition
}

type payload struct {
	Event           string `json:"event"`
	EventID         string `json:"event_id"`
	NzoID           string `json:"nzo_id"`
	NzbSHA256       string `json:"nzb_sha256,omitempty"`
	Source          string `json:"source,omitempty"`
	From            string `json:"from"`
	To              string `json:"to"`
	Filename        string `json:"filename,omitempty"`
	SizeBytes       int64  `json:"size_bytes,omitempty"`
	BytesDownloaded int64  `json:"bytes_downloaded,omitempty"`
	BytesTotal      int64  `json:"bytes_total,omitempty"`
	LocalPath       string `json:"local_path,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	TS              string `json:"ts"`
}

func New(url, token string, timeout time.Duration, getJob GetJobFunc, log *slog.Logger) *Emitter {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Emitter{
		url:    url,
		token:  token,
		getJob: getJob,
		log:    log,
		http:   &http.Client{Timeout: timeout},
		ch:     make(chan transition, 256),
	}
}

// Run drains the queue until ctx is done. Start it once as a goroutine.
func (e *Emitter) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-e.ch:
			e.deliver(t)
		}
	}
}

// Enqueue is the store transition hook. Non-blocking: if the buffer is full
// (tracker down / slow), the oldest pending event is dropped with a warning
// rather than stalling the worker. The tracker reconciles via its pollers.
func (e *Emitter) Enqueue(nzoID, from, to string) {
	t := transition{nzoID: nzoID, from: from, to: to}
	select {
	case e.ch <- t:
	default:
		select {
		case <-e.ch:
			e.log.Warn("events: buffer full, dropped oldest transition")
		default:
		}
		select {
		case e.ch <- t:
		default:
		}
	}
}

func (e *Emitter) deliver(t transition) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := payload{
		Event:   "job.transition",
		EventID: uuid.NewString(),
		NzoID:   t.nzoID,
		From:    t.from,
		To:      t.to,
		TS:      time.Now().UTC().Format(time.RFC3339),
	}
	// Enrich from the current job row (best-effort).
	if j, err := e.getJob(ctx, t.nzoID); err == nil && j != nil {
		p.NzbSHA256 = j.NzbSHA256
		p.Source = j.Source
		p.Filename = j.Filename
		if j.SizeBytes.Valid {
			p.SizeBytes = j.SizeBytes.Int64
		}
		p.BytesDownloaded = j.BytesDownloaded
		p.BytesTotal = j.BytesTotal
		if j.LocalPath.Valid {
			p.LocalPath = j.LocalPath.String
		}
		if j.LastError.Valid {
			p.LastError = j.LastError.String
		}
	}

	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	// 3 attempts with backoff; delivery failure is non-fatal.
	backoff := []time.Duration{0, 1 * time.Second, 5 * time.Second}
	for i, wait := range backoff {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		if e.post(ctx, body) {
			return
		}
		if i == len(backoff)-1 {
			e.log.Warn("events: delivery failed after retries", "nzo_id", t.nzoID, "to", t.to)
		}
	}
}

func (e *Emitter) post(ctx context.Context, body []byte) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if e.token != "" {
		req.Header.Set("X-Webhook-Token", e.token)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}
