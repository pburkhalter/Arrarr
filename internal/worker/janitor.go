package worker

import (
	"context"
	"time"

	"github.com/pburkhalter/arrarr/internal/job"
)

// releaseTimeout bounds the TorBox delete that runs inside a transition.
const releaseTimeout = 10 * time.Second

// onTransition is the store hook: a job that reaches a terminal state gives
// its TorBox entry back.
//
// TorBox keeps every entry — finished, failed or abandoned — until it is
// deleted. Each one counts against the account's active limit while it runs
// and sits in mylist forever afterwards (~5 KB per entry, which is what
// outgrew the response cap in Aug 2026). Only stalled and timed-out jobs were
// ever deleted; by Sep 2026 the list held 1 001 entries, 941 of them dead.
//
// Running as a hook covers every path at once — the puller's READY, the
// poller's and submitter's FAILED, the SAB and qBit cancels — and keeps the
// delete inside the transition, so a history delete that follows a cancel
// cannot race the lookup of the TorBox id. Best effort: a failed delete is
// logged and the job stays terminal.
func (m *Manager) onTransition(nzoID, _, to string) {
	switch job.State(to) {
	case job.StateReady, job.StateFailed, job.StateCanceled:
	default:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	j, err := m.o.Store.Get(ctx, nzoID)
	if err != nil {
		m.log.Warn("janitor: job lookup failed", "nzo_id", nzoID, "err", err)
		return
	}
	m.releaseTorboxSlot(ctx, j)
}

// releaseTorboxSlot deletes the job's remote entry. Nothing to do for jobs
// TorBox never accepted (no id).
func (m *Manager) releaseTorboxSlot(ctx context.Context, j *job.Job) {
	id := j.EffectiveTorboxID()
	if id == 0 {
		return
	}
	var err error
	if j.Source == "torrent" {
		err = m.o.Torbox.ControlTorrent(ctx, id, "delete")
	} else {
		err = m.o.Torbox.ControlUsenet(ctx, id, "delete")
	}
	if err != nil {
		m.log.Warn("janitor: releasing torbox entry failed",
			"nzo_id", j.NzoID, "torbox_id", id, "state", j.State, "err", describe(err))
		return
	}
	m.log.Info("janitor: released torbox entry", "nzo_id", j.NzoID, "torbox_id", id, "state", j.State)
}
