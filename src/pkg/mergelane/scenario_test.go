package mergelane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	ghub "github.com/hivecommons/hive/pkg/github"
)

// AC6, AC15 (R15), AC35: two green PRs into one branch. Only one is ever at the
// front; the second merges only after the first merged and it was brought up
// to date and passed its required checks on the new tip. Every decision is
// audited with PR, head, tip and reason.
func TestTwoGreenPRsMergeOneAtATimeAgainstTheNewTip(t *testing.T) {
	h := newHarness(t)
	h.f.addPR(1, "h1")
	h.f.addPR(2, "h2")
	expectOutcome(t, h.acquire(1), OutcomeFront, "")
	h.now = h.now.Add(time.Minute)
	expectOutcome(t, h.acquire(2), OutcomeDeferred, ReasonNotAtFront)
	expectOutcome(t, h.advance(2), OutcomeDeferred, ReasonNotAtFront)

	d := h.advance(1)
	expectOutcome(t, d, OutcomeMerged, "merged")
	if d.MergeSHA == "" || h.f.tip != d.MergeSHA {
		t.Fatalf("merge sha = %q, tip = %q", d.MergeSHA, h.f.tip)
	}
	if h.frontPR() != 2 {
		t.Fatalf("front = #%d after #1 merged, want #2", h.frontPR())
	}
	if len(h.f.updates) != 0 {
		t.Fatalf("#2 must not be updated while #1 is at the front (R16), got %v", h.f.updates)
	}

	// #2 was green on t0; one validation never covers two merges.
	expectOutcome(t, h.advance(2), OutcomeUpdated, "merged the tip in")
	if want := []string{"2:h2"}; fmt.Sprint(h.f.updates) != fmt.Sprint(want) {
		t.Fatalf("updates = %v, want %v (pinned to the evaluated head, AC25)", h.f.updates, want)
	}
	newHead := h.f.prs[2].Head
	expectOutcome(t, h.advance(2), OutcomeWaiting, "build (missing)")
	h.f.pass(newHead)
	expectOutcome(t, h.advance(2), OutcomeMerged, "")
	if want := []string{"1:h1", "2:" + newHead}; fmt.Sprint(h.f.merges) != fmt.Sprint(want) {
		t.Fatalf("merges = %v, want %v", h.f.merges, want)
	}
	if h.record().Front != nil {
		t.Fatal("the front must be empty after the last merge")
	}
	if len(h.alerts) != 0 {
		t.Fatalf("no alert expected, got %+v", h.alerts)
	}

	for _, e := range h.events {
		if e.Repo != testRepo || e.Branch != testBranch || e.PR <= 0 || e.Reason == "" || e.At.IsZero() {
			t.Fatalf("incomplete audit entry %+v", e)
		}
	}
	for _, action := range []string{ActionFrontEnter, ActionFrontExit, ActionDeferred, ActionUpdate, ActionMerge, ActionRefusal} {
		if len(h.eventsFor(action)) == 0 {
			t.Fatalf("no %s audit entry in %+v", action, h.events)
		}
	}
	for _, action := range []string{ActionUpdate, ActionMerge} {
		for _, e := range h.eventsFor(action) {
			if e.Head == "" || e.Tip == "" {
				t.Fatalf("%s audit entry lacks head or tip: %+v", action, e)
			}
		}
	}
}

// AC8, R17: PRs reach the front in the order they became eligible; one that
// leaves the front goes to the back.
func TestOldestEligibleFirstAndLeaversGoToTheBack(t *testing.T) {
	h := newHarness(t)
	for _, n := range []int{3, 1, 2} {
		h.f.addPR(n, fmt.Sprintf("h%d", n))
		h.acquire(n)
		h.now = h.now.Add(time.Minute)
	}
	h.f.setRun("h3", "build", "completed", "failure")
	expectOutcome(t, h.advance(3), OutcomeLeft, "failure")
	h.f.pass("h3")
	expectOutcome(t, h.acquire(3), OutcomeDeferred, ReasonNotAtFront)

	h.acquire(1)
	expectOutcome(t, h.advance(1), OutcomeMerged, "")
	h.acquire(2)
	expectOutcome(t, h.advance(2), OutcomeUpdated, "")
	h.f.pass(h.f.prs[2].Head)
	expectOutcome(t, h.advance(2), OutcomeMerged, "")
	var order []int
	for _, e := range h.eventsFor(ActionFrontEnter) {
		order = append(order, e.PR)
	}
	if want := []int{3, 1, 2, 3}; fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("front order = %v, want %v", order, want)
	}
}

