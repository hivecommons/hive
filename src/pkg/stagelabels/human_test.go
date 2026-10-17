package stagelabels

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readyEvidence() Evidence {
	return Evidence{
		Head:        "abc1234567890",
		Verdict:     VerdictApproved,
		Checks:      CheckSummary{Total: 55, Green: 55, Known: true},
		DCO:         DCOYes,
		Soak:        2 * time.Hour,
		SoakElapsed: 2 * time.Hour,
		Cause:       CauseTrust,
	}
}

// TestRouteHumanReady: every gate green and only trust in the way → 090,
// with the digest the entry comment quotes (hivecommons/hive#12528).
func TestRouteHumanReady(t *testing.T) {
	r := RouteHuman(readyEvidence())
	if r.Stage != ReadyForHumanMerge || len(r.Reasons) != 0 || r.LaneReason != "" {
		t.Fatalf("route = %+v", r)
	}
	if r.Summary != "verdict approved @abc1234, 55/55 checks green, DCO yes, soak 2h0m0s elapsed" {
		t.Fatalf("summary = %q", r.Summary)
	}
	got := ReadyComment(r, "`author/newcomer` (below require_tiers)")
	if !strings.HasPrefix(got, ReadyCommentMarker+"\nReady for a human merge: verdict approved @abc1234") || !strings.Contains(got, "Blocked only by `author/newcomer` (below require_tiers).") {
		t.Fatalf("ReadyComment = %q", got)
	}
	if strings.Contains(strings.ToLower(got), "approve\n") {
		t.Fatal("comment must never read as a review approval")
	}
}

// TestRouteHumanFixEvidence: each "do-not-merge" reason sends the PR back
// to the author (072, or 070 when the hive wrote it), listing the concrete
// reason and a lane-vocabulary reason for the nudge.
func TestRouteHumanFixEvidence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Evidence)
		reason string
		lane   string
	}{
		{"changes_requested", func(e *Evidence) { e.Verdict = VerdictChangesRequested }, "verdict changes_requested @abc1234", LaneReasonChangesRequested},
		{"red check", func(e *Evidence) {
			e.Checks = CheckSummary{Total: 3, Green: 1, Red: []string{"lint", "unit"}, Known: true}
		}, "red checks: lint, unit", "check-failing:lint"},
		{"dco no", func(e *Evidence) { e.DCO = DCONo }, "DCO sign-off missing", LaneReasonDCOMissing},
		{"conflict", func(e *Evidence) { e.Conflict = true }, "merge conflict with the base branch", LaneReasonConflicting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := readyEvidence()
			tc.mutate(&e)
			r := RouteHuman(e)
			if r.Stage != ReturnedToContributor || len(r.Reasons) != 1 || r.Reasons[0] != tc.reason || r.LaneReason != tc.lane {
				t.Fatalf("route = %+v", r)
			}
			e.SelfAuthored = true
			if r := RouteHuman(e); r.Stage != Fixing {
				t.Fatalf("self-authored route = %+v", r)
			}
		})
	}
	// Fix evidence beats everything else, even a hold or a sentinel alert
	// (#12471: sentinel + conflicts → 072, the author must rebase first).
	e := readyEvidence()
	e.Sentinel, e.Conflict = true, true
	e.Holds = []string{"hold"}
	if r := RouteHuman(e); r.Stage != ReturnedToContributor || r.LaneReason != LaneReasonConflicting {
		t.Fatalf("sentinel+conflict route = %+v", r)
	}
	// Several reasons are all listed; the first sets the lane reason.
	e = readyEvidence()
	e.Verdict, e.DCO = VerdictChangesRequested, DCONo
	if r := RouteHuman(e); len(r.Reasons) != 2 || r.LaneReason != LaneReasonChangesRequested {
		t.Fatalf("multi-reason route = %+v", r)
	}
}

// TestRouteHumanPendingStaysAtReview: no current-head verdict, or a head
// nothing has reported on yet, is 050 — never 090.
func TestRouteHumanPendingStaysAtReview(t *testing.T) {
	e := readyEvidence()
	e.Verdict = VerdictPending
	if r := RouteHuman(e); r.Stage != ReviewPending {
		t.Fatalf("pending verdict route = %+v", r)
	}
	e = readyEvidence()
	e.HeadFresh = true
	e.Checks = CheckSummary{Known: true}
	if r := RouteHuman(e); r.Stage != ReviewPending {
		t.Fatalf("fresh head route = %+v", r)
	}
	// A fresh head with a hold still waits for the verdict first.
	e.Holds = []string{"hold"}
	if r := RouteHuman(e); r.Stage != ReviewPending {
		t.Fatalf("fresh head + hold route = %+v", r)
	}
}

