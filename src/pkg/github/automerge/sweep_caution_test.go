package automerge

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
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

// A candidate's evaluation and merge run at normal pacing class, lifted out
// of the lane's low class; a class the caller chose is left alone.
func TestMergeDecisionPriority(t *testing.T) {
	low := lowPriority(context.Background())
	if got := hgithub.PacingClassOf(mergeDecisionPriority(low)); got != hgithub.PacingNormal {
		t.Fatalf("low lane ctx → %s, want normal", got)
	}
	high := hgithub.WithPacingClass(context.Background(), hgithub.PacingHigh)
	if got := hgithub.PacingClassOf(mergeDecisionPriority(high)); got != hgithub.PacingHigh {
		t.Fatalf("high ctx → %s, want high kept", got)
	}
}

// During a caution window whose queue is deeper than the low class may wait
// (projectbluefin hive 2026-10-28: "class low, queue 5.454s ahead, limit
// 5s"), an approved green App PR the pass reaches is still read and merged:
// its status / check-run reads are not shed as housekeeping.
func TestSweepMergesGreenPRWhileCautionQueueExceedsLowBudget(t *testing.T) {
	var merged []int
	src := newFakePRSource("acme/widget", greenCachedPR(11))
	c, _ := newCachedSweepEngine(t, src, greenAPIPRs(11), &merged)
	// 5.5s ahead: over the 5s low budget, inside the 30s normal one. Later
	// slots are 1ms apart so the pass costs one wait, not a slot per call.
	t.Cleanup(hgithub.EngageRateLimitCautionForTest(5500*time.Millisecond, time.Millisecond))

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(merged) != 1 || merged[0] != 11 {
		t.Fatalf("merged=%v skipped=%d, want PR 11 merged during the caution window", merged, result.Skipped)
	}
}
