package automerge

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Stale-red recovery for adopted PRs (hivecommons/hive#14500).

const recoveryReason = "required-check-failing:coverage"

// recoveryHarness is an in-place adoption of PR 7 at head "aaa" whose only
// blocker is the red `coverage` check, with the 074 dwell already elapsed.
func recoveryHarness(t *testing.T) (*adoptHarness, string, func()) {
	t.Helper()
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	key := adoptInPlaceAt(h, "aaa", recoveryReason)
	h.adopt.compareBase = "base1"
	h.clock.advance(adoptedDwell + time.Minute)
	return h, key, done
}

func TestAdoptedStaleRedRecovery(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(a *adoptAPI)
		wantUpdates int
		wantReruns  int
		wantStage   stagelabels.Stage
		wantAttempt string
	}{
		{
			name: "behind with the check green on base updates the branch and holds 074",
			setup: func(a *adoptAPI) {
				a.compareBehind = 15
				a.baseChecks = map[string]string{"coverage": "success", "build": "success"}
			},
			wantUpdates: 1, wantStage: stagelabels.AdoptedByHive, wantAttempt: "aaa@base1",
		},
		{
			name: "current head with the check green on base re-runs the failed jobs",
			setup: func(a *adoptAPI) {
				a.baseChecks = map[string]string{"coverage": "success"}
				a.failedRuns = []int64{41, 42}
			},
			wantReruns: 2, wantStage: stagelabels.AdoptedByHive, wantAttempt: "aaa@base1",
		},
		{
			name: "base does not run the check but is fully green: still recoverable",
			setup: func(a *adoptAPI) {
				a.compareBehind = 3
				a.baseChecks = map[string]string{"build": "success"}
			},
			wantUpdates: 1, wantStage: stagelabels.AdoptedByHive, wantAttempt: "aaa@base1",
		},
		{
			name: "check still red on base: nothing to recover, escalate as before",
			setup: func(a *adoptAPI) {
				a.compareBehind = 15
				a.baseChecks = map[string]string{"coverage": "failure"}
			},
			wantStage: stagelabels.NeedsDecision,
		},
		{
			name: "base checks unknown: escalate as before",
			setup: func(a *adoptAPI) {
				a.compareBehind = 15
				a.baseChecks = map[string]string{"coverage": "pending"}
			},
			wantStage: stagelabels.NeedsDecision,
		},
		{
			name: "base check listing fails: escalate, attempt not consumed",
			setup: func(a *adoptAPI) {
				a.compareBehind = 15
				a.failBaseChecks = true
			},
			wantStage: stagelabels.NeedsDecision,
		},
		{
			name: "fork head not editable by the app: escalate",
			setup: func(a *adoptAPI) {
				a.compareBehind = 15
				a.baseChecks = map[string]string{"coverage": "success"}
				a.updateBranchStatus = http.StatusUnprocessableEntity
				a.updateBranchMessage = "maintainer edits are not allowed"
			},
			wantUpdates: 1, wantStage: stagelabels.NeedsDecision, wantAttempt: "aaa@base1",
		},
		{
			name: "update-branch says up to date: fall through to the re-run",
			setup: func(a *adoptAPI) {
				a.compareBehind = 1
				a.baseChecks = map[string]string{"coverage": "success"}
				a.updateBranchStatus = http.StatusUnprocessableEntity
				a.updateBranchMessage = "There are no new commits on the base branch."
				a.failedRuns = []int64{41}
			},
			wantUpdates: 1, wantReruns: 1, wantStage: stagelabels.AdoptedByHive, wantAttempt: "aaa@base1",
		},
		{
			name: "current head with no failed runs to re-run: escalate",
			setup: func(a *adoptAPI) {
				a.baseChecks = map[string]string{"coverage": "success"}
			},
			wantStage: stagelabels.NeedsDecision, wantAttempt: "aaa@base1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, key, done := recoveryHarness(t)
			defer done()
			tc.setup(h.adopt)

			res := h.observe(adoptPR(7, "aaa", adoptedLabels...), recoveryReason)
			if got := len(h.adopt.branchUpdates); got != tc.wantUpdates {
				t.Fatalf("branch updates=%v, want %d", h.adopt.branchUpdates, tc.wantUpdates)
			}
			if got := len(h.adopt.reruns); got != tc.wantReruns {
				t.Fatalf("reruns=%v, want %d", h.adopt.reruns, tc.wantReruns)
			}
			if len(h.adopt.pulls) != 0 {
				t.Fatalf("red CI is never a re-home: %v", h.adopt.pulls)
			}
			rec, _ := h.e.stageStoreOrShared().Get(key)
			if rec.Stage != tc.wantStage {
				t.Fatalf("stage=%v, want %v (record %+v)", rec.Stage, tc.wantStage, rec)
			}
			if rec.AdoptRecoveryAttempt != tc.wantAttempt {
				t.Fatalf("recovery attempt=%q, want %q", rec.AdoptRecoveryAttempt, tc.wantAttempt)
			}
			escalated := tc.wantStage == stagelabels.NeedsDecision
			if notes := stalledComments(h.api); (len(notes) == 1) != escalated || (res.Comments == 1) != escalated {
				t.Fatalf("stalled notes=%v res=%+v, escalated=%v", notes, res, escalated)
			}
			if escalated != rec.AdoptionEscalated("aaa") || !rec.AdoptedInPlace() {
				t.Fatalf("record after sweep: %+v", rec)
			}
			for _, u := range h.adopt.branchUpdates {
				if u != "7@aaa" {
					t.Fatalf("update-branch must name the evaluated head: %v", h.adopt.branchUpdates)
				}
			}
		})
	}
}

