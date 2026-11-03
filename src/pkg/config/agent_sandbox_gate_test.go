package config

import (
	"strings"
	"testing"
)

// The sandbox opt-in is two-gated: agent_sandbox.enabled AND a per-agent
// sandbox.enabled. The dashboard's Security tab writes only the first, and it
// is the only sandbox control the UI offers — so an owner can turn "agent
// sandbox" on, be told the setting was updated, and have every agent keep
// running unconfined on the operator's host.
//
// #4918 is what that silence costs: an agent doing correct work on an assigned
// third-party repo ran that repo's test suite, a hook escaped its stubs, and
// `rpm-ostree kargs` reached the operator's real deployment. An operator who
// had flipped the toggle would reasonably have believed they were covered.
//
// These tests pin the diagnostic, not a behaviour change. Collapsing the gate
// itself is deliberately NOT done — see AgentSandboxGateWarnings.

func gateCfg(t *testing.T, globalEnabled bool, globalImage string, agents map[string]AgentConfig) *Config {
	t.Helper()
	return &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			AgentSandbox: AgentSandboxConfig{Enabled: globalEnabled, Image: globalImage},
			Agents:       agents,
		},
	}
}

// TestSandboxOffGloballyIsNotAWarning: the documented default posture is the
// sandbox being off. Warning about it on every hive would be noise that trains
// operators to ignore the line that matters.
func TestSandboxOffGloballyIsNotAWarning(t *testing.T) {
	cfg := gateCfg(t, false, "", map[string]AgentConfig{"scanner": {}})
	if got := AgentSandboxGateWarnings(cfg); len(got) != 0 {
		t.Errorf("AgentSandboxGateWarnings = %q, want none when the sandbox is off globally", got)
	}
}

// TestGlobalGateAloneIsReportedInert is the #4918 case: the exact state the
// dashboard toggle produces on a hive with no per-agent sandbox blocks.
func TestGlobalGateAloneIsReportedInert(t *testing.T) {
	cfg := gateCfg(t, true, "ghcr.io/example/agent:latest", map[string]AgentConfig{
		"scanner": {},
		"quality": {},
	})
	got := AgentSandboxGateWarnings(cfg)
	if len(got) != 1 {
		t.Fatalf("AgentSandboxGateWarnings = %q, want exactly one warning", got)
	}
	for _, want := range []string{"NO agent is opted in", "inert", "unconfined", "#4918"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q does not mention %q", got[0], want)
		}
	}
	// The remedy has to be in the message. A warning that names a problem and
	// not the key that fixes it sends the operator back to the source.
	if !strings.Contains(got[0], "sandbox: {enabled: true}") {
		t.Errorf("warning %q does not name the per-agent key that fixes it", got[0])
	}
}

// TestFullyOptedInIsSilent: a correctly configured hive must produce no
// warnings at all, or the diagnostic is noise.
func TestFullyOptedInIsSilent(t *testing.T) {
	cfg := gateCfg(t, true, "ghcr.io/example/agent:latest", map[string]AgentConfig{
		"scanner": {Sandbox: &AgentSandboxOverride{Enabled: boolPtr(true)}},
	})
	if got := AgentSandboxGateWarnings(cfg); len(got) != 0 {
		t.Errorf("AgentSandboxGateWarnings = %q, want none for a fully opted-in hive", got)
	}
}

// TestPartialOptInNamesTheUnconfinedRemainder. A hive that sandboxed one agent
// and forgot the others is the state most likely to be mistaken for "we are
// sandboxed" — the Security tab reports the global flag as on either way.
func TestPartialOptInNamesTheUnconfinedRemainder(t *testing.T) {
	cfg := gateCfg(t, true, "ghcr.io/example/agent:latest", map[string]AgentConfig{
		"scanner": {Sandbox: &AgentSandboxOverride{Enabled: boolPtr(true)}},
		"quality": {},
		"planner": {},
	})
	got := AgentSandboxGateWarnings(cfg)
	if len(got) != 1 {
		t.Fatalf("AgentSandboxGateWarnings = %q, want exactly one warning", got)
	}
	for _, want := range []string{"only 1 of 3", "scanner", "unconfined"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q does not mention %q", got[0], want)
		}
	}
}

// TestOptedInWithoutAnImageIsReported. startSandboxKickLocked refuses a kick
// when no image resolves, and there is NO fallback to the tmux path — so this
// misconfiguration does not degrade, it fails every kick. That is also the
// reason the second gate cannot simply be collapsed, so it is worth its own
// line rather than being folded into the inert-gate message.
func TestOptedInWithoutAnImageIsReported(t *testing.T) {
	cfg := gateCfg(t, true, "", map[string]AgentConfig{
		"scanner": {Sandbox: &AgentSandboxOverride{Enabled: boolPtr(true)}},
	})
	got := AgentSandboxGateWarnings(cfg)
	if len(got) != 1 {
		t.Fatalf("AgentSandboxGateWarnings = %q, want exactly one warning", got)
	}
	for _, want := range []string{"scanner", "no sandbox image", "no tmux fallback"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("warning %q does not mention %q", got[0], want)
		}
	}
}

// TestPerAgentImageSatisfiesTheImageCheck: the per-agent override is a real
// source of the image, so an agent carrying its own must not be reported.
func TestPerAgentImageSatisfiesTheImageCheck(t *testing.T) {
	cfg := gateCfg(t, true, "", map[string]AgentConfig{
		"scanner": {Sandbox: &AgentSandboxOverride{
			Enabled: boolPtr(true),
			Image:   "ghcr.io/example/scanner:latest",
		}},
	})
	if got := AgentSandboxGateWarnings(cfg); len(got) != 0 {
		t.Errorf("AgentSandboxGateWarnings = %q, want none when the agent carries its own image", got)
	}
}

// TestExplicitPerAgentFalseStillCountsAsUnconfined. `sandbox: {enabled: false}`
// is a deliberate opt-out, but the agent is still running unconfined, so the
// count the operator is shown must include it.
func TestExplicitPerAgentFalseStillCountsAsUnconfined(t *testing.T) {
	cfg := gateCfg(t, true, "ghcr.io/example/agent:latest", map[string]AgentConfig{
		"scanner": {Sandbox: &AgentSandboxOverride{Enabled: boolPtr(true)}},
		"quality": {Sandbox: &AgentSandboxOverride{Enabled: boolPtr(false)}},
	})
	got := AgentSandboxGateWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "only 1 of 2") {
		t.Errorf("AgentSandboxGateWarnings = %q, want it to count the opted-out agent as unconfined", got)
	}
}

// TestNilConfigIsSafe: the diagnostic runs on the boot path and on every config
// reload; it must never be the thing that panics there.
func TestNilConfigIsSafe(t *testing.T) {
	if got := AgentSandboxGateWarnings(nil); len(got) != 0 {
		t.Errorf("AgentSandboxGateWarnings(nil) = %q, want none", got)
	}
}
