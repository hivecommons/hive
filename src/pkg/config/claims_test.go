package config

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/issueclaim"
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