// AC9, AC10, AC25: a PR GitHub reports "clean" that does not contain the tip
// is detected through the compare API and brought up to date with a merge
// update pinned to its head; the lane waits for the new head before checking.
func TestStalePRIsUpdatedWhateverGitHubReports(t *testing.T) {
	h := newHarness(t)
	h.f.autoLand = false
	p := h.f.addPR(1, "h1")
	h.f.contained["h1"] = "old-tip"
	h.acquire(1)

	expectOutcome(t, h.advance(1), OutcomeUpdated, "")
	if p.MergeableState != "clean" || len(h.f.merges) != 0 {
		t.Fatalf("a stale PR must not merge, merges = %v", h.f.merges)
	}
	if st := h.record().Front.Stage; st != StageUpdating {
		t.Fatalf("stage = %q, want %q", st, StageUpdating)
	}
	expectOutcome(t, h.advance(1), OutcomeWaiting, "waiting for the branch update")
	if len(h.f.updates) != 1 {
		t.Fatalf("the update must not be repeated while it is landing, got %v", h.f.updates)
	}
	h.f.land(1)
	expectOutcome(t, h.advance(1), OutcomeWaiting, "build (missing)")
	h.f.pass(p.Head)
	expectOutcome(t, h.advance(1), OutcomeMerged, "")
	if want := []string{"1:h1"}; fmt.Sprint(h.f.updates) != fmt.Sprint(want) {
		t.Fatalf("updates = %v, want %v", h.f.updates, want)
	}
	if h.f.count("ViewerCanUpdateBranch") != 0 || h.f.count("IssueCommentsContain") != 0 {
		t.Fatalf("same-repo update made fork-only calls: %v", h.f.calls)
	}
}

// AC24, R20, R21: stale fork PRs are updated only when GitHub says this token
// can update the branch and the owner enabled contributor base-sync. Otherwise
// the author gets one durable comment and the next PR reaches the front.
func TestForkPRWithoutBothUpdateGatesLeavesFrontWithOneAuthorComment(t *testing.T) {
	h := newHarness(t)
	p := h.f.addPR(1, "h1")
	p.FromFork = true
	h.f.contained["h1"] = "old-tip"
	h.f.canUpdate[1] = true
	h.f.setRun("h1", "build", "completed", "action_required")
	h.f.addPR(2, "h2")
	h.acquire(1)
	h.acquire(2)

	expectOutcome(t, h.advance(1), OutcomeLeft, "review.contributor_prs.base_sync is disabled")
	if h.frontPR() != 2 {
		t.Fatalf("front = #%d, want #2 after the fork leaves", h.frontPR())
	}
	if len(h.f.updates) != 0 {
		t.Fatalf("fork branch must not be updated without both gates, got %v", h.f.updates)
	}
	if got := len(h.f.comments[1]); got != 1 || !strings.Contains(h.f.comments[1][0], forkWaitCommentMarker) {
		t.Fatalf("comments = %#v, want one durable fork-wait marker", h.f.comments[1])
	}

	h.acquire(1)
	expectOutcome(t, h.advance(2), OutcomeMerged, "")
	expectOutcome(t, h.advance(1), OutcomeLeft, "review.contributor_prs.base_sync is disabled")
	if got := len(h.f.comments[1]); got != 1 {
		t.Fatalf("fork-wait comment repeated across turns: %#v", h.f.comments[1])
	}

	h.f.contained["h1"] = h.f.tip
	h.f.pass("h1")
	h.acquire(1)
	expectOutcome(t, h.advance(1), OutcomeMerged, "")
	if want := []string{"2:h2", "1:h1"}; fmt.Sprint(h.f.merges) != fmt.Sprint(want) {
		t.Fatalf("merges = %v, want %v", h.f.merges, want)
	}
}

// R20, AC25: when both fork gates allow it, the lane uses the same pinned
// merge-update path as same-repository PRs.
func TestForkPRWithBothUpdateGatesGetsPinnedMergeUpdate(t *testing.T) {
	h := newHarness(t)
	h.baseSync = true
	p := h.f.addPR(1, "h1")
	p.FromFork = true
	h.f.contained["h1"] = "old-tip"
	h.f.canUpdate[1] = true
	h.acquire(1)

	expectOutcome(t, h.advance(1), OutcomeUpdated, "pinned to head h1")
	if want := []string{"1:h1"}; fmt.Sprint(h.f.updates) != fmt.Sprint(want) {
		t.Fatalf("updates = %v, want %v", h.f.updates, want)
	}
	if h.f.count("ViewerCanUpdateBranch") != 1 {
		t.Fatalf("ViewerCanUpdateBranch calls = %d, want 1", h.f.count("ViewerCanUpdateBranch"))
	}
	if len(h.f.comments[1]) != 0 {
		t.Fatalf("allowed fork update should not comment, got %#v", h.f.comments[1])
	}
}

