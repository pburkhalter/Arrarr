package worker

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
	"github.com/pburkhalter/arrarr/internal/store"
)

func (m *Manager) verifierLoop(ctx context.Context) {
	ticker := time.NewTicker(m.o.VerifyEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.verifyOnce(ctx)
		}
	}
}

func (m *Manager) verifyOnce(ctx context.Context) {
	jobs, err := m.o.Store.ListByStates(ctx, []job.State{job.StateCompletedTorbox}, 500)
	if err != nil {
		m.log.Error("verify: list failed", "err", err)
		return
	}
	for _, j := range jobs {
		if !j.TorboxFolderName.Valid || j.TorboxFolderName.String == "" {
			continue
		}
		path, err := m.o.PathMap.Local(j.TorboxFolderName.String)
		if err != nil {
			m.log.Warn("verify: bad folder name", "nzo_id", j.NzoID, "err", err)
			_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
				From:        job.StateCompletedTorbox,
				To:          job.StateFailed,
				LastError:   strPtr("invalid folder name from torbox"),
				CompletedAt: nowPtr(),
			})
			continue
		}
		exists, err := m.exists(path)
		if err != nil {
			m.log.Warn("verify: stat failed", "nzo_id", j.NzoID, "path", path, "err", err)
			continue
		}
		if exists {
			if err := m.o.Store.Transition(ctx, j.NzoID, store.Transition{
				From:         job.StateCompletedTorbox,
				To:           job.StateReady,
				CompletedAt:  nowPtr(),
				ClearBlob:    true,
				ClearClaimed: true,
			}); err != nil {
				m.log.Error("verify: transition failed", "nzo_id", j.NzoID, "err", err)
				continue
			}
			m.log.Info("verified", "nzo_id", j.NzoID, "path", path)
			continue
		}
		if time.Since(j.UpdatedAt) > MaxVerifyDuration {
			m.log.Warn("verify: webdav lag exceeded budget", "nzo_id", j.NzoID, "path", path)
			_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
				From:        job.StateCompletedTorbox,
				To:          job.StateFailed,
				LastError:   strPtr("webdav cache never updated"),
				CompletedAt: nowPtr(),
			})
		}
	}
}

func (m *Manager) exists(path string) (bool, error) {
	if m.o.FS != nil {
		return m.o.FS.Exists(path)
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
