package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/reviewrules"
)

func TestReviewFeedbackRulesYAML(t *testing.T) {
	var c Config
	src := `
review_feedback:
  rules:
    enabled: false
    trusted_associations: [OWNER, MEMBER]
    extra_phrases: ["house style"]
    disabled_signals: [authoritative]
`
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	r := c.ReviewFeedback.Rules
	if r.Enabled == nil || *r.Enabled {
		t.Fatalf("enabled = %v, want false", r.Enabled)
	}
	want := reviewrules.Settings{Enabled: r.Enabled, TrustedAssociations: []string{"OWNER", "MEMBER"},
		ExtraPhrases: []string{"house style"}, DisabledSignals: []string{"authoritative"}}
	if got := r.Settings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewFeedbackRulesUnsetIsEmpty(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("hive_id: x\n"), &c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.ReviewFeedback, ReviewFeedbackConfig{}) {
		t.Fatalf("review_feedback = %+v, want empty", c.ReviewFeedback)
	}
	if err := c.ReviewFeedback.Rules.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewFeedbackRulesValidate(t *testing.T) {
	many := make([]string, reviewrules.MaxExtraPhrases+1)
	for i := range many {
		many[i] = "phrase"
	}
	tests := []struct {
		name string
		cfg  ReviewFeedbackRulesConfig
		want string
	}{
		{"lower-case association", ReviewFeedbackRulesConfig{TrustedAssociations: []string{"contributor"}}, ""},
		{"unknown association", ReviewFeedbackRulesConfig{TrustedAssociations: []string{"ADMIN"}}, "trusted_associations"},
		{"blank phrase", ReviewFeedbackRulesConfig{ExtraPhrases: []string{"  "}}, "extra_phrases[0]"},
		{"too many phrases", ReviewFeedbackRulesConfig{ExtraPhrases: many}, "at most"},
		{"known signal", ReviewFeedbackRulesConfig{DisabledSignals: []string{" Going-Forward "}}, ""},
		{"unknown signal", ReviewFeedbackRulesConfig{DisabledSignals: []string{"extra-phrase"}}, "disabled_signals"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}