// AC24: an up-to-date fork PR whose required workflow is action_required is not
// merged; after the author gets it passing, the same head can merge normally.
func TestForkPRActionRequiredDoesNotMergeUntilChecksPass(t *testing.T) {
	h := newHarness(t)
	p := h.f.addPR(1, "h1")
	p.FromFork = true
	h.f.setRun("h1", "build", "completed", "action_required")
	h.acquire(1)

	expectOutcome(t, h.advance(1), OutcomeLeft, "action_required")
	if len(h.f.merges) != 0 || len(h.f.updates) != 0 || len(h.f.comments[1]) != 0 {
		t.Fatalf("merges=%v updates=%v comments=%#v, want no lane mutation except leaving", h.f.merges, h.f.updates, h.f.comments[1])
	}

	h.f.pass("h1")
	h.acquire(1)
	expectOutcome(t, h.advance(1), OutcomeMerged, "")
}

// AC9: if the head changed since it was evaluated, the update is not made;
// the new head is evaluated and the PR keeps the front.
func TestUpdateRefusedWhenHeadChanged(t *testing.T) {
	h := newHarness(t)
	p := h.f.addPR(1, "h1")
	h.f.contained["h1"] = "old-tip"
	h.acquire(1)
	h.f.on("UpdateBranch", 1, func() { p.Head = "h1b" })

	expectOutcome(t, h.advance(1), OutcomeWaiting, "head changed before the branch update")
	if p.Head != "h1b" || h.frontPR() != 1 {
		t.Fatalf("head = %q, front = #%d; the PR keeps the front and its new head", p.Head, h.frontPR())
	}

	h.f.errs["UpdateBranch"] = errBoom
	expectOutcome(t, h.advance(1), OutcomeWaiting, "branch update failed")
	if len(h.f.merges) != 0 {
		t.Fatalf("merges = %v", h.f.merges)
	}
}

// AC11, R12: the target branch moving before the final re-check stops the
// merge; the PR is brought up to date again and the reason is audited.
func TestMovedBranchStopsTheMerge(t *testing.T) {
	h := newHarness(t)
	h.f.addPR(1, "h1")
	h.acquire(1)
	h.f.on("BranchTip", 2, func() { h.f.tip = "t1" })

	expectOutcome(t, h.advance(1), OutcomeWaiting, "target branch moved from t0 to t1")
	if len(h.f.merges) != 0 {
		t.Fatalf("no merge call may be made, got %v", h.f.merges)
	}
	refusals := h.eventsFor(ActionRefusal)
	if len(refusals) != 1 || !strings.Contains(refusals[0].Reason, "target branch moved") || refusals[0].Head != "h1" {
		t.Fatalf("refusal audit = %+v", refusals)
	}
	expectOutcome(t, h.advance(1), OutcomeUpdated, "")
	if want := []string{"1:h1"}; fmt.Sprint(h.f.updates) != fmt.Sprint(want) {
		t.Fatalf("updates = %v, want %v", h.f.updates, want)
	}
}

// AC12, R11: a head that changes during validation (or at the merge, 409)
// is never merged; the new head is validated from the start.
func TestChangedHeadStopsTheMerge(t *testing.T) {
	h := newHarness(t)
	p := h.f.addPR(1, "h1")
	h.acquire(1)
	h.f.on("PullRequest", 2, func() {
		p.Head = "h1b"
		h.f.contained["h1b"] = h.f.tip
		h.f.pass("h1b")
	})
	expectOutcome(t, h.advance(1), OutcomeWaiting, "head changed from h1 to h1b")
	if len(h.f.merges) != 0 {
		t.Fatalf("no merge call may be made, got %v", h.f.merges)
	}

	h.f.on("Merge", 1, func() { p.Head = "h1c" })
	expectOutcome(t, h.advance(1), OutcomeWaiting, "merge refused: head moved")
	if p.Merged || h.record().Front.Stage != StageValidating {
		t.Fatalf("merged=%v stage=%q after a 409", p.Merged, h.record().Front.Stage)
	}
	h.f.contained["h1c"] = h.f.tip
	h.f.pass("h1c")
	expectOutcome(t, h.advance(1), OutcomeMerged, "")
	if want := []string{"1:h1b", "1:h1c"}; fmt.Sprint(h.f.merges) != fmt.Sprint(want) {
		t.Fatalf("merge calls = %v, want %v", h.f.merges, want)
	}
	found := false
	for _, e := range h.eventsFor(ActionRefusal) {
		found = found || strings.Contains(e.Reason, "head changed during validation")
	}
	if !found {
		t.Fatalf("head change during validation not audited: %+v", h.events)
	}
}

