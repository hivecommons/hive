package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the default-resolving getters that had no coverage:
// sandbox network/timeout overrides, the budget probe interval, review
// fan-out, escalation threshold, and the secret-file read-gate registrar.

func TestSandboxNetworkMode(t *testing.T) {
	global := AgentSandboxConfig{NetworkMode: "none"}

	var nilAgent *AgentConfig
	if got := nilAgent.SandboxNetworkMode(global); got != "none" {
		t.Errorf("nil agent: got %q, want global %q", got, "none")
	}
	plain := &AgentConfig{}
	if got := plain.SandboxNetworkMode(global); got != "none" {
		t.Errorf("no override block: got %q, want global %q", got, "none")
	}
	empty := &AgentConfig{Sandbox: &AgentSandboxOverride{}}
	if got := empty.SandboxNetworkMode(global); got != "none" {
		t.Errorf("empty override: got %q, want global %q", got, "none")
	}
	override := &AgentConfig{Sandbox: &AgentSandboxOverride{NetworkMode: "bridge"}}
	if got := override.SandboxNetworkMode(global); got != "bridge" {
		t.Errorf("per-agent override must win: got %q, want %q", got, "bridge")
	}
	if got := override.SandboxNetworkMode(AgentSandboxConfig{}); got != "bridge" {
		t.Errorf("override with empty global: got %q, want %q", got, "bridge")
	}
}

func TestSandboxTimeoutS(t *testing.T) {
	global := AgentSandboxConfig{TimeoutS: 600}

	var nilAgent *AgentConfig
	if got := nilAgent.SandboxTimeoutS(global); got != 600 {
		t.Errorf("nil agent: got %d, want global 600", got)
	}
	// Non-positive per-agent values mean "unset", not "zero timeout".
	zero := &AgentConfig{Sandbox: &AgentSandboxOverride{TimeoutS: 0}}
	if got := zero.SandboxTimeoutS(global); got != 600 {
		t.Errorf("zero override must fall back to global: got %d, want 600", got)
	}
	neg := &AgentConfig{Sandbox: &AgentSandboxOverride{TimeoutS: -5}}
	if got := neg.SandboxTimeoutS(global); got != 600 {
		t.Errorf("negative override must fall back to global: got %d, want 600", got)
	}
	override := &AgentConfig{Sandbox: &AgentSandboxOverride{TimeoutS: 120}}
	if got := override.SandboxTimeoutS(global); got != 120 {
		t.Errorf("per-agent override must win: got %d, want 120", got)
	}
}

func TestEffectiveProbeInterval(t *testing.T) {
	// Unset, zero, and negative all mean "use the 30-minute default" —
	// never-probe is not a reachable configuration.
	for _, s := range []int{0, -1} {
		p := ProviderBudgetConfig{ProbeIntervalS: s}
		if got := p.EffectiveProbeInterval(); got != 30*time.Minute {
			t.Errorf("ProbeIntervalS=%d: got %v, want 30m default", s, got)
		}
	}
	p := ProviderBudgetConfig{ProbeIntervalS: 90}
	if got := p.EffectiveProbeInterval(); got != 90*time.Second {
		t.Errorf("got %v, want 90s", got)
	}
}

func TestEffectiveMaxParallelReviews(t *testing.T) {
	if got := (ReviewConfig{}).EffectiveMaxParallelReviews(); got != DefaultMaxParallelReviews {
		t.Errorf("unset: got %d, want default %d", got, DefaultMaxParallelReviews)
	}
	if got := (ReviewConfig{MaxParallelReviews: -1}).EffectiveMaxParallelReviews(); got != DefaultMaxParallelReviews {
		t.Errorf("negative: got %d, want default %d", got, DefaultMaxParallelReviews)
	}
	if got := (ReviewConfig{MaxParallelReviews: 2}).EffectiveMaxParallelReviews(); got != 2 {
		t.Errorf("set: got %d, want 2", got)
	}
}

func TestEffectiveThreshold(t *testing.T) {
	if got := (EscalationConfig{}).EffectiveThreshold(); got != DefaultEscalationThreshold {
		t.Errorf("unset: got %d, want default %d", got, DefaultEscalationThreshold)
	}
	if got := (EscalationConfig{Threshold: -2}).EffectiveThreshold(); got != DefaultEscalationThreshold {
		t.Errorf("negative: got %d, want default %d", got, DefaultEscalationThreshold)
	}
	if got := (EscalationConfig{Threshold: 7}).EffectiveThreshold(); got != 7 {
		t.Errorf("set: got %d, want 7", got)
	}
}

func TestAllowSecretFileRoot(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "gateway.key")
	if err := os.WriteFile(keyFile, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	if SecretFilePathAllowed(keyFile) {
		t.Fatalf("%s must be denied before the root is registered", keyFile)
	}

	restore := AllowSecretFileRoot(dir)
	if !SecretFilePathAllowed(keyFile) {
		t.Errorf("%s must be allowed after registering %s", keyFile, dir)
	}
	// The registered directory itself is not a key file.
	if SecretFilePathAllowed(dir) {
		t.Error("the root directory itself must not be readable as a secret")
	}
	// A sibling directory sharing the prefix must not match.
	if SecretFilePathAllowed(dir + "-evil/gateway.key") {
		t.Error("prefix-sibling escape must be denied")
	}

	// The returned function restores the previous gate.
	restore()
	if SecretFilePathAllowed(keyFile) {
		t.Errorf("%s must be denied again after restore", keyFile)
	}

	// Blank registration is a no-op with a safe cleanup.
	noop := AllowSecretFileRoot("   ")
	noop()
}
