package knowledge

import (
	"context"
	"testing"
)

func TestAgentScope_EmptyIsUnrestricted(t *testing.T) {
	sc := AgentScope(nil, nil, nil, nil, nil)
	for _, f := range []Fact{
		tocFact("a", LayerPersonal, StateApproved, "repo:hive"),
		tocFact("b", LayerCommunity, StateDraft),
		tocFact("c", LayerOrg, StateDeprecated, "x"),
		tocFact("d", LayerProject, StateSuperseded),
	} {
		if !sc.InScope(f) {
			t.Errorf("empty agent scope hid %s (%s)", f.Slug, f.State)
		}
	}
	if all := AgentScope(nil, nil, nil, nil, []string{"ALL"}); len(all.IncludeStates) != len(AllLifecycleStates) {
		t.Errorf("include_states all = %v", all.IncludeStates)
	}
}

func TestAgentScope_NormalizesAndFailsClosed(t *testing.T) {
	sc := AgentScope([]string{" Org ", ""}, nil, nil, nil, []string{"Deprecated"})
	if len(sc.Layers) != 1 || sc.Layers[0] != LayerOrg {
		t.Fatalf("layers = %v, want [org]", sc.Layers)
	}
	if len(sc.IncludeStates) != 1 || sc.IncludeStates[0] != StateDeprecated {
		t.Fatalf("include_states = %v, want [deprecated]", sc.IncludeStates)
	}
	bogus := AgentScope(nil, nil, nil, nil, []string{"verified"})
	if bogus.InScope(tocFact("d", LayerOrg, StateDeprecated)) {
		t.Error("a scope whose only state is unknown must admit approved knowledge only")
	}
	if !bogus.InScope(tocFact("a", LayerOrg, StateApproved)) {
		t.Error("approved knowledge must always pass the lifecycle filter")
	}
}

func agentScopeFacts() []Fact {
	return []Fact{
		tocFact("org-hive", LayerOrg, StateApproved, "repo:hive"),
		tocFact("org-other", LayerOrg, StateApproved, "repo:other"),
		tocFact("org-wide", LayerOrg, StateApproved),
		tocFact("project-hive", LayerProject, StateApproved, "repo:hive"),
		tocFact("org-retired", LayerOrg, StateDeprecated, "repo:hive"),
		tocFact("org-draft", LayerOrg, StateDraft, "repo:hive"),
	}
}

func tocIDs(toc TOC) map[string]bool {
	out := make(map[string]bool, len(toc.Entries))
	for _, e := range toc.Entries {
		out[e.ID] = true
	}
	return out
}

func TestAgentScope_RequestCannotWiden(t *testing.T) {
	agent := AgentScope([]string{"org"}, []string{"hive"}, nil, nil, []string{"deprecated"})
	wide := TOCScope{
		Layers:        []LayerType{LayerProject, LayerOrg, LayerPersonal},
		Repos:         []string{"hive", "other"},
		IncludeStates: AllLifecycleStates,
	}
	got := tocIDs(BuildTOC(agent.Filter(agentScopeFacts()), wide, 0))
	for _, id := range []string{"org-hive", "org-wide", "org-retired"} {
		if !got[id] {
			t.Errorf("in-scope entry %s missing: %v", id, got)
		}
	}
	for _, id := range []string{"org-other", "project-hive", "org-draft"} {
		if got[id] {
			t.Errorf("request widened the agent scope to %s", id)
		}
	}

	// The request still narrows: no include_states means approved only, even
	// though the agent scope lists deprecated.
	got = tocIDs(BuildTOC(agent.Filter(agentScopeFacts()), TOCScope{}, 0))
	if got["org-retired"] {
		t.Error("agent include_states admitted a state the request did not list")
	}
	if !got["org-hive"] || len(got) != 2 {
		t.Errorf("narrow request = %v, want org-hive and org-wide", got)
	}
}

func TestPrimeScoped_AgentNeverReceivesOutOfScopeFacts(t *testing.T) {
	projectDir, orgDir := t.TempDir(), t.TempDir()
	writeLifecycleFact(t, projectDir, "deploy-project", "title: Project deploy flow\ntype: pattern\n", "Deploy the project.")
	writeLifecycleFact(t, orgDir, "deploy-org", "title: Org deploy flow\ntype: pattern\n", "Deploy across the org.")
	writeLifecycleFact(t, orgDir, "deploy-gotcha", "title: Org deploy gotcha\ntype: gotcha\n", "Deploy gotcha.")
	writeLifecycleFact(t, orgDir, "deploy-retired", "title: Retired deploy flow\ntype: pattern\nstate: deprecated\n", "Old deploy.")
	writeLifecycleFact(t, projectDir, "linked-secret", "title: Linked note\ntype: pattern\n", "Unrelated linked text.")
	projectStore, err := NewFileStore(projectDir, "project-vault", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	orgStore, err := NewFileStore(orgDir, "org-vault", fileStoreTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	gs := newTestGraphStore(t)
	if err := gs.AddTriple(Triple{Subject: "deploy-org", Predicate: PredicateRelatedTo, Object: "linked-secret"}); err != nil {
		t.Fatal(err)
	}
	newPrimer := func(include []LifecycleState) *Primer {
		p := NewPrimer(nil, PrimerConfig{MaxFacts: 50, IncludeStates: include}, fileStoreTestLogger())
		p.AddFileStore("project-vault", projectStore, LayerProject)
		p.AddFileStore("org-vault", orgStore, LayerOrg)
		p.SetGraphStore(gs)
		return p
	}
	kw := []string{"deploy"}
	p := newPrimer([]LifecycleState{StateDeprecated})

	all := slugStates(p.Prime(context.Background(), nil, kw).Facts)
	for _, slug := range []string{"deploy-project", "deploy-org", "deploy-gotcha", "deploy-retired", "linked-secret"} {
		if _, ok := all[slug]; !ok {
			t.Fatalf("unscoped primer missing %s: %v", slug, all)
		}
	}

	orgPatterns := AgentScope([]string{"org"}, nil, []string{"pattern"}, nil, nil)
	got := slugStates(p.PrimeScoped(context.Background(), &orgPatterns, nil, kw).Facts)
	for _, slug := range []string{"deploy-org", "deploy-retired"} {
		if _, ok := got[slug]; !ok {
			t.Errorf("scoped primer missing in-scope %s: %v", slug, got)
		}
	}
	for _, slug := range []string{"deploy-project", "deploy-gotcha", "linked-secret"} {
		if _, ok := got[slug]; ok {
			t.Errorf("scoped primer leaked out-of-scope %s", slug)
		}
	}

	draftOnly := AgentScope([]string{"org"}, nil, nil, nil, []string{"draft"})
	got = slugStates(p.PrimeScoped(context.Background(), &draftOnly, nil, kw).Facts)
	if _, ok := got["deploy-retired"]; ok {
		t.Error("deprecated fact reached an agent whose scope does not list deprecated")
	}

	operatorDefault := newPrimer(nil)
	deprecatedAgent := AgentScope(nil, nil, nil, nil, []string{"deprecated"})
	got = slugStates(operatorDefault.PrimeScoped(context.Background(), &deprecatedAgent, nil, kw).Facts)
	if _, ok := got["deploy-retired"]; ok {
		t.Error("agent include_states admitted a state the primer does not include")
	}
	if _, ok := got["deploy-org"]; !ok {
		t.Errorf("approved fact missing: %v", got)
	}
}
