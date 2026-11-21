package stagelabels

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
)

// trustedAuthorQuestionEvidence is a trusted author's PR (a clanker wrote
// it, as on hivecommons/hive#19421) with the reviewer's requires_human
// question open on the head and CI still running.
func trustedAuthorQuestionEvidence() Evidence {
	e := readyEvidence()
	e.Verdict = VerdictRequiresHuman
	e.AuthorKind = authorkind.Clanker
	e.Cause = CauseDecision
	e.Checks = CheckSummary{Total: 64, Green: 62, Pending: 2, Known: true}
	e.DecisionFinding = "the reviewer marked this change high-risk"
	e.DecisionQuestion = "@clubanderson should the hive resolve its own threads?"
	return e
}

// The trusted-author lane merges a requires_human head when its review gate
// does not enforce (hivecommons/hive#19421): the 085 comment keeps the
// reviewer's question but says the PR will still merge and names the label
// that stops it, instead of "will not merge this unattended" and an
// Approve step.
func TestDecisionCommentTrustedAuthorMergesDespiteQuestion(t *testing.T) {
	e := trustedAuthorQuestionEvidence()
	e.TrustedAuthorMerges = true
	r := RouteHuman(e)
	if r.Stage != NeedsDecision || !r.TrustedAuthorMerges || r.TrustedAuthorHoldLabel != "hold" {
		t.Fatalf("route = %+v", r)
	}
	if containsString(r.Reasons, decisionConsequenceReason) || !containsString(r.Reasons, trustedAuthorMergesReason) {
		t.Fatalf("reasons = %q", r.Reasons)
	}
	got := DecisionComment(r)
	for _, want := range []string{
		"\nNeeds a decision, but the hive will still merge this PR:",
		"- the reviewer swarm returned `requires_human` @abc1234",
		"- reviewer question: @clubanderson should the hive resolve its own threads?",
		"- the trusted-author lane will merge this head once its checks are green, even with the reviewer's question open",
		"**What to do:** answer the reviewer's question on this PR",
		"to stop the merge until it is answered, add the `hold` label.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("DecisionComment missing %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"will not merge this unattended", "Needs a decision, not a merge", "**Approve** review", "a trusted merger approves this head"} {
		if strings.Contains(got, bad) {
			t.Fatalf("DecisionComment still says %q:\n%s", bad, got)
		}
	}

	// The configured first exclude label is the one named.
	e.TrustedAuthorHoldLabel = "do-not-merge"
	if got := DecisionComment(RouteHuman(e)); !strings.Contains(got, "add the `do-not-merge` label.") {
		t.Fatalf("DecisionComment does not name the configured label:\n%s", got)
	}
}

// Without the lane's word — its review gate enforces, the PR is the hive's
// own (the self-authored lane always holds on the verdict), or it is a
// contributor-lane PR — the 085 comment is the one it always was.
func TestDecisionCommentUnchangedWhenTheLaneHolds(t *testing.T) {
	cases := map[string]func(*Evidence){
		"trusted author, gate enforces": func(*Evidence) {},
		"self-authored": func(e *Evidence) {
			e.SelfAuthored = true
			e.AuthorKind = authorkind.Hive
		},
		"contributor lane": func(e *Evidence) { e.Cause = CauseTrust },
	}
	for name, mutate := range cases {
		e := trustedAuthorQuestionEvidence()
		mutate(&e)
		r := RouteHuman(e)
		got := DecisionComment(r)
		if r.TrustedAuthorMerges || containsString(r.Reasons, trustedAuthorMergesReason) || strings.Contains(got, "will still merge") {
			t.Fatalf("%s: route = %+v\n%s", name, r, got)
		}
		if !strings.Contains(got, "\nNeeds a decision, not a merge:") {
			t.Fatalf("%s: lead line changed:\n%s", name, got)
		}
		if e.Cause == CauseDecision && !containsString(r.Reasons, decisionConsequenceReason) {
			t.Fatalf("%s: reasons = %q, want the lane-rule consequence", name, r.Reasons)
		}
	}
}

