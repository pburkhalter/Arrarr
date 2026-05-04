package sab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if name := q.Get("name"); name != "" {
		s.handleQueueAction(w, r, name, q.Get("value"))
		return
	}
	limit := parseLimit(q.Get("limit"), 100, 1000)

	jobs, err := s.store.ListByStates(r.Context(),
		[]job.State{job.StateNew, job.StateSubmitted, job.StateDownloading, job.StateCompletedTorbox},
		limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := QueueResp{Queue: Queue{
		Status:         "Idle",
		Speed:          "0 B/s",
		Kbpersec:       "0",
		Speedlimit:     "100",
		SpeedlimitAbs:  "0",
		Paused:         false,
		Limit:          limit,
		Start:          0,
		NoofSlotsTotal: len(jobs),
		NoofSlots:      len(jobs),
	}}
	if len(jobs) > 0 {
		resp.Queue.Status = "Downloading"
	}
	for i, j := range jobs {
		resp.Queue.Slots = append(resp.Queue.Slots, queueSlotFromJob(i, j))
	}
	writeJSON(w, http.StatusOK, resp)
}

func queueSlotFromJob(idx int, j *job.Job) QueueSlot {
	size := j.SizeBytes.Int64
	pct := percentageFor(j.State)
	return QueueSlot{
		Index:      idx,
		NzoID:      j.NzoID,
		Filename:   j.Filename,
		Cat:        j.Category,
		Status:     stateToSABStatus(j.State),
		Priority:   "Normal",
		Percentage: pct,
		Size:       formatBytes(size),
		Sizeleft:   formatBytes(size),
		Mb:         formatMB(size),
		Mbleft:     formatMB(size),
		Timeleft:   "0:00:00",
		ETA:        "unknown",
		Script:     "None",
	}
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if name := q.Get("name"); name != "" {
		s.handleHistoryAction(w, r, name, q.Get("value"))
		return
	}
	limit := parseLimit(q.Get("limit"), 200, 1000)

	jobs, err := s.store.ListReady(r.Context(), limit)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := HistoryResp{History: History{NoofSlots: len(jobs)}}
	for _, j := range jobs {
		slot := HistorySlot{
			NzoID:        j.NzoID,
			Name:         strings.TrimSuffix(j.Filename, ".nzb"),
			Category:     j.Category,
			PP:           "3",
			Script:       "None",
			NzbName:      j.Filename,
			Bytes:        j.SizeBytes.Int64,
			Size:         formatBytes(j.SizeBytes.Int64),
			DownloadTime: 0,
		}
		if j.CompletedAt.Valid {
			slot.Completed = j.CompletedAt.Time.Unix()
		}
		switch j.State {
		case job.StateReady:
			slot.Status = "Completed"
			if j.TorboxFolderName.Valid {
				if p, err := s.pathMap.Visible(j.TorboxFolderName.String); err == nil {
					slot.Storage = p
					slot.Path = p
				}
			}
		case job.StateFailed:
			slot.Status = "Failed"
			slot.FailMessage = j.LastError.String
		case job.StateCanceled:
			slot.Status = "Failed"
			slot.FailMessage = "Canceled"
		}
		resp.History.Slots = append(resp.History.Slots, slot)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleQueueAction(w http.ResponseWriter, r *http.Request, action, value string) {
	if action != "delete" {
		s.writeError(w, http.StatusBadRequest, "unsupported queue action: "+action)
		return
	}
	if value == "" {
		s.writeError(w, http.StatusBadRequest, "missing value=<nzo_id>")
		return
	}
	if err := s.cancel(r.Context(), value); err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, SimpleStatus{Status: true})
}

func (s *Server) handleHistoryAction(w http.ResponseWriter, r *http.Request, action, value string) {
	if action != "delete" {
		s.writeError(w, http.StatusBadRequest, "unsupported history action: "+action)
		return
	}
	if value == "" {
		s.writeError(w, http.StatusBadRequest, "missing value=<nzo_id>")
		return
	}
	if err := s.store.Delete(r.Context(), value); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, SimpleStatus{Status: true})
}

func (s *Server) cancel(ctx context.Context, nzoID string) error {
	j, err := s.store.Get(ctx, nzoID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if j.State.Terminal() {
		return s.store.Delete(ctx, nzoID)
	}
	if err := s.store.Transition(ctx, nzoID, store.Transition{
		From:        j.State,
		To:          job.StateCanceled,
		LastError:   strPtr("canceled by client"),
		CompletedAt: timePtrNow(),
	}); err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	s.signalWake()
	return nil
}
