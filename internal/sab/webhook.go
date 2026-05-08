package sab

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/pushover"
	"github.com/pburkhalter/arrarr/internal/store"
	"github.com/pburkhalter/arrarr/internal/torbox"
)

// Webhook headers per Standard Webhooks.
const (
	hdrWebhookID        = "Webhook-Id"
	hdrWebhookTimestamp = "Webhook-Timestamp"
	hdrWebhookSignature = "Webhook-Signature"
)

// maxWebhookBody caps the JSON we'll read from a webhook request. Standard
// Webhooks payloads are small (KB at most); anything larger is suspicious.
const maxWebhookBody = 16 << 10 // 16 KiB

// WebhookOptions wires the optional dependencies the receiver needs.
type WebhookOptions struct {
	// Secret is the HMAC key TorBox uses to sign webhook deliveries. Empty
	// disables the receiver — the handler returns 503 to make the misconfig
	// loud, rather than silently accepting unsigned payloads.
	//
	// NOTE: TorBox does not currently expose a webhook signing secret in its
	// UI or API, despite their docs claiming Standard Webhooks compliance.
	// Real deliveries arrive without webhook-id / webhook-signature headers.
	// Set RequireSignature=false to accept these, paying the cost in security
	// (compensating with CF rate limits / WAF rules at the edge).
	Secret string
	// RequireSignature gates signature verification. Default true (strict).
	// Set false to accept unsigned deliveries (e.g. TorBox today). The
	// handler still does ownership lookup and origin gating; only the HMAC
	// check is skipped.
	RequireSignature bool
	// ReplayWindow caps how far in the past/future a signed timestamp can be.
	// Zero disables the check.
	ReplayWindow time.Duration
	// Pushover is optional; nil means notifications are disabled at this
	// instance. The handler still processes ownership lookups.
	Pushover *pushover.Client
	// PushoverNotifyOn controls which events fire Pushover: "off", "ready",
	// "failed", or "both".
	PushoverNotifyOn string
	// AppURL, if set, is included as the click target on Pushover messages
	// (typically points at the Jellyfin URL or a status dashboard).
	AppURL string
}

