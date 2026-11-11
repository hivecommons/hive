package stageengine

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// rulesProfile hand-builds a rules-mode profile so every predicate kind,
// the kind filter, terminal/no-number states and the defence-in-depth
// paths in decisionFor can be exercised without a model fixture.
func rulesProfile(rules ...workflowmodel.Rule) *workflowmodel.Profile {
	st := func(n int) *workflowmodel.State {
		return &workflowmodel.State{Lifecycle: workflowmodel.StateLifecycle{Number: n}}
	}
	return &workflowmodel.Profile{
		Name: "t", Version: "1", Mode: workflowmodel.DerivationRules,
		States: map[string]*workflowmodel.State{
			"ten": st(10), "fifty": st(20), "done": st(0), "nonum": st(0), "bogus": st(999), "pronly": st(80),
		},
		StateOfNumber: map[int]string{10: "ten", 20: "fifty", 80: "pronly"},
		Terminal:      map[string]bool{"done": true},
		Rules:         rules,
	}
}

func pred(fact string, op workflowmodel.Op, value string) workflowmodel.Predicate {
	return workflowmodel.Predicate{Fact: fact, Op: op, Value: value}
}

func simIssue() Facts {
	return Facts{
		Ref:    Ref{Owner: "o", Repo: "r", Number: 1, Kind: stagelabels.IssueItem},
		Open:   true,
		Labels: []string{"Kind/Bug", "area/docs"},
		Origin: "origin/scanner",
		Author: AuthorFacts{Login: "nate", Kind: authorkind.Human},
		Park:   ParkFacts{Holds: []string{"hold"}, NeedsHuman: true},
		Issue:  &IssueFacts{Bucket: BucketActionable, Claim: &ClaimFact{}, CoveringPR: &Ref{}, Stalled: true, OpenChildren: 2},
	}
}

func simPR() Facts {
	return Facts{
		Ref:    Ref{Owner: "o", Repo: "r", Number: 2, Kind: stagelabels.PRItem},
		Open:   true,
		Author: AuthorFacts{Login: "bot", Kind: authorkind.Hive, SelfAuthored: true, Role: stagelabels.RoleTrusted},
		PR: &PRFacts{
			BaseRef: "main", HeadFresh: true, Conflicting: true, ChangesRequested: true,
			Checks: stagelabels.CheckSummary{Known: true, Total: 1, Red: []string{"lint"}},
			Review: ReviewFacts{State: stagelabels.VerdictChangesRequested},
			DCO:    stagelabels.DCONo,
		},
	}
}

