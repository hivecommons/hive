package automerge

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestCautionSweepInterval(t *testing.T) {
	if got := cautionSweepInterval(selfAuthoredAutoMergeSweepInterval); got != selfAuthoredSweepCautionInterval {
		t.Fatalf("10s tick under caution = %s, want the %s floor", got, selfAuthoredSweepCautionInterval)
	}
	if got := cautionSweepInterval(5 * time.Minute); got != 5*time.Minute {
		t.Fatalf("adaptive 5m tick under caution = %s, want unchanged", got)
	}
}

// Under a caution window the self-authored sweep backs its ticker off to the
// 60s floor and sheds ticks the pacer says would queue too deep; when the
// window ends it resumes the adaptive interval.
func TestSweepTickGateBacksOffAndShedsUnderCaution(t *testing.T) {
	var logs bytes.Buffer
	cautious := true
	e := &Engine{logger: slog.New(slog.NewTextHandler(&logs, nil)), rateLimitCautious: func() bool { return cautious }}
	var shedCalls int
	shed := func(name string, budget time.Duration) bool {
		shedCalls++
		if name != "automerge:self-authored-sweep" || budget != selfAuthoredSweepTickBudget {
			t.Fatalf("shed(%q, %s)", name, budget)
		}
		return true
	}

	d := e.sweepTickGate(selfAuthoredAutoMergeSweepInterval, 0, shed)
	if !d.reset || d.tick != selfAuthoredSweepCautionInterval || d.cautionTick != selfAuthoredSweepCautionInterval || !d.skip {
		t.Fatalf("first cautious tick = %+v, want reset to 60s floor and skip", d)
	}
	if !strings.Contains(logs.String(), "backing off while rate-limit caution window is engaged") {
		t.Fatalf("logs = %q, want back-off log", logs.String())
	}

	// Already at the floor: no further reset; still consults the pacer.
	d = e.sweepTickGate(selfAuthoredAutoMergeSweepInterval, d.cautionTick, func(string, time.Duration) bool { return false })
	if d.reset || d.skip || d.cautionTick != selfAuthoredSweepCautionInterval {
		t.Fatalf("steady cautious tick = %+v, want no reset, no skip", d)
	}
	if shedCalls != 1 {
		t.Fatalf("shed calls = %d, want 1", shedCalls)
	}

	// Caution ends: one reset back to the adaptive interval, never skipped.
	cautious = false
	d = e.sweepTickGate(selfAuthoredAutoMergeSweepInterval, d.cautionTick, shed)
	if !d.reset || d.tick != selfAuthoredAutoMergeSweepInterval || d.cautionTick != 0 || d.skip {
		t.Fatalf("tick after caution = %+v, want reset to adaptive interval", d)
	}
	d = e.sweepTickGate(selfAuthoredAutoMergeSweepInterval, 0, shed)
	if d.reset || d.skip {
		t.Fatalf("uncautious steady tick = %+v, want no change", d)
	}
	if shedCalls != 1 {
		t.Fatalf("shed consulted outside caution: calls = %d", shedCalls)
	}
}