// AC13: a hold, do-not-merge, exempt or pause label (or a repo pause) added
// while checks run stops the merge, even though the PR was eligible when
// validation started.
func TestHoldAddedDuringValidationStopsTheMerge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(h *harness, p *PullRequest)
		want  string
	}{
		{"hold", func(_ *harness, p *PullRequest) { p.Labels = []string{"hold"} }, "hold label"},
		{"on-hold", func(_ *harness, p *PullRequest) { p.Labels = []string{"on-hold"} }, "hold label"},
		{"do-not-merge", func(_ *harness, p *PullRequest) { p.Labels = []string{"do-not-merge"} }, "do-not-merge"},
		{"pause label", func(_ *harness, p *PullRequest) { p.Labels = []string{"hive-pause/hive-1"} }, "pause label"},
		{"exempt", func(h *harness, p *PullRequest) {
			p.Labels = []string{"keep-open"}
			h.blocked = func(_ string, labels []string) string {
				if len(labels) > 0 && labels[0] == "keep-open" {
					return "an exempt label is present"
				}
				return ""
			}
		}, "exempt label"},
		{"repo paused", func(h *harness, _ *PullRequest) { h.paused = true }, "repository is paused"},
	} {
		t.Run(tc.name+"/between rounds", func(t *testing.T) {
			h := newHarness(t)
			p := h.f.addPR(1, "h1")
			h.f.setRun("h1", "build", "in_progress", "")
			h.acquire(1)
			expectOutcome(t, h.advance(1), OutcomeWaiting, "build (in_progress)")
			tc.apply(h, p)
			h.f.pass("h1")
			expectOutcome(t, h.advance(1), OutcomeLeft, tc.want)
			if len(h.f.merges) != 0 {
				t.Fatalf("no merge call may be made, got %v", h.f.merges)
			}
		})
		t.Run(tc.name+"/at the final re-check", func(t *testing.T) {
			h := newHarness(t)
			p := h.f.addPR(1, "h1")
			h.acquire(1)
			h.f.on("PullRequest", 2, func() { tc.apply(h, p) })
			expectOutcome(t, h.advance(1), OutcomeLeft, "final re-check: ")
			if len(h.f.merges) != 0 {
				t.Fatalf("no merge call may be made, got %v", h.f.merges)
			}
		})
	}
}

// AC14, AC16: a required check that never starts never passes; an unknown or
// empty required set never merges. The PR leaves only by timing out.
func TestMissingOrUnknownRequiredChecksNeverPass(t *testing.T) {
	h := newHarness(t)
	h.f.addPR(1, "h1")
	delete(h.f.runs, "h1")
	h.acquire(1)
	for i := 0; i < 5; i++ {
		expectOutcome(t, h.advance(1), OutcomeWaiting, "build (missing)")
		h.now = h.now.Add(10 * time.Minute)
	}
	h.now = h.now.Add(11 * time.Minute)
	expectOutcome(t, h.advance(1), OutcomeDeferred, ReasonNotAtFront)
	if rec := h.record(); rec.Front != nil || rec.LastExit == nil || !strings.Contains(rec.LastExit.Reason, "front timeout") {
		t.Fatalf("lane = %+v, want a timeout exit", rec)
	}
	if len(h.f.merges) != 0 {
		t.Fatalf("merges = %v", h.f.merges)
	}

	for _, tc := range []struct {
		name  string
		rules func(f *fakeGH)
		want  string
	}{
		{"unknown", func(f *fakeGH) { f.errs["RequiredChecks"] = errBoom }, "required checks unknown, no merge: boom"},
		{"empty", func(f *fakeGH) { f.required.Required = map[string]bool{} }, "required check set is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.f.addPR(1, "h1")
			tc.rules(h.f)
			h.acquire(1)
			expectOutcome(t, h.advance(1), OutcomeWaiting, tc.want)
			if len(h.f.merges) != 0 {
				t.Fatalf("merges = %v", h.f.merges)
			}
		})
	}
}

