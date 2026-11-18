package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReviewRulesSettingsRoundTrip(t *testing.T) {
	var c Config
	if got := c.ReviewRulesSettingsSnapshot(); got.Enabled != nil || len(got.TrustedAssociations) != 0 {
		t.Fatalf("zero config: %+v", got)
	}
	var nilCfg *Config
	if got := nilCfg.ReviewRulesSettingsSnapshot(); got.Enabled != nil {
		t.Fatal("nil config")
	}
	off := false
	prev := c.SetReviewRulesSettings(ReviewRulesSettings{Enabled: &off, TrustedAssociations: []string{"OWNER"}, ExtraPhrases: []string{"worth considering"}, DisabledSignals: []string{"never"}})
	if prev.Enabled != nil {
		t.Fatalf("prev = %+v", prev)
	}
	got := c.ReviewRulesSettingsSnapshot()
	if got.Enabled == nil || *got.Enabled || got.TrustedAssociations[0] != "OWNER" || got.ExtraPhrases[0] != "worth considering" || got.DisabledSignals[0] != "never" {
		t.Fatalf("snapshot = %+v", got)
	}
	got.TrustedAssociations[0] = "NONE"
	*got.Enabled = true
	if again := c.ReviewRulesSettingsSnapshot(); again.TrustedAssociations[0] != "OWNER" || *again.Enabled {
		t.Fatal("the snapshot is a copy")
	}

	out, err := yaml.Marshal(&c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"review_feedback:", "rules:", "enabled: false", "trusted_associations:", "extra_phrases:", "disabled_signals:"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("yaml missing %q:\n%s", want, out)
		}
	}
	var back Config
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if b := back.ReviewRulesSettingsSnapshot(); b.Enabled == nil || *b.Enabled || len(b.ExtraPhrases) != 1 {
		t.Fatalf("reparsed = %+v", b)
	}

	if prev := c.SetReviewRulesSettings(ReviewRulesSettings{}); prev.Enabled == nil {
		t.Fatalf("prev = %+v", prev)
	}
	if c.ReviewFeedback != nil {
		t.Fatal("an all-default value removes the block")
	}
	out, _ = yaml.Marshal(&c)
	if strings.Contains(string(out), "review_feedback") {
		t.Fatalf("yaml still carries the block:\n%s", out)
	}
}

func TestIsReviewAssociation(t *testing.T) {
	for _, a := range []string{"owner", " MEMBER ", "NONE", "FIRST_TIMER"} {
		if !IsReviewAssociation(a) {
			t.Errorf("%q should be known", a)
		}
	}
	if IsReviewAssociation("ADMIN") || IsReviewAssociation("") {
		t.Fatal("unknown association accepted")
	}
}