// TestRouteHumanDecision: sentinel, hold-class labels, no reviewer, or a
// non-trust human-only lane rule → 085 naming the decision owed.
func TestRouteHumanDecision(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Evidence)
		want   string
	}{
		{"sentinel", func(e *Evidence) { e.Sentinel = true }, "a sentinel alert is open on this PR"},
		{"hold", func(e *Evidence) { e.Holds = Holds([]string{"Hold", "kind/bug"}) }, "the `hold` label is set"},
		{"do-not-merge family", func(e *Evidence) { e.Holds = Holds([]string{"do-not-merge/work-in-progress"}) }, "the `do-not-merge/work-in-progress` label is set"},
		{"needs-decision", func(e *Evidence) { e.Holds = Holds([]string{"needs-decision"}) }, "the `needs-decision` label is set"},
		{"no reviewer", func(e *Evidence) { e.Verdict = VerdictUnavailable }, "no reviewer agent is configured, so this head will never get a swarm verdict"},
		{"requires_human", func(e *Evidence) { e.Verdict = VerdictRequiresHuman }, "the reviewer swarm returned `requires_human` @abc1234"},
		{"requires_human question", func(e *Evidence) {
			e.Verdict = VerdictRequiresHuman
			e.DecisionQuestion = "@clubanderson should decide whether to land this now."
		}, "reviewer question: @clubanderson should decide whether to land this now."},
		{"lane rule", func(e *Evidence) { e.Cause, e.CauseReason = CauseDecision, "the lane excludes `workflow-paths` edits" }, "the lane excludes `workflow-paths` edits"},
		{"lane rule unnamed", func(e *Evidence) { e.Cause = CauseDecision }, "the automerge lane will not merge this unattended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := readyEvidence()
			tc.mutate(&e)
			r := RouteHuman(e)
			if r.Stage != NeedsDecision || !containsString(r.Reasons, tc.want) {
				t.Fatalf("route = %+v", r)
			}
			got := DecisionComment(r)
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "Head: ") {
				t.Fatalf("DecisionComment = %q", got)
			}
		})
	}
	e := readyEvidence()
	e.Sentinel = true
	e.Holds = []string{"hold"}
	r := RouteHuman(e)
	if len(r.Reasons) != 2 || !strings.Contains(DecisionComment(r), "\n- the `hold` label is set\n") {
		t.Fatalf("multi-reason decision = %+v / %q", r, DecisionComment(r))
	}
}

// TestRouteHumanWaits: pending checks, unreadable checks, or a soak still
// running keep the PR at 060 so it never claims 090 early.
func TestRouteHumanWaits(t *testing.T) {
	e := readyEvidence()
	e.Checks = CheckSummary{Total: 5, Green: 4, Pending: 1, Known: true}
	if r := RouteHuman(e); r.Stage != Reviewed || r.Reasons[0] != "1 check(s) still running" {
		t.Fatalf("pending checks route = %+v", r)
	}
	e = readyEvidence()
	e.Checks = CheckSummary{}
	if r := RouteHuman(e); r.Stage != Reviewed || r.Reasons[0] != "checks could not be read" {
		t.Fatalf("unknown checks route = %+v", r)
	}
	e = readyEvidence()
	e.SoakElapsed = 30 * time.Minute
	r := RouteHuman(e)
	if r.Stage != Reviewed || r.Reasons[0] != "soak 1h30m0s remaining" || !strings.Contains(r.Summary, "soak 30m0s of 2h0m0s") {
		t.Fatalf("soak route = %+v", r)
	}
	// No soak configured and DCO context absent: neither blocks.
	e = readyEvidence()
	e.Soak, e.SoakElapsed, e.DCO = 0, 0, DCOUnknown
	if r := RouteHuman(e); r.Stage != ReadyForHumanMerge || strings.Contains(r.Summary, "DCO") || strings.Contains(r.Summary, "soak") {
		t.Fatalf("no-soak route = %+v", r)
	}
}

func TestHoldsAndShortSHA(t *testing.T) {
	if got := Holds([]string{"kind/bug", " HOLD ", "do-not-merge", "do-not-merge/hold", "needs-decisions", "holdx"}); strings.Join(got, ",") != "do-not-merge,do-not-merge/hold,hold" {
		t.Fatalf("Holds = %v", got)
	}
	if ShortSHA("abc") != "abc" || ShortSHA(" 0123456789 ") != "0123456" {
		t.Fatal("ShortSHA")
	}
}

func containsString(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// TestRenamedLabelStillReads: a PR carrying the pre-#12528 spelling reads as
// 090, and Plan swaps it for the new name in one write.
func TestRenamedLabelStillReads(t *testing.T) {
	if got := Current([]string{"kind/bug", "stage/090-needs-human"}); got != ReadyForHumanMerge {
		t.Fatalf("Current(old spelling) = %v", got)
	}
	add, remove := Plan([]string{"stage/090-needs-human", "author/newcomer"}, Desired{Stage: ReadyForHumanMerge, Author: AuthorNewcomer})
	if strings.Join(add, ",") != "stage/090-ready-for-human-merge" || strings.Join(remove, ",") != "stage/090-needs-human" {
		t.Fatalf("Plan add=%v remove=%v", add, remove)
	}
	add, remove = Plan([]string{"stage/090-needs-human"}, Desired{Stage: ReturnedToContributor})
	if strings.Join(add, ",") != "stage/072-returned-to-contributor" || strings.Join(remove, ",") != "stage/090-needs-human" {
		t.Fatalf("Plan to 072 add=%v remove=%v", add, remove)
	}
}

// TestEntryCommentedResetsOnStageChange: the 085/090 entry comment is posted
// once per visit — the flag survives same-stage observations and clears when
// the stage changes.
func TestEntryCommentedResetsOnStageChange(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("o/r", 12528)
	st.Observe(key, ReadyForHumanMerge, "untrusted-author-role")
	st.MarkEntryCommented(key)
	st.Observe(key, ReadyForHumanMerge, "untrusted-author-role")
	if rec, _ := st.Get(key); !rec.EntryCommented {
		t.Fatalf("same-stage observation cleared the flag: %+v", rec)
	}
	st.Observe(key, NeedsDecision, "excluded-label:hold")
	if rec, _ := st.Get(key); rec.EntryCommented {
		t.Fatalf("stage flip kept the flag: %+v", rec)
	}
	st.MarkEntryCommented(key)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2 := NewStore(st.path, func() time.Time { return now })
	if err := st2.Load(); err != nil {
		t.Fatal(err)
	}
	if rec, _ := st2.Get(key); !rec.EntryCommented || rec.Stage != NeedsDecision {
		t.Fatalf("flag not persisted: %+v", rec)
	}
	st.MarkEntryCommented("missing")
}
