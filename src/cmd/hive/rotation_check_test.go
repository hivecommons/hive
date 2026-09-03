package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/rotation"
)

// Tests for runRotationCheck (cmd/hive/main.go) — the RFC #3958 eval-loop
// wiring that moves idle agents off positively-exhausted providers, strands
// them loudly when nothing has headroom, and auto-resumes strands when their
// provider recovers. The rotation DECISIONS (nextBackend, headroom classes,
// high-volume guard) are covered in pkg/rotation; these tests cover the
// wiring: which agents are candidates, which pauses it may touch, and what it
// writes back into the agent manager.

func rotationTestLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// rotationTestConfig builds a config with rotation enabled and two providers:
// anthropic (subscription, fronted by "claude") and openai (metered, fronted
// by "codex"). The single agent "quality" runs on claude.
func rotationTestConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Repos: []string{"r"}},
		Agents: map[string]config.AgentConfig{
			"quality": {Backend: "claude", Enabled: true},
		},
		Governor: config.GovernorConfig{
			Rotation: config.RotationConfig{
				Enabled: true,
				Providers: map[string]config.ProviderRotationConfig{
					"anthropic": {Class: rotation.ClassSubscription, Backends: []string{"claude"}},
					"openai":    {Class: rotation.ClassMetered, Backends: []string{"codex"}},
				},
			},
		},
	}
}

type rotationHarness struct {
	cfg      *config.Config
	rotMgr   *rotation.Manager
	gov      *governor.Governor
	agentMgr *agent.Manager
}

func newRotationHarness(t *testing.T) *rotationHarness {
	t.Helper()
	cfg := rotationTestConfig()
	return &rotationHarness{
		cfg:      cfg,
		rotMgr:   rotation.NewManager(cfg.Governor.Rotation),
		gov:      governor.New(config.GovernorConfig{}, cfg.Agents, rotationTestLogger()),
		agentMgr: agent.NewManager(cfg.Agents, rotationTestLogger(), agent.ProjectContext{}),
	}
}

func (h *rotationHarness) run(t *testing.T) {
	t.Helper()
	runRotationCheck(context.Background(), h.cfg, h.rotMgr, h.gov, h.agentMgr, rotationTestLogger())
}

func (h *rotationHarness) status(t *testing.T, name string) *agent.AgentProcess {
	t.Helper()
	proc, ok := h.agentMgr.AllStatuses()[name]
	if !ok {
		t.Fatalf("agent %q not found in AllStatuses", name)
	}
	return proc
}

// exhaust marks a provider positively measured as out of headroom.
func (h *rotationHarness) exhaust(provider string) {
	h.rotMgr.SetHeadroom(rotation.Headroom{Provider: provider, Available: false, PctRemaining: 0})
}

// recover marks a provider positively measured as having headroom.
func (h *rotationHarness) recover(provider string, pct int) {
	h.rotMgr.SetHeadroom(rotation.Headroom{Provider: provider, Available: true, PctRemaining: pct})
}

// A nil rotation manager must be a straight no-op: rotation is opt-in and the
// eval loop passes nil when it was never constructed.
func TestRunRotationCheck_NilManagerIsNoOp(t *testing.T) {
	h := newRotationHarness(t)
	runRotationCheck(context.Background(), h.cfg, nil, h.gov, h.agentMgr, rotationTestLogger())

	proc := h.status(t, "quality")
	if proc.Paused || proc.BackendOverride != "" {
		t.Errorf("nil rotMgr mutated agent state: paused=%v override=%q", proc.Paused, proc.BackendOverride)
	}
}

// Rotation disabled in config must be a no-op even when the manager exists
// and the provider is positively exhausted.
func TestRunRotationCheck_DisabledConfigIsNoOp(t *testing.T) {
	h := newRotationHarness(t)
	h.cfg.Governor.Rotation.Enabled = false
	h.exhaust("anthropic")
	h.recover("openai", 90)

	h.run(t)

	proc := h.status(t, "quality")
	if proc.Paused || proc.BackendOverride != "" {
		t.Errorf("disabled rotation mutated agent state: paused=%v override=%q", proc.Paused, proc.BackendOverride)
	}
}

