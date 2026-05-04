package sab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

func (s *Server) handleAddFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	q := r.URL.Query()
	cat := strings.ToLower(q.Get("cat"))

	if err := r.ParseMultipartForm(s.maxNZB + (1 << 20)); err != nil {
		s.logger.Warn("addfile parse multipart failed", "err", err)
		s.writeError(w, http.StatusBadRequest, "invalid multipart body")
		return
	}
	defer drainAndClose(r.Body)

	filename, body, err := readNZBPart(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if int64(len(body)) > s.maxNZB {
		s.writeError(w, http.StatusRequestEntityTooLarge, "NZB too large")
		return
	}
	nzoID, err := s.acceptNZB(r.Context(), filename, cat, body)
	if err != nil {
		s.logger.Error("accept nzb", "err", err)
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AddResp{Status: true, NzoIDs: []string{nzoID}})
}

// readNZBPart looks for the file under SAB's "name" field, falling back to "nzbfile".
func readNZBPart(r *http.Request) (string, []byte, error) {
	for _, field := range []string{"name", "nzbfile"} {
		if r.MultipartForm == nil || r.MultipartForm.File == nil {
			break
		}
		fhs := r.MultipartForm.File[field]
		if len(fhs) == 0 {
			continue
		}
		fh := fhs[0]
		f, err := fh.Open()
		if err != nil {
			return "", nil, err
		}
		defer f.Close()
		body, err := io.ReadAll(f)
		if err != nil {
			return "", nil, err
		}
		return fh.Filename, body, nil
	}
	return "", nil, errors.New("no NZB file in form (expected field 'name' or 'nzbfile')")
}

func (s *Server) handleAddURL(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	target := q.Get("name")
	if target == "" {
		target = q.Get("nzbname")
	}
	if target == "" {
		s.writeError(w, http.StatusBadRequest, "missing name=<url>")
		return
	}
	cat := strings.ToLower(q.Get("cat"))

	body, filename, err := s.fetchURL(r.Context(), target)
	if err != nil {
		s.writeError(w, http.StatusBadGateway, "fetch nzb: "+err.Error())
		return
	}
	if int64(len(body)) > s.maxNZB {
		s.writeError(w, http.StatusRequestEntityTooLarge, "NZB too large")
		return
	}
	nzoID, err := s.acceptNZB(r.Context(), filename, cat, body)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AddResp{Status: true, NzoIDs: []string{nzoID}})
}

// acceptNZB de-dups by sha256 against any non-terminal job, then signals the dispatcher.
func (s *Server) acceptNZB(ctx context.Context, filename, category string, body []byte) (string, error) {
	if category == "" {
		category = "default"
	}
	if filename == "" {
		filename = "upload.nzb"
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	if existing, err := s.store.FindActiveBySHA(ctx, sha); err == nil && existing != nil {
		s.logger.Info("addfile dedup", "nzo_id", existing.NzoID, "sha", sha[:12])
		return existing.NzoID, nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("dedup check: %w", err)
	}

	nzoID := "arrarr_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	j := &job.Job{
		NzoID:     nzoID,
		Category:  category,
		Filename:  filename,
		NzbSHA256: sha,
		NzbBlob:   body,
		State:     job.StateNew,
	}
	j.SizeBytes.Valid = true
	j.SizeBytes.Int64 = int64(len(body))
	if err := s.store.Insert(ctx, j); err != nil {
		return "", fmt.Errorf("insert: %w", err)
	}
	s.signalWake()
	s.logger.Info("addfile accepted", "nzo_id", nzoID, "filename", filename, "category", category, "bytes", len(body))
	return nzoID, nil
}

func (s *Server) fetchURL(ctx context.Context, target string) ([]byte, string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, "", fmt.Errorf("invalid url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxNZB+1))
	if err != nil {
		return nil, "", err
	}
	filename := u.Query().Get("filename")
	if filename == "" {
		base := u.Path
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		filename = base
	}
	if filename == "" || !strings.HasSuffix(strings.ToLower(filename), ".nzb") {
		filename = "remote.nzb"
	}
	return body, filename, nil
}
