package meetings

import (
	"context"
	"errors"
	"time"
)

// DefaultRetentionInterval is how often the retention sweeper runs.
const DefaultRetentionInterval = time.Hour

// ErrNotEnded is returned by HandOff for a meeting that is still running.
var ErrNotEnded = errors.New("meetings: meeting has not ended")

// HandOff marks an ended meeting handed off after its transcript and notes
// went to the inbox, and erases the transcript upstream with
// meeting.delete. Hive keeps its own copy until the retention sweeper
// purges it. Handing off twice is a no-op.
func (m *Manager) HandOff(ctx context.Context, id string) error {
	mt, err := m.opts.Store.Get(id)
	if err != nil {
		return err
	}
	switch mt.State {
	case StateHandedOff, StatePurged:
		return nil
	}
	if !mt.State.Terminal() {
		return ErrNotEnded
	}
	if mt.NativeID != "" {
		if err := m.opts.Quorum.Delete(ctx, mt.NativeID); err != nil && !IsCode(err, CodeNotFound) {
			return err
		}
	}
	_, err = m.update(id, nil, func(x *Meeting) {
		x.State = StateHandedOff
		x.HandedOffAt = timePtr(m.opts.Now())
	})
	return err
}

// Sweep purges the stored transcript and side documents of every meeting
// that ended more than retention ago, erasing it upstream first when it
// was never handed off. It returns how many meetings it purged.
func (m *Manager) Sweep(ctx context.Context, retention time.Duration, docs ...string) int {
	if retention <= 0 {
		return 0
	}
	list, err := m.opts.Store.List()
	if err != nil {
		m.log.Warn("[meetings] retention: listing meetings", "error", err)
		return 0
	}
	cut := m.opts.Now().Add(-retention)
	n := 0
	for _, mt := range list {
		if mt.State == StatePurged || !mt.State.Terminal() || mt.EndedAt == nil || mt.EndedAt.After(cut) {
			continue
		}
		if mt.State != StateHandedOff && mt.NativeID != "" {
			if err := m.opts.Quorum.Delete(ctx, mt.NativeID); err != nil && !IsCode(err, CodeNotFound) {
				m.log.Warn("[meetings] retention: upstream delete failed; will retry", "meeting", mt.ID, "error", err)
				continue
			}
		}
		if err := m.opts.Store.PurgeTranscript(mt.ID); err != nil {
			m.log.Warn("[meetings] retention: purging transcript", "meeting", mt.ID, "error", err)
			continue
		}
		for _, d := range docs {
			if err := m.opts.Store.RemoveDoc(mt.ID, d); err != nil {
				m.log.Warn("[meetings] retention: removing document", "meeting", mt.ID, "doc", d, "error", err)
			}
		}
		if _, err := m.update(mt.ID, nil, func(x *Meeting) {
			x.State = StatePurged
			x.PurgedAt = timePtr(m.opts.Now())
		}); err == nil {
			n++
		}
	}
	if n > 0 {
		m.log.Info("[meetings] retention: purged meetings", "count", n, "retention", retention.String())
	}
	return n
}

// StartRetention runs Sweep every interval (DefaultRetentionInterval when
// zero) until Close.
func (m *Manager) StartRetention(retention, interval time.Duration, docs ...string) {
	if interval <= 0 {
		interval = DefaultRetentionInterval
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		m.Sweep(m.ctx, retention, docs...)
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-t.C:
				m.Sweep(m.ctx, retention, docs...)
			}
		}
	}()
}
