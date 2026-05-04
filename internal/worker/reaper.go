package worker

import (
	"context"
	"time"
)

func (m *Manager) reaperLoop(ctx context.Context) {
	ticker := time.NewTicker(m.o.ReapEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().UTC().Add(-m.o.ReapOlderThan)
			n, err := m.o.Store.Reap(ctx, cutoff)
			if err != nil {
				m.log.Error("reap failed", "err", err)
				continue
			}
			if n > 0 {
				m.log.Info("reaped", "rows", n, "older_than", m.o.ReapOlderThan)
			}
		}
	}
}