func TestAdoptedRecoveryPreparationWaitDoesNotConsumeAttempt(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "duplicate"}[duplicate], func(t *testing.T) {
			h, key, done := recoveryHarness(t)
			defer done()
			h.e.requireCurrentBase = func(string) bool { return true }
			h.e.basePreparation = NewBasePreparationStore("")
			h.adopt.compareBehind = 2
			h.adopt.baseChecks = map[string]string{"coverage": "success"}
			front, head, wantReason := 8, "head8", preparationQueued(8)
			if duplicate {
				front, head, wantReason = 7, "aaa", basePreparationReady
			}
			h.adopt.superseders[front] = map[string]any{
				"number": front, "state": "open",
				"head": map[string]string{"sha": head},
				"base": map[string]string{"ref": "v5", "sha": "base1"},
			}
			if reason, err := h.e.admitBasePreparation(context.Background(), "acme", "widget", "v5", front, head, stageLaneContributor, false); reason != "" || err != nil {
				t.Fatalf("admit owner: reason=%q error=%v", reason, err)
			}
			if duplicate {
				if err := h.e.claimBasePreparationUpdate(context.Background(), "acme", "widget", front, head); err != nil {
					t.Fatal(err)
				}
			}
			rec, _ := h.e.stageStoreOrShared().Get(key)
			out := h.e.recoverStaleRedAdoption(context.Background(), h.e.stageStoreOrShared(), key, stageObservation{
				Owner: "acme", Repo: "widget", Display: "acme/widget", Number: 7,
				BaseRef: "v5", HeadSHA: "aaa", Lane: stageLaneContributor, Reason: recoveryReason,
			}, rec)
			if out.Attempted || out.Reason != adoptionRecoverySkipPrefix+wantReason || out.Deferred != duplicate {
				t.Fatalf("recovery while waiting: %+v", out)
			}
			rec, _ = h.e.stageStoreOrShared().Get(key)
			if rec.AdoptRecoveryCount != 0 || rec.AdoptRecoveryAttempt != "" || len(h.adopt.branchUpdates) != 0 {
				t.Fatalf("waiting recovery consumed attempt: record=%+v updates=%v", rec, h.adopt.branchUpdates)
			}
		})
	}
}

