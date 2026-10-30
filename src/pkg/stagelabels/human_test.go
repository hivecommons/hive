package stagelabels

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
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
	got := ReadyComment(r, "`role/newcomer` (below require_tiers)")
	if !strings.HasPrefix(got, ReadyCommentMarker+"\nReady for a human merge: verdict approved @abc1234") || !strings.Contains(got, "Blocked only by `role/newcomer` (below require_tiers).") {
		t.Fatalf("ReadyComment = %q", got)
	}
	if strings.Contains(strings.ToLower(got), "approve\n") {
		t.Fatal("comment must never read as a review approval")
	}
}

// TestRouteHumanFixEvidence: each "do-not-merge" reason sends the PR back
// to the author (071 for a human, 072 for a clanker, 070 when the hive wrote
// it), listing the concrete reason and a lane-vocabulary reason for the nudge.
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
			if r.Stage != NeedsAuthor || len(r.Reasons) != 1 || r.Reasons[0] != tc.reason || r.LaneReason != tc.lane {
				t.Fatalf("route = %+v", r)
			}
			e.AuthorKind = authorkind.Clanker
			if r := RouteHuman(e); r.Stage != ReturnedToContributor || r.LaneReason != tc.lane {
				t.Fatalf("clanker route = %+v", r)
			}
			e.AuthorKind = authorkind.Hive
			if r := RouteHuman(e); r.Stage != Fixing {
				t.Fatalf("hive-kind route = %+v", r)
			}
			e.AuthorKind = authorkind.Unknown
			e.SelfAuthored = true
			if r := RouteHuman(e); r.Stage != Fixing {
				t.Fatalf("self-authored route = %+v", r)
			}
		})
	}
	// Fix evidence beats everything else, even a hold or a sentinel alert
	// (#12471: sentinel + conflicts → 07x, the author must rebase first).
	e := readyEvidence()
	e.Sentinel, e.Conflict = true, true
	e.Holds = []string{"hold"}
	if r := RouteHuman(e); r.Stage != NeedsAuthor || r.LaneReason != LaneReasonConflicting {
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
	// Unless the verdict for that head is already requires_human: the
	// reviewer has answered, so the PR is a decision, not a wait
	// (hivecommons/hive#13115).
	e = readyEvidence()
	e.HeadFresh = true
	e.Checks = CheckSummary{Known: true}
	e.Verdict = VerdictRequiresHuman
	if r := RouteHuman(e); r.Stage != NeedsDecision {
		t.Fatalf("fresh head + requires_human route = %+v", r)
	}
	e.Cause = CauseDecision
	if r := RouteHuman(e); r.Stage != NeedsDecision {
		t.Fatalf("fresh head + requires_human decision route = %+v", r)
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
		{"requires_human finding", func(e *Evidence) {
			e.Verdict = VerdictRequiresHuman
			e.DecisionQuestion = "close in favour of #15309?"
			e.DecisionFinding = "duplicate: the review found this change already landed or open elsewhere"
		}, "the review found: duplicate: the review found this change already landed or open elsewhere"},
		{"requires_human summary", func(e *Evidence) {
			e.Verdict = VerdictRequiresHuman
			e.HumanSummaries = []string{"security requires human: relay owner should confirm before merge"}
		}, "reviewer summary: security requires human: relay owner should confirm before merge"},
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

// TestRouteHumanApproved: a trusted merger's approval on the head is the
// human decision (hivecommons/hive#14568, rationguard#401). It settles a
// requires_human verdict, and with a manual-merge lane rule — the hive never
// merges CI workflow edits itself — it moves the PR to 090 with the one
// step left named: the merge click. Anything else still outstanding (the
// sentinel label still on, a hold, a reviewer-less verdict, a fork guard)
// keeps the 085 decision, and a rule the person has not approved stays
// an open 085 as before.
func TestRouteHumanApproved(t *testing.T) {
	const workflow = "the PR edits CI workflow / action files, which the lane never merges unattended"
	manual := func(e *Evidence) {
		e.Cause, e.CauseReason, e.ManualMergeRule, e.LaneReason = CauseDecision, workflow, true, "contributor-lane:workflow-path"
	}
	cases := []struct {
		name           string
		mutate         func(*Evidence)
		wantStage      Stage
		wantManual     bool
		wantLaneReason string
		wantReason     string
		wantNoReason   string
	}{
		{
			name:         "requires_human approved, trust cause",
			mutate:       func(e *Evidence) { e.Verdict, e.HumanApprover = VerdictRequiresHuman, "octocat" },
			wantStage:    ReadyForHumanMerge,
			wantNoReason: "requires_human",
		},
		{
			name: "requires_human approved, workflow-path rule",
			mutate: func(e *Evidence) {
				e.Verdict, e.HumanApprover = VerdictRequiresHuman, "clubanderson"
				manual(e)
			},
			wantStage: ReadyForHumanMerge, wantManual: true,
			wantLaneReason: "contributor-lane:workflow-path" + HumanApprovedReasonSuffix + "clubanderson",
			wantReason:     workflow + "; @clubanderson approved this head, so the only step left is the merge",
			wantNoReason:   "requires_human",
		},
		{
			name:           "swarm approved, workflow-path rule, human approved",
			mutate:         func(e *Evidence) { e.HumanApprover = "octocat"; manual(e) },
			wantStage:      ReadyForHumanMerge,
			wantManual:     true,
			wantLaneReason: "contributor-lane:workflow-path" + HumanApprovedReasonSuffix + "octocat",
			wantReason:     workflow + "; @octocat approved this head",
		},
		{
			name:       "workflow-path rule, nobody approved yet",
			mutate:     manual,
			wantStage:  NeedsDecision,
			wantReason: workflow,
		},
		{
			name:       "requires_human, nobody approved yet",
			mutate:     func(e *Evidence) { e.Verdict = VerdictRequiresHuman; manual(e) },
			wantStage:  NeedsDecision,
			wantReason: "the reviewer swarm returned `requires_human` @abc1234",
		},
		{
			name: "approved but the sentinel label is still on",
			mutate: func(e *Evidence) {
				e.Verdict, e.HumanApprover, e.Sentinel = VerdictRequiresHuman, "octocat", true
				manual(e)
			},
			wantStage:  NeedsDecision,
			wantReason: "a sentinel alert is open on this PR",
		},
		{
			name: "approved but a hold label is set",
			mutate: func(e *Evidence) {
				e.HumanApprover, e.Holds = "octocat", Holds([]string{"hold"})
				manual(e)
			},
			wantStage:  NeedsDecision,
			wantReason: "the `hold` label is set",
		},
		{
			name:       "approved but no reviewer configured",
			mutate:     func(e *Evidence) { e.Verdict, e.HumanApprover = VerdictUnavailable, "octocat"; manual(e) },
			wantStage:  NeedsDecision,
			wantReason: "no reviewer agent is configured, so this head will never get a swarm verdict",
		},
		{
			name: "approved, lane rule that is not a manual-merge rule",
			mutate: func(e *Evidence) {
				e.HumanApprover, e.Cause, e.CauseReason = "octocat", CauseDecision, "the reporter-trust hold is in place"
			},
			wantStage:  NeedsDecision,
			wantReason: "the reporter-trust hold is in place",
		},
		{
			name: "approved manual rule still waits for pending checks",
			mutate: func(e *Evidence) {
				e.HumanApprover = "octocat"
				e.Checks = CheckSummary{Total: 55, Green: 54, Pending: 1, Known: true}
				manual(e)
			},
			wantStage:  Reviewed,
			wantReason: "1 check(s) still running",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := readyEvidence()
			tc.mutate(&e)
			r := RouteHuman(e)
			if r.Stage != tc.wantStage || r.ManualMerge != tc.wantManual || r.LaneReason != tc.wantLaneReason {
				t.Fatalf("route = %+v, want stage %s manual %v lane %q", r, tc.wantStage, tc.wantManual, tc.wantLaneReason)
			}
			if r.HumanApprover != e.HumanApprover {
				t.Fatalf("route approver = %q, want %q", r.HumanApprover, e.HumanApprover)
			}
			joined := strings.Join(r.Reasons, "\n")
			if tc.wantReason != "" && !strings.Contains(joined, tc.wantReason) {
				t.Fatalf("reasons = %q, want %q", joined, tc.wantReason)
			}
			if tc.wantNoReason != "" && strings.Contains(joined, tc.wantNoReason) {
				t.Fatalf("reasons = %q must not mention %q once a human approved", joined, tc.wantNoReason)
			}
			if e.HumanApprover != "" && !strings.Contains(r.Summary, "approved by @"+e.HumanApprover) {
				t.Fatalf("summary = %q must name the approver", r.Summary)
			}
			if tc.wantStage == ReadyForHumanMerge && tc.wantManual {
				got := ReadyComment(r, "`role/newcomer` (below require_tiers)")
				if !strings.Contains(got, "click **Merge**") || strings.Contains(got, "Blocked only by") || !strings.Contains(got, "@"+e.HumanApprover+" approved this head") {
					t.Fatalf("ReadyComment = %q", got)
				}
			}
			if tc.wantStage == NeedsDecision && !strings.Contains(DecisionComment(r), tc.wantReason) {
				t.Fatalf("DecisionComment = %q", DecisionComment(r))
			}
		})
	}
}

func TestHumanApprovedReason(t *testing.T) {
	for _, tc := range []struct{ in, base, approver string }{
		{"contributor-lane:workflow-path:human-approved@octocat", "contributor-lane:workflow-path", "octocat"},
		{"reviewer-verdict:requires_human:human-approved@a-b", "reviewer-verdict:requires_human", "a-b"},
		{"contributor-lane:workflow-path", "contributor-lane:workflow-path", ""},
		{"", "", ""},
	} {
		if base, approver := HumanApprovedReason(tc.in); base != tc.base || approver != tc.approver {
			t.Errorf("HumanApprovedReason(%q) = %q, %q; want %q, %q", tc.in, base, approver, tc.base, tc.approver)
		}
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
	add, remove := Plan([]string{"stage/090-needs-human", "role/newcomer"}, Desired{Stage: ReadyForHumanMerge, Role: RoleNewcomer})
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
	st.MarkEntryCommented(key, "h1")
	st.Observe(key, ReadyForHumanMerge, "untrusted-author-role")
	if rec, _ := st.Get(key); !rec.EntryCommented {
		t.Fatalf("same-stage observation cleared the flag: %+v", rec)
	}
	st.Observe(key, NeedsDecision, "excluded-label:hold")
	if rec, _ := st.Get(key); rec.EntryCommented {
		t.Fatalf("stage flip kept the flag: %+v", rec)
	}
	st.MarkEntryCommented(key, "h1")
	if rec, _ := st.Get(key); rec.EntryCommentedHead != "h1" || rec.EntryCommentedStage != NeedsDecision {
		t.Fatalf("head/stage not recorded: %+v", rec)
	}
	st.Observe(key, ReviewPending, "review-requested")
	st.Observe(key, NeedsDecision, "excluded-label:hold")
	if rec, _ := st.Get(key); rec.EntryCommented || rec.EntryCommentedHead != "h1" || rec.EntryCommentedStage != NeedsDecision {
		t.Fatalf("flap reset the per-head record: %+v", rec)
	}
	st.MarkEntryCommented(key, "h1")
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
	st.MarkEntryCommented("missing", "h1")
}

// TestDecisionCommentNamesActionsForSelfAuthoredRequiresHuman pins
// hivecommons/hive#13041: the 085 comment on a hive-authored PR parked on
// `requires_human` says what a maintainer can do — approve the head, or
// request changes (review or `/hive decision`) and the hive pushes the fix.
func TestDecisionCommentNamesActionsForSelfAuthoredRequiresHuman(t *testing.T) {
	e := readyEvidence()
	e.SelfAuthored = true
	e.Verdict = VerdictRequiresHuman
	r := RouteHuman(e)
	if r.Stage != NeedsDecision || !r.SelfAuthored || r.Verdict != VerdictRequiresHuman || r.Head != e.Head {
		t.Fatalf("route = %+v", r)
	}
	got := DecisionComment(r)
	for _, want := range []string{
		"**What to do:** approve this head (`abc1234`) to merge as-is",
		"*changes requested* review",
		"`/hive decision <what to change>`",
		"the hive will push the fix",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("DecisionComment missing %q:\n%s", want, got)
		}
	}

	// Not for a PR the hive did not open, and not for a hold-label park.
	human := readyEvidence()
	human.Verdict = VerdictRequiresHuman
	if got := DecisionComment(RouteHuman(human)); strings.Contains(got, "What to do") {
		t.Fatalf("actions offered on a non-hive PR:\n%s", got)
	}
	held := readyEvidence()
	held.SelfAuthored = true
	held.Holds = Holds([]string{"hold"})
	if got := DecisionComment(RouteHuman(held)); strings.Contains(got, "What to do") {
		t.Fatalf("actions offered on a hold-label park:\n%s", got)
	}
}

// TestEntryCommentedCauses: each distinct cause is remembered per
// stage+head, a new head or stage starts the list over, and a record from
// before causes were kept counts every cause as explained.
func TestEntryCommentedCauses(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("o/r", 250)
	sentinel := EntryCause([]string{"a sentinel alert is open on this PR"})
	workflow := EntryCause([]string{"the PR edits CI workflow / action files"})
	if sentinel == workflow || sentinel == "" || EntryCause(nil) != "" {
		t.Fatalf("EntryCause digests: %q %q", sentinel, workflow)
	}
	st.Observe(key, NeedsDecision, "contributor-lane:sentinel-blocked")
	rec, _ := st.Get(key)
	if rec.EntryCommentExplains("h1", sentinel) {
		t.Fatal("nothing posted yet")
	}
	st.MarkEntryCommentedCause(key, "h1", sentinel)
	rec, _ = st.Get(key)
	if !rec.EntryCommentExplains("h1", sentinel) || rec.EntryCommentExplains("h1", workflow) || rec.EntryCommentExplains("h2", sentinel) {
		t.Fatalf("after sentinel: %+v", rec)
	}
	st.MarkEntryCommentedCause(key, "h1", workflow)
	st.MarkEntryCommentedCause(key, "h1", workflow)
	rec, _ = st.Get(key)
	if !rec.EntryCommentExplains("h1", workflow) || len(rec.EntryCommentedCauses) != 2 {
		t.Fatalf("after workflow: %+v", rec)
	}
	// Plain MarkEntryCommented keeps the list; a new head resets it.
	st.MarkEntryCommented(key, "h1")
	if rec, _ = st.Get(key); len(rec.EntryCommentedCauses) != 2 {
		t.Fatalf("cause-less mark dropped causes: %+v", rec)
	}
	st.MarkEntryCommentedCause(key, "h2", workflow)
	rec, _ = st.Get(key)
	if rec.EntryCommentedHead != "h2" || len(rec.EntryCommentedCauses) != 1 || rec.EntryCommentExplains("h2", sentinel) {
		t.Fatalf("new head kept old causes: %+v", rec)
	}
	// Legacy record: commented, no causes → everything counts as explained.
	legacy := Record{Stage: NeedsDecision, EntryCommented: true, EntryCommentedStage: NeedsDecision, EntryCommentedHead: "h1"}
	if !legacy.EntryCommentExplains("h1", workflow) || legacy.EntryCommentExplains("h2", workflow) {
		t.Fatal("legacy record semantics")
	}
}

// TestDecisionQuestionPlaceholderMention: a reviewer question addressed to
// the policy example's `@maintainer` placeholder is flagged in the 085
// comment, a real login is quoted as-is.
func TestDecisionQuestionPlaceholderMention(t *testing.T) {
	for q, want := range map[string]bool{
		"@maintainer: confirm this CI permissions change may land.":  true,
		"@maintainers should decide whether to keep the flag.":       true,
		"@clubanderson should decide whether a release may survive.": false,
		"@maintainer-bot owns this":                                  false,
		"the maintainer should decide":                               false,
	} {
		if got := decisionQuestionPlaceholderMention(q); got != want {
			t.Errorf("%q: got %v want %v", q, got, want)
		}
	}
	base := Evidence{Head: "989908e", Verdict: VerdictRequiresHuman, Checks: CheckSummary{Total: 6, Green: 6, Known: true}, DCO: DCOYes, Cause: CauseDecision, SelfAuthored: true}
	e := base
	e.DecisionQuestion = "@maintainer: confirm this CI permissions change may land."
	r := RouteHuman(e)
	if r.Stage != NeedsDecision || !strings.Contains(strings.Join(r.Reasons, "\n"), "named no specific person") {
		t.Fatalf("placeholder not flagged: %+v", r)
	}
	e = base
	e.DecisionQuestion = "@clubanderson should decide whether a release may survive."
	if r := RouteHuman(e); strings.Contains(strings.Join(r.Reasons, "\n"), "named no specific person") {
		t.Fatalf("real login flagged: %+v", r)
	}
}

// TestRouteHumanForkGuard: a red check that refuses the PR only because it
// is from a fork is a maintainer's, not the author's: 085 naming the check,
// never the fixing stage, even before the reviewer answers. Any other red
// check still sends the PR back to its author (hivecommons/hive#14273).
func TestRouteHumanForkGuard(t *testing.T) {
	e := readyEvidence()
	e.Cause = CauseDecision
	e.AuthorKind = authorkind.Clanker
	e.Verdict = VerdictPending
	e.Checks = CheckSummary{Total: 5, Green: 4, Red: []string{"Canary"}, Known: true}
	e.ForkGuard = []string{"Canary"}
	r := RouteHuman(e)
	const want = "check `Canary` refuses pull requests from a fork, so neither the author nor the hive can turn it green: a maintainer must push this branch to the repository (or approve the run as the repository requires)"
	if r.Stage != NeedsDecision || r.LaneReason != "" || !containsString(r.Reasons, want) {
		t.Fatalf("fork-guard route = %+v", r)
	}
	if containsString(r.Reasons, "the automerge lane will not merge this unattended") {
		t.Fatalf("fork-guard route names the generic lane rule: %+v", r)
	}
	if got := DecisionComment(r); !strings.Contains(got, want) {
		t.Fatalf("DecisionComment = %q", got)
	}
	e.Checks.Red = []string{"Canary", "unit"}
	if r := RouteHuman(e); r.Stage != ReturnedToContributor || r.LaneReason != "check-failing:unit" || !containsString(r.Reasons, "red checks: unit") {
		t.Fatalf("fork-guard + other red route = %+v", r)
	}
	e.Checks.Red = []string{"Canary"}
	e.ForkGuard = nil
	if r := RouteHuman(e); r.Stage != ReturnedToContributor || r.LaneReason != "check-failing:Canary" {
		t.Fatalf("no fork-guard evidence route = %+v", r)
	}
}

// hivecommons/hive#15202: a requires_human verdict on a PR a person pushed
// themselves routes to 071 Needs author with the reviewer's question (the
// human the reviewer wants IS the author), never to 085 and never to
// adoption. A conflicting head names the rebase. A hive or clanker PR, a
// trusted approval, a hold, a sentinel or a lane rule keep the 085 route.
func TestRouteHumanAuthorOwesAnswer(t *testing.T) {
	human := func(e *Evidence) {
		e.AuthorKind = authorkind.Human
		e.Verdict = VerdictRequiresHuman
		e.Cause, e.CauseReason = CauseDecision, "contributor-lane:decision-requested"
		e.DecisionQuestion = "@clubanderson should this land before the freeze?"
	}
	cases := []struct {
		name       string
		mutate     func(*Evidence)
		wantStage  Stage
		wantLane   string
		wantReason string
	}{
		{"human author question → 071", func(e *Evidence) {}, NeedsAuthor, LaneReasonReviewerQuestion, "reviewer question: @clubanderson should this land before the freeze?"},
		{"human author summary → 071", func(e *Evidence) {
			e.DecisionQuestion = ""
			e.HumanSummaries = []string{"security requires human: confirm the token scope"}
		}, NeedsAuthor, LaneReasonReviewerQuestion, "reviewer summary: security requires human: confirm the token scope"},
		{"human author conflicting → 071 needs rebase", func(e *Evidence) { e.Conflict = true }, NeedsAuthor, LaneReasonConflicting, "merge conflict with the base branch"},
		{"human author no cause reason → 071", func(e *Evidence) { e.Cause, e.CauseReason = CauseTrust, "" }, NeedsAuthor, LaneReasonReviewerQuestion, "the reviewer asked the author a question @abc1234"},
		{"clanker author keeps 085", func(e *Evidence) { e.AuthorKind = authorkind.Clanker }, NeedsDecision, "", "the reviewer swarm returned `requires_human` @abc1234"},
		{"hive author keeps 085", func(e *Evidence) { e.AuthorKind = authorkind.Hive }, NeedsDecision, "", "the reviewer swarm returned `requires_human` @abc1234"},
		{"self-authored keeps 085", func(e *Evidence) { e.SelfAuthored = true }, NeedsDecision, "", "the reviewer swarm returned `requires_human` @abc1234"},
		{"hold keeps 085", func(e *Evidence) { e.Holds = Holds([]string{"hold"}) }, NeedsDecision, "", "the `hold` label is set"},
		{"sentinel keeps 085", func(e *Evidence) { e.Sentinel = true }, NeedsDecision, "", "a sentinel alert is open on this PR"},
		{"lane rule keeps 085", func(e *Evidence) { e.CauseReason = "the lane excludes `workflow-paths` edits" }, NeedsDecision, "", "the lane excludes `workflow-paths` edits"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := readyEvidence()
			human(&e)
			tc.mutate(&e)
			r := RouteHuman(e)
			if r.Stage != tc.wantStage || !containsString(r.Reasons, tc.wantReason) {
				t.Fatalf("route = %+v", r)
			}
			if tc.wantLane != "" && r.LaneReason != tc.wantLane {
				t.Fatalf("lane reason = %q want %q (%+v)", r.LaneReason, tc.wantLane, r)
			}
		})
	}
	// A trusted merger's approval on the head still settles the verdict
	// ahead of the author route.
	e := readyEvidence()
	human(&e)
	e.HumanApprover = "clubanderson"
	e.Cause, e.CauseReason = CauseTrust, ""
	if r := RouteHuman(e); r.Stage == NeedsAuthor || r.Stage == NeedsDecision {
		t.Fatalf("approved head routed to %s: %+v", r.Stage, r)
	}
}