// The lane's word is not the whole story when something else on the head
// stops it or answered the question: a hold label, an open sentinel alert,
// a manual-merge rule, a fork guard, a trusted approval, or a dirty or
// conflicting head (the lane cannot merge it as it stands) keeps the
// comment it always had.
func TestDecisionCommentTrustedAuthorMergesYieldsToOtherStops(t *testing.T) {
	cases := map[string]func(*Evidence){
		"hold label":    func(e *Evidence) { e.Holds = Holds([]string{"hold"}) },
		"sentinel":      func(e *Evidence) { e.Sentinel = true },
		"manual merge":  func(e *Evidence) { e.ManualMerge, e.CauseReason = true, "the PR edits CI workflow files" },
		"fork guard":    func(e *Evidence) { e.ForkGuard = []string{"Canary"} },
		"approved head": func(e *Evidence) { e.HumanApprover = "octocat" },
		"dirty head":    func(e *Evidence) { e.HeadDirty = true },
		"conflict":      func(e *Evidence) { e.Conflict = true },
	}
	for name, mutate := range cases {
		e := trustedAuthorQuestionEvidence()
		e.TrustedAuthorMerges = true
		mutate(&e)
		r := RouteHuman(e)
		if r.TrustedAuthorMerges || containsString(r.Reasons, trustedAuthorMergesReason) {
			t.Fatalf("%s: route = %+v", name, r)
		}
	}
}

// The comment is posted once and edited only when its cause changes
// (EntryCause): the trusted-author bullet does not depend on which lane
// observed the PR, so two lanes observing it on alternate passes digest it
// the same, while the old wording on the same head digests differently and
// is rewritten once.
func TestEntryCauseTrustedAuthorMergesStable(t *testing.T) {
	decision := trustedAuthorQuestionEvidence()
	decision.TrustedAuthorMerges = true
	trust := decision
	trust.Cause = CauseTrust
	a, b := RouteHuman(decision), RouteHuman(trust)
	if EntryCause(a.Reasons) != EntryCause(b.Reasons) {
		t.Fatalf("digest differs by observing lane:\n%q\n%q", a.Reasons, b.Reasons)
	}
	if EntryCause(a.Reasons) != EntryCause(RouteHuman(decision).Reasons) {
		t.Fatal("digest differs between two passes on the same evidence")
	}
	old := RouteHuman(trustedAuthorQuestionEvidence())
	if EntryCause(old.Reasons) == EntryCause(a.Reasons) {
		t.Fatalf("the wording that promised no merge digests as the corrected one: %q", old.Reasons)
	}
}

// The stage pass records the head it routed as a trusted-author 085 for the
// Activity monitor and the "Your move" section; a route that says otherwise
// clears it, a new head reads as no note, and a note no pass renewed within
// TrustedAuthorMergesMaxAge has expired.
func TestStoreNoteTrustedAuthorMerges(t *testing.T) {
	at := time.Date(2026, 11, 21, 13, 54, 0, 0, time.UTC)
	s := NewStore("", nil)
	key := Key("acme/widget", 7)
	s.NoteTrustedAuthorMerges(key, "h1", "hold", at)
	if _, ok := s.Get(key); ok {
		t.Fatal("NoteTrustedAuthorMerges created a record")
	}
	s.Observe(key, NeedsDecision, "reviewer-verdict:requires_human")
	s.NoteTrustedAuthorMerges(key, "h1", "hold", at)
	rec, _ := s.Get(key)
	if rec.TrustedAuthorMergesOn("h1", at) != "hold" || rec.TrustedAuthorMergesOn("h2", at) != "" || rec.TrustedAuthorMergesOn("", at) != "" {
		t.Fatalf("record = %q@%q", rec.TrustedAuthorHoldLabel, rec.TrustedAuthorMergesHead)
	}
	if rec.TrustedAuthorMergesOn("h1", at.Add(TrustedAuthorMergesMaxAge+time.Minute)) != "" {
		t.Fatal("a note no pass renewed past TrustedAuthorMergesMaxAge still reads")
	}
	// A later pass renews it.
	later := at.Add(TrustedAuthorMergesMaxAge)
	s.NoteTrustedAuthorMerges(key, "h1", "hold", later)
	if rec, _ := s.Get(key); rec.TrustedAuthorMergesOn("h1", later.Add(time.Minute)) != "hold" {
		t.Fatalf("renewed note = %q@%q at %v", rec.TrustedAuthorHoldLabel, rec.TrustedAuthorMergesHead, rec.TrustedAuthorMergesAt)
	}
	s.NoteTrustedAuthorMerges(key, "h1", "", later)
	if rec, _ := s.Get(key); rec.TrustedAuthorMergesOn("h1", later) != "" || rec.TrustedAuthorMergesHead != "" {
		t.Fatalf("cleared record = %q@%q", rec.TrustedAuthorHoldLabel, rec.TrustedAuthorMergesHead)
	}
}