// torBoxWebhookEnvelope is the documented payload shape (Standard Webhooks).
//
// `data` fields aren't fully specified in the OpenAPI; we accept several keys
// that have been seen in practice and degrade gracefully when they're absent.
type torBoxWebhookEnvelope struct {
	Event     string          `json:"event"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type webhookData struct {
	Title   string `json:"title"`
	Message string `json:"message"`

	// Best-guess structured fields the payload may contain. None are
	// guaranteed; they're tried in order before the message-parse fallback.
	ID            int64  `json:"id"`
	UsenetID      int64  `json:"usenet_id"`
	TorrentID     int64  `json:"torrent_id"`
	QueueID       int64  `json:"queue_id"`
	DownloadID    int64  `json:"download_id"`
	Name          string `json:"name"`
	OriginalName  string `json:"original_name"`
	DownloadName  string `json:"download_name"`
	DownloadState string `json:"download_state"`
	Hash          string `json:"hash"`
}

// nzoIDFromMessage matches an arrarr-prefixed nzo_id embedded in free text.
// Format: "arrarr_" + 32 hex chars. We use it as a fallback when the data
// object lacks a structured id.
var nzoIDFromMessage = regexp.MustCompile(`(arrarr_[a-f0-9]{32})`)

// handleTorBoxWebhook is the HTTP handler. Always returns 204 on the success
// path or for "not ours" rows so TorBox doesn't retry. Returns 4xx only for
// signature/parse failures, and 503 when the receiver is disabled.
func (s *Server) handleTorBoxWebhook(w http.ResponseWriter, r *http.Request) {
	if s.webhook == nil || s.webhook.Secret == "" {
		// Receiver disabled — reject loudly so misconfigured callers notice.
		http.Error(w, "webhook receiver disabled", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		http.Error(w, "body read", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxWebhookBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	id := r.Header.Get(hdrWebhookID)
	ts := r.Header.Get(hdrWebhookTimestamp)
	sig := r.Header.Get(hdrWebhookSignature)
	if s.webhook.RequireSignature {
		if err := torbox.VerifyWebhook(s.webhook.Secret, id, ts, sig, body, s.webhook.ReplayWindow, time.Now()); err != nil {
			s.logger.Warn("webhook: signature verify failed",
				"err", err, "remote", r.RemoteAddr,
				"ua", r.Header.Get("User-Agent"),
				"content_type", r.Header.Get("Content-Type"),
				"body_len", len(body))
			http.Error(w, "signature invalid", http.StatusUnauthorized)
			return
		}
	} else {
		// Insecure mode (TorBox today): accept unsigned. Log enough to detect
		// abuse if we ever flip this back on, and to help characterize what
		// the legitimate sender's traffic looks like for future hardening.
		s.logger.Info("webhook: accepting unsigned (RequireSignature=false)",
			"remote", r.RemoteAddr,
			"ua", r.Header.Get("User-Agent"),
			"content_type", r.Header.Get("Content-Type"),
			"body_len", len(body),
			"have_id", id != "",
			"have_sig", sig != "")
	}

	var env torBoxWebhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		s.logger.Warn("webhook: bad envelope", "err", err)
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	var data webhookData
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data) // tolerate sparse payloads
	}

	// Find the matching job using the lookup chain. A miss = "not ours" =
	// 204, no retry, no notification.
	matched, lookupVia := s.lookupOwnedJob(r.Context(), &data)
	if matched == nil {
		s.logger.Info("webhook: not ours, dropping",
			"event", env.Event, "remote", r.RemoteAddr,
			"data_id", firstNonZero(data.ID, data.UsenetID, data.QueueID, data.DownloadID, data.TorrentID),
			"name", firstNonEmpty(data.Name, data.OriginalName, data.DownloadName))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.logger.Info("webhook: matched",
		"event", env.Event, "nzo_id", matched.NzoID,
		"via", lookupVia, "state", matched.State)

	// Wake the dispatcher so the poller picks up state changes immediately
	// instead of waiting for its tick.
	s.signalWake()

	// Pushover gating: origin='self' AND notify-on matches AND not already sent.
	go s.maybeFirePushover(env.Event, matched)

	w.WriteHeader(http.StatusNoContent)
}

// lookupOwnedJob walks the lookup chain trying to find an Arrarr job that
// owns this webhook event. Returns the job and a short tag describing which
// match path succeeded (for logs).
func (s *Server) lookupOwnedJob(ctx context.Context, d *webhookData) (*job.Job, string) {
	// 1. Structured id from the payload — most reliable when present.
	for _, id := range []int64{d.UsenetID, d.ID, d.QueueID, d.DownloadID, d.TorrentID} {
		if id == 0 {
			continue
		}
		if j, err := s.store.FindByTorboxID(ctx, id); err == nil && j != nil {
			return j, "torbox_id"
		}
	}
	// 2. Folder/name match — TorBox sometimes echoes the original name.
	for _, name := range []string{d.Name, d.DownloadName, d.OriginalName} {
		if name == "" {
			continue
		}
		if j, err := s.store.FindByFolderName(ctx, name); err == nil && j != nil {
			return j, "folder_name"
		}
	}
	// 3. nzo_id embedded in the message (paranoia fallback — most TorBox
	// payloads don't contain it, but we may set it via `name` later).
	if d.Message != "" {
		if m := nzoIDFromMessage.FindString(d.Message); m != "" {
			if j, err := s.store.Get(ctx, m); err == nil && j != nil {
				return j, "nzo_id_in_message"
			}
		}
	}
	return nil, ""
}

// maybeFirePushover sends a Pushover notification iff:
//   - Pushover is configured
//   - Notify-on matches the event
//   - Job's origin = 'self' (we don't notify on mirror rows)
//   - pushover_sent_at is still NULL (idempotency on duplicate webhook delivery)
//
// To dedup correctly under concurrent webhook deliveries, we *claim* the
// notification slot via the atomic MarkPushoverSent UPDATE before calling
// Pushover. Whoever wins the UPDATE gets to send. ErrNotFound from the
// UPDATE means another concurrent handler already claimed it — we drop.
func (s *Server) maybeFirePushover(event string, j *job.Job) {
	if s.webhook == nil || s.webhook.Pushover == nil {
		return
	}
	if !s.eventMatchesNotify(event) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	origin, err := s.store.JobOrigin(ctx, j.NzoID)
	if err != nil || origin != "self" {
		return
	}
	// Claim the slot atomically. If we lose the race, drop quietly.
	if err := s.store.MarkPushoverSent(ctx, j.NzoID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return // another handler already won the race
		}
		s.logger.Warn("webhook: claim pushover slot failed", "nzo_id", j.NzoID, "err", err)
		return
	}

	title, message, prio := buildPushoverMessage(event, j)
	if err := s.webhook.Pushover.Notify(ctx, pushover.Notification{
		Title:    title,
		Message:  message,
		Priority: prio,
		URL:      s.webhook.AppURL,
	}); err != nil {
		s.logger.Warn("webhook: pushover send failed", "nzo_id", j.NzoID, "err", err)
		// We chose claim-then-send for dedup safety. A failed send means the
		// user loses this single notification — better than risking double
		// notifications on duplicate webhook delivery.
	}
}

func (s *Server) eventMatchesNotify(event string) bool {
	if s.webhook == nil {
		return false
	}
	switch s.webhook.PushoverNotifyOn {
	case "ready":
		return strings.HasSuffix(event, ".ready") || strings.HasSuffix(event, ".completed") || event == "download.ready"
	case "failed":
		return strings.HasSuffix(event, ".failed") || strings.HasSuffix(event, ".error")
	case "both":
		return strings.HasSuffix(event, ".ready") || strings.HasSuffix(event, ".completed") ||
			strings.HasSuffix(event, ".failed") || strings.HasSuffix(event, ".error")
	}
	return false
}

func buildPushoverMessage(event string, j *job.Job) (title, message string, priority int) {
	folder := ""
	if j.TorboxFolderName.Valid {
		folder = j.TorboxFolderName.String
	}
	if folder == "" {
		folder = j.Filename
	}
	switch {
	case strings.HasSuffix(event, ".failed") || strings.HasSuffix(event, ".error"):
		return "Arrarr — download failed", folder, 1
	default:
		return "Arrarr — ready", folder, 0
	}
}

func firstNonZero(vs ...int64) int64 {
	for _, v := range vs {
		if v != 0 {
			return v
		}
	}
	return 0
}
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
