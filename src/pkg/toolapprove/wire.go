package toolapprove

// Config → desk wiring. Mirrors pkg/celtrigger/wire.go: one entry point called
// after config load, fail-closed on malformed rules, and a no-op when the
// feature block is absent.

import (
	"log/slog"

	"github.com/hivecommons/hive/pkg/crux/config"
)

// DeskFromConfig builds a Desk and its operator Inbox from the loaded
// tool_approval block (the host passes cfg.ToolApproval).
//
// Fail-closed: a malformed rule returns an error and NO desk, so the operator
// rejects the config at load rather than discovering at decision time that half
// the rule set compiled. Callers treat a non-nil error as fatal to startup, the
// same posture celtrigger.CompileFromConfig established.
//
// Returns (nil, nil, nil) when the feature is disabled — the default. A nil
// desk is the signal to every producer to keep using its legacy gate, so an
// upgrade with no config change is byte-identical in behavior.
//
// The desk the host gets back answers plan-approval kinds through its
// PlanPolicy, which the host sets (SetPlanPolicy) from the ACMM pack and
// planning config it alone can read; see PlanPolicy.
func DeskFromConfig(tc config.ToolApprovalConfig, scanner SecurityScanner, logger *slog.Logger) (*Desk, *Inbox, error) {
	if !tc.Enabled {
		return nil, nil, nil
	}

	rules := make([]Rule, 0, len(tc.Rules))
	for _, r := range tc.Rules {
		rules = append(rules, Rule{
			Name:         r.Name,
			Expr:         r.Expr,
			Action:       RuleAction(r.Action),
			Priority:     r.Priority,
			MinACMMLevel: r.MinACMMLevel,
		})
	}

	engine, err := CompileRules(rules, logger)
	if err != nil {
		return nil, nil, err
	}

	return NewDesk(engine, scanner), NewInbox(tc.InboxPath), nil
}