// AC17, R4: other changes while checks run stop the merge.
func TestOtherChangesDuringValidationStopTheMerge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		apply   func(h *harness, p *PullRequest)
		outcome Outcome
		want    string
	}{
		{"closed", func(_ *harness, p *PullRequest) { p.Open = false }, OutcomeLeft, "pull request is closed"},
		{"draft", func(_ *harness, p *PullRequest) { p.Draft = true }, OutcomeLeft, "pull request is a draft"},
		{"retargeted", func(_ *harness, p *PullRequest) { p.Base = "release" }, OutcomeLeft, `now targets "release"`},
		{"merged elsewhere", func(_ *harness, p *PullRequest) { p.Merged = true }, OutcomeLeft, "already merged"},
		{"switched to direct", func(h *harness, _ *PullRequest) { h.strategy = "direct" }, OutcomeLeft, "validation ended without a merge"},
		{"auto-merge off", func(h *harness, _ *PullRequest) { h.autoMerge = false }, OutcomeLeft, "auto-merge is not allowed"},
		{"authorization lost", func(h *harness, _ *PullRequest) { h.authErr = errors.New("approval does not match head") }, OutcomeWaiting, "authorization no longer holds"},
		{"new required check", func(h *harness, _ *PullRequest) { h.f.required.Required["e2e"] = true }, OutcomeWaiting, "e2e (missing)"},
		{"required check failed", func(h *harness, _ *PullRequest) { h.f.setRun("h1", "build", "completed", "cancelled") }, OutcomeLeft, "cancelled"},
		{"became unmergeable", func(_ *harness, p *PullRequest) { p.MergeableState = "dirty" }, OutcomeLeft, "merge conflicts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := h.f.addPR(1, "h1")
			h.acquire(1)
			h.f.on("PullRequest", 2, func() { tc.apply(h, p) })
			expectOutcome(t, h.advance(1), tc.outcome, tc.want)
			if len(h.f.merges) != 0 {
				t.Fatalf("no merge call may be made, got %v", h.f.merges)
			}
		})
	}

	t.Run("direct before validation needs no GitHub call", func(t *testing.T) {
		h := newHarness(t)
		h.f.addPR(1, "h1")
		h.acquire(1)
		h.strategy = "direct"
		expectOutcome(t, h.advance(1), OutcomeLeft, "validation ended without a merge")
		if len(h.f.calls) != 0 {
			t.Fatalf("GitHub calls = %v, want none", h.f.calls)
		}
	})
}

// AC18, AC20: skipped and neutral pass like success; failure, cancelled,
// timed_out and action_required fail, the PR leaves the front, the next one
// starts and PRs further back are not updated.
func TestRequiredCheckClassification(t *testing.T) {
	for _, tc := range []struct {
		conclusion string
		outcome    Outcome
	}{
		{"success", OutcomeMerged},
		{"skipped", OutcomeMerged},
		{"neutral", OutcomeMerged},
		{"failure", OutcomeLeft},
		{"cancelled", OutcomeLeft},
		{"timed_out", OutcomeLeft},
		{"action_required", OutcomeLeft},
	} {
		t.Run(tc.conclusion, func(t *testing.T) {
			h := newHarness(t)
			h.f.addPR(1, "h1")
			h.f.setRun("h1", "build", "completed", tc.conclusion)
			for _, n := range []int{2, 3} {
				h.f.addPR(n, fmt.Sprintf("h%d", n))
				h.f.contained[fmt.Sprintf("h%d", n)] = "old-tip"
			}
			for _, n := range []int{1, 2, 3} {
				h.acquire(n)
			}
			expectOutcome(t, h.advance(1), tc.outcome, "")
			if h.frontPR() != 2 {
				t.Fatalf("front = #%d, want #2 to start next", h.frontPR())
			}
			if len(h.f.updates) != 0 {
				t.Fatalf("no PR behind the front may be updated, got %v", h.f.updates)
			}
			expectOutcome(t, h.advance(2), OutcomeUpdated, "")
			expectOutcome(t, h.advance(3), OutcomeDeferred, ReasonNotAtFront)
			for _, u := range h.f.updates {
				if strings.HasPrefix(u, "3:") {
					t.Fatalf("#3 was updated while waiting: %v", h.f.updates)
				}
			}
		})
	}
}

