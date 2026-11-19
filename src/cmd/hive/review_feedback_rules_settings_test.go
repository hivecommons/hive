package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/reviewrules"
)

// A plain PR comment that states a rule counts even though it does not address
// the hive; one that quotes the hive and asks a question does not.
func TestReviewFeedbackSweep_ConversationCommentsAreRuleCandidates(t *testing.T) {
	h := newRuleHarness(t, reviewrules.Config{})
	ask := github.FeedbackItem{ID: "conversation:6990", Kind: github.PRCommentConversation, Author: "nate-double-u", AuthorAssociation: "MEMBER",
		Body: "> The rule I would record: maintainers.yaml is the source of truth.\n\n@operator, is there a way to have the hive add to the knowledge-base?",
		URL:  "https://github.com/cncf/techdocs/pull/424#issuecomment-6990", CreatedAt: time.Now().Add(-time.Hour)}
	state := github.FeedbackItem{ID: "conversation:7001", Kind: github.PRCommentConversation, Author: "nate-double-u", AuthorAssociation: "MEMBER",
		Body: "In future assessments, the project's security contact comes from SECURITY.md, not the README.",
		URL:  "https://github.com/cncf/techdocs/pull/424#issuecomment-7001", CreatedAt: time.Now().Add(-time.Hour)}
	fb := openDraftFeedback()
	fb.Items = nil
	fb.RuleCandidates = []github.FeedbackItem{ask, state}
	h.src.byKey[feedbackTestKey] = fb

	res := h.sweep(t).run(context.Background(), parseReviewFeedbackKey)
	rules := h.rules.List()
	if res.RulesCaptured != 1 || len(rules) != 1 {
		t.Fatalf("res = %+v rules = %+v", res, rules)
	}
	if rules[0].Provenance[0].CommentID != "conversation:7001" || !strings.Contains(rules[0].Text, "SECURITY.md") {
		t.Fatalf("rule = %+v", rules[0])
	}
}

// The owner's saved preferences win over the env knobs; unsaved ones keep the
// env value or default.
func TestReviewRulesPolicy(t *testing.T) {
	t.Setenv(reviewrules.DisableEnvVar, "")
	t.Setenv(reviewrules.TrustedAssociationsEnvVar, "OWNER")
	t.Setenv(reviewrules.ExtraPhrasesEnvVar, "env phrase")
	t.Setenv(reviewrules.DisabledSignalsEnvVar, "")

	cfg := &config.Config{}
	rc, on := reviewRulesPolicy(cfg)
	if !on || !rc.Trusted["OWNER"] || rc.Trusted["MEMBER"] || len(rc.ExtraPhrases) != 1 || rc.ExtraPhrases[0] != "env phrase" {
		t.Fatalf("env only: on=%v cfg=%+v", on, rc)
	}

	off := false
	cfg.SetReviewRulesSettings(config.ReviewRulesSettings{Enabled: &off, TrustedAssociations: []string{"member", "CONTRIBUTOR"},
		ExtraPhrases: []string{"worth considering"}, DisabledSignals: []string{"Never"}})
	rc, on = reviewRulesPolicy(cfg)
	if on || rc.Trusted["OWNER"] || !rc.Trusted["MEMBER"] || !rc.Trusted["CONTRIBUTOR"] || rc.ExtraPhrases[0] != "worth considering" || !rc.DisabledSignals[reviewrules.SignalNever] {
		t.Fatalf("settings: on=%v cfg=%+v", on, rc)
	}
	if activeRuleStore(&reviewrules.Store{}, false) != nil || activeRuleStore(nil, true) != nil {
		t.Fatal("activeRuleStore")
	}

	t.Setenv(reviewrules.DisableEnvVar, "off")
	if reviewRulesEnabled(&config.Config{}) || reviewRulesEnabled(nil) {
		t.Fatal("env off, nothing saved: capture off")
	}
	on2 := true
	cfg.SetReviewRulesSettings(config.ReviewRulesSettings{Enabled: &on2})
	if !reviewRulesEnabled(cfg) {
		t.Fatal("a saved on wins over the env")
	}
}