// TestRouteHumanChangeRequesterWinsOverParks: a trusted maintainer's
// CHANGES_REQUESTED review on the head is the person's answer to every
// "needs a human" park on it — the sentinel alert, a hold, a manual-merge
// lane rule — so the PR goes back to its author (072 for a clanker's, 071
// for a person's, 070 for the hive's) instead of 085
// (hivecommons/hive#15159: "Sentinel alert · needs human" re-derived over
// the owner's 072 every sweep).
func TestRouteHumanChangeRequesterWinsOverParks(t *testing.T) {
	e := readyEvidence()
	e.Sentinel = true
	e.Cause = CauseDecision
	e.CauseReason = "contributor-lane:sentinel-blocked"
	e.Holds = []string{"hold"}
	e.ManualMergeRule = true
	if r := RouteHuman(e); r.Stage != NeedsDecision {
		t.Fatalf("without a review the parks hold: %+v", r)
	}
	e.HumanChangeRequester = "@clubanderson"
	e.AuthorKind = authorkind.Clanker
	r := RouteHuman(e)
	if r.Stage != ReturnedToContributor || r.LaneReason != LaneReasonHumanChangesRequested {
		t.Fatalf("clanker route = %+v", r)
	}
	if len(r.Reasons) != 1 || r.Reasons[0] != "@clubanderson requested changes @abc1234" {
		t.Fatalf("reasons = %q", r.Reasons)
	}
	if !strings.Contains(r.Summary, "changes requested by @clubanderson") {
		t.Fatalf("summary = %q", r.Summary)
	}
	e.AuthorKind = authorkind.Human
	if r := RouteHuman(e); r.Stage != NeedsAuthor {
		t.Fatalf("human route = %+v", r)
	}
	e.SelfAuthored = true
	if r := RouteHuman(e); r.Stage != Fixing {
		t.Fatalf("self-authored route = %+v", r)
	}
}

