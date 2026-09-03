package panes

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// AgentStatus is the live state displayed beside an agent.
type AgentStatus string

const (
	AgentStatusRunning AgentStatus = "running"
	AgentStatusPaused  AgentStatus = "paused"
	AgentStatusError   AgentStatus = "error"
)

// AgentState contains the live fields that GET /api/agents does not expose.
// The app can join these fields from /api/status without adding values to
// client.Agent that the agent-list endpoint never sends.
type AgentState struct {
	Status       AgentStatus
	LastActivity time.Time
}

// AgentsMsg delivers a completed fleet snapshot to the Agents pane.
//
// Agents remains the direct result of GET /api/agents so the existing polling
// path can send this message unchanged. States is optional supplemental live
// data keyed by client.Agent.Name; T13b can populate it when it joins status
// updates. ObservedAt makes relative activity labels stable for the lifetime
// of a snapshot and deterministic in tests.
type AgentsMsg struct {
	Agents     []client.Agent
	States     map[string]AgentState
	ObservedAt time.Time
}

// Agents is the fleet pane. It renders content only; the app owns borders and
// focus chrome and routes keys only to the focused pane.
type Agents struct {
	stub
	agents     []client.Agent
	states     map[string]AgentState
	selected   int
	loaded     bool
	observedAt time.Time
}

// NewAgents returns the Agents pane in its pre-poll state.
func NewAgents() Agents { return Agents{stub: stub{title: "AGENTS"}} }

// SelectedAgent returns the dashboard name and paused state of the row under
// the cursor. The ok result is false until a successful fleet snapshot has
// supplied at least one row, so callers can make an action key a no-op instead
// of inventing a target.
//
// The status feed is authoritative when present. Before it arrives, Enabled
// is the same fallback the rendered status glyph uses, which keeps the action
// offered by `p` consistent with what the operator sees in the pane.
func (p Agents) SelectedAgent() (name string, paused bool, ok bool) {
	if p.selected < 0 || p.selected >= len(p.agents) {
		return "", false, false
	}
	agent := p.agents[p.selected]
	state, hasState := p.states[agent.Name]
	if hasState {
		return agent.Name, state.Status == AgentStatusPaused, true
	}
	return agent.Name, !agent.Enabled, true
}

// SelectedAgentDetail returns the fields the model picker needs about the row
// under the cursor: the canonical config-key name the /api/model write is
// addressed to, the display label, the configured backend whose catalogue is
// fetched, and the configured model to preselect.
//
// It is a second narrow accessor rather than a widening of SelectedAgent
// because the two callers want different things and SelectedAgent's paused bit
// is joined from supplemental status data that this caller has no use for.
// Both exist so the app never reaches into pane internals for a row.
//
// Name is deliberately separate from Display: the write must address the agent
// by its config key, and an agent whose displayName differs would be sent to a
// path that does not exist if the label leaked into the request.
func (p Agents) SelectedAgentDetail() (name, display, backend, model string, ok bool) {
	if p.selected < 0 || p.selected >= len(p.agents) {
		return "", "", "", "", false
	}
	agent := p.agents[p.selected]
	display = agent.DisplayName
	if display == "" {
		display = agent.Name
	}
	return agent.Name, display, agent.Backend, agent.Model, true
}

// SetAgentModel applies the authoritative model returned by a successful model
// change immediately, for the same reason SetAgentPaused does: the app also
// refreshes the fleet afterwards, but reflecting the response here stops a
// successful change from leaving the old model on screen while that refresh is
// in flight, or if it fails.
//
// An unknown agent is ignored rather than inserted. A response naming an agent
// that is not in the current roster is either stale or wrong, and inventing a
// row from it would put an agent on screen that /api/agents does not list.
func (p Agents) SetAgentModel(name, model string) Agents {
	if model == "" {
		// The write's authoritative response is the only source for this. An
		// empty model would blank the column with no fact behind it.
		return p
	}
	agents := append([]client.Agent(nil), p.agents...)
	found := false
	for i := range agents {
		if agents[i].Name == name {
			agents[i].Model = model
			found = true
			break
		}
	}
	if !found {
		return p
	}
	p.agents = agents
	return p
}

// SetAgentPaused applies the authoritative state returned by a pause/resume
// operation immediately. The app still refreshes the full fleet afterwards,
// but reflecting the response here prevents a successful action from leaving
// a stale row on screen while that refresh is in flight (or if it fails).
func (p Agents) SetAgentPaused(name string, paused bool) Agents {
	found := false
	for _, agent := range p.agents {
		if agent.Name == name {
			found = true
			break
		}
	}
	if !found {
		return p
	}
	p.states = cloneAgentStates(p.states)
	if p.states == nil {
		p.states = make(map[string]AgentState)
	}
	state := p.states[name]
	state.Status = AgentStatusRunning
	if paused {
		state.Status = AgentStatusPaused
	}
	p.states[name] = state
	return p
}