func TestHoldsEveryFactAndOperator(t *testing.T) {
	is, pr := simIssue(), simPR()
	cases := []struct {
		f    Facts
		p    workflowmodel.Predicate
		want bool
	}{
		{is, pred("open", workflowmodel.OpTrue, ""), true},
		{is, pred("open", workflowmodel.OpFalse, ""), false},
		{is, pred("merged", workflowmodel.OpFalse, ""), true},
		{is, pred("draft", workflowmodel.OpTrue, ""), false},
		{is, pred("hold", workflowmodel.OpTrue, ""), true},
		{is, pred("park", workflowmodel.OpTrue, ""), true},
		{pr, pred("park", workflowmodel.OpFalse, ""), true},
		{is, pred("issue.claimed", workflowmodel.OpTrue, ""), true},
		{is, pred("issue.covered", workflowmodel.OpTrue, ""), true},
		{is, pred("issue.stalled", workflowmodel.OpTrue, ""), true},
		{is, pred("issue.open_children", workflowmodel.OpTrue, ""), true},
		{pr, pred("issue.claimed", workflowmodel.OpFalse, ""), true},
		{pr, pred("pr.changes_requested", workflowmodel.OpTrue, ""), true},
		{pr, pred("pr.conflicting", workflowmodel.OpTrue, ""), true},
		{pr, pred("pr.head_fresh", workflowmodel.OpTrue, ""), true},
		{is, pred("pr.head_fresh", workflowmodel.OpFalse, ""), true},
		{is, pred("origin", workflowmodel.OpIs, "ORIGIN/scanner"), true},
		{is, pred("origin", workflowmodel.OpNot, "origin/scanner"), false},
		{is, pred("author.kind", workflowmodel.OpIs, "human"), true},
		{pr, pred("author.kind", workflowmodel.OpIs, "hive"), true},
		{is, pred("author.login", workflowmodel.OpIs, "nate"), true},
		{is, pred("issue.bucket", workflowmodel.OpIs, "actionable"), true},
		{pr, pred("issue.bucket", workflowmodel.OpIs, ""), true},
		{pr, pred("pr.checks", workflowmodel.OpIs, "red"), true},
		{is, pred("pr.checks", workflowmodel.OpIs, ""), true},
		{pr, pred("pr.review", workflowmodel.OpIs, "changes_requested"), true},
		{is, pred("pr.review", workflowmodel.OpIs, ""), true},
		{pr, pred("pr.dco", workflowmodel.OpIs, "no"), true},
		{is, pred("pr.dco", workflowmodel.OpIs, ""), true},
		{pr, pred("pr.base", workflowmodel.OpIs, "main"), true},
		{is, pred("pr.base", workflowmodel.OpIs, ""), true},
		{is, pred("pipeline.status", workflowmodel.OpIs, "unknown"), true},
		{is, pred("labels", workflowmodel.OpHas, "kind/*"), true},
		{is, pred("labels", workflowmodel.OpHas, "AREA/DOCS"), true},
		{is, pred("labels", workflowmodel.OpLacks, "kind/*"), false},
		{is, pred("labels", workflowmodel.OpLacks, "priority/*"), true},
		{is, pred("no.such.fact", workflowmodel.OpTrue, ""), false},
	}
	for _, c := range cases {
		if got := holds(c.f, c.p); got != c.want {
			t.Errorf("%s %s %q on %s: got %v want %v", c.p.Fact, c.p.Op, c.p.Value, c.f.Ref.Kind, got, c.want)
		}
	}
	// Facts the vocabulary knows but the readers do not are "unknown" to
	// explainPredicate and never hold.
	saved := workflowmodel.Facts
	defer func() { workflowmodel.Facts = saved }()
	workflowmodel.Facts = map[string]workflowmodel.FactType{"x.bool": workflowmodel.FactBool, "x.scalar": workflowmodel.FactScalar, "x.set": workflowmodel.FactSet}
	for _, p := range []workflowmodel.Predicate{pred("x.bool", workflowmodel.OpTrue, ""), pred("x.scalar", workflowmodel.OpIs, "a"), pred("x.set", workflowmodel.OpHas, "a")} {
		if holds(is, p) {
			t.Errorf("%s holds", p.Fact)
		}
		if pt := explainPredicate(is, p); pt.Known || pt.Holds || pt.Actual != "" {
			t.Errorf("%s explained as %+v", p.Fact, pt)
		}
	}
}

func TestScalarNamesAndParsersRoundTrip(t *testing.T) {
	for _, k := range []authorkind.Kind{authorkind.Human, authorkind.Hive, authorkind.Clanker, authorkind.Unknown} {
		if got := ParseAuthorKind(" " + strings.ToUpper(AuthorKindName(k)) + " "); got != k {
			t.Errorf("author kind %v → %q → %v", k, AuthorKindName(k), got)
		}
	}
	if ParseAuthorKind("robot") != authorkind.Unknown || AuthorKindName(authorkind.Kind("martian")) != "unknown" {
		t.Error("unknown author kind")
	}

	for _, name := range []string{"green", "red", "pending", "missing", "unknown"} {
		if got := ChecksName(ParseChecks(strings.ToUpper(name))); got != name {
			t.Errorf("checks %q → %q", name, got)
		}
	}
	if c := ParseChecks("none"); !c.Known || c.Total != 0 || ChecksName(c) != "green" {
		t.Errorf("none → %+v", c)
	}
	if c := ParseChecks("missing"); !c.Known || c.Total != 0 {
		t.Errorf("missing → %+v", c)
	}
	if c := ParseChecks(""); c.Known {
		t.Errorf("empty → %+v", c)
	}
	if c := ParseChecks("red"); len(c.Red) != 1 || c.Red[0] != "ci" {
		t.Errorf("red → %+v", c)
	}

	for _, name := range []string{"approved", "changes_requested", "unavailable", "pending"} {
		if got := ReviewName(ParseReview(name)); got != name {
			t.Errorf("review %q → %q", name, got)
		}
	}
	if ParseReview("requires_human") != stagelabels.VerdictRequiresHuman || ParseReview("lgtm") != stagelabels.VerdictPending {
		t.Error("review parse")
	}
	if ReviewName(stagelabels.VerdictRequiresHuman) != "pending" {
		t.Error("requires_human has no scalar spelling and reads pending")
	}

	for _, name := range []string{"yes", "no", "unknown"} {
		if got := DCOName(ParseDCO(name)); got != name {
			t.Errorf("dco %q → %q", name, got)
		}
	}
	if ParseDCO("maybe") != stagelabels.DCOUnknown {
		t.Error("dco parse")
	}
}

