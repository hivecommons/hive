package knowledge

import "testing"

func TestTOCScopeExclusionReason(t *testing.T) {
	scope := TOCScope{
		Layers: []LayerType{LayerProject},
		Repos:  []string{"hive"},
		Types:  []string{"gotcha"},
		Tags:   []string{"ci"},
	}
	cases := []struct {
		name string
		f    Fact
		want string
	}{
		{"in scope", tocFact("a", LayerProject, StateApproved, "repo:hive", "ci"), ""},
		{"org-wide passes repo", tocFact("b", LayerProject, StateApproved, "ci"), ""},
		{"lifecycle", tocFact("c", LayerProject, StateDraft, "repo:hive", "ci"), ExcludedLifecycle},
		{"layer", tocFact("d", LayerOrg, StateApproved, "repo:hive", "ci"), ExcludedLayer},
		{"repo", tocFact("e", LayerProject, StateApproved, "repo:other", "ci"), ExcludedRepo},
		{"type", Fact{Slug: "f", Type: FactPattern, Layer: LayerProject, State: StateApproved, Tags: []string{"repo:hive", "ci"}}, ExcludedType},
		{"tag", tocFact("g", LayerProject, StateApproved, "repo:hive", "go"), ExcludedTag},
		// Every check fails: the first in lifecycle, layer, repo, type, tag order wins.
		{"lifecycle first", Fact{Slug: "h", Type: FactPattern, Layer: LayerOrg, State: StateDeprecated, Tags: []string{"repo:other"}}, ExcludedLifecycle},
		{"layer before repo", Fact{Slug: "i", Type: FactPattern, Layer: LayerOrg, State: StateApproved, Tags: []string{"repo:other"}}, ExcludedLayer},
		{"repo before type", Fact{Slug: "j", Type: FactPattern, Layer: LayerProject, State: StateApproved, Tags: []string{"repo:other"}}, ExcludedRepo},
		{"type before tag", Fact{Slug: "k", Type: FactPattern, Layer: LayerProject, State: StateApproved, Tags: []string{"repo:hive"}}, ExcludedType},
	}
	for _, tc := range cases {
		if got := scope.ExclusionReason(tc.f); got != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, got, tc.want)
		}
		if got := scope.InScope(tc.f); got != (tc.want == "") {
			t.Errorf("%s: InScope = %v, want %v", tc.name, got, tc.want == "")
		}
	}
}

func TestExplainTOCUnknownAgentIsUnrestricted(t *testing.T) {
	facts := []Fact{
		tocFact("a", LayerProject, StateApproved, "repo:hive"),
		tocFact("b", LayerOrg, StateApproved),
		tocFact("c", LayerProject, StateDraft),
	}
	// An agent without a knowledge.agent_scopes entry gets the zero scope's
	// fields unrestricted; only the default approved-only lifecycle applies.
	ex := ExplainTOC(facts, 0, AgentScope(nil, nil, nil, nil, nil), TOCScope{})
	if ex.IncludedTotal != 2 || ex.ExcludedTotal != 1 || ex.Truncated {
		t.Fatalf("counts = %+v", ex)
	}
	if ex.Excluded[0].ID != "c" || ex.Excluded[0].Reason != ExcludedLifecycle {
		t.Fatalf("excluded = %+v", ex.Excluded)
	}
}

func TestExplainTOCCombinesScopesAndCaps(t *testing.T) {
	agent := AgentScope([]string{"project"}, []string{"hive"}, nil, nil, []string{"draft"})
	facts := []Fact{
		tocFact("in", LayerProject, StateApproved, "repo:hive"),
		tocFact("draft", LayerProject, StateDraft, "repo:hive"),
		tocFact("org", LayerOrg, StateApproved),
		tocFact("other", LayerProject, StateApproved, "repo:other"),
		// Lifecycle (request scope) outranks layer (agent scope).
		tocFact("org-deprecated", LayerOrg, StateDeprecated),
		// A lower-precedence copy of an included slug is shadowed, not excluded.
		tocFact("in", LayerOrg, StateApproved),
	}
	ex := ExplainTOC(facts, 0, agent, TOCScope{})
	want := map[string]string{"draft": ExcludedLifecycle, "org": ExcludedLayer, "other": ExcludedRepo, "org-deprecated": ExcludedLifecycle}
	if ex.IncludedTotal != 1 || ex.Included[0].ID != "in" || ex.Included[0].Layer != LayerProject {
		t.Fatalf("included = %+v", ex.Included)
	}
	if ex.ExcludedTotal != len(want) {
		t.Fatalf("excluded = %+v", ex.Excluded)
	}
	for _, e := range ex.Excluded {
		if want[e.ID] != e.Reason {
			t.Errorf("%s reason = %q, want %q", e.ID, e.Reason, want[e.ID])
		}
	}

	capped := ExplainTOC(facts, 1, agent, TOCScope{})
	if !capped.Truncated || capped.ExcludedReturned != 1 || capped.ExcludedTotal != len(want) {
		t.Fatalf("capped = %+v", capped)
	}
}

func TestStatesAdmitted(t *testing.T) {
	agent := AgentScope(nil, nil, nil, nil, []string{"draft", "deprecated"})
	request := TOCScope{IncludeStates: []LifecycleState{StateDraft}}
	got := StatesAdmitted(agent, request)
	if len(got) != 2 || got[0] != StateDraft || got[1] != StateApproved {
		t.Fatalf("states = %v, want [draft approved]", got)
	}
}
