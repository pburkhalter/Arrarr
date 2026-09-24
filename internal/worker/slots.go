package worker

import (
	"context"

	"github.com/pburkhalter/arrarr/internal/job"
)

// releaseTorboxSlot deletes the remote entry of a download we have given up
// on because it hangs: stalled without progress, or past the 24h ceiling.
//
// TorBox caps concurrent downloads per account (10 usenet on this plan) and
// keeps counting an entry against that cap until it is deleted, so every
// abandoned download costs a slot for good. In Sep 2026 sixteen of them filled
// the account and nothing could be submitted for 15 days.
//
// Those two cases are the only ones that delete anything. The TorBox account
// is shared with another person: finished, failed and canceled downloads stay
// in the account. They hold no slot, and what else lives in a shared account
// is not arrarr's to tidy up. (3.1.11 deleted every finished entry and a one-
// off sweep removed ~1 050 history entries by name; both were reverted. The
// list endpoint returns at most ~1 000 entries anyway, so deleting history
// never made the poll cheaper.)
//
// Best effort: a failed delete is logged and the job still ends as failed.
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
		m.log.Warn("poll: releasing torbox slot failed",
			"nzo_id", j.NzoID, "torbox_id", id, "err", describe(err))
		return
	}
	m.log.Info("poll: released torbox slot", "nzo_id", j.NzoID, "torbox_id", id)
}
