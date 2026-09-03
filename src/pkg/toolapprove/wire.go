package toolapprove

// Config → desk wiring. Mirrors pkg/celtrigger/wire.go: one entry point called
// after config load, fail-closed on malformed rules, and a no-op when the
// feature block is absent.

import (
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
)

// DeskFromConfig builds a Desk and its operator Inbox from loaded config.
//
// Fail-closed: a malformed rule returns an error and NO desk, so the operator
// rejects the config at load rather than discovering at decision time that half
// the rule set compiled. Callers treat a non-nil error as fatal to startup, the
// same posture celtrigger.CompileFromConfig established.
//
// Returns (nil, nil, nil) when the feature is disabled — the default. A nil
// desk is the signal to every producer to keep using its legacy gate, so an
// upgrade with no config change is byte-identical in behavior.
func DeskFromConfig(cfg *config.Config, scanner SecurityScanner, logger *slog.Logger) (*Desk, *Inbox, error) {
	if cfg == nil || !cfg.ToolApproval.Enabled {
		return nil, nil, nil
	}

	rules := make([]Rule, 0, len(cfg.ToolApproval.Rules))
	for _, r := range cfg.ToolApproval.Rules {
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

	return NewDesk(engine, scanner), NewInbox(cfg.ToolApproval.InboxPath), nil
}

// ACMMLevelOf reads the hive's configured ACMM level, defaulting to the most
// restrictive lane when unset. An absent level must never widen authority —
// the same fail-closed reading SelfAuthoredAutoMergeAllowed applies to its nil
// level pointer.
func ACMMLevelOf(cfg *config.Config) int {
	if cfg == nil || cfg.ACMMLevel == nil {
		return 0
	}
	return *cfg.ACMMLevel
}
