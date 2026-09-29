package retro

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestAutonomyPolicyEitherDemoteMode covers matchesDemoteOn's "either" branch,
// which no existing test exercised (only the default "rollback" mode and the
// explicit "rework" mode had coverage). "either" must demote on a
// PRReworkedAfterReview finding exactly like "rework" mode does.
func TestAutonomyPolicyEitherDemoteMode(t *testing.T) {
	store := newStore(t, "policy-either")
	cfg := autonomyTestConfig(t, 5)
	cfg.Autonomy.DemoteOn = "either"
	addAutonomyFact(t, store, PatternPRReworkedAfterReview, "hivecommons/hive")

	decisions := NewAutonomyPolicyEngine(cfg, store, &captureAutonomySink{}).Evaluate()
	if len(decisions) != 1 || decisions[0].Direction != "demote" || decisions[0].To != 4 {
		t.Fatalf("decisions = %#v, want either-mode rework demotion", decisions)
	}
}

// TestAutonomyPolicyEitherDemoteModeAlsoMatchesRollback covers the other half
// of the "either" branch: a rollback finding must also trigger a demotion
// under "either" mode, not just a rework finding.
func TestAutonomyPolicyEitherDemoteModeAlsoMatchesRollback(t *testing.T) {
	store := newStore(t, "policy-either-rollback")
	cfg := autonomyTestConfig(t, 5)
	cfg.Autonomy.DemoteOn = "either"
	addAutonomyFact(t, store, PatternRunRolledBack, "hivecommons/hive")

	decisions := NewAutonomyPolicyEngine(cfg, store, &captureAutonomySink{}).Evaluate()
	if len(decisions) != 1 || decisions[0].Direction != "demote" || decisions[0].To != 4 {
		t.Fatalf("decisions = %#v, want either-mode rollback demotion", decisions)
	}
}

// TestAutonomyPolicyBoundedLevelClampsBelowMin covers boundedLevel's lower
// clamp (level < config.MinACMMLevel). A misconfigured/negative hive ACMM
// level is passed straight through by EffectiveACMMLevelForRepo (it only
// short-circuits when hive<=0, returning the raw value), so
// promoteDecision's boundedLevel(...) call is the only place that clamps it
// back into range. No existing test used an out-of-range level.
func TestAutonomyPolicyBoundedLevelClampsBelowMin(t *testing.T) {
	store := newStore(t, "policy-bounded-min")
	cfg := autonomyTestConfig(t, -1)
	for i := 0; i < config.DefaultAutonomyPromoteAfter; i++ {
		addAutonomyFact(t, store, PatternPlanAcceptedFirstPass, "hivecommons/hive")
	}

	decisions := NewAutonomyPolicyEngine(cfg, store, &captureAutonomySink{}).Evaluate()
	if len(decisions) != 1 {
		t.Fatalf("decisions = %#v, want one promotion", decisions)
	}
	if decisions[0].From != config.MinACMMLevel {
		t.Fatalf("From = %d, want clamped to MinACMMLevel %d", decisions[0].From, config.MinACMMLevel)
	}
	if decisions[0].To != config.MinACMMLevel+1 {
		t.Fatalf("To = %d, want %d", decisions[0].To, config.MinACMMLevel+1)
	}
}

// TestAutonomyPolicyBoundedLevelClampsAboveMax covers boundedLevel's upper
// clamp (level > config.MaxACMMLevel) via a repo-level override that exceeds
// the max, exercised through demoteDecision this time so both call sites of
// boundedLevel get direct coverage.
func TestAutonomyPolicyBoundedLevelClampsAboveMax(t *testing.T) {
	store := newStore(t, "policy-bounded-max")
	cfg := autonomyTestConfig(t, config.MaxACMMLevel+3)
	addAutonomyFact(t, store, PatternRunRolledBack, "hivecommons/hive")

	decisions := NewAutonomyPolicyEngine(cfg, store, &captureAutonomySink{}).Evaluate()
	if len(decisions) != 1 || decisions[0].Direction != "demote" {
		t.Fatalf("decisions = %#v, want one demotion", decisions)
	}
	if decisions[0].From != config.MaxACMMLevel {
		t.Fatalf("From = %d, want clamped to MaxACMMLevel %d", decisions[0].From, config.MaxACMMLevel)
	}
	if decisions[0].To != config.MaxACMMLevel-1 {
		t.Fatalf("To = %d, want %d", decisions[0].To, config.MaxACMMLevel-1)
	}
}