func TestExplainRulesBranches(t *testing.T) {
	p := rulesProfile(
		workflowmodel.Rule{ID: "pr-only", Kind: workflowmodel.KindPR, State: "pronly", When: []workflowmodel.Predicate{pred("open", workflowmodel.OpTrue, "")}},
		workflowmodel.Rule{ID: "needs-both", State: "ten", When: []workflowmodel.Predicate{pred("labels", workflowmodel.OpHas, "kind/*"), pred("author.kind", workflowmodel.OpIs, "clanker")}},
		workflowmodel.Rule{Kind: workflowmodel.KindIssue, State: "fifty", Reason: "docs issue", When: []workflowmodel.Predicate{pred("labels", workflowmodel.OpHas, "area/docs")}},
		workflowmodel.Rule{ID: "after", State: "ten"},
	)
	f := simIssue()
	dec, trace := Explain(p, stagelabels.IssueItem, f)
	if dec.Abstain || dec.Stage.Number() != 20 || dec.Reason != "docs issue" || dec.Kind != authorkind.Human {
		t.Fatalf("decision %+v", dec)
	}
	if len(trace) != 4 {
		t.Fatalf("trace %+v", trace)
	}
	if trace[0].Skipped == "" || trace[0].Evaluated || trace[0].Kind != "pr" {
		t.Errorf("pr-only not skipped: %+v", trace[0])
	}
	if !trace[1].Evaluated || trace[1].Matched || len(trace[1].Predicates) != 2 || !trace[1].Predicates[0].Holds || trace[1].Predicates[1].Holds || trace[1].Predicates[1].Actual != "human" || trace[1].Predicates[0].Actual != "Kind/Bug area/docs" {
		t.Errorf("needs-both: %+v", trace[1])
	}
	if !trace[2].Matched || trace[2].ID != "rules[2]" || trace[2].State != "fifty" || trace[2].Reason != "docs issue" {
		t.Errorf("match: %+v", trace[2])
	}
	if trace[3].Evaluated || trace[3].Matched || trace[3].ID != "after" {
		t.Errorf("after: %+v", trace[3])
	}

	// Empty When matches unconditionally and the default reason names the rule.
	dec, trace = Explain(rulesProfile(workflowmodel.Rule{State: "ten"}), stagelabels.IssueItem, f)
	if dec.Abstain || dec.Stage.Number() != 10 || !strings.Contains(dec.Reason, "rules[0]") || !trace[0].Matched {
		t.Fatalf("empty when: %+v %+v", dec, trace)
	}

	// Kind mismatch between facts and deriver: abstain, no trace.
	dec, trace = Explain(p, stagelabels.PRItem, f)
	if !dec.Abstain || trace != nil {
		t.Fatalf("kind mismatch: %+v %+v", dec, trace)
	}
	// PR facts take the PR-only rule.
	dec, trace = Explain(p, stagelabels.PRItem, simPR())
	if dec.Abstain || dec.Stage.Number() != 80 || !trace[0].Matched || dec.DCO != stagelabels.DCONo {
		t.Fatalf("pr: %+v %+v", dec, trace)
	}
}

func TestDecisionForEdges(t *testing.T) {
	f := simIssue()
	for _, c := range []struct {
		state    string
		abstain  bool
		terminal bool
		reason   string
	}{
		{"done", false, true, ""},
		{"nonum", true, false, "carries no stage number"},
		{"bogus", true, false, "not in the stage table"},
		{"pronly", true, false, "not a issue stage"},
		{"ghost", true, false, "is not in t@1"},
	} {
		dec, _ := Explain(rulesProfile(workflowmodel.Rule{State: c.state}), stagelabels.IssueItem, f)
		if dec.Abstain != c.abstain || dec.Terminal != c.terminal || !strings.Contains(dec.Reason, c.reason) {
			t.Errorf("%s: %+v", c.state, dec)
		}
	}

	// Unknown derivation mode is not covered either.
	p := rulesProfile()
	p.Mode = workflowmodel.DerivationMode("magic")
	if dec := DeriveFromModel(p, stagelabels.IssueItem).Derive(f); !dec.Abstain || !strings.Contains(dec.Reason, "magic") {
		t.Errorf("mode: %+v", dec)
	}
	if DeriveFromModel(p, stagelabels.IssueItem).Kind() != stagelabels.IssueItem {
		t.Error("kind")
	}
}

