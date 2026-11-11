package stageengine_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

func techdocsProfile(t *testing.T) *workflowmodel.Profile {
	t.Helper()
	b, err := os.ReadFile("../workflowmodel/testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	p, diags, err := workflowmodel.Compile(&m)
	if err != nil {
		t.Fatalf("%v %+v", err, diags)
	}
	return p
}

func issueFacts(labels ...string) stageengine.Facts {
	return stageengine.Facts{
		Ref:    stageengine.Ref{Owner: "cncf", Repo: "techdocs", Number: 1, Kind: stagelabels.IssueItem},
		Open:   true,
		Labels: labels,
		Author: stageengine.AuthorFacts{Kind: authorkind.Human, Login: "nate"},
		Issue:  &stageengine.IssueFacts{},
	}
}

func prFacts(labels ...string) stageengine.Facts {
	return stageengine.Facts{
		Ref:    stageengine.Ref{Owner: "cncf", Repo: "techdocs", Number: 2, Kind: stagelabels.PRItem},
		Open:   true,
		Labels: labels,
		Author: stageengine.AuthorFacts{Kind: authorkind.Hive},
		PR:     &stageengine.PRFacts{Checks: stagelabels.CheckSummary{Known: true, Total: 2, Green: 2}, DCO: stagelabels.DCOYes},
	}
}

// TestDeriveFromModelRules walks a TechDocs engagement through the rules
// deriver: ordered first match, kind filtering, glob labels, scalar
// facts, terminal states and the not-covered abstention.
func TestDeriveFromModelRules(t *testing.T) {
	p := techdocsProfile(t)
	issues := stageengine.DeriveFromModel(p, stagelabels.IssueItem)
	prs := stageengine.DeriveFromModel(p, stagelabels.PRItem)

	cases := []struct {
		name   string
		d      stageengine.Deriver
		f      func() stageengine.Facts
		stage  stagelabels.Stage
		term   bool
		reason string
	}{
		{"request filed", issues, func() stageengine.Facts { return issueFacts("techdocs/request") }, stagelabels.Actionable, false, "workflow cncf-techdocs@1: rule requested"},
		{"accepted", issues, func() stageengine.Facts { return issueFacts("techdocs/request", "techdocs/accepted") }, stagelabels.Claimed, false, ""},
		{"analysis posted", issues, func() stageengine.Facts { return issueFacts("techdocs/accepted", "techdocs/analysis") }, stagelabels.IssueNeedsDecision, false, ""},
		{"covered by a draft PR", issues, func() stageengine.Facts {
			f := issueFacts("techdocs/analysis")
			f.Issue.CoveringPR = &stageengine.Ref{Owner: "cncf", Repo: "techdocs", Number: 2, Kind: stagelabels.PRItem}
			return f
		}, stagelabels.CoveredByPR, false, ""},
		{"closed unpublished", issues, func() stageengine.Facts { f := issueFacts("techdocs/request"); f.Open = false; return f }, stagelabels.None, true, "closed without publishing"},
		{"draft PR", prs, func() stageengine.Facts { f := prFacts("techdocs/project-x"); f.Draft = true; return f }, stagelabels.Fixing, false, ""},
		{"ready PR awaits review", prs, func() stageengine.Facts { return prFacts("TechDocs/project-x") }, stagelabels.ReviewPending, false, ""},
		{"approved green PR", prs, func() stageengine.Facts {
			f := prFacts("techdocs/project-x")
			f.PR.Review.State = stagelabels.VerdictApproved
			return f
		}, stagelabels.ReadyForHumanMerge, false, ""},
		{"approved but red stays in review", prs, func() stageengine.Facts {
			f := prFacts("techdocs/project-x")
			f.PR.Review.State = stagelabels.VerdictApproved
			f.PR.Checks.Red = []string{"lint"}
			return f
		}, stagelabels.ReviewPending, false, ""},
		{"merged", prs, func() stageengine.Facts { f := prFacts(); f.Open, f.Merged = false, true; return f }, stagelabels.None, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.d.Derive(tc.f())
			if got.Abstain {
				t.Fatalf("abstained: %s", got.Reason)
			}
			if got.Terminal != tc.term || got.Stage != tc.stage {
				t.Fatalf("got %+v; want stage %s terminal %v", got, tc.stage, tc.term)
			}
			if tc.reason != "" && got.Reason != tc.reason {
				t.Fatalf("reason %q, want %q", got.Reason, tc.reason)
			}
			if tc.f().Ref.Kind == stagelabels.PRItem && !tc.term && got.DCO != stagelabels.DCOYes {
				t.Fatalf("DCO not carried: %+v", got)
			}
		})
	}

	t.Run("no rule matches", func(t *testing.T) {
		got := issues.Derive(issueFacts("bug"))
		if !got.Abstain || !strings.HasPrefix(got.Reason, stageengine.ReasonNotCovered) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("kind mismatch", func(t *testing.T) {
		if got := prs.Derive(issueFacts("techdocs/request")); !got.Abstain {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("stage numbers follow the model", func(t *testing.T) {
		if got := issues.Derive(issueFacts("techdocs/request")); got.Stage.Number() != 10 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("issue-side number on a PR state is not covered", func(t *testing.T) {
		q := *p
		q.Rules = []workflowmodel.Rule{{State: "requested", When: []workflowmodel.Predicate{{Fact: "open", Op: workflowmodel.OpTrue}}}}
		got := stageengine.DeriveFromModel(&q, stagelabels.PRItem).Derive(prFacts())
		if !got.Abstain || !strings.Contains(got.Reason, "not a pr stage") {
			t.Fatalf("%+v", got)
		}
	})
}

// Every fact in the vocabulary evaluates; none is silently false because
// the evaluator forgot it.
func TestRulesFactVocabularyIsComplete(t *testing.T) {
	p := techdocsProfile(t)
	f := prFacts("x/y")
	f.PR.Review.State = stagelabels.VerdictChangesRequested
	f.PR.BaseRef = "main"
	f.PR.ChangesRequested, f.PR.Conflicting, f.PR.HeadFresh = true, true, true
	f.Draft, f.Merged = true, true
	f.Park.Holds = []string{"hold"}
	f.Park.NeedsHuman = true
	f.Origin = "origin/scanner"
	f.Author.Login = "someone"
	fi := issueFacts("x/y")
	fi.Issue.Bucket = stageengine.BucketClaimed
	fi.Issue.Claim = &stageengine.ClaimFact{}
	fi.Issue.CoveringPR = &stageengine.Ref{}
	fi.Issue.Stalled = true
	fi.Issue.OpenChildren = 1

	for name, ft := range workflowmodel.Facts {
		var pos, neg workflowmodel.Op
		value := ""
		switch ft {
		case workflowmodel.FactBool:
			pos, neg = workflowmodel.OpTrue, workflowmodel.OpFalse
		case workflowmodel.FactScalar:
			pos, neg = workflowmodel.OpIs, workflowmodel.OpNot
			value = scalarValue(name)
		case workflowmodel.FactSet:
			pos, neg = workflowmodel.OpHas, workflowmodel.OpLacks
			value = "x/*"
		}
		facts, state := f, "drafting"
		if strings.HasPrefix(name, "issue.") {
			facts, state = fi, "requested"
		}
		rp := *p
		rp.Rules = []workflowmodel.Rule{{ID: "t", State: state, When: []workflowmodel.Predicate{{Fact: name, Op: pos, Value: value}}}}
		rn := *p
		rn.Rules = []workflowmodel.Rule{{ID: "t", State: state, When: []workflowmodel.Predicate{{Fact: name, Op: neg, Value: value}}}}
		gp := stageengine.DeriveFromModel(&rp, facts.Ref.Kind).Derive(facts)
		gn := stageengine.DeriveFromModel(&rn, facts.Ref.Kind).Derive(facts)
		if gp.Abstain == gn.Abstain {
			t.Errorf("fact %q: %s → %v, %s → %v (one must match)", name, pos, gp.Abstain, neg, gn.Abstain)
		}
		if gp.Abstain {
			t.Errorf("fact %q: positive predicate %s %q did not hold on %+v", name, pos, value, facts)
		}
	}
}

func scalarValue(fact string) string {
	switch fact {
	case "origin":
		return "origin/scanner"
	case "author.kind":
		return "hive"
	case "author.login":
		return "someone"
	case "issue.bucket":
		return string(stageengine.BucketClaimed)
	case "pr.checks":
		return "green"
	case "pr.review":
		return "changes_requested"
	case "pr.dco":
		return "yes"
	case "pipeline.status":
		// Post-merge runs are not observed yet; the engine reports unknown.
		return "unknown"
	case "pr.base":
		return "main"
	}
	return "?"
}
