package agent

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ModelRejectedClass is the ProviderErrorClass set when the agent's CLI
// refuses the configured model id. It rides the existing provider-error
// block, so the dashboard card shows "blocked: inference (model_rejected):
// model <id> rejected by the CLI: ..." and kicks back off exponentially
// (providerErrorBackoffDelay) instead of re-hitting the same refusal.
const ModelRejectedClass = "model_rejected"

// ModelRejectedTrigger is the pause trigger used when an agent is parked
// after modelRejectionParkAfter identical model-rejection failures.
const ModelRejectedTrigger = "model-rejected"

// modelRejectionParkAfter is how many identical rejections of the same model
// id (one per kicked turn or launch) are tolerated before the agent is paused.
// The first ones only back off: Claude Code shows the same refusal for a
// model this account briefly cannot reach, so one sighting is not proof the
// id is wrong. Three in a row, each after a longer kick backoff (2m, then
// 4m), is.
const modelRejectionParkAfter = 3

// claudeModelRejectionRe matches Claude Code's refusal of the --model id, as
// captured from a live claude 2.1.311 pane (interactive and -p):
//
//	● There's an issue with the selected model (claude-sonnet-5.5). It may not exist or you may not have access to it. Run /model to pick a different model.
//
// Anchored at the start of the line (after the CLI's own response glyphs) so
// an agent that QUOTES the sentence mid-prose, or a kick that echoes it after
// "❯ " or "> ", is not read as the CLI refusing its own model. Both the ASCII
// and the typographic apostrophe are accepted.
var claudeModelRejectionRe = regexp.MustCompile(`^[\s●⏺⎿]*There(?:'|’)s an issue with the selected model \(([^)\s]+)\)`)

// lineModelRejection reports the model id a single pane line says the CLI
// rejected.
func lineModelRejection(line string) (model string, ok bool) {
	m := claudeModelRejectionRe.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// paneShowsModelRejection returns the newest model-rejection line in lines,
// with the rejected id. Scanned bottom-up so the latest refusal wins.
func paneShowsModelRejection(lines []string) (model, line string, ok bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		if id, found := lineModelRejection(lines[i]); found {
			return id, modelRejectionCLIText(lines[i]), true
		}
	}
	return "", "", false
}

// modelRejectionCLIText is the CLI's refusal sentence without the response
// glyphs it is rendered under.
func modelRejectionCLIText(line string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "●⏺⎿ "))
}

// modelRejectionMatch is the provider-error verdict for a rejected model. The
// line is built from the id and the CLI's own text only, so the poller and
// the inference stall watchdog produce the SAME line for the same refusal and
// markProviderErrorLocked treats them as one failure, not two.
func modelRejectionMatch(model, cliLine string) providerErrorMatch {
	return providerErrorMatch{
		Class: ModelRejectedClass,
		Line:  fmt.Sprintf("model %s rejected by the CLI: %s", model, cliLine),
		Model: model,
	}
}

// modelRejectionState is the per-agent count of identical rejections.
type modelRejectionState struct {
	model string    // the id the CLI rejected
	count int       // identical rejections of model, one per turn
	turn  time.Time // the turn (kick or launch) the last counted rejection belonged to
}

// note records one sighting of a rejection of model during turn. A sighting
// in a turn already counted is not a new failure: the refusal line stays on
// screen for every 3s poll until the next kick. A different model starts a
// fresh count.
func (s *modelRejectionState) note(model string, turn time.Time) (count int, fresh bool) {
	if s.model != model {
		*s = modelRejectionState{model: model}
	}
	if s.count > 0 && s.turn.Equal(turn) {
		return s.count, false
	}
	s.count++
	s.turn = turn
	return s.count, true
}

// modelRejectionTurnKey identifies the turn a rejection on screen belongs to:
// the last kick of the current launch, or the launch itself when this launch
// has not been kicked yet.
func modelRejectionTurnKey(agent *AgentProcess) time.Time {
	if agent.LastKick != nil && (agent.StartedAt == nil || !agent.LastKick.Before(*agent.StartedAt)) {
		return *agent.LastKick
	}
	if agent.StartedAt != nil {
		return *agent.StartedAt
	}
	return time.Time{}
}

