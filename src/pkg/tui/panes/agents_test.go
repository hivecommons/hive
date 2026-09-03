package panes

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/client"
)

func agent(name string) client.Agent {
	return client.Agent{Name: name, ID: "agt_" + name, Backend: "claude", Model: "claude-opus-4-5", Enabled: true}
}

// TestAgentsStopsWaitingOnceDataArrives pins the honesty rule the pane's
// `loaded` flag exists for: "waiting for data" must stop being shown the
// moment data has in fact arrived, including when that data is an empty fleet.
func TestAgentsStopsWaitingOnceDataArrives(t *testing.T) {
	if view := NewAgents().View(40, 10); !strings.Contains(view, placeholder) {
		t.Fatalf("pre-poll view missing %q:\n%s", placeholder, view)
	}

	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner"), agent("quality")}})
	view := loaded.View(40, 10)
	if strings.Contains(view, placeholder) {
		t.Fatalf("view still shows %q after a successful poll:\n%s", placeholder, view)
	}
	if !strings.Contains(view, "2 agents") {
		t.Fatalf("view does not report the fleet size:\n%s", view)
	}
}

// TestAgentsEmptyFleetIsNotWaiting is the case the flag exists to separate: a
// hive with no agents configured has polled successfully and must say so,
// rather than looking identical to a TUI that has fetched nothing.
func TestAgentsEmptyFleetIsNotWaiting(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{})
	view := loaded.View(40, 10)
	if strings.Contains(view, placeholder) {
		t.Fatalf("an empty fleet renders as %q, which claims nothing was fetched:\n%s", placeholder, view)
	}
	if !strings.Contains(view, "no agents configured") {
		t.Fatalf("an empty fleet does not say so:\n%s", view)
	}
}

// TestAgentsSingularWording: "1 agents" is the kind of detail that survives
// forever once shipped.
func TestAgentsSingularWording(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner")}})
	view := loaded.View(40, 10)
	if !strings.Contains(view, "1 agent") || strings.Contains(view, "1 agents") {
		t.Fatalf("single-agent fleet reads wrong:\n%s", view)
	}
}

// TestAgentsKeepsDataAcrossForeignMessages pins the pane's half of "a failed
// poll keeps the previous data". The app swallows errors, so what a pane
// actually sees between two successful polls is other panes' messages — and
// none of them may clear its fleet.
func TestAgentsKeepsDataAcrossForeignMessages(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner"), agent("quality")}})
	before := loaded.View(40, 10)

	type otherPaneMsg struct{ Data string }
	after, cmd := loaded.Update(otherPaneMsg{Data: "not for you"})
	if cmd != nil {
		t.Error("a foreign message produced a command")
	}
	if got := after.View(40, 10); got != before {
		t.Errorf("a foreign message changed the pane:\nbefore:\n%s\nafter:\n%s", before, got)
	}

	keyed, _ := after.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if got := keyed.View(40, 10); got != before {
		t.Error("an unrelated key changed the pane")
	}
}

// TestAgentsSelectionMovement covers j/k without coupling cursor behaviour to
// the rendering golden. Selection clamps rather than wrapping at either edge.
func TestAgentsSelectionMovement(t *testing.T) {
	next, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{
		agent("scanner"), agent("quality"), agent("reviewer"),
	}})
	p := next.(Agents)

	press := func(key string) {
		t.Helper()
		next, cmd := p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd != nil {
			t.Fatalf("key %q returned a command", key)
		}
		p = next.(Agents)
	}
	assertSelected := func(want int) {
		t.Helper()
		if p.selected != want {
			t.Fatalf("selected = %d, want %d", p.selected, want)
		}
	}

	assertSelected(0)
	press("j")
	assertSelected(1)
	press("j")
	press("j")
	assertSelected(2)
	press("k")
	assertSelected(1)
	press("k")
	press("k")
	assertSelected(0)
}

func TestAgentsSelectedAgentUsesDisplayedState(t *testing.T) {
	next, _ := NewAgents().Update(AgentsMsg{
		Agents: []client.Agent{
			agent("scanner"),
			{Name: "quality", Enabled: false},
		},
		States: map[string]AgentState{
			"scanner": {Status: AgentStatusPaused},
		},
	})
	p := next.(Agents)

	name, paused, ok := p.SelectedAgent()
	if !ok || name != "scanner" || !paused {
		t.Fatalf("first SelectedAgent() = (%q, %v, %v), want scanner, paused, true", name, paused, ok)
	}

	next, _ = p.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	p = next.(Agents)
	name, paused, ok = p.SelectedAgent()
	if !ok || name != "quality" || !paused {
		t.Fatalf("second SelectedAgent() = (%q, %v, %v), want quality, paused, true", name, paused, ok)
	}

	if _, _, ok := NewAgents().SelectedAgent(); ok {
		t.Fatal("SelectedAgent() reported a row before the first fleet snapshot")
	}
}

