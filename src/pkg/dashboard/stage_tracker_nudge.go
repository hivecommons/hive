package dashboard

import (
	"sync"
	"time"
)

// stageTrackerNudgeInterval is the minimum spacing between `stage-tracker`
// SSE frames. A webhook burst or a rapid run of status publishes would
// otherwise make every open tab refetch /api/stage-tracker per event.
const stageTrackerNudgeInterval = 2 * time.Second

// stageTrackerNudger coalesces nudges to at most one per interval: the first
// goes out immediately, later ones inside the window collapse into a single
// trailing frame carrying the newest seq, sent when the window ends.
type stageTrackerNudger struct {
	interval time.Duration // zero -> stageTrackerNudgeInterval
	mu       sync.Mutex
	last     time.Time
	armed    bool
	seq      uint64
	now      func() time.Time
	newTimer func(d time.Duration, f func()) trackerRefreshTimer
}

func (n *stageTrackerNudger) clock() time.Time {
	if n.now != nil {
		return n.now()
	}
	return time.Now()
}

// nudge records seq and calls send now, or defers a single send to the end
// of the current window.
func (n *stageTrackerNudger) nudge(seq uint64, send func(seq uint64)) {
	n.mu.Lock()
	n.seq = seq
	if n.armed {
		n.mu.Unlock()
		return
	}
	now := n.clock()
	interval := n.interval
	if interval == 0 {
		interval = stageTrackerNudgeInterval
	}
	if wait := interval - now.Sub(n.last); !n.last.IsZero() && wait > 0 {
		n.armed = true
		newTimer := n.newTimer
		if newTimer == nil {
			newTimer = func(d time.Duration, f func()) trackerRefreshTimer { return time.AfterFunc(d, f) }
		}
		newTimer(wait, func() {
			n.mu.Lock()
			n.armed = false
			n.last = n.clock()
			seq := n.seq
			n.mu.Unlock()
			send(seq)
		})
		n.mu.Unlock()
		return
	}
	n.last = now
	n.mu.Unlock()
	send(seq)
}

// nudgeStageTracker broadcasts the coalesced `stage-tracker` SSE frame.
func (s *Server) nudgeStageTracker(seq uint64) {
	s.stageTrackerNudge.nudge(seq, func(seq uint64) {
		s.broadcastFrame(stageTrackerSSEFrame(seq))
	})
}