// AC19: uncertainty (mergeability unknown, a failed API call) blocks the
// merge for that round with a reason.
func TestUncertaintyBlocksTheRound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(h *harness, p *PullRequest)
		want  string
	}{
		{"mergeability unknown", func(_ *harness, p *PullRequest) { p.MergeableState = "unknown" }, "mergeability is not yet known"},
		{"mergeability blocked", func(_ *harness, p *PullRequest) { p.MergeableState = "blocked" }, "not mergeable"},
		{"pr read fails", func(h *harness, _ *PullRequest) { h.f.errs["PullRequest"] = errBoom }, "reading the pull request failed"},
		{"tip read fails", func(h *harness, _ *PullRequest) { h.f.errs["BranchTip"] = errBoom }, "reading the target branch tip failed"},
		{"compare fails", func(h *harness, _ *PullRequest) { h.f.errs["HeadContains"] = errBoom }, "comparing the head"},
		{"check runs fail", func(h *harness, _ *PullRequest) { h.f.errs["CheckRuns"] = errBoom }, "reading check results failed"},
		{"final pr read fails", func(h *harness, _ *PullRequest) {
			h.f.on("PullRequest", 2, func() { h.f.errs["PullRequest"] = errBoom })
		}, "final re-check: reading the pull request failed"},
		{"final mergeability unknown", func(h *harness, p *PullRequest) {
			h.f.on("PullRequest", 2, func() { p.MergeableState = "unknown" })
		}, "final re-check: mergeability is not yet known"},
		{"final tip read fails", func(h *harness, _ *PullRequest) {
			h.f.on("BranchTip", 2, func() { h.f.errs["BranchTip"] = errBoom })
		}, "final re-check: reading the target branch tip failed"},
		{"final required checks unknown", func(h *harness, _ *PullRequest) {
			h.f.on("RequiredChecks", 2, func() { h.f.errs["RequiredChecks"] = errBoom })
		}, "final re-check: required checks unknown"},
		{"merge call fails", func(h *harness, _ *PullRequest) { h.f.errs["Merge"] = errBoom }, "merge call failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := h.f.addPR(1, "h1")
			h.acquire(1)
			tc.apply(h, p)
			expectOutcome(t, h.advance(1), OutcomeWaiting, tc.want)
			if p.Merged || h.frontPR() != 1 {
				t.Fatalf("merged=%v front=#%d; uncertainty must keep the front without merging", p.Merged, h.frontPR())
			}
			if refusals := h.eventsFor(ActionRefusal); len(refusals) == 0 || !strings.Contains(refusals[len(refusals)-1].Reason, tc.want) {
				t.Fatalf("refusal not audited: %+v", refusals)
			}
		})
	}
}

// AC21, R18: a stuck front times out with a reason and the next PR starts; a
// per-repo override applies; the timeout restarts on every branch update.
func TestFrontTimeout(t *testing.T) {
	t.Run("hive-wide default", func(t *testing.T) {
		h := newHarness(t)
		h.f.addPR(1, "h1")
		h.f.addPR(2, "h2")
		h.f.setRun("h1", "build", "queued", "")
		h.acquire(1)
		h.acquire(2)
		h.now = h.now.Add(59 * time.Minute)
		expectOutcome(t, h.advance(1), OutcomeWaiting, "build (queued)")
		h.now = h.now.Add(2 * time.Minute)
		expectOutcome(t, h.advance(1), OutcomeDeferred, ReasonNotAtFront)
		rec := h.record()
		if rec.Front == nil || rec.Front.PR != 2 {
			t.Fatalf("front = %+v, want #2 after the timeout", rec.Front)
		}
		if rec.LastExit == nil || rec.LastExit.PR != 1 || !strings.Contains(rec.LastExit.Reason, "within 1h0m0s") {
			t.Fatalf("last exit = %+v", rec.LastExit)
		}
		expectOutcome(t, h.acquire(1), OutcomeDeferred, ReasonNotAtFront)
	})

	t.Run("per-repo override restarted by an update", func(t *testing.T) {
		h := newHarness(t)
		h.override = 10 * time.Minute
		h.f.addPR(1, "h1")
		h.f.contained["h1"] = "old-tip"
		h.acquire(1)
		h.now = h.now.Add(8 * time.Minute)
		expectOutcome(t, h.advance(1), OutcomeUpdated, "")
		h.now = h.now.Add(7 * time.Minute)
		expectOutcome(t, h.advance(1), OutcomeWaiting, "missing")
		h.now = h.now.Add(4 * time.Minute)
		expectOutcome(t, h.advance(1), OutcomeDeferred, ReasonNotAtFront)
		exits := h.eventsFor(ActionFrontExit)
		if len(exits) != 1 || !strings.Contains(exits[0].Reason, "within 10m0s") {
			t.Fatalf("exit audit = %+v", exits)
		}
	})
}

