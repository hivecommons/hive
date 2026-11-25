package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// An unset auto_merge.decision_gate is the safe default: on, the shipped
// credential paths, and the sentinel switch on.
func TestDecisionGateDefaultsAreSafe(t *testing.T) {
	var gate DecisionGateConfig
	if !gate.IsEnabled() || !gate.SentinelNeedsDecision() {
		t.Fatalf("unset gate: enabled = %v, sentinel = %v; want both on", gate.IsEnabled(), gate.SentinelNeedsDecision())
	}
	paths := gate.EffectivePaths()
	if len(paths) == 0 || len(paths) != len(DefaultDecisionGatePaths) {
		t.Fatalf("unset gate paths = %v, want DefaultDecisionGatePaths", paths)
	}
	for _, want := range []string{"**/pushbroker/**", "**/keystore/**", "**/*credential*", "**/*secret*", "**/auth/**"} {
		found := false
		for _, p := range paths {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("default decision paths lack %q", want)
		}
	}
	paths[0] = "mutated"
	if DefaultDecisionGatePaths[0] == "mutated" {
		t.Fatal("EffectivePaths returned the shared default slice, not a copy")
	}
}

func TestDecisionGateExplicitValues(t *testing.T) {
	off, on := false, true
	if (DecisionGateConfig{Enabled: &off}).IsEnabled() {
		t.Fatal("enabled: false still on")
	}
	if (DecisionGateConfig{Sentinel: &off}).SentinelNeedsDecision() {
		t.Fatal("sentinel: false still on")
	}
	if !(DecisionGateConfig{Enabled: &on, Sentinel: &on}).IsEnabled() {
		t.Fatal("enabled: true is off")
	}
	if got := (DecisionGateConfig{Paths: []string{}}).EffectivePaths(); got == nil || len(got) != 0 {
		t.Fatalf("explicit empty paths = %v, want an empty non-nil list", got)
	}
	if got := (DecisionGateConfig{Paths: []string{"src/secure/**"}}).EffectivePaths(); len(got) != 1 || got[0] != "src/secure/**" {
		t.Fatalf("explicit paths = %v, want the operator's list only", got)
	}
}

func TestDecisionGateYAMLRoundTrip(t *testing.T) {
	var cfg AutoMergeConfig
	src := "decision_gate:\n  enabled: true\n  sentinel: false\n  paths:\n    - \"src/secure/**\"\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	gate := cfg.DecisionGate
	if !gate.IsEnabled() || gate.SentinelNeedsDecision() || len(gate.EffectivePaths()) != 1 {
		t.Fatalf("decoded gate = %+v", gate)
	}
	var unset AutoMergeConfig
	if err := yaml.Unmarshal([]byte("{}\n"), &unset); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if !unset.DecisionGate.IsEnabled() || len(unset.DecisionGate.EffectivePaths()) != len(DefaultDecisionGatePaths) {
		t.Fatalf("absent decision_gate = %+v, want the default", unset.DecisionGate)
	}
}

func TestDecisionGateValidateRejectsBlankPattern(t *testing.T) {
	err := (DecisionGateConfig{Paths: []string{"src/**", "  "}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "auto_merge.decision_gate.paths") {
		t.Fatalf("Validate = %v, want a blank-pattern error naming the key", err)
	}
	if err := (DecisionGateConfig{}).Validate(); err != nil {
		t.Fatalf("Validate(default) = %v, want nil", err)
	}
}
