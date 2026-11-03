package mergelane

import (
	"strings"
	"testing"

	ghub "github.com/hivecommons/hive/pkg/github"
)

// #12955, #10884 R17/R18/R31: a block that survives passing checks on an
// up-to-date head must not occupy the front until its timeout. This also
// covers the front-only path used when a sweep rejects the blocked PR.
func TestBlockedGreenFrontLeavesPromptly(t *testing.T) {
	for _, phase := range []string{"initial", "final", "front-only"} {
		t.Run(phase, func(t *testing.T) {
			h := newGateHarness(t)
			p := h.f.addPR(1, "h1")
			for _, n := range []int{1, 2, 3} {
				h.acquire(n)
			}
			h.f.addPR(2, "h2")
			deadline := h.record().Front.DeadlineAt
			if phase == "final" {
				h.f.on("PullRequest", 2, func() { p.MergeableState = "blocked" })
			} else {
				p.MergeableState = "blocked"
			}

			const reason = "final re-check: pull request is blocked despite passing required checks on an up-to-date head"
			var exitReason string
			if phase == "front-only" {
				res := h.frontOnly(1)
				expectGate(t, res, ghub.LaneOutcomeLeft, reason)
				exitReason = res.Reason
			} else {
				d := h.advance(1)
				expectOutcome(t, d, OutcomeLeft, reason)
				exitReason = d.Reason
			}
			if !h.now.Before(deadline) || h.frontPR() != 2 {
				t.Fatalf("front = #%d at %s, want #2 before deadline %s", h.frontPR(), h.now, deadline)
			}
			if len(h.f.merges) != 0 || len(h.f.updates) != 0 {
				t.Fatalf("blocked front made writes: merges=%v updates=%v", h.f.merges, h.f.updates)
			}
			if exit := h.record().LastExit; exit == nil || exit.PR != 1 || exit.Reason != exitReason || !exit.At.Equal(h.now) {
				t.Fatalf("last exit = %+v, want the blocked front's immediate exit", exit)
			}
			events := h.eventsFor(ActionFrontExit)
			if len(events) != 1 || events[0].PR != 1 || events[0].Head != "h1" || events[0].Tip != "t0" || events[0].Reason != exitReason {
				t.Fatalf("front exit audit = %+v", events)
			}
			// A returning PR must go behind existing waiters, and cannot
			// prevent the newly promoted green PR from merging.
			d := h.acquire(1)
			expectOutcome(t, d, OutcomeDeferred, ReasonNotAtFront)
			if d.Position != 2 {
				t.Fatalf("returning PR position = %d, want 2", d.Position)
			}
			expectGate(t, h.gate(2), ghub.LaneOutcomeMerged, "merged")
			if len(h.f.merges) != 1 || h.f.merges[0] != "2:h2" {
				t.Fatalf("merges = %v, want only #2", h.f.merges)
			}
		})
	}
}

// The initial PR snapshot can still say blocked when the last required check
// passes during this round. Only the live re-check may interpret that block.
func TestBlockedFrontRechecksAfterChecksPass(t *testing.T) {
	for _, state := range []string{"clean", "unknown"} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			p := h.f.addPR(1, "h1")
			p.MergeableState = "blocked"
			h.f.setRun(p.Head, "build", "in_progress", "")
			h.acquire(1)
			h.acquire(2)
			h.f.on("CheckRuns", 1, func() {
				h.f.pass(p.Head)
				p.MergeableState = state
			})

			d := h.advance(1)
			if state == "unknown" {
				expectOutcome(t, d, OutcomeWaiting, "final re-check: mergeability is not yet known")
				if h.frontPR() != 1 || h.record().LastExit != nil || len(h.eventsFor(ActionFrontExit)) != 0 || len(h.f.merges) != 0 {
					t.Fatalf("recomputing mergeability must retain the front: record=%+v calls=%v", h.record(), h.f.calls)
				}
				p.MergeableState = "clean"
				d = h.advance(1)
			}
			expectOutcome(t, d, OutcomeMerged, "merged")
			if h.frontPR() != 2 || len(h.f.merges) != 1 || h.f.merges[0] != "1:h1" || len(h.f.updates) != 0 {
				t.Fatalf("newly green front must merge before its waiter: record=%+v calls=%v", h.record(), h.f.calls)
			}
			for _, event := range h.eventsFor(ActionFrontExit) {
				if strings.Contains(event.Reason, "no remaining lane action") {
					t.Fatalf("stale block recorded as an unresolvable exit: %+v", event)
				}
			}
		})
	}
}

