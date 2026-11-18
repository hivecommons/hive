package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/reviewrules"
)

// hivecommons/hive#19138: the sweep reads review_feedback.rules from hive.yaml
// on every pass, so an owner's edit applies without a restart.
func TestReviewRulePolicyFollowsConfigLive(t *testing.T) {
	t.Setenv(reviewrules.DisableEnvVar, "")
	t.Setenv(reviewrules.TrustedAssociationsEnvVar, "")
	cfg := &config.Config{}
	if p := reviewRulePolicy(cfg); p.Off || !p.Trusted["COLLABORATOR"] || len(p.ExtraPhrases) != 0 {
		t.Fatalf("defaults = %+v", p)
	}
	off := false
	if err := cfg.SetReviewFeedbackRules(config.ReviewFeedbackRulesConfig{
		Enabled: &off, TrustedAssociations: []string{"OWNER"}, ExtraPhrases: []string{"house rule"}, DisabledSignals: []string{"never"},
	}); err != nil {
		t.Fatal(err)
	}
	p := reviewRulePolicy(cfg)
	if !p.Off || len(p.Trusted) != 1 || !p.Trusted["OWNER"] || p.ExtraPhrases[0] != "house rule" || !p.DisabledSignals["never"] {
		t.Fatalf("after edit = %+v", p)
	}
	// The environment variables keep working and win.
	t.Setenv(reviewrules.DisableEnvVar, "1")
	if p := reviewRulePolicy(cfg); p.Off {
		t.Fatal("HIVE_REVIEW_FEEDBACK_RULES=1 must keep capture on")
	}
}

// A store that is already open takes the new policy on the next pass.
func TestReviewRuleStoreTakesPolicyWithoutReopen(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	t.Setenv(reviewrules.DisableEnvVar, "")
	t.Setenv(reviewrules.TrustedAssociationsEnvVar, "")
	cfg := &config.Config{}
	if err := cfg.SetReviewFeedbackRules(config.ReviewFeedbackRulesConfig{ExtraPhrases: []string{"house rule"}}); err != nil {
		t.Fatal(err)
	}
	h.rules.SetPolicy(reviewRulePolicy(cfg))
	h.feedback(ruleItem("inline:7", "OWNER", "House rule: every analysis cites the scorecard."))
	res := h.sweep(t).run(t.Context(), parseReviewFeedbackKey)
	if res.RulesCaptured != 1 {
		t.Fatalf("captured = %d, want 1 (%+v)", res.RulesCaptured, res)
	}
}
