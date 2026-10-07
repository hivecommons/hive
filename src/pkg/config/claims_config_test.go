package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestClaimsConfigDefaults(t *testing.T) {
	var c ClaimsConfig
	if c.IsEnabled() || !c.CommentEnabled() || !c.LabelEnabled() {
		t.Fatal("claims must default off while comment/label default on")
	}
	h, a, ct, m := c.TTLs()
	if h != 0 || a != 0 || ct != 0 || m != 0 {
		t.Fatalf("zero config must yield zero TTLs (package defaults): %v %v %v %v", h, a, ct, m)
	}
}

func TestClaimsConfigYAML(t *testing.T) {
	var cfg Config
	src := "governor:\n  claims:\n    enabled: true\n    ttl_s: 3600\n    human_ttl_s: 7200\n    contributor_ttl_s: 600\n    label: false\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Governor.Claims.IsEnabled() {
		t.Fatal("explicit enabled: true ignored")
	}
	if cfg.Governor.Claims.LabelEnabled() || !cfg.Governor.Claims.CommentEnabled() {
		t.Fatal("label/comment toggles wrong")
	}
	h, _, ct, _ := cfg.Governor.Claims.TTLs()
	if h != 2*time.Hour || ct != 10*time.Minute {
		t.Fatalf("TTLs=%v %v", h, ct)
	}
}

func TestClaimsEscalationThreshold(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{
		{"", DefaultClaimEscalateAfter},
		{"    escalate_after_claims: 3\n", 3},
		{"    escalate_after_claims: -1\n", 0},
	} {
		var cfg Config
		if err := yaml.Unmarshal([]byte("governor:\n  claims:\n    enabled: true\n"+tc.yaml), &cfg); err != nil {
			t.Fatal(err)
		}
		if got := cfg.Governor.Claims.EscalationThreshold(); got != tc.want {
			t.Fatalf("%q: EscalationThreshold=%d want %d", tc.yaml, got, tc.want)
		}
	}
}

// #10981: the dashboard reports effective lifetimes and their defaults, and
// rejects negative values or a lifetime above the effective max.
func TestClaimsConfigEffectiveTTLsAndValidate(t *testing.T) {
	var c ClaimsConfig
	h, a, ct, m := c.EffectiveTTLs()
	if h != 4*time.Hour || a != 2*time.Hour || ct != 30*time.Minute || m != 24*time.Hour {
		t.Fatalf("zero config effective TTLs = %v %v %v %v", h, a, ct, m)
	}
	c.TTLS = 3600
	if dh, _, _, _ := c.DefaultTTLs(); dh != time.Hour {
		t.Fatalf("human default with ttl_s set = %v, want 1h", dh)
	}
	c.AgentTTLS = 600
	if _, a, _, _ = c.EffectiveTTLs(); a != 10*time.Minute {
		t.Fatalf("agent effective = %v", a)
	}
	if err := c.ValidateTTLs(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, bad := range []ClaimsConfig{
		{HumanTTLS: -1},
		{MaxTTLS: -1},
		{MaxTTLS: 3600, AgentTTLS: 7200},
		{TTLS: 90000},
		{MaxTTLS: 600},
	} {
		if err := bad.ValidateTTLs(); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
}
