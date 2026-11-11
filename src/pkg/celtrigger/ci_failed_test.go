package celtrigger

import (
	"slices"
	"testing"
)

// ciFailedTestEvent is the event the red-main sweep's ci.failed source
// produces (cmd/hive ciFailedEvent): number 0, check name as title, default
// branch as head_branch, conclusion as state.
func ciFailedTestEvent() NormalizedEvent {
	return NormalizedEvent{Kind: KindCIFailed, Repo: "org/widget", Title: "build", HeadBranch: "main", State: "failure"}
}

func TestKindCIFailedValue(t *testing.T) {
	if KindCIFailed != "ci.failed" {
		t.Fatalf("KindCIFailed = %q, want the documented \"ci.failed\"", KindCIFailed)
	}
}

// `event.kind == "ci.failed"` matches the ci.failed event, and the reused
// fields are reachable from CEL.
func TestMatchAgents_CIFailedEvent(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "main-red", Expr: `event.kind == "ci.failed"`, Agent: "ci-maintainer"},
		{Name: "one-check", Expr: `event.kind == "ci.failed" && event.title == "build" && event.head_branch == "main" && event.state == "failure" && event.number == 0`, Agent: "builder"},
		{Name: "timeouts", Expr: `event.kind == "ci.failed" && event.state == "timed_out"`, Agent: "timeouts"},
		{Name: "issues", Expr: `event.kind == "issue.opened"`, Agent: "scanner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := eng.MatchAgents(ciFailedTestEvent())
	if !slices.Equal(got, []string{"ci-maintainer", "builder"}) {
		t.Fatalf("MatchAgents(ci.failed) = %v, want [ci-maintainer builder]", got)
	}
}

// ForKind keeps only the rules that name the kind, so a rule that never
// tests event.kind (here, a repo filter written for issues) is not
// evaluated for ci.failed, and a rule set that never names it yields nil.
func TestForKind(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "repo-wide", Expr: `event.repo == "org/widget"`, Agent: "scanner"},
		{Name: "main-red", Expr: `event.kind == "ci.failed"`, Agent: "ci-maintainer"},
		{Name: "listed", Expr: `event.kind in ["pr.opened", "ci.failed"]`, Agent: "reviewer", Priority: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The unfiltered engine would route the repo-wide rule too.
	if got := eng.MatchAgents(ciFailedTestEvent()); !slices.Contains(got, "scanner") {
		t.Fatalf("precondition: repo-wide rule should match the raw engine, got %v", got)
	}
	sub := eng.ForKind(KindCIFailed)
	if sub.Len() != 2 {
		t.Fatalf("ForKind(ci.failed).Len() = %d, want 2", sub.Len())
	}
	if got := sub.MatchAgents(ciFailedTestEvent()); !slices.Equal(got, []string{"reviewer", "ci-maintainer"}) {
		t.Fatalf("ForKind(ci.failed) matched %v, want [reviewer ci-maintainer] (priority order, no scanner)", got)
	}
	if eng.Len() != 3 {
		t.Fatalf("ForKind must not change the source engine, Len = %d", eng.Len())
	}

	none, err := Compile([]Rule{{Name: "issues", Expr: `event.kind == "issue.opened"`, Agent: "scanner"}})
	if err != nil {
		t.Fatal(err)
	}
	if sub := none.ForKind(KindCIFailed); sub != nil || sub.Len() != 0 {
		t.Fatalf("ForKind with no rule naming the kind = %v, want nil", sub)
	}
	var nilEngine *Engine
	if sub := nilEngine.ForKind(KindCIFailed); sub != nil || sub.MatchAgents(ciFailedTestEvent()) != nil {
		t.Fatal("nil engine ForKind must be nil and match nothing")
	}
}
