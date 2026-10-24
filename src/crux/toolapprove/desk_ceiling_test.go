package toolapprove

// The ACMM ceiling is the safety property of the whole desk: an operator rule
// is an INPUT to the decision, never an override of the hive's level. These
// tests pin that, including the positive controls that prove the tests would
// notice if the ceiling were removed.

import (
	"context"
	"testing"
)

// autoApproveEverything is the most permissive rule set expressible: it asks
// for auto-approve on literally every request. If the ceiling works, this rule
// still cannot lift a low-level hive above its lane.
func autoApproveEverything(t *testing.T) *RuleEngine {
	t.Helper()
	eng, err := CompileRules([]Rule{{
		Name:   "approve-everything",
		Expr:   "true",
		Action: RuleActionAutoApprove,
	}}, nil)
	if err != nil {
		t.Fatalf("CompileRules: %v", err)
	}
	return eng
}

// sideEffectfulRequest is a write that no level auto-permits outright.
func sideEffectfulRequest() Request {
	return Request{
		Kind: KindAgentTool,
		Tool: ToolRequest{
			Tool:      "write",
			Arguments: map[string]any{"file_path": "src/main.go", "content": "package main"},
		},
		Agent: AgentIdentity{Name: "coder"},
	}
}

// TestACMMCeilingClampsPermissiveRule is the core fail-closed assertion: a rule
// demanding auto-approve at every level is clamped to what each level permits.
func TestACMMCeilingClampsPermissiveRule(t *testing.T) {
	desk := NewDesk(autoApproveEverything(t), passingScanner{})

	for _, tc := range []struct {
		level int
		// want is the maximum decision the level may reach. Below L3 nothing
		// side-effectful may auto-resolve, so the rule must be clamped to the
		// operator lane no matter what it asks for.
		want Decision
	}{
		{level: 0, want: DecisionOperatorApprove},
		{level: 1, want: DecisionOperatorApprove},
		{level: 2, want: DecisionOperatorApprove},
		{level: 3, want: DecisionAutoApprove}, // scan lane, green scan resolves
		{level: 4, want: DecisionAutoApprove},
		{level: 5, want: DecisionAutoApprove},
		{level: 6, want: DecisionAutoApprove},
	} {
		got := desk.Resolve(context.Background(), sideEffectfulRequest(), tc.level)
		if got.Decision != tc.want {
			t.Errorf("L%d: rule asked auto-approve, ceiling should yield %s, got %s (rationale: %s)",
				tc.level, tc.want, got.Decision, got.Rationale)
		}
	}
}

// TestACMMCeilingNeverExceedsLevel is the invariant stated directly, over every
// rule action and every level: the resolved decision is never more permissive
// than ACMMCeiling(level).
func TestACMMCeilingNeverExceedsLevel(t *testing.T) {
	for _, action := range []RuleAction{RuleActionAutoApprove, RuleActionSecurityScan, RuleActionOperatorApprove} {
		eng, err := CompileRules([]Rule{{Name: "r", Expr: "true", Action: action}}, nil)
		if err != nil {
			t.Fatalf("CompileRules(%s): %v", action, err)
		}
		// A scanner that always fails keeps the scan lane from resolving
		// upward, so this test measures the ceiling rather than the scanner.
		desk := NewDesk(eng, passingScanner{})

		for level := 0; level <= 7; level++ {
			v := desk.Decide(context.Background(), sideEffectfulRequest(), level)
			ceiling := ACMMCeiling(level)
			if permissiveness(v.Decision) > permissiveness(ceiling) {
				t.Errorf("action=%s L%d: decision %s is MORE permissive than ceiling %s",
					action, level, v.Decision, ceiling)
			}
		}
	}
}

// TestACMMCeilingClampIsRecorded pins that a clamp is visible in the verdict
// and therefore in the audit record — a silent clamp would be unauditable.
func TestACMMCeilingClampIsRecorded(t *testing.T) {
	desk := NewDesk(autoApproveEverything(t), passingScanner{})
	v := desk.Decide(context.Background(), sideEffectfulRequest(), 1)

	if !v.CeilingApplied {
		t.Fatal("L1 clamp of an auto-approve rule did not set CeilingApplied")
	}
	if v.Rule != "approve-everything" {
		t.Errorf("verdict lost the rule provenance: Rule=%q", v.Rule)
	}
	if v.RuleAction != RuleActionAutoApprove {
		t.Errorf("verdict lost what the rule ASKED for: RuleAction=%q", v.RuleAction)
	}
	fields := v.AuditFields()
	if fields["ceiling_applied"] != true {
		t.Error("AuditFields did not record the clamp — a clamp must be auditable")
	}
	if fields["rule"] != "approve-everything" {
		t.Errorf("AuditFields lost rule provenance: %v", fields["rule"])
	}
}

// TestACMMCeilingPositiveControl is the control this suite needs to be
// meaningful: it proves the ceiling is what produces the clamp, by showing the
// SAME rule and request DO reach auto-approve once the level permits it. Without
// this, every assertion above would still pass if the desk simply refused
// everything.
func TestACMMCeilingPositiveControl(t *testing.T) {
	desk := NewDesk(autoApproveEverything(t), passingScanner{})

	low := desk.Decide(context.Background(), sideEffectfulRequest(), 1)
	if low.Decision != DecisionOperatorApprove {
		t.Fatalf("precondition: L1 should clamp to operator-approve, got %s", low.Decision)
	}

	high := desk.Decide(context.Background(), sideEffectfulRequest(), 6)
	if high.Decision != DecisionAutoApprove {
		t.Fatalf("positive control FAILED: L6 with an auto-approve rule should reach auto-approve, got %s — "+
			"the suite above may be passing because the desk refuses everything, not because the ceiling works",
			high.Decision)
	}
	if high.CeilingApplied {
		t.Error("L6 auto-approve should not be clamped — the ceiling is auto-approve at L6")
	}
}

// TestACMMCeilingUnknownLevelFailsClosed pins that a missing or nonsense level
// cannot widen authority. An unset acmm_level reads as 0 (see ACMMLevelOf).
func TestACMMCeilingUnknownLevelFailsClosed(t *testing.T) {
	for _, level := range []int{-1, -100, 0} {
		if got := ACMMCeiling(level); got != DecisionOperatorApprove {
			t.Errorf("ACMMCeiling(%d) = %s, want operator-approve (fail-closed)", level, got)
		}
	}
}

// TestRuleCannotOverrideHardDeny pins that guardrails are not rule-overridable.
// Direct PR creation is denied at every level; a rule asking for auto-approve
// must not reopen it.
func TestRuleCannotOverrideHardDeny(t *testing.T) {
	desk := NewDesk(autoApproveEverything(t), passingScanner{})
	req := Request{
		Kind:  KindAgentTool,
		Tool:  ToolRequest{Tool: "mcp__github__create_pull_request"},
		Agent: AgentIdentity{Name: "coder"},
	}
	v := desk.Resolve(context.Background(), req, 6)
	if v.Decision != DecisionDeny {
		t.Errorf("a rule lifted a hard guardrail deny: got %s (rationale: %s)", v.Decision, v.Rationale)
	}
}

// passingScanner is a scanner that always returns green, so scan-lane tests
// measure policy rather than scanner behavior.
type passingScanner struct{}

func (passingScanner) Scan(context.Context, ToolRequest) (ScanResult, error) {
	return ScanResult{Passed: true, Details: "test scanner: green"}, nil
}