// modelRejectedForCurrentModel reports whether the CLI has rejected the model
// this agent will launch with. A restart relaunches with the same --model, so
// while this holds a restart cannot help.
func (a *AgentProcess) modelRejectedForCurrentModel() bool {
	return a.modelRejection.count > 0 && a.modelRejection.model != "" &&
		a.modelRejection.model == a.effectiveModel()
}

// recordModelRejectionLocked records one sighting of the CLI rejecting model.
// A fresh failure (a new turn) sets the provider-error block — which backs
// off kicks exponentially and shows the error on the agent's status — and
// park reports that the agent has now failed modelRejectionParkAfter times
// on the same id and must be paused. Caller holds m.mu.
func (m *Manager) recordModelRejectionLocked(agent *AgentProcess, match providerErrorMatch, now time.Time) (count int, fresh, park bool) {
	model := match.Model
	count, fresh = agent.modelRejection.note(model, modelRejectionTurnKey(agent))
	if !fresh {
		return count, false, false
	}
	// A new turn that hit the same refusal is a new failure even inside
	// the current window (a kick that slipped past it, or a launch), so
	// expire the window first: markProviderErrorLocked then advances the
	// attempt and the next backoff doubles instead of being re-served.
	agent.ProviderErrorBackoffUntil = time.Time{}
	delay := m.markProviderErrorLocked(agent, match, now)
	if m.logger != nil {
		m.logger.Warn("agent CLI rejected its configured model",
			"agent", agent.Name,
			"model", model,
			"identical_failures", count,
			"park_after", modelRejectionParkAfter,
			"kick_backoff", delay.Round(time.Second),
			"error", match.Line,
		)
	}
	return count, true, count >= modelRejectionParkAfter && !agent.Paused
}

// observeModelRejection is the pane poller's hook: it records a rejection
// shown in lines and, once the same id has failed modelRejectionParkAfter
// times, pauses the agent with a reason that names the model. Returns true
// when it paused the agent (the caller stops polling: Pause cancels it).
func (m *Manager) observeModelRejection(agent *AgentProcess, lines []string, now time.Time) bool {
	model, cliLine, ok := paneShowsModelRejection(lines)
	if !ok {
		return false
	}
	m.mu.Lock()
	count, _, park := m.recordModelRejectionLocked(agent, modelRejectionMatch(model, cliLine), now)
	m.mu.Unlock()
	if !park {
		return false
	}
	return m.parkForModelRejection(agent.Name, model, count)
}

// parkForModelRejection pauses the agent after repeated identical rejections.
func (m *Manager) parkForModelRejection(name, model string, count int) bool {
	reason := modelRejectionPauseReason(model, count)
	if err := m.Pause(name, ModelRejectedTrigger, reason); err != nil {
		if m.logger != nil {
			m.logger.Warn("model-rejection pause failed", "agent", name, "error", err)
		}
		return false
	}
	return true
}

// modelRejectionPauseReason is the operator-facing pause reason.
func modelRejectionPauseReason(model string, count int) string {
	return fmt.Sprintf("model %s rejected by the CLI %d times in a row; set a valid model for this agent, then resume it", model, count)
}

// clearModelRejectionLocked drops the rejection count and, when the active
// provider-error block is a model rejection, that block too. Called when the
// model the agent launches with changes, or the operator resumes the agent.
// Caller holds m.mu.
func (m *Manager) clearModelRejectionLocked(agent *AgentProcess, now time.Time) {
	agent.modelRejection = modelRejectionState{}
	if agent.ProviderErrorClass == ModelRejectedClass {
		m.clearProviderErrorLocked(agent, now)
	}
}

// clearStaleModelRejectionLocked clears the rejection state when model (the
// one about to be launched or just selected) is not the rejected one.
// Caller holds m.mu.
func (m *Manager) clearStaleModelRejectionLocked(agent *AgentProcess, model string, now time.Time) {
	if agent.modelRejection.model != "" && agent.modelRejection.model != model {
		m.clearModelRejectionLocked(agent, now)
	}
}
