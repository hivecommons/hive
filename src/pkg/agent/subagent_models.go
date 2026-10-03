package agent

import (
	"regexp"
	"strconv"
	"strings"
)

// SubAgentModel is an explicit model observation, not the model requested by
// task(). Copilot 1.0.88 (Dockerfile pin) prints these in its terminal task rows.
// The session-state layout is not a stable effective-model contract, so use
// those rows as a read-only fallback. Missing/changed layouts yield no rows.
type SubAgentModel struct {
	AgentType       string `json:"agentType"`
	Model           string `json:"model"`
	OlderGeneration bool   `json:"olderGeneration,omitempty"`
}

// Anchor the whole row: policy prose mentioning a model must not become a
// model observation. Accept the TUI's status/tree decoration, not arbitrary text.
var subAgentModelRow = regexp.MustCompile(`^[\s│├└─╰•●○◐◑◒◓✓✔✗✘]*([A-Za-z][A-Za-z0-9 -]{0,63}) \(model: ([a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127})\)\s*$`)
var claudeGeneration = regexp.MustCompile(`^claude-(?:haiku|sonnet|opus)-(\d+)(?:[.-](\d+))?$`)

func subAgentRows(lines []string) []string {
	var rows []string
	for _, line := range lines {
		if match := subAgentModelRow.FindStringSubmatch(line); match != nil {
			rows = append(rows, strings.TrimSpace(match[1])+"\x00"+match[2])
		}
	}
	return rows
}

// Compare generations within the Claude family, across tiers. Aliases,
// unknown families and version suffixes are deliberately not guessed.
func olderModelGeneration(model, parent string) bool {
	child := claudeGeneration.FindStringSubmatch(model)
	launch := claudeGeneration.FindStringSubmatch(parent)
	if child == nil || launch == nil {
		return false
	}
	major, _ := strconv.Atoi(child[1])
	minor, _ := strconv.Atoi(child[2])
	parentMajor, _ := strconv.Atoi(launch[1])
	parentMinor, _ := strconv.Atoi(launch[2])
	return major < parentMajor || major == parentMajor && minor < parentMinor
}

func (m *Manager) observeSubAgentModels(a *AgentProcess, lines []string) {
	// Match the manager -> pane lock order used by snapshot. LastKick and the
	// launch overrides belong to the manager; observations belong to paneMu.
	m.mu.RLock()
	defer m.mu.RUnlock()
	a.paneMu.Lock()
	defer a.paneMu.Unlock()
	a.observeSubAgentModelsLocked(lines)
}

func (a *AgentProcess) observeSubAgentModelsLocked(lines []string) {
	if effectiveBackend(a) != "copilot" || a.LastKick == nil {
		a.SubAgentModels = nil
		a.subAgentRows = nil
		return
	}
	if !a.subAgentKick.Equal(*a.LastKick) {
		a.SubAgentModels = nil
		a.subAgentKick = *a.LastKick
		// Existing scrollback belongs to the previous kick. Seed the diff from
		// the previous capture so it cannot be reported as new dispatches.
		a.subAgentRows = subAgentRows(a.lastPaneCapture)
	}
	rows := subAgentRows(lines)
	parent := a.Config.Model
	if a.ModelOverride != "" {
		parent = a.ModelOverride
	}
	for _, row := range diffNewLines(a.subAgentRows, rows) {
		// Bound retained observations even during an unusually long kick.
		if len(a.SubAgentModels) >= 256 {
			break
		}
		parts := strings.SplitN(row, "\x00", 2)
		a.SubAgentModels = append(a.SubAgentModels, SubAgentModel{
			AgentType: parts[0], Model: parts[1],
			OlderGeneration: olderModelGeneration(parts[1], CopilotLaunchModel(parent)),
		})
	}
	// Empty panes (screen clears, collapsed task output) aren't evidence that
	// the same rows appearing on the next repaint are new sub-agents.
	if len(rows) > 0 {
		a.subAgentRows = rows
	}
}