// The same head stays red after the re-run: one attempt per (head, base),
// then the escalation runs; a new hive head starts over.
func TestAdoptedStaleRedRecoveryEscalatesAfterOneAttempt(t *testing.T) {
	h, key, done := recoveryHarness(t)
	defer done()
	h.adopt.baseChecks = map[string]string{"coverage": "success"}
	h.adopt.failedRuns = []int64{41}

	h.observe(adoptPR(7, "aaa", adoptedLabels...), recoveryReason)
	if len(h.adopt.reruns) != 1 || len(stalledComments(h.api)) != 0 {
		t.Fatalf("first sweep: reruns=%v comments=%v", h.adopt.reruns, h.api.comments)
	}

	// Re-run finished, still red, same base: no second attempt, escalate.
	h.api.reset()
	h.clock.advance(time.Hour)
	res := h.observe(adoptPR(7, "aaa", adoptedLabels...), recoveryReason)
	if len(h.adopt.reruns) != 1 || len(h.adopt.branchUpdates) != 0 {
		t.Fatalf("recovery repeated: reruns=%v updates=%v", h.adopt.reruns, h.adopt.branchUpdates)
	}
	if notes := stalledComments(h.api); len(notes) != 1 || res.Comments != 1 {
		t.Fatalf("escalation after the attempt: notes=%v res=%+v", notes, res)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.NeedsDecision || !rec.AdoptionEscalated("aaa") || rec.AdoptRecoveryCount != 1 {
		t.Fatalf("record: %+v", rec)
	}

	// Base moved while escalated: one more attempt releases the escalation.
	h.api.reset()
	h.adopt.compareBase = "base2"
	h.adopt.compareBehind = 4
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"), recoveryReason)
	if len(h.adopt.branchUpdates) != 1 {
		t.Fatalf("base moved: updates=%v", h.adopt.branchUpdates)
	}
	rec, _ = h.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("aaa") || rec.AdoptRecoveryAttempt != "aaa@base2" || rec.AdoptRecoveryCount != 2 {
		t.Fatalf("record after release: %+v", rec)
	}
	if len(stalledComments(h.api)) != 0 {
		t.Fatalf("release must not note: %v", h.api.comments)
	}

	// The update landed as a hive head: 074 with a fresh clock.
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", adoptedLabels...), "check-pending")
	rec, _ = h.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.AdoptedByHive || rec.AdoptedHead != "bbb" || !rec.AdoptedHeadSince().Equal(h.clock.at) {
		t.Fatalf("new hive head: %+v", rec)
	}
}

// The lifetime budget: a base that keeps moving under a red head gets
// adoptionRecoveryMaxAttempts refreshes, then the escape runs as before.
func TestAdoptedStaleRedRecoveryBudget(t *testing.T) {
	h, key, done := recoveryHarness(t)
	defer done()
	h.adopt.baseChecks = map[string]string{"coverage": "success"}
	h.adopt.compareBehind = 2
	for i := 0; i < adoptionRecoveryMaxAttempts; i++ {
		h.adopt.compareBase = "base" + string(rune('1'+i))
		h.observe(adoptPR(7, "aaa", adoptedLabels...), recoveryReason)
		h.clock.advance(time.Hour)
	}
	if len(h.adopt.branchUpdates) != adoptionRecoveryMaxAttempts || len(stalledComments(h.api)) != 0 {
		t.Fatalf("within budget: updates=%v comments=%v", h.adopt.branchUpdates, h.api.comments)
	}
	h.adopt.compareBase = "base9"
	h.observe(adoptPR(7, "aaa", adoptedLabels...), recoveryReason)
	if len(h.adopt.branchUpdates) != adoptionRecoveryMaxAttempts {
		t.Fatalf("budget not enforced: %v", h.adopt.branchUpdates)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.NeedsDecision || len(stalledComments(h.api)) != 1 {
		t.Fatalf("exhausted budget must escalate: %+v comments=%v", rec, h.api.comments)
	}
}

func TestAdoptionRecoveryCandidate(t *testing.T) {
	rec := stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeInPlace}
	base := func() stageObservation {
		return stageObservation{HeadSHA: "aaa", LaneReason: recoveryReason}
	}
	cases := []struct {
		name string
		obs  func() stageObservation
		rec  func() stagelabels.Record
		want bool
	}{
		{"red in-place adoption", base, func() stagelabels.Record { return rec }, true},
		{"check-failing reason", func() stageObservation { o := base(); o.LaneReason = "check-failing:unit"; return o }, func() stagelabels.Record { return rec }, true},
		{"pending CI", func() stageObservation { o := base(); o.LaneReason = "check-pending"; return o }, func() stagelabels.Record { return rec }, false},
		{"green with no verdict", func() stageObservation { o := base(); o.LaneReason = contributorLaneReasonNoVerdict; return o }, func() stagelabels.Record { return rec }, false},
		{"open shared-CI incident", func() stageObservation { o := base(); o.SharedCIIncident = 10528; return o }, func() stagelabels.Record { return rec }, false},
		{"base red", func() stageObservation { o := base(); o.BaseRedChecks = []string{"coverage"}; return o }, func() stagelabels.Record { return rec }, false},
		{"base-red reason", func() stageObservation { o := base(); o.Reason = stageReasonBaseBranchRed + "coverage"; return o }, func() stagelabels.Record { return rec }, false},
		{"fork guard", func() stageObservation { o := base(); o.ForkGuardChecks = []string{"coverage"}; return o }, func() stagelabels.Record { return rec }, false},
		{"hold label", func() stageObservation { o := base(); o.Labels = []string{"do-not-merge/hold"}; return o }, func() stagelabels.Record { return rec }, false},
		{"blocking verdict", func() stageObservation {
			o := base()
			o.Detail.VerdictKnown = true
			o.Detail.Verdict = ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}
			return o
		}, func() stagelabels.Record { return rec }, false},
		{"head moved", func() stageObservation { o := base(); o.HeadSHA = "bbb"; return o }, func() stagelabels.Record { return rec }, false},
		{"supersession", base, func() stagelabels.Record {
			r := rec
			r.AdoptMode = stagelabels.AdoptModeSupersede
			r.AdoptedBy = 90
			return r
		}, false},
		{"workflow-path refusal", base, func() stagelabels.Record {
			r := rec
			r.AdoptEscalatedHead, r.AdoptEscalatedReason = "aaa", adoptionWorkflowRefusedReason
			return r
		}, false},
		{"stalled escalation", base, func() stagelabels.Record {
			r := rec
			r.AdoptEscalatedHead, r.AdoptEscalatedReason = "aaa", stagelabels.LaneReasonAdoptionStalledPrefix+recoveryReason
			return r
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adoptionRecoveryCandidate(tc.obs(), tc.rec()); got != tc.want {
				t.Fatalf("candidate=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestBaseClearsFailure(t *testing.T) {
	st := func(red, green []string) baseCheckState {
		s := baseCheckState{completed: map[string]bool{}, red: map[string]bool{}}
		for _, n := range red {
			s.completed[n], s.red[n] = true, true
		}
		for _, n := range green {
			s.completed[n] = true
		}
		return s
	}
	cases := []struct {
		name     string
		st       baseCheckState
		failing  string
		wantOK   bool
		wantSkip string
	}{
		{"named check green", st(nil, []string{"coverage"}), "coverage", true, ""},
		{"named check green while another is red", st([]string{"docs"}, []string{"coverage"}), "coverage", true, ""},
		{"no name, nothing completed", st(nil, nil), "", false, "base-checks-unknown"},
		{"named check red on base", st([]string{"coverage"}, nil), "coverage", false, "base-red:coverage"},
		{"check absent, base green", st(nil, []string{"build"}), "coverage", true, ""},
		{"check absent, base red", st([]string{"build"}, nil), "coverage", false, "base-red"},
		{"no name, base green", st(nil, []string{"build"}), "", true, ""},
		{"nothing completed on base", st(nil, nil), "coverage", false, "base-checks-unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, skip := baseClearsFailure(tc.st, tc.failing)
			if ok != tc.wantOK || skip != tc.wantSkip {
				t.Fatalf("got (%v, %q), want (%v, %q)", ok, skip, tc.wantOK, tc.wantSkip)
			}
		})
	}
}

func TestAdoptionFailingCheck(t *testing.T) {
	for in, want := range map[string]string{
		"required-check-failing:coverage": "coverage",
		"check-failing:unit":              "unit",
		"check-runs":                      "",
		"status-failure":                  "",
		"":                                "",
	} {
		if got := adoptionFailingCheck(in); got != want {
			t.Errorf("adoptionFailingCheck(%q)=%q, want %q", in, got, want)
		}
	}
}
