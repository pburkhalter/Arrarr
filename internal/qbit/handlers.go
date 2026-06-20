package qbit

import (
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

// qBit ETA infinity sentinel — clients display this as "∞".
const etaInfinity int64 = 8640000

const nzoIDPrefix = "arrarr_"

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (s *Server) handleAppVersion(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, AppVersion)
}

func (s *Server) handleWebAPIVersion(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, WebAPIVersion)
}

func (s *Server) handlePreferences(w http.ResponseWriter, _ *http.Request) {
	p := Preferences{
		SavePath:           s.downloadDir,
		TempPath:           filepath.Join(s.downloadDir, ".incomplete"),
		TempPathEnabled:    false,
		DHT:                false, // We're a shim → no peer-network features.
		PeX:                false,
		LSD:                false,
		Encryption:         0,
		AnonymousMode:      false,
		QueueingEnabled:    true,
		MaxActiveDownloads: 5,
		MaxActiveTorrents:  5,
		MaxActiveUploads:   5,
		ListenPort:         6881,
		WebUIUsername:      s.username,
		WebUIPort:          8080,
		AutoTMMEnabled:     false,
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleTorrentsInfo(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	jobs, err := s.store.ListByCategory(r.Context(), category)
	if err != nil {
		s.logger.Error("qbit list torrents", "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	// Sonarr/Radarr also accept ?hashes=h1|h2 — filter post-hoc so we don't
	// need a second store query.
	if hs := r.URL.Query().Get("hashes"); hs != "" {
		want := map[string]bool{}
		for _, h := range strings.Split(hs, "|") {
			want[strings.ToLower(strings.TrimSpace(h))] = true
		}
		filtered := jobs[:0]
		for _, j := range jobs {
			if want[strings.ToLower(j.NzbSHA256)] {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}
	out := make([]TorrentInfo, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, s.torrentInfoFromJob(j))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) torrentInfoFromJob(j *job.Job) TorrentInfo {
	state, progress := mapState(j.State)
	savePath := s.savePathFor(j.Category)
	size := j.SizeBytes.Int64
	downloaded := int64(float64(size) * progress)

	info := TorrentInfo{
		Hash:        strings.ToLower(j.NzbSHA256),
		Name:        displayName(j),
		SavePath:    savePath,
		ContentPath: s.contentPathFor(j, savePath),
		Category:    j.Category,
		State:       state,
		Progress:    progress,
		Downloaded:  downloaded,
		TotalSize:   size,
		Size:        size,
		DlSpeed:     0,
		UpSpeed:     0,
		ETA:         etaInfinity,
		AddedOn:     j.CreatedAt.Unix(),
		Ratio:       0,
		Tags:        "",
		Tracker:     "",
		MagnetURI:   "",
		Priority:    1,
	}
	if j.CompletedAt.Valid {
		info.CompletionOn = j.CompletedAt.Time.Unix()
	}
	return info
}

// displayName prefers the librarian's path basename (which is the canonical
// release name once written), falls back to the torbox folder, then the raw
// filename Sonarr handed us at submit time.
func displayName(j *job.Job) string {
	if j.LibraryPath.Valid && j.LibraryPath.String != "" {
		return filepath.Base(j.LibraryPath.String)
	}
	if j.TorboxFolderName.Valid && j.TorboxFolderName.String != "" {
		return j.TorboxFolderName.String
	}
	return strings.TrimSuffix(j.Filename, filepath.Ext(j.Filename))
}

func (s *Server) savePathFor(category string) string {
	if category == "" {
		return s.downloadDir
	}
	return filepath.Join(s.downloadDir, category)
}

func (s *Server) contentPathFor(j *job.Job, savePath string) string {
	if j.LibraryPath.Valid && j.LibraryPath.String != "" {
		return j.LibraryPath.String
	}
	if j.TorboxFolderName.Valid && j.TorboxFolderName.String != "" {
		return filepath.Join(savePath, j.TorboxFolderName.String)
	}
	return filepath.Join(savePath, displayName(j))
}

// mapState collapses our 7-state machine into the qBit string vocabulary.
// Progress is a coarse approximation — we don't track byte-level download
// progress on the TorBox side (the webhook only fires on completion).
func mapState(s job.State) (string, float64) {
	switch s {
	case job.StateNew:
		return "queuedDL", 0
	case job.StateSubmitted:
		return "queuedDL", 0.01
	case job.StateDownloading:
		return "downloading", 0.5
	case job.StateCompletedTorbox:
		// TorBox has the file; we're now pulling/linking it locally. Sonarr
		// still wants to see "downloading" until content_path is final.
		return "downloading", 0.9
	case job.StateReady:
		// "completed" is the qBit terminal-success state; Sonarr treats it
		// as the import trigger.
		return "completed", 1.0
	case job.StateFailed:
		return "error", 0
	case job.StateCanceled:
		return "error", 0
	}
	return "unknown", 0
}

// handleTorrentsAdd parses the multipart form, extracts hashes from each
// .torrent or magnet, and inserts one job per torrent. Returns "Ok." on
// success even for partial failures — qBit's add endpoint is fire-and-forget
// from the client perspective. Concrete failures get logged.
func (s *Server) handleTorrentsAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(s.maxTorrentBytes + (1 << 20)); err != nil {
		// qBit also accepts plain x-www-form-urlencoded (magnet-only path).
		// Fall back to ParseForm so we don't reject those callers.
		if err2 := r.ParseForm(); err2 != nil {
			s.logger.Warn("qbit add parse failed", "err", err)
			writeText(w, http.StatusOK, "Fails.")
			return
		}
	}
	category := strings.ToLower(strings.TrimSpace(r.FormValue("category")))
	tags := r.FormValue("tags")
	// Treat presence of either field as enough to proceed.
	added := 0

	// Magnet / URL list (newline-separated).
	if urls := r.FormValue("urls"); urls != "" {
		for _, raw := range strings.Split(urls, "\n") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			if err := s.acceptMagnet(r.Context(), raw, category, tags); err != nil {
				s.logger.Warn("qbit add magnet failed", "err", err, "url", raw)
				continue
			}
			added++
		}
	}

	// .torrent file uploads (one or many under "torrents").
	if r.MultipartForm != nil && r.MultipartForm.File != nil {
		for _, fhs := range r.MultipartForm.File {
			for _, fh := range fhs {
				if fh.Size > s.maxTorrentBytes {
					s.logger.Warn("qbit add torrent oversize", "name", fh.Filename, "size", fh.Size)
					continue
				}
				f, err := fh.Open()
				if err != nil {
					s.logger.Warn("qbit open torrent", "err", err)
					continue
				}
				body, err := io.ReadAll(f)
				_ = f.Close()
				if err != nil {
					s.logger.Warn("qbit read torrent", "err", err)
					continue
				}
				if err := s.acceptTorrent(r.Context(), fh.Filename, body, category, tags); err != nil {
					s.logger.Warn("qbit add torrent failed", "err", err, "name", fh.Filename)
					continue
				}
				added++
			}
		}
	}

	if added == 0 {
		writeText(w, http.StatusOK, "Fails.")
		return
	}
	s.signalWake()
	writeText(w, http.StatusOK, "Ok.")
}

// acceptTorrent extracts the info-hash from the .torrent bytes, dedups, and
// inserts a job whose nzb_blob is the raw torrent (so the worker can hand it
// back to TorBox unchanged).
func (s *Server) acceptTorrent(ctx context.Context, filename string, body []byte, category, _ string) error {
	hash, name, err := extractInfoHash(body)
	if err != nil {
		return fmt.Errorf("info-hash: %w", err)
	}
	hashLower := strings.ToLower(hash)
	if name == "" {
		name = strings.TrimSuffix(filename, filepath.Ext(filename))
	}
	return s.insertJob(ctx, hashLower, name, body, "", category)
}

// acceptMagnet parses the magnet URI for xt=urn:btih:<hash> (hex or base32)
// and inserts a magnet-only job. NzbBlob is the magnet bytes — the worker
// will forward the URI string to TorBox.
func (s *Server) acceptMagnet(ctx context.Context, magnet, category, _ string) error {
	if !strings.HasPrefix(magnet, "magnet:") {
		return errors.New("not a magnet URI")
	}
	u, err := url.Parse(magnet)
	if err != nil {
		return fmt.Errorf("parse magnet: %w", err)
	}
	q := u.Query()
	var hash string
	for _, xt := range q["xt"] {
		if strings.HasPrefix(xt, "urn:btih:") {
			hash = decodeBTIH(strings.TrimPrefix(xt, "urn:btih:"))
			break
		}
	}
	if hash == "" {
		return errors.New("no urn:btih: in magnet")
	}
	name := q.Get("dn")
	if name == "" {
		name = "magnet-" + hash[:8]
	}
	return s.insertJob(ctx, hash, name, nil, magnet, category)
}

// decodeBTIH normalizes a BT info-hash to lowercase hex (40 chars). Magnet
// links use either hex or RFC4648 base32 — both yield a 20-byte hash.
func decodeBTIH(s string) string {
	switch len(s) {
	case 40:
		if _, err := hex.DecodeString(s); err == nil {
			return strings.ToLower(s)
		}
	case 32:
		b, err := base32.StdEncoding.DecodeString(strings.ToUpper(s))
		if err == nil && len(b) == 20 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

func (s *Server) insertJob(ctx context.Context, hash, name string, blob []byte, magnet, category string) error {
	if category == "" {
		category = "default"
	}
	// Dedup: if a job is already in flight with this hash, no-op. The qBit
	// /torrents/add is idempotent under repeat submissions and Sonarr/Radarr
	// rely on that during retries.
	if existing, err := s.store.GetByHash(ctx, hash); err == nil && existing != nil {
		s.logger.Info("qbit add dedup", "hash", hash, "nzo_id", existing.NzoID)
		return nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("dedup check: %w", err)
	}
	nzoID := nzoIDPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	j := &job.Job{
		NzoID:     nzoID,
		Category:  category,
		Filename:  name,
		NzbSHA256: hash, // doubles as the BT info-hash for qBit lookups
		NzbBlob:   blob,
		State:     job.StateNew,
		Source:    "torrent",
	}
	if magnet != "" {
		j.Magnet.Valid = true
		j.Magnet.String = magnet
	}
	j.SizeBytes.Valid = true
	j.SizeBytes.Int64 = int64(len(blob))
	if err := s.store.AddTorrent(ctx, j); err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	s.logger.Info("qbit add accepted", "nzo_id", nzoID, "hash", hash, "category", category, "name", name, "magnet", magnet != "")
	return nil
}

func (s *Server) handleTorrentsDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	hashes := r.FormValue("hashes")
	deleteFiles := r.FormValue("deleteFiles") == "true"
	if hashes == "" {
		http.Error(w, "missing hashes", http.StatusBadRequest)
		return
	}
	for _, h := range splitHashes(hashes) {
		j, err := s.store.GetByHash(r.Context(), h)
		if err != nil || j == nil {
			continue
		}
		if deleteFiles {
			s.tryRemoveLocal(j)
		}
		if err := s.store.Delete(r.Context(), j.NzoID); err != nil {
			s.logger.Warn("qbit delete failed", "err", err, "nzo_id", j.NzoID)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleTorrentsPause(w http.ResponseWriter, r *http.Request) {
	s.bulkStateChange(w, r, job.StateCanceled, "paused by qbit client")
}

func (s *Server) handleTorrentsResume(w http.ResponseWriter, r *http.Request) {
	// We don't actually pause — TorBox does the download regardless. Resume
	// is a no-op except for canceled rows where there's nothing to resume.
	w.WriteHeader(http.StatusOK)
}

func (s *Server) bulkStateChange(w http.ResponseWriter, r *http.Request, to job.State, reason string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	hashes := r.FormValue("hashes")
	if hashes == "" {
		http.Error(w, "missing hashes", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if hashes == "all" {
		js, err := s.store.ListByCategory(ctx, "")
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		for _, j := range js {
			_ = s.store.UpdateState(ctx, j.NzoID, string(to), reason)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	for _, h := range splitHashes(hashes) {
		j, err := s.store.GetByHash(ctx, h)
		if err != nil || j == nil {
			continue
		}
		_ = s.store.UpdateState(ctx, j.NzoID, string(to), reason)
	}
	s.signalWake()
	w.WriteHeader(http.StatusOK)
}

func splitHashes(s string) []string {
	parts := strings.Split(s, "|")
	if len(parts) == 1 {
		// Some clients send comma-separated, others pipe-separated. Accept both.
		parts = strings.Split(s, ",")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleCategoriesGet(w http.ResponseWriter, _ *http.Request) {
	// We don't persist categories — Sonarr/Radarr each create + reuse one
	// (sonarr / radarr). Hard-coding the common ones means a fresh install
	// works without a createCategory round-trip.
	cats := map[string]Category{
		"sonarr": {Name: "sonarr", SavePath: s.savePathFor("sonarr")},
		"radarr": {Name: "radarr", SavePath: s.savePathFor("radarr")},
	}
	writeJSON(w, http.StatusOK, cats)
}

func (s *Server) handleCategoriesCreate(w http.ResponseWriter, r *http.Request) {
	// Accept + ack but don't persist. Categories are derived from the
	// per-job category column; no separate registry is needed for the shim
	// to function.
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	s.logger.Info("qbit createCategory", "category", r.FormValue("category"), "savePath", r.FormValue("savePath"))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleTorrentsFiles(w http.ResponseWriter, r *http.Request) {
	hash := readHashParam(r)
	if hash == "" {
		http.Error(w, "missing hash", http.StatusBadRequest)
		return
	}
	j, err := s.store.GetByHash(r.Context(), hash)
	if err != nil || j == nil {
		writeJSON(w, http.StatusOK, []TorrentFile{})
		return
	}
	// We don't track per-file breakdown until the file is local. For READY
	// rows with a LibraryPath, return a one-file stub so Sonarr's import
	// scanner has something to walk.
	name := displayName(j)
	if j.LibraryPath.Valid && j.LibraryPath.String != "" {
		name = filepath.Base(j.LibraryPath.String)
	}
	progress := 0.0
	if j.State == job.StateReady {
		progress = 1.0
	}
	writeJSON(w, http.StatusOK, []TorrentFile{{
		Index:        0,
		Name:         name,
		Size:         j.SizeBytes.Int64,
		Progress:     progress,
		Priority:     1,
		IsSeed:       false,
		PieceRange:   []int{0, 0},
		Availability: 1.0,
	}})
}

func (s *Server) handleTorrentsProperties(w http.ResponseWriter, r *http.Request) {
	hash := readHashParam(r)
	if hash == "" {
		http.Error(w, "missing hash", http.StatusBadRequest)
		return
	}
	j, err := s.store.GetByHash(r.Context(), hash)
	if err != nil || j == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	savePath := s.savePathFor(j.Category)
	props := TorrentProperties{
		SavePath:        savePath,
		AdditionDate:    j.CreatedAt.Unix(),
		TotalSize:       j.SizeBytes.Int64,
		TotalDownloaded: int64(float64(j.SizeBytes.Int64) * progressFor(j.State)),
		Eta:             etaInfinity,
		PieceSize:       16 * 1024,
		NbConnectionsLimit: 100,
	}
	if j.CompletedAt.Valid {
		props.CompletionDate = j.CompletedAt.Time.Unix()
		props.TimeElapsed = j.CompletedAt.Time.Unix() - j.CreatedAt.Unix()
	} else {
		props.CompletionDate = -1
		props.TimeElapsed = time.Since(j.CreatedAt).Milliseconds() / 1000
	}
	writeJSON(w, http.StatusOK, props)
}

func progressFor(s job.State) float64 {
	_, p := mapState(s)
	return p
}

// readHashParam grabs the single-hash arg these endpoints use, accepting
// either query or form (qBit's docs say form, but clients are inconsistent).
func readHashParam(r *http.Request) string {
	if h := r.URL.Query().Get("hash"); h != "" {
		return strings.ToLower(h)
	}
	_ = r.ParseForm()
	return strings.ToLower(r.FormValue("hash"))
}

// tryRemoveLocal is best-effort cleanup when deleteFiles=true. We never
// surface the error to the client — qBit's delete is non-blocking on disk
// state and Sonarr doesn't retry on partial cleanup failure.
func (s *Server) tryRemoveLocal(j *job.Job) {
	target := ""
	if j.LibraryPath.Valid && j.LibraryPath.String != "" {
		target = j.LibraryPath.String
	} else if j.TorboxFolderName.Valid && j.TorboxFolderName.String != "" {
		target = filepath.Join(s.savePathFor(j.Category), j.TorboxFolderName.String)
	}
	if target == "" {
		return
	}
	if err := os.RemoveAll(target); err != nil {
		s.logger.Warn("qbit delete files", "err", err, "path", target)
	}
}

// nullStringValue is a tiny helper used only in tests, kept here so the
// package doesn't need a separate testing util file. Avoids importing
// database/sql in handlers where we already have it.
var _ = sql.NullString{}
