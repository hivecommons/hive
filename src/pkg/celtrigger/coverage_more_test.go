package celtrigger

import (
	"testing"

	"github.com/google/cel-go/common/types"
)

// TestHasLabelImpl_GuardBranches calls hasLabelImpl directly to cover its
// defensive guards. CEL's type checker rejects mismatched arg types at compile
// time, so these branches are only reachable by calling the impl directly —
// they exist as fail-closed safety, and this locks in that behavior.
func TestHasLabelImpl_GuardBranches(t *testing.T) {
	// Non-string needle → false.
	if got := hasLabelImpl(types.NewStringList(types.DefaultTypeAdapter, []string{"bug"}), types.Int(5)); got != types.Bool(false) {
		t.Fatalf("non-string needle: want false, got %v", got)
	}
	// Non-list haystack → false.
	if got := hasLabelImpl(types.String("bug"), types.String("bug")); got != types.Bool(false) {
		t.Fatalf("non-list haystack: want false, got %v", got)
	}
	// Positive + negative through the iterator.
	list := types.NewStringList(types.DefaultTypeAdapter, []string{"bug", "p1"})
	if got := hasLabelImpl(list, types.String("bug")); got != types.Bool(true) {
		t.Fatalf("present label: want true, got %v", got)
	}
	if got := hasLabelImpl(list, types.String("nope")); got != types.Bool(false) {
		t.Fatalf("absent label: want false, got %v", got)
	}
}

// TestCompile_NonBoolExpressionRejected covers the output-type guard: an
// expression that type-checks but returns a non-bool must be rejected at
// compile time (fail-closed), not silently never-match.
func TestCompile_NonBoolExpressionRejected(t *testing.T) {
	_, err := Compile([]Rule{{Name: "returns-string", Expr: `event.title`, Agent: "a"}})
	if err == nil {
		t.Fatal("expected error for non-bool expression, got nil")
	}
}

// TestMatchAgents_DedupAndEmpty covers MatchAgents: duplicate agents collapse to
// one, rules with an empty Agent are skipped, and no matches → nil.
func TestMatchAgents_DedupAndEmpty(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "r1", Expr: `event.kind == "issue.opened"`, Agent: "dup", Priority: 10},
		{Name: "r2", Expr: `event.kind == "issue.opened"`, Agent: "dup", Priority: 5}, // duplicate agent
		{Name: "r3", Expr: `event.kind == "issue.opened"`, Agent: "", Priority: 3},    // empty agent skipped
		{Name: "r4", Expr: `event.kind == "issue.opened"`, Agent: "other", Priority: 1},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	agents := eng.MatchAgents(NormalizedEvent{Kind: "issue.opened"})
	if len(agents) != 2 || agents[0] != "dup" || agents[1] != "other" {
		t.Fatalf("expected [dup other], got %v", agents)
	}

	// No matches → nil.
	if got := eng.MatchAgents(NormalizedEvent{Kind: "pr.opened"}); got != nil {
		t.Fatalf("expected nil for no matches, got %v", got)
	}
}