// Update replaces the latest successful snapshot and moves the selection with
// j/k (or the matching arrow keys), clamping at the fleet's edges.
func (p Agents) Update(msg tea.Msg) (Pane, tea.Cmd) {
	switch msg := msg.(type) {
	case AgentsMsg:
		p.agents = append([]client.Agent(nil), msg.Agents...)
		// States is optional supplemental data. A fleet-only refresh carries a
		// nil map and must not erase an authoritative pause/resume result that
		// the app just applied while the refresh was in flight.
		if msg.States != nil {
			p.states = cloneAgentStates(msg.States)
		}
		p.loaded = true
		p.observedAt = msg.ObservedAt
		if p.observedAt.IsZero() {
			p.observedAt = time.Now()
		}
		if len(p.agents) == 0 {
			p.selected = 0
		} else if p.selected >= len(p.agents) {
			p.selected = len(p.agents) - 1
		}
		return p, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "j", "down":
			if p.selected+1 < len(p.agents) {
				p.selected++
			}
			return p, nil
		case "k", "up":
			if p.selected > 0 {
				p.selected--
			}
			return p, nil
		}
	}
	return p.update(msg, p)
}

func cloneAgentStates(in map[string]AgentState) map[string]AgentState {
	if in == nil {
		return nil
	}
	out := make(map[string]AgentState, len(in))
	for name, state := range in {
		out[name] = state
	}
	return out
}

// View renders one clipped, non-wrapping row per agent into the exact box the
// grid assigned to this pane.
func (p Agents) View(width, height int) string {
	if !p.loaded {
		return stubView(p.Title(), width, height)
	}
	if width <= 0 || height <= 0 {
		return ""
	}

	lines := []string{p.titleLine()}
	if len(p.agents) == 0 {
		lines = append(lines, "", "no agents configured")
	} else {
		lines = append(lines, agentsHeader(width))
		for i, agent := range p.agents {
			lines = append(lines, p.agentLine(agent, i == p.selected, width))
		}
	}

	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		MaxWidth(width).
		MaxHeight(height).
		Render(strings.Join(lines, "\n"))
}

func (p Agents) titleLine() string {
	switch n := len(p.agents); n {
	case 0:
		return p.Title()
	case 1:
		return p.Title() + "  1 agent"
	default:
		return fmt.Sprintf("%s  %d agents", p.Title(), n)
	}
}

const (
	agentNameWidth     = 10
	agentBackendWidth  = 8
	agentModelWidth    = 14
	agentActivityWidth = 9
)

func agentsHeader(width int) string {
	return clipAgentLine(fmt.Sprintf("    %s %s %s %s",
		agentColumn("NAME", agentNameWidth),
		agentColumn("BACKEND", agentBackendWidth),
		agentColumn("MODEL", agentModelWidth),
		agentColumn("ACTIVITY", agentActivityWidth),
	), width)
}

func (p Agents) agentLine(agent client.Agent, selected bool, width int) string {
	cursor := " "
	if selected {
		cursor = "▸"
	}
	name := agent.DisplayName
	if name == "" {
		name = agent.Name
	}
	state, ok := p.states[agent.Name]
	if !ok {
		// GET /api/agents has only the configured enabled bit. It is enough
		// for the polling-only UI to distinguish active from stopped agents;
		// supplemental /api/status data supersedes it when available.
		state.Status = AgentStatusRunning
		if !agent.Enabled {
			state.Status = AgentStatusPaused
		}
	}
	return clipAgentLine(fmt.Sprintf("%s %s %s %s %s %s",
		cursor,
		statusGlyph(state.Status),
		agentColumn(name, agentNameWidth),
		agentColumn(agent.Backend, agentBackendWidth),
		agentColumn(agent.Model, agentModelWidth),
		agentColumn(relativeActivity(state.LastActivity, p.observedAt), agentActivityWidth),
	), width)
}

func agentColumn(value string, width int) string {
	return lipgloss.NewStyle().Inline(true).Width(width).MaxWidth(width).Render(value)
}

func clipAgentLine(line string, width int) string {
	return lipgloss.NewStyle().MaxWidth(width).Render(line)
}

func statusGlyph(status AgentStatus) string {
	switch status {
	case AgentStatusRunning:
		return "●"
	case AgentStatusPaused:
		return "Ⅱ"
	case AgentStatusError:
		return "×"
	default:
		return "?"
	}
}

func relativeActivity(activity, now time.Time) string {
	if activity.IsZero() {
		return "—"
	}
	age := now.Sub(activity)
	if age < time.Minute {
		return "now"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(age/(24*time.Hour)))
}
