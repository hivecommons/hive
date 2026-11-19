package stageengine_test

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// Explain agrees with Derive and records the ordered rule walk: kind
// mismatches are skipped, the first match stops evaluation, predicates
// carry their verdicts, and the uncovered walk has no match at all.
func TestExplainRulesTrace(t *testing.T) {
	p := techdocsProfile(t)
	f := issueFacts("techdocs/request")
	want := stageengine.DeriveFromModel(p, stagelabels.IssueItem).Derive(f)
	got, trace := stageengine.Explain(p, stagelabels.IssueItem, f)
	if got.Stage != want.Stage || got.Abstain != want.Abstain || got.Reason != want.Reason || got.Abstain {
		t.Fatalf("explain %+v derive %+v", got, want)
	}
	if len(trace) != len(p.Rules) {
		t.Fatalf("trace has %d entries for %d rules", len(trace), len(p.Rules))
	}
	matched := -1
	for i, tr := range trace {
		if tr.Builtin {
			t.Fatalf("builtin entry in rules trace: %+v", tr)
		}
		if tr.Skipped != "" && tr.Evaluated {
			t.Fatalf("skipped rule was evaluated: %+v", tr)
		}
		if tr.Matched {
			if matched >= 0 {
				t.Fatalf("second match at %d (first %d)", i, matched)
			}
			matched = i
			if st, _ := p.StateForNumber(got.Stage.Number()); tr.State != p.Rules[i].State || st != tr.State {
				t.Fatalf("matched rule %+v for decision %+v", tr, got)
			}
			for _, pr := range tr.Predicates {
				if !pr.Holds || !pr.Known {
					t.Fatalf("matched predicate does not hold: %+v", pr)
				}
			}
		} else if matched >= 0 && tr.Evaluated {
			t.Fatalf("rule %d evaluated after the match at %d", i, matched)
		}
	}
	if matched < 0 {
		t.Fatal("no matched rule")
	}

	dec, trace := stageengine.Explain(p, stagelabels.IssueItem, issueFacts("bug"))
	if !dec.Abstain || !strings.HasPrefix(dec.Reason, stageengine.ReasonNotCovered) {
		t.Fatalf("uncovered %+v", dec)
	}
	for _, tr := range trace {
		if tr.Matched {
			t.Fatalf("uncovered walk matched %+v", tr)
		}
	}
}

func TestExplainBuiltinTrace(t *testing.T) {
	p, _, err := workflowmodel.Compile(hivedefault.Generate())
	if err != nil {
		t.Fatal(err)
	}
	f := issueFacts("stage/010-actionable")
	f.Issue.Bucket = stageengine.BucketActionable
	dec, trace := stageengine.Explain(p, stagelabels.IssueItem, f)
	st, _ := p.StateForNumber(dec.Stage.Number())
	if dec.Abstain || len(trace) != 1 || !trace[0].Builtin || !trace[0].Matched || trace[0].State != st || st == "" {
		t.Fatalf("decision %+v trace %+v", dec, trace)
	}
}