// A blocked state alone does not establish an unresolvable block. Required
// checks must pass, including when their state changes at the final re-check.
func TestBlockedFrontWaitsForRequiredChecks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*fakeGH)
		want  string
	}{
		{"queued", func(f *fakeGH) { f.setRun("h1", "build", "queued", "") }, "build (queued)"},
		{"running", func(f *fakeGH) { f.setRun("h1", "build", "in_progress", "") }, "build (in_progress)"},
		{"missing", func(f *fakeGH) { delete(f.runs, "h1") }, "build (missing)"},
		{"unknown rules", func(f *fakeGH) { f.errs["RequiredChecks"] = errBoom }, "required checks unknown"},
		{"empty rules", func(f *fakeGH) { f.required.Required = nil }, "required check set is empty"},
		{"unreadable runs", func(f *fakeGH) { f.errs["CheckRuns"] = errBoom }, "reading check results failed"},
	} {
		for _, phase := range []string{"initial", "final"} {
			t.Run(tc.name+"/"+phase, func(t *testing.T) {
				h := newHarness(t)
				p := h.f.addPR(1, "h1")
				h.acquire(1)
				h.acquire(2)
				apply := func() {
					p.MergeableState = "blocked"
					tc.apply(h.f)
				}
				if phase == "final" {
					h.f.on("PullRequest", 2, apply)
				} else {
					apply()
				}
				expectOutcome(t, h.advance(1), OutcomeWaiting, tc.want)
				if h.frontPR() != 1 || h.record().LastExit != nil || len(h.f.merges) != 0 || len(h.f.updates) != 0 {
					t.Fatalf("uncertain checks must retain the front without writes: record=%+v calls=%v", h.record(), h.f.calls)
				}
			})
		}
	}
}

func TestBlockedFrontStillUpdatesStaleHead(t *testing.T) {
	h := newHarness(t)
	p := h.f.addPR(1, "h1")
	p.MergeableState = "blocked"
	h.f.contained[p.Head] = "old-tip"
	h.f.autoLand = false
	h.acquire(1)
	h.acquire(2)

	expectOutcome(t, h.advance(1), OutcomeUpdated, "merged the tip in")
	expectOutcome(t, h.advance(1), OutcomeWaiting, "waiting for the branch update")
	h.f.land(1)
	expectOutcome(t, h.advance(1), OutcomeWaiting, "build (missing)")
	h.f.pass(p.Head)
	expectOutcome(t, h.advance(1), OutcomeLeft, "no remaining lane action")
	if h.frontPR() != 2 || len(h.f.updates) != 1 || len(h.f.merges) != 0 {
		t.Fatalf("front=%d updates=%v merges=%v", h.frontPR(), h.f.updates, h.f.merges)
	}
}

func TestFinalBlockedFrontRevalidatesHeadAndTip(t *testing.T) {
	for _, moved := range []string{"head", "tip"} {
		t.Run(moved, func(t *testing.T) {
			h := newHarness(t)
			p := h.f.addPR(1, "h1")
			h.acquire(1)
			h.f.on("PullRequest", 2, func() {
				p.MergeableState = "blocked"
				if moved == "head" {
					p.Head = "h1-new"
				} else {
					h.f.tip = "t1"
				}
			})
			d := h.advance(1)
			expectOutcome(t, d, OutcomeWaiting, "final re-check:")
			if strings.Contains(d.Reason, "no remaining lane action") || h.frontPR() != 1 || len(h.f.merges) != 0 {
				t.Fatalf("changed %s must be revalidated, decision=%+v calls=%v", moved, d, h.f.calls)
			}
		})
	}
}
