package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// modelRejectionPane is a Claude Code 2.1.311 pane captured after launching
// `claude --model claude-sonnet-5.5` and sending one prompt: the CLI starts,
// refuses the turn, and returns to its idle prompt, alive and "logged in".
const modelRejectionPane = ` ▐▛███▛█   Claude Code v2.1.311
▝▜██████▀  Sonnet 5 with high effort
 ▝▝   ▝▝   /data/agents/guide
❯ continue your guide work
● There's an issue with the selected model (claude-sonnet-5.5). It may not exist or you may not have access to it. Run /model to pick a different model.
✻ Worked for 0s · done 10:07 AM
────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

// The CLI's model refusal must be classified, not read as a healthy idle
// pane: on tip it matched nothing, so the inference stall watchdog cleared
// the provider-error state and nudged the agent into the same refusal.
func TestClassifyProviderErrorModelRejection(t *testing.T) {
	match, ok := classifyProviderError(modelRejectionPane)
	if !ok {
		t.Fatal("Claude Code's model refusal was not classified as a provider error")
	}
	if match.Class != "model_rejected" {
		t.Fatalf("class = %q, want model_rejected", match.Class)
	}
	if !strings.HasPrefix(match.Line, "model claude-sonnet-5.5 rejected by the CLI: There's an issue") {
		t.Fatalf("line = %q, want it to name the model and carry the CLI text", match.Line)
	}

	// The sentence quoted rather than printed by the CLI is not a refusal.
	for _, pane := range []string{
		"❯ There's an issue with the selected model (claude-sonnet-5.5). fix it",
		"> There's an issue with the selected model (claude-sonnet-5.5).",
		"● The CLI said: There's an issue with the selected model (claude-sonnet-5.5).",
	} {
		if m, ok := classifyProviderError(pane); ok && m.Class == "model_rejected" {
			t.Errorf("quoted text %q classified as a model rejection", pane)
		}
	}
}

// Repeated identical refusals on an inference agent must back kicks off and
// hold automatic restarts, instead of nudging and restarting at full rate.
func TestInferenceModelRejectionBacksOffInsteadOfRestarting(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"guide": {Backend: "vllm", Model: "claude-sonnet-5.5"},
	}, discardLogger(), ProjectContext{})
	termSeams(m) // keep every keystroke inside the fake terminal
	agent := m.agents["guide"]
	agent.State = StateRunning
	kickAt := time.Now().Add(-time.Minute)
	agent.LastKick = &kickAt
	agent.lastInferKickAt = kickAt
	agent.lastInferKickPane = paneContentHash("pane at kick time")

	m.nudgeIfKickStalled("guide", modelRejectionPane)

	remaining, class, line, blocked := m.ProviderErrorBackoffRemaining("guide")
	if !blocked || class != "model_rejected" {
		t.Fatalf("after a model refusal: blocked=%v class=%q, want a model_rejected kick backoff", blocked, class)
	}
	if remaining <= 0 || !strings.Contains(line, "model claude-sonnet-5.5 rejected by the CLI") {
		t.Fatalf("backoff=%v line=%q", remaining, line)
	}
	if agent.ActionNudges != 0 || agent.StallNudges != 0 {
		t.Fatalf("nudged into a refused model: action=%d stall=%d", agent.ActionNudges, agent.StallNudges)
	}
	if err := m.SendKick("guide", "continue"); err == nil || !strings.Contains(err.Error(), "model_rejected") {
		t.Fatalf("kick error = %v, want it refused by the model_rejected backoff", err)
	}
	err := m.restartForKick(context.Background(), "guide")
	if err == nil || !strings.Contains(err.Error(), "restart-storm breaker") {
		t.Fatalf("restart error = %v, want the restart held while the model is rejected", err)
	}
	if agent.RestartCount != 0 {
		t.Fatalf("RestartCount = %d, want 0: a restart cannot fix a rejected model", agent.RestartCount)
	}
}

// The claude-backend pane poller path: one failure per kicked turn, kick
// backoff growing each time, and the agent parked with a clear reason after
// modelRejectionParkAfter identical failures.
func TestModelRejectionParksAfterRepeatedIdenticalFailures(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"guide": {Backend: "claude", Model: "claude-sonnet-5.5"},
	}, discardLogger(), ProjectContext{})
	termSeams(m)
	agent := m.agents["guide"]
	agent.State = StateRunning
	started := time.Now().Add(-time.Hour)
	agent.StartedAt = &started
	lines := strings.Split(modelRejectionPane, "\n")

	var lastBackoff time.Duration
	for turn := 1; turn <= modelRejectionParkAfter; turn++ {
		kickAt := started.Add(time.Duration(turn) * time.Minute)
		agent.LastKick = &kickAt
		now := time.Now()
		parked := m.observeModelRejection(agent, lines, now)
		// The refusal stays on screen for every poll of the same turn; that
		// is still one failure.
		if again := m.observeModelRejection(agent, lines, now); again {
			t.Fatalf("turn %d: a repeat sighting of the same turn parked the agent", turn)
		}
		if agent.modelRejection.count != turn {
			t.Fatalf("turn %d: count = %d, want one per turn", turn, agent.modelRejection.count)
		}
		if turn < modelRejectionParkAfter {
			if parked || agent.Paused {
				t.Fatalf("turn %d: parked before %d identical failures", turn, modelRejectionParkAfter)
			}
			backoff := agent.ProviderErrorBackoffUntil.Sub(now)
			if backoff <= lastBackoff {
				t.Fatalf("turn %d: kick backoff %v did not grow past %v", turn, backoff, lastBackoff)
			}
			lastBackoff = backoff
			if !strings.Contains(agent.LastError, "model claude-sonnet-5.5 rejected by the CLI") {
				t.Fatalf("turn %d: LastError = %q, want the model rejection", turn, agent.LastError)
			}
			if plan := m.planAutomaticRestart("guide", "crashed agent recovery", "", now); plan.allowed || plan.backoff <= 0 {
				t.Fatalf("turn %d: restart plan = %+v, want it held by the kick backoff", turn, plan)
			}
			continue
		}
		if !parked || !agent.Paused || agent.PausedTrigger != ModelRejectedTrigger {
			t.Fatalf("after %d identical failures: parked=%v paused=%v trigger=%q", turn, parked, agent.Paused, agent.PausedTrigger)
		}
		if !strings.Contains(agent.PausedReason, "model claude-sonnet-5.5 rejected by the CLI") {
			t.Fatalf("pause reason = %q, want it to name the model", agent.PausedReason)
		}
	}

	// Fixing the model lifts the backoff and the count.
	if err := m.SetModelOverride("guide", "claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	if agent.modelRejection.count != 0 || agent.ProviderErrorClass != "" {
		t.Fatalf("after a model change: count=%d class=%q, want both cleared", agent.modelRejection.count, agent.ProviderErrorClass)
	}
}

// A restart attempted after the park threshold pauses through the existing
// restart-storm breaker instead of relaunching on the same rejected id.
func TestRestartPlanParksModelRejectedAgent(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"guide": {Backend: "claude", Model: "claude-sonnet-5.5"},
	}, discardLogger(), ProjectContext{})
	agent := m.agents["guide"]
	agent.modelRejection = modelRejectionState{model: "claude-sonnet-5.5", count: modelRejectionParkAfter}
	plan := m.planAutomaticRestart("guide", "crashed agent recovery", "", time.Now())
	if plan.pauseTrigger != ModelRejectedTrigger || !strings.Contains(plan.pauseReason, "claude-sonnet-5.5") {
		t.Fatalf("plan = %+v, want a model-rejected pause", plan)
	}

	// A different launch model is not held by the old rejection.
	agent.ModelOverride = "claude-sonnet-5-5"
	if plan := m.planAutomaticRestart("guide", "crashed agent recovery", "", time.Now()); !plan.allowed {
		t.Fatalf("plan = %+v, want an ordinary restart once the model changed", plan)
	}
}
