package celtrigger

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestCompileFromConfig_Empty(t *testing.T) {
	for _, cfg := range []*config.Config{nil, {}} {
		eng, err := CompileFromConfig(cfg)
		if err != nil {
			t.Fatalf("CompileFromConfig(empty) error: %v", err)
		}
		if eng.Len() != 0 {
			t.Fatalf("empty config engine Len = %d, want 0", eng.Len())
		}
		if got := eng.Match(NormalizedEvent{Kind: KindIssueOpened}); len(got) != 0 {
			t.Fatalf("empty config engine matched %d, want 0", len(got))
		}
	}
}

func TestCompileFromConfig_ValidAndMatch(t *testing.T) {
	cfg := &config.Config{
		Triggers: []config.TriggerRule{
			{Name: "bug", Expr: `hasLabel(event.labels, "bug")`, Agent: "triager", Priority: 10},
			{Name: "pr", Expr: `event.kind == "pr.opened"`, Agent: "reviewer", Priority: 5},
		},
	}
	eng, err := CompileFromConfig(cfg)
	if err != nil {
		t.Fatalf("CompileFromConfig error: %v", err)
	}
	if eng.Len() != 2 {
		t.Fatalf("engine Len = %d, want 2", eng.Len())
	}
	agents := eng.MatchAgents(NormalizedEvent{Kind: KindIssueLabeled, Labels: []string{"bug"}})
	if len(agents) != 1 || agents[0] != "triager" {
		t.Fatalf("MatchAgents = %v, want [triager]", agents)
	}
}

func TestCompileFromConfig_BadRuleFailsClosed(t *testing.T) {
	cfg := &config.Config{
		Triggers: []config.TriggerRule{
			{Name: "ok", Expr: `event.kind == "issue.opened"`, Agent: "a"},
			{Name: "bad", Expr: `event.nope == "x"`, Agent: "b"},
		},
	}
	eng, err := CompileFromConfig(cfg)
	if err == nil {
		t.Fatal("expected error for bad rule, got nil")
	}
	if eng != nil {
		t.Fatal("expected nil engine on error")
	}
}

func TestMatchAgents_DedupAndOrder(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "r1", Expr: `true`, Agent: "same", Priority: 1},
		{Name: "r2", Expr: `true`, Agent: "high", Priority: 100},
		{Name: "r3", Expr: `true`, Agent: "same", Priority: 50},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := eng.MatchAgents(NormalizedEvent{})
	want := []string{"high", "same"}
	if len(got) != len(want) {
		t.Fatalf("MatchAgents = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MatchAgents order = %v, want %v", got, want)
		}
	}
}
