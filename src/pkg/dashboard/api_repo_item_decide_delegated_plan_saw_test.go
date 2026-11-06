package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// The preview carries what the rules read ("What hive saw"), the rule that
// decided with its whole reason, and each ruled-out option with the fact
// that ruled it out, so the owner can check the decision, not just its
// one-line reason.
func TestDelegatedDecisionPlanShowsWhatHiveSaw(t *testing.T) {
	mock := &repoItemDecisionMock{prForeign: true, prLabels: []string{"stage/085-needs-decision", "needs-human"}}
	srv := newDelegatedServer(t, mock)
	srv.deps.Config.Governor.Labels.Stage.Adoption.Enabled = boolPtr(true)
	body := `{"type":"pr","reason":"adoption-stalled:contributor-offline","caption":"adoption stalled"}`
	w := postDelegatedPlan(t, srv, 8, "writer", body, map[string]string{"X-Hive-Role": "merger"})
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var plan delegatedDecisionPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	saw := map[string]delegatedecision.Fact{}
	for _, f := range plan.Saw {
		saw[f.Label] = f
	}
	for _, label := range []string{"Author", "Contributor", "CI", "Conflicts", "Sign-off", "Review", "Sentinel"} {
		if _, ok := saw[label]; !ok {
			t.Errorf("What hive saw has no %q line: %+v", label, plan.Saw)
		}
	}
	if f := saw["Contributor"]; !strings.Contains(f.Value, "since the nudge") {
		t.Errorf("Contributor = %+v, want the offline fact the adopt decision rests on", f)
	}
	if plan.RulePlain == "" || plan.RulePlain == plan.Rule {
		t.Errorf("rule_plain = %q for rule %q, want plain words", plan.RulePlain, plan.Rule)
	}
	if strings.Contains(plan.Rationale, "hivecommons/hive#") {
		t.Errorf("rationale keeps issue references: %q", plan.Rationale)
	}
	if plan.Ladder == nil || len(plan.WhyNot) != len(plan.Ladder.RuledOut) {
		t.Fatalf("why_not = %+v for ladder %+v", plan.WhyNot, plan.Ladder)
	}
	found := false
	for _, wn := range plan.WhyNot {
		if wn.Option == "Approve as it is" {
			found = wn.Because != ""
		}
	}
	if !found {
		t.Errorf("why_not does not say why approving as it is was ruled out: %+v", plan.WhyNot)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, key := range []string{"saw", "rule_plain", "why_not"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("plan JSON has no %q", key)
		}
	}
}

func TestDelegatedPlanRuleDropsARationaleTheWhyAlreadySays(t *testing.T) {
	in := delegatedecision.Input{Kind: delegatedecision.KindPR}
	d := delegatedecision.Decision{Rule: delegatedecision.RuleMergeableAsIs, Rationale: "a mergeable, green head needs no adoption; the merge queue takes it (hivecommons/hive#15289)"}
	rule, rationale := delegatedPlanRule(in, d, "Mergeable and green.")
	if rule == "" || rationale != "a mergeable, green head needs no adoption; the merge queue takes it" {
		t.Fatalf("rule %q rationale %q", rule, rationale)
	}
	if _, r := delegatedPlanRule(in, delegatedecision.Decision{Rationale: "CI is red."}, "CI is red."); r != "" {
		t.Fatalf("a rationale equal to the why is repeated: %q", r)
	}
}
