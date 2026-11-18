package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// hivecommons/hive#19138: review_feedback.rules.* in hive.yaml.

func TestNormalizeReviewFeedbackRules(t *testing.T) {
	off := false
	got, err := NormalizeReviewFeedbackRules(ReviewFeedbackRulesConfig{
		Enabled:             &off,
		TrustedAssociations: []string{" member ", "OWNER", "owner", ""},
		ExtraPhrases:        []string{"  house   rule ", "House Rule", "as a rule"},
		DisabledSignals:     []string{"Knowledge-Base", "never", "never"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled == nil || *got.Enabled {
		t.Fatalf("enabled = %v", got.Enabled)
	}
	if want := "MEMBER,OWNER"; strings.Join(got.TrustedAssociations, ",") != want {
		t.Errorf("trusted = %v, want %s", got.TrustedAssociations, want)
	}
	if want := "house rule,as a rule"; strings.Join(got.ExtraPhrases, ",") != want {
		t.Errorf("phrases = %v, want %s", got.ExtraPhrases, want)
	}
	if want := "knowledge-base,never"; strings.Join(got.DisabledSignals, ",") != want {
		t.Errorf("signals = %v, want %s", got.DisabledSignals, want)
	}
}

func TestNormalizeReviewFeedbackRulesRejects(t *testing.T) {
	cases := map[string]ReviewFeedbackRulesConfig{
		"unknown association": {TrustedAssociations: []string{"STRANGER"}},
		"unknown signal":      {DisabledSignals: []string{"vibes"}},
		"phrase too short":    {ExtraPhrases: []string{"abc"}},
		"phrase too long":     {ExtraPhrases: []string{strings.Repeat("x", MaxReviewFeedbackPhraseRunes+1)}},
	}
	many := ReviewFeedbackRulesConfig{}
	for i := 0; i <= MaxReviewFeedbackExtraPhrases; i++ {
		many.ExtraPhrases = append(many.ExtraPhrases, "phrase number "+strings.Repeat("a", i+1))
	}
	cases["too many phrases"] = many
	for name, in := range cases {
		if _, err := NormalizeReviewFeedbackRules(in); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestSetReviewFeedbackRulesKeepsOldValueOnError(t *testing.T) {
	c := &Config{}
	if err := c.SetReviewFeedbackRules(ReviewFeedbackRulesConfig{ExtraPhrases: []string{"house rule"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReviewFeedbackRules(ReviewFeedbackRulesConfig{TrustedAssociations: []string{"nope"}}); err == nil {
		t.Fatal("want an error")
	}
	if got := c.ReviewFeedbackRules().ExtraPhrases; len(got) != 1 || got[0] != "house rule" {
		t.Fatalf("a refused set changed the config: %v", got)
	}
	// The getter returns a copy.
	r := c.ReviewFeedbackRules()
	r.ExtraPhrases[0] = "mutated"
	if got := c.ReviewFeedbackRules().ExtraPhrases[0]; got != "house rule" {
		t.Fatalf("getter leaked its slice: %q", got)
	}
}

func TestReviewFeedbackYAMLRoundTrip(t *testing.T) {
	src := `review_feedback:
  rules:
    enabled: false
    trusted_associations: [OWNER]
    extra_phrases: ["house rule"]
    disabled_signals: [knowledge-base]
`
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	r := c.ReviewFeedbackRules()
	if r.Enabled == nil || *r.Enabled || r.TrustedAssociations[0] != "OWNER" || r.ExtraPhrases[0] != "house rule" || r.DisabledSignals[0] != "knowledge-base" {
		t.Fatalf("parsed = %+v", r)
	}
	out, err := yaml.Marshal(c.ReviewFeedback)
	if err != nil || !strings.Contains(string(out), "house rule") {
		t.Fatalf("marshal: %v %s", err, out)
	}
	// An absent block stays absent.
	empty, _ := yaml.Marshal(struct {
		R ReviewFeedbackConfig `yaml:"review_feedback,omitempty"`
	}{})
	if strings.Contains(string(empty), "enabled") {
		t.Fatalf("zero block wrote keys: %s", empty)
	}
}