// Healthy current provider: nothing moves, nothing pauses.
func TestRunRotationCheck_HealthyProviderUntouched(t *testing.T) {
	h := newRotationHarness(t)
	h.recover("anthropic", 60)
	h.recover("openai", 90)

	h.run(t)

	proc := h.status(t, "quality")
	if proc.Paused {
		t.Error("agent on healthy provider was paused")
	}
	if proc.BackendOverride != "" {
		t.Errorf("agent on healthy provider got BackendOverride %q", proc.BackendOverride)
	}
}

// Exhausted provider with a measured-available alternative: the agent gets a
// backend override onto the alternative and is NOT paused.
func TestRunRotationCheck_ExhaustedRotatesToHeadroom(t *testing.T) {
	h := newRotationHarness(t)
	h.exhaust("anthropic")
	h.recover("openai", 80)

	h.run(t)

	proc := h.status(t, "quality")
	if proc.BackendOverride != "codex" {
		t.Errorf("BackendOverride = %q, want %q", proc.BackendOverride, "codex")
	}
	if proc.Paused {
		t.Error("rotated agent must not be paused")
	}
}

// Exhausted provider and NO alternative with measured headroom: strand — the
// agent is paused with the rotation trigger so auto-resume can find it later.
func TestRunRotationCheck_NoHeadroomAnywhereStrands(t *testing.T) {
	h := newRotationHarness(t)
	h.exhaust("anthropic")
	h.exhaust("openai")

	h.run(t)

	proc := h.status(t, "quality")
	if !proc.Paused {
		t.Fatal("agent with no headroom anywhere was not strand-paused")
	}
	if proc.PausedTrigger != rotationTrigger {
		t.Errorf("PausedTrigger = %q, want %q (auto-resume keys off this)", proc.PausedTrigger, rotationTrigger)
	}
	if proc.BackendOverride != "" {
		t.Errorf("stranded agent got BackendOverride %q, want none", proc.BackendOverride)
	}
}

// A probe error is fail-open: it must never be treated as exhaustion, so the
// agent is neither rotated nor stranded.
func TestRunRotationCheck_ProbeErrorNeverRotates(t *testing.T) {
	h := newRotationHarness(t)
	h.rotMgr.SetHeadroom(rotation.Headroom{Provider: "anthropic", Available: false, ProbeErr: context.DeadlineExceeded})
	h.recover("openai", 90)

	h.run(t)

	proc := h.status(t, "quality")
	if proc.Paused || proc.BackendOverride != "" {
		t.Errorf("failed probe treated as exhaustion: paused=%v override=%q", proc.Paused, proc.BackendOverride)
	}
}

