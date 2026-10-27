package toolapprove

// Rules-as-data: compile-time rejection, fail-closed evaluation, and the
// canonical operator cases from the RFC.

import (
	"strings"
	"testing"
)

// TestCompileRejectsMalformedRules pins the fail-closed compile posture: an
// operator learns about a bad rule at config load, not at decision time.
func TestCompileRejectsMalformedRules(t *testing.T) {
	for name, rule := range map[string]Rule{
		"syntax error":     {Name: "a", Expr: "request.kind ==", Action: RuleActionAutoApprove},
		"unknown field":    {Name: "b", Expr: `request.no_such_field == "x"`, Action: RuleActionAutoApprove},
		"not a bool":       {Name: "c", Expr: `request.kind`, Action: RuleActionAutoApprove},
		"unknown action":   {Name: "d", Expr: "true", Action: RuleAction("merge-it")},
		"empty name":       {Name: "", Expr: "true", Action: RuleActionAutoApprove},
		"empty expression": {Name: "e", Expr: "  ", Action: RuleActionAutoApprove},
	} {
		if _, err := CompileRules([]Rule{rule}, nil); err == nil {
			t.Errorf("%s: CompileRules accepted a rule it should reject", name)
		}
	}
}

// TestCompileRejectsDuplicateNames pins that rule names stay unique — they are
// the operator-facing identifier in verdicts, audit records, and filter chips.
func TestCompileRejectsDuplicateNames(t *testing.T) {
	_, err := CompileRules([]Rule{
		{Name: "dup", Expr: "true", Action: RuleActionAutoApprove},
		{Name: "dup", Expr: "false", Action: RuleActionOperatorApprove},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate rule names should be rejected, got err=%v", err)
	}
}

// TestEmptyRuleSetNeverMatches pins the additive default: no rules means no
// behavior change.
func TestEmptyRuleSetNeverMatches(t *testing.T) {
	eng, err := CompileRules(nil, nil)
	if err != nil {
		t.Fatalf("CompileRules(nil): %v", err)
	}
	if eng.Len() != 0 {
		t.Errorf("empty rule set compiled %d rules", eng.Len())
	}
	if _, ok := eng.Match(sideEffectfulRequest(), 6); ok {
		t.Error("an empty rule set matched a request")
	}
}

// TestCanonicalDependabotRule is the case the RFC names directly: bulk approve
// of green dependabot patch bumps, while a feature PR is NOT swept up.
func TestCanonicalDependabotRule(t *testing.T) {
	eng, err := CompileRules([]Rule{{
		Name: "green-dependabot-patch",
		Expr: `request.kind == "self-merge" && request.checks_green &&
		        request.author == "dependabot[bot]" && request.title.startsWith("chore(deps)")`,
		Action: RuleActionAutoApprove,
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}

	green := Request{
		Kind: KindSelfMerge, Author: "dependabot[bot]", ChecksGreen: true,
		Title: "chore(deps): bump golang.org/x/net from 0.1.0 to 0.1.1",
	}
	if m, ok := eng.Match(green, 6); !ok || m.Name != "green-dependabot-patch" {
		t.Errorf("the canonical green dependabot patch bump did not match: ok=%v m=%+v", ok, m)
	}

	// The rule MUST distinguish a feature PR from a patch bump — the RFC calls
	// this out explicitly as the thing operators do not want swept up.
	feature := Request{
		Kind: KindSelfMerge, Author: "somebody", ChecksGreen: true,
		Title: "feat: add a whole new subsystem",
	}
	if _, ok := eng.Match(feature, 6); ok {
		t.Error("a feature PR matched the dependabot patch rule — the rule must distinguish them")
	}

	// A red dependabot PR must not match either.
	red := green
	red.ChecksGreen = false
	if _, ok := eng.Match(red, 6); ok {
		t.Error("a RED dependabot PR matched a rule requiring green checks")
	}
}

// TestRulePriorityOrdering pins that the highest-priority matching rule wins.
func TestRulePriorityOrdering(t *testing.T) {
	eng, err := CompileRules([]Rule{
		{Name: "low", Expr: "true", Action: RuleActionAutoApprove, Priority: 1},
		{Name: "high", Expr: "true", Action: RuleActionOperatorApprove, Priority: 100},
	}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	m, ok := eng.Match(sideEffectfulRequest(), 6)
	if !ok {
		t.Fatal("no rule matched")
	}
	if m.Name != "high" {
		t.Errorf("priority ignored: matched %q, want %q", m.Name, "high")
	}
}

// TestRuleMinACMMLevelScoping pins that a level-scoped rule is skipped below
// its floor.
func TestRuleMinACMMLevelScoping(t *testing.T) {
	eng, err := CompileRules([]Rule{{
		Name: "only-high", Expr: "true", Action: RuleActionAutoApprove, MinACMMLevel: 5,
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	if _, ok := eng.Match(sideEffectfulRequest(), 3); ok {
		t.Error("a rule with MinACMMLevel=5 matched at L3")
	}
	if _, ok := eng.Match(sideEffectfulRequest(), 5); !ok {
		t.Error("a rule with MinACMMLevel=5 did not match at L5")
	}
}

// TestHasLabelHelper pins the label convenience function.
func TestHasLabelHelper(t *testing.T) {
	eng, err := CompileRules([]Rule{{
		Name: "held", Expr: `hasLabel(request.labels, "hold")`, Action: RuleActionOperatorApprove,
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	if _, ok := eng.Match(Request{Labels: []string{"bug", "hold"}}, 6); !ok {
		t.Error("hasLabel did not find a present label")
	}
	if _, ok := eng.Match(Request{Labels: []string{"bug"}}, 6); ok {
		t.Error("hasLabel matched an absent label")
	}
	// A nil label slice must not panic or match.
	if _, ok := eng.Match(Request{}, 6); ok {
		t.Error("hasLabel matched on a request with no labels")
	}
}

// TestRuleNamesAreStable pins the chip ordering source.
func TestRuleNamesAreStable(t *testing.T) {
	eng, err := CompileRules([]Rule{
		{Name: "b", Expr: "false", Action: RuleActionAutoApprove, Priority: 5},
		{Name: "a", Expr: "false", Action: RuleActionAutoApprove, Priority: 9},
	}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	names := eng.Names()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("Names() = %v, want evaluation order [a b] (priority 9 before 5)", names)
	}
}
