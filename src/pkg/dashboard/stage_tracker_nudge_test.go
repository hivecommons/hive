package dashboard

import (
	"testing"
	"time"
)

func TestStageTrackerNudgerCoalescesWithinInterval(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	timers := &fakeTimers{}
	n := &stageTrackerNudger{now: func() time.Time { return now }, newTimer: timers.newTimer}
	var sent []uint64
	send := func(seq uint64) { sent = append(sent, seq) }

	n.nudge(1, send)
	if len(sent) != 1 || sent[0] != 1 {
		t.Fatalf("first nudge should send immediately, sent=%v", sent)
	}

	now = now.Add(500 * time.Millisecond)
	n.nudge(2, send)
	n.nudge(3, send)
	n.nudge(4, send)
	if len(sent) != 1 || timers.count() != 1 {
		t.Fatalf("burst must collapse into one pending frame: sent=%v timers=%d", sent, timers.count())
	}
	if d := timers.last().d; d != stageTrackerNudgeInterval-500*time.Millisecond {
		t.Fatalf("trailing frame delay = %v", d)
	}

	now = now.Add(1500 * time.Millisecond)
	timers.last().fire()
	if len(sent) != 2 || sent[1] != 4 {
		t.Fatalf("trailing frame must carry newest seq, sent=%v", sent)
	}

	// A nudge right after the trailing send is inside a new window.
	now = now.Add(100 * time.Millisecond)
	n.nudge(5, send)
	if len(sent) != 2 || timers.count() != 2 {
		t.Fatalf("expected deferral after trailing send: sent=%v timers=%d", sent, timers.count())
	}

	// Once the window has passed, the next nudge sends immediately again.
	now = now.Add(1900 * time.Millisecond)
	timers.last().fire()
	now = now.Add(stageTrackerNudgeInterval + time.Second)
	n.nudge(6, send)
	if len(sent) != 4 || sent[3] != 6 {
		t.Fatalf("sent=%v", sent)
	}
}

func TestStageTrackerETagIgnoresGeneratedAt(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	s, _ := stageTrackerFixture(t, now)
	status := s.status
	_, etag1, at1, fresh1 := s.stageTrackerPayload(status, now)
	s.invalidateStageTrackerCache()
	later := now.Add(7 * time.Second)
	_, etag2, at2, fresh2 := s.stageTrackerPayload(status, later)
	if !fresh1 || !fresh2 || at1.Equal(at2) {
		t.Fatalf("both calls must rebuild at different times: %v %v %v %v", fresh1, fresh2, at1, at2)
	}
	if etag1 != etag2 {
		t.Fatalf("ETag changed with only generated_at moving: %q vs %q", etag1, etag2)
	}
}