// hivecommons/hive#15499: the same reviewer question re-asked on a new head
// is not a new entry comment; a different question is.
func TestEntryCommentRepeatsDecisionAcrossHeads(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("o/r", 15499)
	st.Observe(key, NeedsDecision, "verdict:decision-requested")
	st.MarkEntryCommentedCause(key, "h1", EntryCause([]string{"reviewer question: close as a no-op?"}))
	st.MarkEntryCommentedDecisionKey(key, "redundant-of:15427")
	rec, _ := st.Get(key)
	if !rec.EntryCommentRepeatsDecision("redundant-of:15427") || rec.EntryCommentRepeatsDecision("redundant-of:1") || rec.EntryCommentRepeatsDecision("") {
		t.Fatalf("after first entry: %+v", rec)
	}
	// A new head (merge-from-base) resets the per-head cause list but not
	// the decision identity.
	st.MarkEntryCommented(key, "h2")
	if rec, _ = st.Get(key); !rec.EntryCommentRepeatsDecision("redundant-of:15427") {
		t.Fatalf("new head lost the decision identity: %+v", rec)
	}
	// The 50↔85 flap #15499 showed (re-review on each head, then the same
	// verdict) keeps the identity: it is still the one question.
	st.Observe(key, ReviewPending, "")
	st.Observe(key, NeedsDecision, "verdict:decision-requested")
	if rec, _ = st.Get(key); !rec.EntryCommentRepeatsDecision("redundant-of:15427") {
		t.Fatalf("stage flap lost the decision identity: %+v", rec)
	}
}

func TestAlreadyLandedReasonCaption(t *testing.T) {
	if got := AlreadyLandedReason(15506); got != "self-authored:already-landed:#15506" {
		t.Fatalf("reason=%q", got)
	}
	if c, ok := AlreadyLandedCaption(AlreadyLandedReason(15506)); !ok || c != "already landed via #15506 — closing as superseded" {
		t.Fatalf("caption=%q ok=%v", c, ok)
	}
	if c, ok := AlreadyLandedCaption(AlreadyLandedReason(0)); !ok || c != "already landed on the base — closing as superseded" {
		t.Fatalf("bare caption=%q ok=%v", c, ok)
	}
	if _, ok := AlreadyLandedCaption("superseded-by-merge"); ok {
		t.Fatal("other reasons are not already-landed")
	}
}