// An operator pause (any non-rotation trigger) is sacrosanct: even with the
// provider exhausted and an alternative available, runRotationCheck must not
// touch the agent — no resume, no override, trigger unchanged.
func TestRunRotationCheck_OperatorPauseUntouched(t *testing.T) {
	h := newRotationHarness(t)
	if err := h.agentMgr.Pause("quality", "dashboard-api", "operator quiesce"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	h.exhaust("anthropic")
	h.recover("openai", 90)

	h.run(t)

	proc := h.status(t, "quality")
	if !proc.Paused {
		t.Fatal("operator-paused agent was resumed by rotation")
	}
	if proc.PausedTrigger != "dashboard-api" {
		t.Errorf("PausedTrigger = %q, want %q (operator pause overwritten)", proc.PausedTrigger, "dashboard-api")
	}
	if proc.BackendOverride != "" {
		t.Errorf("operator-paused agent got BackendOverride %q", proc.BackendOverride)
	}
}

// A rotation-stranded agent whose provider is still exhausted stays paused:
// StrandRecovered is false, and the strand pause must not be re-applied or
// escalated into a rotation.
func TestRunRotationCheck_StrandNotRecoveredStaysPaused(t *testing.T) {
	h := newRotationHarness(t)
	if err := h.agentMgr.Pause("quality", rotationTrigger, "no provider has headroom (RFC #3958)"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	h.exhaust("anthropic")

	h.run(t)

	proc := h.status(t, "quality")
	if !proc.Paused {
		t.Fatal("stranded agent resumed while provider still exhausted")
	}
	if proc.PausedTrigger != rotationTrigger {
		t.Errorf("PausedTrigger = %q, want %q", proc.PausedTrigger, rotationTrigger)
	}
	if proc.BackendOverride != "" {
		t.Errorf("stranded agent got BackendOverride %q while unrecovered", proc.BackendOverride)
	}
}

// A rotation-stranded agent whose provider recovered headroom is auto-resumed.
// The agent is sandbox-enabled so Resume takes the no-tmux path — the wiring
// under test is the StrandRecovered → Resume decision, not the relaunch.
func TestRunRotationCheck_StrandRecoveredAutoResumes(t *testing.T) {
	h := newRotationHarness(t)
	enabled := true
	agents := map[string]config.AgentConfig{
		"quality": {
			Backend: "claude",
			Enabled: true,
			Sandbox: &config.AgentSandboxOverride{Enabled: &enabled},
		},
	}
	h.cfg.Agents = agents
	h.agentMgr = agent.NewManager(agents, rotationTestLogger(), agent.ProjectContext{})
	h.agentMgr.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true})

	if err := h.agentMgr.Pause("quality", rotationTrigger, "no provider has headroom (RFC #3958)"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	h.recover("anthropic", 70)

	h.run(t)

	proc := h.status(t, "quality")
	if proc.Paused {
		t.Fatal("stranded agent not auto-resumed after provider recovery")
	}
	if proc.PausedTrigger != "" {
		t.Errorf("PausedTrigger = %q after resume, want empty", proc.PausedTrigger)
	}
}

// A BackendOverride from an earlier rotation is the agent's effective backend:
// exhaustion checks must key off the override, not the configured backend.
// Here the agent was already moved to codex; anthropic (its configured
// provider) being exhausted is irrelevant while openai has headroom.
func TestRunRotationCheck_ExistingOverrideIsEffectiveBackend(t *testing.T) {
	h := newRotationHarness(t)
	if err := h.agentMgr.SetBackendOverride("quality", "codex"); err != nil {
		t.Fatalf("SetBackendOverride: %v", err)
	}
	h.exhaust("anthropic")
	h.recover("openai", 90)

	h.run(t)

	proc := h.status(t, "quality")
	if proc.Paused {
		t.Error("agent on healthy override backend was paused")
	}
	if proc.BackendOverride != "codex" {
		t.Errorf("BackendOverride = %q, want %q (must judge the override, not the config backend)", proc.BackendOverride, "codex")
	}
}

// Reverse direction of the override test: the override backend (codex/openai)
// is exhausted while the configured one (claude/anthropic) recovered — the
// agent must rotate back onto claude.
func TestRunRotationCheck_OverrideExhaustedRotatesBack(t *testing.T) {
	h := newRotationHarness(t)
	if err := h.agentMgr.SetBackendOverride("quality", "codex"); err != nil {
		t.Fatalf("SetBackendOverride: %v", err)
	}
	h.exhaust("openai")
	h.recover("anthropic", 55)

	h.run(t)

	proc := h.status(t, "quality")
	if proc.BackendOverride != "claude" {
		t.Errorf("BackendOverride = %q, want %q", proc.BackendOverride, "claude")
	}
	if proc.Paused {
		t.Error("rotated agent must not be paused")
	}
}

// Backend not fronted by any configured provider: Exhausted() is false by
// construction, so the agent is untouched (fail-open for unknown backends).
func TestRunRotationCheck_UnknownBackendUntouched(t *testing.T) {
	h := newRotationHarness(t)
	agents := map[string]config.AgentConfig{
		"quality": {Backend: "copilot", Enabled: true},
	}
	h.cfg.Agents = agents
	h.agentMgr = agent.NewManager(agents, rotationTestLogger(), agent.ProjectContext{})
	h.exhaust("anthropic")
	h.exhaust("openai")

	h.run(t)

	proc := h.status(t, "quality")
	if proc.Paused || proc.BackendOverride != "" {
		t.Errorf("agent on unmapped backend mutated: paused=%v override=%q", proc.Paused, proc.BackendOverride)
	}
}