// A person's own PR with the reviewer's question routes to 071 (the author
// answers it, hivecommons/hive#15202). The route carries the lane's note so
// the author's note can say the PR will still merge; a hold label, or a 071
// for a conflict, red check or requested change (which do stop the merge),
// does not carry it.
func TestRouteHumanAuthorQuestionCarriesTrustedAuthorMerges(t *testing.T) {
	e := trustedAuthorQuestionEvidence()
	e.AuthorKind = authorkind.Human
	e.Checks = CheckSummary{Total: 3, Green: 3, Known: true}
	e.TrustedAuthorMerges, e.TrustedAuthorHoldLabel = true, "do-not-merge"
	r := RouteHuman(e)
	if r.Stage != NeedsAuthor || r.LaneReason != LaneReasonReviewerQuestion || !r.TrustedAuthorMerges || r.TrustedAuthorHoldLabel != "do-not-merge" {
		t.Fatalf("route = %+v", r)
	}
	cases := map[string]func(*Evidence){
		"hold label":        func(e *Evidence) { e.Holds = Holds([]string{"hold"}) },
		"conflict":          func(e *Evidence) { e.Conflict = true },
		"red check":         func(e *Evidence) { e.Checks = CheckSummary{Total: 3, Green: 2, Red: []string{"unit"}, Known: true} },
		"changes requested": func(e *Evidence) { e.Verdict = VerdictChangesRequested },
		"dirty head":        func(e *Evidence) { e.HeadDirty = true },
	}
	for name, mutate := range cases {
		c := e
		mutate(&c)
		if r := RouteHuman(c); r.TrustedAuthorMerges {
			t.Fatalf("%s: route = %+v", name, r)
		}
	}
}

// WithTrustedAuthorMerges gives a requires_human 085 built without Evidence
// (the same-head re-park) the same bullet and wording as RouteHuman, so both
// digest alike; any other route is returned as it is.
func TestWithTrustedAuthorMerges(t *testing.T) {
	ev := trustedAuthorQuestionEvidence()
	ev.TrustedAuthorMerges = true
	want := RouteHuman(ev)
	bare := Route{Stage: NeedsDecision, Head: ev.Head, Verdict: VerdictRequiresHuman,
		Reasons: RequiresHumanReasons(ev.Head, ev.DecisionFinding, ev.DecisionQuestion, nil)}
	got := WithTrustedAuthorMerges(bare, "")
	if !got.TrustedAuthorMerges || got.TrustedAuthorHoldLabel != "hold" || EntryCause(got.Reasons) != EntryCause(want.Reasons) {
		t.Fatalf("re-park route = %+v, want the reasons of %+v", got, want)
	}
	if len(bare.Reasons) != 3 {
		t.Fatalf("the caller's reasons were modified: %q", bare.Reasons)
	}
	if again := WithTrustedAuthorMerges(got, "hold"); len(again.Reasons) != len(got.Reasons) {
		t.Fatalf("the bullet was added twice: %q", again.Reasons)
	}
	for name, r := range map[string]Route{
		"approved":     {Stage: NeedsDecision, Verdict: VerdictRequiresHuman, HumanApprover: "octocat"},
		"held":         {Stage: NeedsDecision, Verdict: VerdictRequiresHuman, Holds: []string{"hold"}},
		"manual merge": {Stage: NeedsDecision, Verdict: VerdictRequiresHuman, ManualMerge: true},
		"no verdict":   {Stage: NeedsDecision},
		"not 085":      {Stage: Fixing, Verdict: VerdictRequiresHuman},
	} {
		if out := WithTrustedAuthorMerges(r, "hold"); out.TrustedAuthorMerges || len(out.Reasons) != 0 {
			t.Fatalf("%s: route = %+v", name, out)
		}
	}
}