func TestExplainBuiltinBranches(t *testing.T) {
	// A builtin profile whose state table lacks the derived stage: Derive
	// abstains, and Explain reports it as an unmatched builtin trace.
	p := &workflowmodel.Profile{Name: "b", Version: "1", Mode: workflowmodel.DerivationBuiltin, StateOfNumber: map[int]string{}}
	f := simIssue()
	f.Park = ParkFacts{}
	f.Issue = &IssueFacts{Bucket: BucketActionable}
	dec, trace := Explain(p, stagelabels.IssueItem, f)
	if !dec.Abstain || !strings.Contains(dec.Reason, "no state for stage") || len(trace) != 1 || !trace[0].Builtin || trace[0].Matched || trace[0].State != "" {
		t.Fatalf("%+v %+v", dec, trace)
	}
	// A draft PR is Stage None without abstaining: the trace carries no state.
	pr := simPR()
	pr.Draft = true
	pr.PR = &PRFacts{Lanes: []LaneFact{{Lane: prstage.StageLaneSelf, Reason: "draft"}}}
	dec, trace = Explain(p, stagelabels.PRItem, pr)
	if dec.Abstain || dec.Stage != stagelabels.None || len(trace) != 1 || !trace[0].Matched || trace[0].State != "" {
		t.Fatalf("draft %+v %+v", dec, trace)
	}
}

func TestSimulatedLane(t *testing.T) {
	base := func() Facts {
		return Facts{Open: true, Author: AuthorFacts{Role: stagelabels.RoleContributor},
			PR: &PRFacts{Checks: stagelabels.CheckSummary{Known: true, Total: 1, Green: 1}, Review: ReviewFacts{State: stagelabels.VerdictApproved}}}
	}
	cases := []struct {
		name   string
		mut    func(*Facts)
		lane   string
		reason string
		merged bool
	}{
		{"clean contributor", func(f *Facts) {}, prstage.StageLaneContributor, "", false},
		{"self", func(f *Facts) { f.Author.SelfAuthored = true }, prstage.StageLaneSelf, "", false},
		{"trusted", func(f *Facts) { f.Author.Role = stagelabels.RoleTrusted }, prstage.StageLaneTrusted, "", false},
		{"merged", func(f *Facts) { f.Merged = true; f.Open = false }, prstage.StageLaneContributor, "", true},
		{"closed", func(f *Facts) { f.Open = false }, prstage.StageLaneContributor, "closed", false},
		{"draft", func(f *Facts) { f.Draft = true }, prstage.StageLaneContributor, "draft", false},
		{"conflict", func(f *Facts) { f.PR.Conflicting = true }, prstage.StageLaneContributor, "conflicting", false},
		{"changes", func(f *Facts) { f.PR.ChangesRequested = true }, prstage.StageLaneContributor, "changes-requested", false},
		{"held", func(f *Facts) { f.Park.Holds = []string{"hold"} }, prstage.StageLaneContributor, "held", false},
		{"needs human", func(f *Facts) { f.Park.NeedsHuman = true }, prstage.StageLaneContributor, "label:needs-human", false},
		{"red", func(f *Facts) { f.PR.Checks.Red = []string{"lint"} }, prstage.StageLaneContributor, "check-failing:lint", false},
		{"pending", func(f *Facts) { f.PR.Checks.Pending = 1 }, prstage.StageLaneContributor, "check-pending", false},
		{"unread", func(f *Facts) { f.PR.Checks = stagelabels.CheckSummary{} }, prstage.StageLaneContributor, "check-pending", false},
		{"no pr facts", func(f *Facts) { f.PR = nil }, prstage.StageLaneContributor, "check-pending", false},
		{"no verdict", func(f *Facts) { f.PR.Review.State = stagelabels.VerdictPending }, prstage.StageLaneContributor, prstage.ContributorLaneReasonNoVerdict, false},
		{"unavailable", func(f *Facts) { f.PR.Review.State = stagelabels.VerdictUnavailable }, prstage.StageLaneContributor, prstage.ContributorLaneReasonNoVerdict, false},
		{"untrusted", func(f *Facts) { f.Author.Role = stagelabels.RoleUntrusted }, prstage.StageLaneContributor, "untrusted-author-role", false},
		{"newcomer", func(f *Facts) { f.Author.Role = stagelabels.RoleNewcomer }, prstage.StageLaneContributor, "untrusted-author-role", false},
	}
	for _, c := range cases {
		f := base()
		c.mut(&f)
		l := SimulatedLane(f)
		if l.Lane != c.lane || l.Reason != c.reason || l.Merged != c.merged || l.Role != f.Author.Role {
			t.Errorf("%s: %+v", c.name, l)
		}
	}
}

func TestSimulateRef(t *testing.T) {
	r, err := SimulateRef("issue", "o", "r", 0)
	if err != nil || r.Kind != stagelabels.IssueItem || r.Number != 0 || r.Owner != "o" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = SimulateRef("pr", "", "", 12)
	if err != nil || r.Kind != stagelabels.PRItem || r.Number != 12 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "PR", "issues", "epic"} {
		if _, err := SimulateRef(bad, "o", "r", 1); err == nil || !strings.Contains(err.Error(), "kind must be issue or pr") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
