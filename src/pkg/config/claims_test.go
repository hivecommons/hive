package config

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/issueclaim"
	"gopkg.in/yaml.v3"
)

func TestClaimsConfig_DefaultOffAndDefaultTTL(t *testing.T) {
	var c ClaimsConfig
	if c.Enabled {
		t.Fatal("claims must be off by default")
	}
	if got := c.EffectiveTTL(); got != issueclaim.DefaultTTL {
		t.Fatalf("EffectiveTTL() = %v, want %v", got, issueclaim.DefaultTTL)
	}
	if got := (ClaimsConfig{TTLS: -1}).EffectiveTTL(); got != issueclaim.DefaultTTL {
		t.Fatalf("negative ttl_s must fall back to the default, got %v", got)
	}
	if got := (ClaimsConfig{TTLS: 90}).EffectiveTTL(); got != 90*time.Second {
		t.Fatalf("EffectiveTTL() = %v, want 90s", got)
	}
}

func TestGovernorConfig_ClaimsZeroValueIsOff(t *testing.T) {
	var g GovernorConfig
	if g.Claims.Enabled {
		t.Fatal("an absent governor.claims block must be off")
	}
}

func TestClaimsConfig_EffectiveNeverClaimLabels(t *testing.T) {
	if got := (ClaimsConfig{}).EffectiveNeverClaimLabels(); len(got) != 1 || got[0] != "epic" {
		t.Fatalf("absent never_claim_labels = %v, want [epic]", got)
	}
	if got := (ClaimsConfig{NeverClaimLabels: []string{}}).EffectiveNeverClaimLabels(); got == nil || len(got) != 0 {
		t.Fatalf("explicit empty never_claim_labels = %#v, want empty non-nil", got)
	}
	if got := (ClaimsConfig{NeverClaimLabels: []string{"tracking"}}).EffectiveNeverClaimLabels(); len(got) != 1 || got[0] != "tracking" {
		t.Fatalf("configured never_claim_labels = %v, want [tracking]", got)
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte("governor:\n  claims:\n    never_claim_labels: []\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Governor.Claims.EffectiveNeverClaimLabels(); len(got) != 0 {
		t.Fatalf("yaml never_claim_labels: [] = %v, want empty", got)
	}
}