func TestAgentsSetAgentPausedAppliesActionResult(t *testing.T) {
	next, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner")}})
	p := next.(Agents).SetAgentPaused("scanner", true)

	name, paused, ok := p.SelectedAgent()
	if !ok || name != "scanner" || !paused {
		t.Fatalf("after SetAgentPaused(true) = (%q, %v, %v), want scanner, paused, true", name, paused, ok)
	}

	p = p.SetAgentPaused("scanner", false)
	_, paused, _ = p.SelectedAgent()
	if paused {
		t.Fatal("SetAgentPaused(false) left the selected agent paused")
	}

	// A normal /api/agents poll has no supplemental state map. It refreshes
	// the rows without erasing the authoritative result of the write.
	next, _ = p.SetAgentPaused("scanner", true).Update(AgentsMsg{Agents: []client.Agent{agent("scanner")}})
	_, paused, _ = next.(Agents).SelectedAgent()
	if !paused {
		t.Fatal("a fleet-only refresh erased the pause result")
	}
}

// TestAgentsViewFillsItsBoxExactly is the grid's structural requirement: a
// pane renders exactly the size it was given whatever its content, or the 2×2
// join skews.
func TestAgentsViewFillsItsBoxExactly(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner"), agent("quality")}})
	for _, dims := range [][2]int{{40, 10}, {20, 5}, {80, 24}} {
		w, h := dims[0], dims[1]
		view := loaded.View(w, h)
		if lines := strings.Count(view, "\n") + 1; lines != h {
			t.Errorf("View(%d,%d) rendered %d lines, want %d", w, h, lines, h)
		}
		if vw := visibleWidth(view); vw != w {
			t.Errorf("View(%d,%d) widest line is %d cells, want %d", w, h, vw, w)
		}
	}
	for _, dims := range [][2]int{{0, 5}, {5, 0}, {-1, -1}} {
		if got := loaded.View(dims[0], dims[1]); got != "" {
			t.Errorf("View(%d,%d) = %q, want empty", dims[0], dims[1], got)
		}
	}
}

// TestSelectedAgentDetailReportsTheRowUnderTheCursor is the accessor the model
// picker opens through. It exists so the app never reaches into pane internals
// for a row, and it returns Name separately from Display because the write it
// feeds addresses the agent by its CONFIG KEY.
func TestSelectedAgentDetailReportsTheRowUnderTheCursor(t *testing.T) {
	if _, _, _, _, ok := NewAgents().SelectedAgentDetail(); ok {
		t.Error("a pre-poll pane offers a selected agent, so m would open on nothing")
	}

	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{
		{Name: "scanner", DisplayName: "Fleet Scanner", Backend: "claude", Model: "claude-opus-4-5", Enabled: true},
		{Name: "quality", Backend: "copilot", Model: "gpt-5", Enabled: true},
	}})
	name, display, backend, model, ok := loaded.(Agents).SelectedAgentDetail()
	if !ok {
		t.Fatal("a loaded pane reports no selected agent")
	}
	if name != "scanner" || display != "Fleet Scanner" || backend != "claude" || model != "claude-opus-4-5" {
		t.Errorf("SelectedAgentDetail() = (%q, %q, %q, %q), want the first row", name, display, backend, model)
	}

	moved, _ := loaded.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	name, display, backend, _, _ = moved.(Agents).SelectedAgentDetail()
	if name != "quality" || backend != "copilot" {
		t.Errorf("after j, SelectedAgentDetail() = (%q, %q), want the second row", name, backend)
	}
	// client.Agent marshals displayName with omitempty, so the pane falls back
	// to Name exactly as the handler does.
	if display != "quality" {
		t.Errorf("display = %q, want a fallback to the agent's name", display)
	}
}

// TestSetAgentModelAppliesTheAuthoritativeResponse: the write's response is
// applied to the visible row immediately, so a successful change is not left
// showing the old model for a poll interval.
func TestSetAgentModelAppliesTheAuthoritativeResponse(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner")}})
	p := loaded.(Agents)

	updated := p.SetAgentModel("scanner", "claude-sonnet-4-5")
	if _, _, _, model, _ := updated.SelectedAgentDetail(); model != "claude-sonnet-4-5" {
		t.Errorf("row model = %q, want the applied model", model)
	}
	if !strings.Contains(updated.View(60, 10), "claude-sonnet") {
		t.Errorf("the rendered row does not show the applied model:\n%s", updated.View(60, 10))
	}
	// The original value is untouched: Update's rule is that the input model
	// is never mutated, and the roster is a slice shared by value.
	if _, _, _, model, _ := p.SelectedAgentDetail(); model != "claude-opus-4-5" {
		t.Errorf("SetAgentModel mutated the receiver's roster; model = %q", model)
	}
}

// TestSetAgentModelIgnoresUnknownAgentsAndEmptyModels. A response naming an
// agent the roster does not list is stale or wrong, and inventing a row from
// it would show an agent /api/agents does not. An empty model would blank the
// column with no fact behind it.
func TestSetAgentModelIgnoresUnknownAgentsAndEmptyModels(t *testing.T) {
	loaded, _ := NewAgents().Update(AgentsMsg{Agents: []client.Agent{agent("scanner")}})
	p := loaded.(Agents)

	for _, tc := range []struct{ name, model string }{
		{"ghost", "claude-sonnet-4-5"},
		{"scanner", ""},
	} {
		got := p.SetAgentModel(tc.name, tc.model)
		if _, _, _, model, _ := got.SelectedAgentDetail(); model != "claude-opus-4-5" {
			t.Errorf("SetAgentModel(%q, %q) changed the row to %q", tc.name, tc.model, model)
		}
		if view := got.View(60, 10); !strings.Contains(view, "claude-opus") {
			t.Errorf("SetAgentModel(%q, %q) corrupted the row:\n%s", tc.name, tc.model, view)
		}
	}
}
