package meetings

import (
	"context"
	"sync"
	"time"
)

// The last lines of a companion meeting. Quorum drops a companion
// meeting's outbox when hive leaves it, and the companion window only
// polls while the meeting is live, so a line posted just before the leave
// ("Leaving now.") was never seen: the next poll found the meeting ended
// and no lines. Hive copies the outbox just before it leaves and serves
// those lines from CompanionReplies after the meeting ended, until the
// window's cursor has passed them or companionTailGrace is over.

// companionTailGrace is how long an ended companion meeting's last lines
// stay fetchable.
const companionTailGrace = 2 * time.Minute

// companionTail is one ended meeting's last lines.
type companionTail struct {
	lines   []CompanionLine
	last    int64
	expires time.Time
}

// companionTails holds the last lines of recently ended companion meetings.
type companionTails struct {
	mu sync.Mutex
	by map[string]companionTail
}

// keepCompanionTail copies a companion meeting's outbox before hive leaves
// it. Failures only cost the last lines; the leave goes on.
func (m *Manager) keepCompanionTail(ctx context.Context, mt Meeting) {
	if mt.Platform != PlatformCompanion || mt.NativeID == "" {
		return
	}
	cq, err := m.companionQuorum()
	if err != nil {
		return
	}
	page, err := cq.CompanionOutbox(ctx, mt.NativeID, 0)
	if err != nil {
		m.log.Info("[meetings] companion: last lines not kept", "meeting", mt.ID, "error", err)
		return
	}
	if len(page.Lines) == 0 {
		return
	}
	now := m.opts.Now()
	m.tails.mu.Lock()
	defer m.tails.mu.Unlock()
	if m.tails.by == nil {
		m.tails.by = map[string]companionTail{}
	}
	for id, t := range m.tails.by {
		if !now.Before(t.expires) {
			delete(m.tails.by, id)
		}
	}
	m.tails.by[mt.ID] = companionTail{lines: page.Lines, last: page.NextSeq, expires: now.Add(companionTailGrace)}
}

// companionTail returns an ended meeting's kept lines after the cursor and
// the next cursor. The lines are forgotten once the cursor has passed them
// or the grace period is over.
func (m *Manager) companionTail(id string, after int64) ([]CompanionLine, int64) {
	after = max(after, 0)
	out := []CompanionLine{}
	m.tails.mu.Lock()
	defer m.tails.mu.Unlock()
	t, ok := m.tails.by[id]
	if !ok {
		return out, after
	}
	if after >= t.last || !m.opts.Now().Before(t.expires) {
		delete(m.tails.by, id)
		return out, after
	}
	for _, l := range t.lines {
		if l.Seq > after {
			out = append(out, l)
		}
	}
	return out, max(after, t.last)
}