// AC22, R19: after a restart there is still one front per branch, the
// in-progress validation is reset and repeated in full before any merge, and
// a validation from before the restart is fenced.
func TestRestartRepeatsValidationAndFencesTheOldOne(t *testing.T) {
	h := newHarness(t)
	h.f.addPR(1, "h1")
	h.f.addPR(2, "h2")
	h.f.setRun("h1", "build", "in_progress", "")
	h.acquire(1)
	h.acquire(2)
	expectOutcome(t, h.advance(1), OutcomeWaiting, "")
	before := h.record().Front

	h.restart()
	rec := h.record()
	if rec.Front == nil || rec.Front.PR != 1 || len(rec.Waiting) != 1 || rec.Waiting[0].PR != 2 {
		t.Fatalf("lane after restart = %+v, want #1 front and #2 waiting", rec)
	}
	if rec.Front.Epoch <= before.Epoch || rec.Front.EvaluatedHead != "" || rec.Front.Stage != StageValidating {
		t.Fatalf("front after restart = %+v (before %+v), want a fenced, reset validation", rec.Front, before)
	}
	if len(h.eventsFor(ActionRestart)) != 1 {
		t.Fatalf("restart not audited: %+v", h.events)
	}

	h.f.pass("h1")
	h.f.calls = nil
	expectOutcome(t, h.advance(1), OutcomeMerged, "")
	for _, call := range []string{"PullRequest", "BranchTip", "HeadContains", "RequiredChecks", "CheckRuns"} {
		if h.f.count(call) == 0 {
			t.Fatalf("no %s before the merge after a restart; calls = %v", call, h.f.calls)
		}
	}

	t.Run("stale holder is fenced", func(t *testing.T) {
		h := newHarness(t)
		h.f.addPR(1, "h1")
		h.acquire(1)
		old := h.lane
		h.f.on("PullRequest", 2, func() { h.restart() })
		_, err := old.Advance(context.Background(), testRepo, testBranch, 1, h.authorize)
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("err = %v, want ErrFenced", err)
		}
		if len(h.f.merges) != 0 {
			t.Fatalf("a fenced validation must not merge, got %v", h.f.merges)
		}
	})

	t.Run("release during a round is fenced", func(t *testing.T) {
		h := newHarness(t)
		h.f.addPR(1, "h1")
		h.f.setRun("h1", "build", "in_progress", "")
		h.acquire(1)
		h.f.on("PullRequest", 1, func() {
			if err := h.lane.Release(testRepo, testBranch, 1, "no longer eligible"); err != nil {
				t.Fatalf("Release: %v", err)
			}
		})
		_, err := h.lane.Advance(context.Background(), testRepo, testBranch, 1, h.authorize)
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("err = %v, want ErrFenced", err)
		}
	})

	t.Run("leave after release is fenced", func(t *testing.T) {
		h := newHarness(t)
		h.f.addPR(1, "h1")
		h.f.setRun("h1", "build", "completed", "failure")
		h.acquire(1)
		h.f.on("PullRequest", 1, func() { _ = h.lane.Release(testRepo, testBranch, 1, "gone") })
		d, err := h.lane.Advance(context.Background(), testRepo, testBranch, 1, h.authorize)
		if !errors.Is(err, ErrFenced) || d.Outcome != OutcomeLeft {
			t.Fatalf("d=%+v err=%v, want a fenced leave", d, err)
		}
	})
}

