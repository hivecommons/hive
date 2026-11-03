package config

import (
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRFCExistingCheckDefaults(t *testing.T) {
	var c RFCExistingCheckConfig
	if c.Enabled || c.PostWhenNothingFound {
		t.Fatal("zero value must be off")
	}
	if got := c.EffectiveLabels(); !reflect.DeepEqual(got, DefaultRFCExistingCheckLabels) {
		t.Fatalf("labels = %v", got)
	}
	if got := c.EffectiveMaxMatches(); got != DefaultRFCExistingCheckMaxMatches {
		t.Fatalf("max matches = %d", got)
	}
	if got := c.EffectiveMaxAge(); got != DefaultRFCExistingCheckMaxAge {
		t.Fatalf("max age = %v", got)
	}
}

func TestRFCExistingCheckOverrides(t *testing.T) {
	var cfg Config
	src := []byte("issues:\n  rfc_existing_check:\n    enabled: true\n    post_when_nothing_found: true\n    labels: [' RFC ', '', Proposal]\n    max_matches: 2\n    max_age: 1h\n")
	if err := yaml.Unmarshal(src, &cfg); err != nil {
		t.Fatal(err)
	}
	c := cfg.Issues.RFCExistingCheck
	if !c.Enabled || !c.PostWhenNothingFound {
		t.Fatalf("flags not parsed: %+v", c)
	}
	if got := c.EffectiveLabels(); !reflect.DeepEqual(got, []string{"rfc", "proposal"}) {
		t.Fatalf("labels = %v", got)
	}
	if c.EffectiveMaxMatches() != 2 || c.EffectiveMaxAge() != time.Hour {
		t.Fatalf("overrides not honoured: %d %v", c.EffectiveMaxMatches(), c.EffectiveMaxAge())
	}
}
