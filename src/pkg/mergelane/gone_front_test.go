package mergelane

import (
	"context"
	"strings"
	"testing"

	ghub "github.com/hivecommons/hive/pkg/github"
)

func (h *harness) releaseGone() (ghub.LaneMergeResult, error) {
	h.t.Helper()
	return h.lane.Merge(context.Background(), ghub.LaneMergeRequest{
		Repo: testRepo, Path: ghub.PRAuditPathSweep, ReleaseGoneFronts: true,
	})
}

// A front PR closed, merged, drafted or retargeted outside the lane gets no
// further round from its merge path (the sweeps list open PRs only), so it
// held every waiter until the front timeout. The sweep's front check releases
// it at once with prShape's reason and promotes the oldest waiter, without
// any GitHub write.
func TestGoneFrontLeavesOnTheSweepFrontCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*PullRequest)
		want  string
	}{
		{"closed", func(p *PullRequest) { p.Open = false }, "pull request is closed"},
		{"merged elsewhere", func(p *PullRequest) { p.Open, p.Merged = false, true }, "pull request is already merged"},
		{"draft", func(p *PullRequest) { p.Draft = true }, "pull request is a draft"},
		{"retargeted", func(p *PullRequest) { p.Base = "release" }, `pull request now targets "release", not "main"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGateHarness(t)
			p := h.f.addPR(1, "h1")
			h.f.addPR(2, "h2")
			for _, n := range []int{1, 2, 3} {
				h.acquire(n)
			}
			deadline := h.record().Front.DeadlineAt
			tc.apply(p)
			calls := len(h.f.calls)

			res, err := h.releaseGone()
			if err != nil {
				t.Fatalf("releaseGone: %v", err)
			}
			want := "front check: " + tc.want
			expectGate(t, res, ghub.LaneOutcomeLeft, "#1: "+want)
			rec := h.record()
			if rec.Front == nil || rec.Front.PR != 2 || !h.now.Before(deadline) {
				t.Fatalf("front = %+v at %s, want #2 promoted before deadline %s", rec.Front, h.now, deadline)
			}
			if len(rec.Waiting) != 1 || rec.Waiting[0].PR != 3 {
				t.Fatalf("waiting = %+v, want only #3", rec.Waiting)
			}
			if exit := rec.LastExit; exit == nil || exit.PR != 1 || exit.Reason != want || !exit.At.Equal(h.now) {
				t.Fatalf("last exit = %+v, want #1 with %q", exit, want)
			}
			exits := h.eventsFor(ActionFrontExit)
			if len(exits) != 1 || exits[0].PR != 1 || exits[0].Head != "h1" || exits[0].Reason != want {
				t.Fatalf("front exit audit = %+v", exits)
			}
			if enters := h.eventsFor(ActionFrontEnter); len(enters) != 2 || enters[1].PR != 2 {
				t.Fatalf("front enter audit = %+v, want #2 entering after #1", enters)
			}
			if got := h.f.calls[calls:]; len(got) != 1 || got[0] != "PullRequest" {
				t.Fatalf("GitHub calls = %v, want one PR read", got)
			}
			if len(h.f.merges) != 0 || len(h.f.updates) != 0 || len(h.f.comments) != 0 {
				t.Fatalf("front check wrote to GitHub: merges=%v updates=%v comments=%v", h.f.merges, h.f.updates, h.f.comments)
			}
			// The promoted front then runs its own round and merges.
			expectGate(t, h.gate(2), ghub.LaneOutcomeMerged, "merged")
		})
	}
}

// An open front is untouched, including one legitimately waiting on CI: the
// check reads the PR and changes nothing, so its deadline, extensions and
// pending-checks state stay as they were.
func TestOpenFrontStaysOnTheSweepFrontCheck(t *testing.T) {
	h := newGateHarness(t)
	p := h.f.addPR(1, "h1")
	h.f.setRun(p.Head, "build", "in_progress", "")
	h.acquire(1)
	h.acquire(2)
	expectOutcome(t, h.advance(1), OutcomeWaiting, "build (in_progress)")
	before := h.record()
	if !before.Front.ChecksPending {
		t.Fatalf("front = %+v, want it waiting on pending checks", before.Front)
	}
	events := len(h.events)

	res, err := h.releaseGone()
	if err != nil {
		t.Fatalf("releaseGone: %v", err)
	}
	if res.Outcome != ghub.LaneOutcomeWaiting || res.Reason != "" {
		t.Fatalf("result = %+v, want waiting with no reason", res)
	}
	after := h.record()
	if after.Front == nil || *after.Front != *before.Front || after.LastExit != nil || len(after.Waiting) != 1 {
		t.Fatalf("record changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if len(h.events) != events {
		t.Fatalf("open front check audited %+v", h.events[events:])
	}
}

// A front whose PR cannot be read stays (the timeout still bounds it), and
// the error is reported to the sweep.
func TestUnreadableFrontStaysOnTheSweepFrontCheck(t *testing.T) {
	h := newGateHarness(t)
	h.f.addPR(1, "h1").Open = false
	h.acquire(1)
	h.acquire(2)
	h.f.errs["PullRequest"] = errBoom
	res, err := h.releaseGone()
	if err == nil || !strings.Contains(err.Error(), "#1") {
		t.Fatalf("err = %v, want the PR read failure for #1", err)
	}
	if res.Outcome != ghub.LaneOutcomeWaiting || h.frontPR() != 1 || h.record().LastExit != nil {
		t.Fatalf("result = %+v, front = #%d; want #1 kept", res, h.frontPR())
	}
}

// The release is fenced on the front it read: a front that changed while the
// PR was being read (here released by its path, promoting #2) is not ended.
func TestGoneFrontCheckIsFencedOnTheFrontItRead(t *testing.T) {
	h := newGateHarness(t)
	h.f.addPR(1, "h1").Open = false
	for _, n := range []int{1, 2, 3} {
		h.acquire(n)
	}
	h.f.on("PullRequest", 1, func() {
		if err := h.lane.Release(testRepo, testBranch, 1, "released by its merge path"); err != nil {
			t.Fatalf("Release: %v", err)
		}
	})
	res, err := h.releaseGone()
	if err != nil || res.Outcome != ghub.LaneOutcomeWaiting {
		t.Fatalf("result = %+v, %v; want waiting, nothing left by the check", res, err)
	}
	if rec := h.record(); rec.Front == nil || rec.Front.PR != 2 || len(rec.Waiting) != 1 || rec.LastExit == nil || rec.LastExit.Reason != "released by its merge path" {
		t.Fatalf("record = %+v, want #2 at the front and #3 waiting", rec)
	}
}

// Every branch's front of the repo is checked; an empty lane and another
// repo's lane cost no GitHub call.
func TestGoneFrontCheckCoversEveryBranchOfTheRepo(t *testing.T) {
	h := newGateHarness(t)
	h.f.addPR(1, "h1").Open = false
	rel := h.f.addPR(5, "h5")
	rel.Base = "release"
	h.acquire(1)
	if _, err := h.lane.Acquire(testRepo, "release", 5, "sweep"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.lane.Acquire("acme/other", testBranch, 1, "sweep"); err != nil {
		t.Fatal(err)
	}
	res, err := h.releaseGone()
	if err != nil {
		t.Fatalf("releaseGone: %v", err)
	}
	expectGate(t, res, ghub.LaneOutcomeLeft, "#1: front check: pull request is closed")
	if h.frontPR() != 0 {
		t.Fatalf("main front = #%d, want empty", h.frontPR())
	}
	if rec, _, _ := h.lane.Snapshot(testRepo, "release"); rec.Front == nil || rec.Front.PR != 5 {
		t.Fatalf("release front = %+v, want #5 kept", rec.Front)
	}
	if rec, _, _ := h.lane.Snapshot("acme/other", testBranch); rec.Front == nil || rec.Front.PR != 1 {
		t.Fatalf("other repo front = %+v, want untouched", rec.Front)
	}
	if n := h.f.count("PullRequest"); n != 2 {
		t.Fatalf("PR reads = %d, want one per front of %s", n, testRepo)
	}

	empty := newGateHarness(t)
	if res, err := empty.releaseGone(); err != nil || res.Outcome != ghub.LaneOutcomeWaiting || len(empty.f.calls) != 0 {
		t.Fatalf("empty lane: %+v, %v, calls %v", res, err, empty.f.calls)
	}
}
