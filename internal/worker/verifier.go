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

// verifyOnce promotes COMPLETED_TORBOX → READY based on TorBox's API state,
// not the local filesystem mount.
//
// Earlier this loop required the folder to exist at /torbox/<folder> in
// arrarr's own mount before reporting "Completed" to Sonarr. That broke when
// arrarr's mount diverged from Sonarr/Jellyfin's (different containers can
// capture different rclone namespaces depending on start order). TorBox's
// download_state is the source of truth — Sonarr/Radarr will check the file
// against their own mount when they try to import, which is the right place
// for that check.
//
// We still log a warning when the path isn't visible in arrarr's view, as a
// useful diagnostic, but it doesn't block.
func (m *Manager) verifyOnce(ctx context.Context) {
	jobs, err := m.o.Store.ListByStates(ctx, []job.State{job.StateCompletedTorbox}, 500)
	if err != nil {
		m.log.Error("verify: list failed", "err", err)
		return
	}
	for _, j := range jobs {
		if !j.TorboxFolderName.Valid || j.TorboxFolderName.String == "" {
			// Without a folder name we can't tell consumers where the file is.
			if time.Since(j.UpdatedAt) > MaxVerifyDuration {
				_ = m.o.Store.Transition(ctx, j.NzoID, store.Transition{
					From:        job.StateCompletedTorbox,
					To:          job.StateFailed,
					LastError:   strPtr("torbox returned no folder name"),
					CompletedAt: nowPtr(),
				})
			}
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
		// Advisory only — don't gate on local visibility.
		if exists, _ := m.exists(path); !exists {
			m.log.Info("verify: not visible in arrarr's mount yet — trusting TorBox API state",
				"nzo_id", j.NzoID, "expected_path", path)
		}
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