// AC23, R13: a commit landing between the final re-check and the merge is
// detected after the merge and alerted on, naming the merged PR and the
// unexpected commit and its PR.
func TestMergeOnUnexpectedTipRaisesAlert(t *testing.T) {
	h := newHarness(t)
	h.f.addPR(1, "h1")
	h.acquire(1)
	h.f.on("Merge", 1, func() {
		h.f.tip = "x1"
		h.f.commitPRs["x1"] = []int{77}
	})
	d := h.advance(1)
	expectOutcome(t, d, OutcomeMerged, "")
	if len(h.alerts) != 1 {
		t.Fatalf("alerts = %+v, want one", h.alerts)
	}
	a := h.alerts[0]
	if a.PR != 1 || a.UnexpectedCommit != "x1" || fmt.Sprint(a.UnexpectedPRs) != "[77]" || a.ValidatedTip != "t0" || a.MergeSHA != d.MergeSHA {
		t.Fatalf("alert = %+v", a)
	}
	if !strings.Contains(a.Reason, "PR #1") || !strings.Contains(a.Reason, "x1") || !strings.Contains(a.Reason, "PR #77") {
		t.Fatalf("alert reason = %q", a.Reason)
	}
	if len(h.eventsFor(ActionAlert)) != 1 {
		t.Fatalf("alert not audited: %+v", h.events)
	}

	for _, tc := range []struct {
		name  string
		apply func(f *fakeGH)
		want  string
	}{
		{"parents unreadable", func(f *fakeGH) { f.errs["CommitParents"] = errBoom }, "could not verify"},
		{"no parent", func(f *fakeGH) {
			f.on("CommitParents", 1, func() { f.parents = map[string][]string{} })
		}, "has no parent commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.f.addPR(1, "h1")
			h.acquire(1)
			tc.apply(h.f)
			expectOutcome(t, h.advance(1), OutcomeMerged, "")
			if len(h.alerts) != 1 || !strings.Contains(h.alerts[0].Reason, tc.want) {
				t.Fatalf("alerts = %+v, want %q", h.alerts, tc.want)
			}
		})
	}
}

// #11023: the front PR is behind, the lane updates it, the required check
// fails on the new head and GitHub reports it "blocked". The sweep rejects it
// as not mergeable before the lane gate, yet hands it to the lane front-only;
// it leaves the front at once with the check failure and the next waiter
// reaches the front on the same tick, not after the front timeout.
func TestRedFrontRejectedByItsPathLeavesWithTheCheckFailure(t *testing.T) {
	h := newGateHarness(t)
	p := h.f.addPR(1, "h1")
	h.f.contained["h1"] = "old-tip"
	h.f.addPR(2, "h2")

	expectGate(t, h.gate(1), ghub.LaneOutcomeUpdated, "merged the tip in")
	h.now = h.now.Add(time.Minute)
	expectGate(t, h.gate(2), ghub.LaneOutcomeDeferred, ReasonNotAtFront)

	h.f.setRun(p.Head, "build", "completed", "failure")
	p.MergeableState = "blocked"
	h.now = h.now.Add(time.Minute)
	res := h.frontOnly(1)
	expectGate(t, res, ghub.LaneOutcomeLeft, fmt.Sprintf("required check %q finished as failure on head %s", "build", short(p.Head)))
	if h.frontPR() != 2 {
		t.Fatalf("front = #%d after the red front left, want #2", h.frontPR())
	}
	if exit := h.record().LastExit; exit == nil || exit.PR != 1 || strings.Contains(exit.Reason, "front timeout") {
		t.Fatalf("last exit = %+v, want #1 leaving with the check failure", exit)
	}
	expectGate(t, h.gate(2), ghub.LaneOutcomeMerged, "merged")
	if want := []string{"2:h2"}; fmt.Sprint(h.f.merges) != fmt.Sprint(want) {
		t.Fatalf("merges = %v, want %v", h.f.merges, want)
	}
}

// A front timeout says what the lane last knew instead of claiming the
// required checks did not finish.
func TestFrontTimeoutReasonSaysWhatWasLastKnown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *harness)
		want  string
	}{
		{"updated head never evaluated", func(t *testing.T, h *harness) {
			h.f.contained["h1"] = "old-tip"
			expectOutcome(t, h.advance(1), OutcomeUpdated, "")
		}, "the head after the branch update was never evaluated"},
		{"waiting for checks", func(t *testing.T, h *harness) {
			h.f.setRun("h1", "build", "in_progress", "")
			expectOutcome(t, h.advance(1), OutcomeWaiting, "build (in_progress)")
		}, "last known state: waiting for required checks on head h1: build (in_progress)"},
		{"never evaluated", func(*testing.T, *harness) {}, "no evaluation of the front reached a verdict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.f.addPR(1, "h1")
			h.acquire(1)
			tc.setup(t, h)
			h.now = h.now.Add(DefaultFrontTimeout + time.Minute)
			h.acquire(2)
			exit := h.record().LastExit
			if exit == nil || exit.PR != 1 || !strings.Contains(exit.Reason, "front timeout after 1h0m0s: "+tc.want) || strings.Contains(exit.Reason, "did not finish") {
				t.Fatalf("last exit = %+v, want a timeout naming %q", exit, tc.want)
			}
		})
	}
}
