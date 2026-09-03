package tui

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

var (
	pauseKey = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")}
	kickKey  = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("K")}
)

func modelWithAgent(paused bool) model {
	m := newModel()
	agent := client.Agent{Name: "scanner", Enabled: !paused, Backend: "claude", Model: "claude-opus-4-5"}
	next, _ := m.Update(panes.AgentsMsg{Agents: []client.Agent{agent}})
	return next.(model)
}

// TestPauseResumeConfirmationThroughTeatest drives the complete operator path
// through bubbletea and an httptest dashboard: p opens the right dialog, y
// sends the matching operation, and success starts an immediate fleet refresh.
func TestPauseResumeConfirmationThroughTeatest(t *testing.T) {
	tests := []struct {
		name          string
		initialPaused bool
		verb          string
		path          string
		status        string
		state         string
	}{
		{name: "pause running agent", verb: "Pause", path: "/api/pause/scanner", status: "paused", state: client.AgentStatePaused},
		{name: "resume paused agent", initialPaused: true, verb: "Resume", path: "/api/resume/scanner", status: "resumed", state: client.AgentStateRunning},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var acted atomic.Bool
			var refreshOnce sync.Once
			request := make(chan string, 1)
			refreshed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/agents":
					if acted.Load() {
						refreshOnce.Do(func() { close(refreshed) })
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `[{"name":"scanner","enabled":%t,"backend":"claude","model":"claude-opus-4-5"}]`, !tc.initialPaused)
				case r.Method == http.MethodPost && r.URL.Path == tc.path:
					request <- r.Method + " " + r.URL.Path
					acted.Store(true)
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"ok":true,"status":%q,"agent":"scanner","changed":true,"state":%q}`, tc.status, tc.state)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			pinDashboard(t, server.URL)

			tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(100, 30))
			teatest.WaitFor(t, tm.Output(), func(out []byte) bool {
				return strings.Contains(string(out), "scanner")
			}, teatest.WithDuration(finalWait))

			tm.Send(pauseKey)
			prompt := tc.verb + " agent scanner?"
			teatest.WaitFor(t, tm.Output(), func(out []byte) bool {
				return strings.Contains(string(out), prompt)
			}, teatest.WithDuration(finalWait))
			tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})

			select {
			case got := <-request:
				if want := http.MethodPost + " " + tc.path; got != want {
					t.Errorf("action request = %q, want %q", got, want)
				}
			case <-time.After(finalWait):
				t.Fatal("confirmed action did not reach the dashboard")
			}
			select {
			case <-refreshed:
			case <-time.After(finalWait):
				t.Fatal("successful action did not trigger an immediate fleet refresh")
			}

			tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
			tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
			final := tm.FinalModel(t).(model)
			if final.confirm != nil {
				t.Fatal("confirmation modal remained open after a successful action")
			}
			agents := final.panes[0].(panes.Agents)
			_, paused, ok := agents.SelectedAgent()
			if !ok || paused != (tc.state == client.AgentStatePaused) {
				t.Errorf("selected paused state = (%v, %v), want (%v, true)", paused, ok, tc.state == client.AgentStatePaused)
			}
		})
	}
}

func TestPauseConfirmationDismissesWithoutRequest(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("n")},
		{Type: tea.KeyEsc},
	} {
		m := modelWithAgent(false)
		next, _ := m.Update(pauseKey)
		shown := next.(model)
		if shown.confirm == nil {
			t.Fatal("p did not open the confirmation modal")
		}
		next, cmd := shown.Update(key)
		if cmd != nil {
			t.Errorf("dismiss key %q returned a command; no request should be made", key.String())
		}
		if next.(model).confirm != nil {
			t.Errorf("dismiss key %q left the modal open", key.String())
		}
	}
}

func TestPauseConfirmationSurfacesCallError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/pause/scanner" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"owner role required"}`))
	}))
	t.Cleanup(server.Close)
	pinDashboard(t, server.URL)

	m := modelWithAgent(false)
	m.width, m.height = 100, 30
	next, _ := m.Update(pauseKey)
	shown := next.(model)
	next, cmd := shown.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	pending := next.(model)
	msgs := drain(cmd)
	if len(msgs) != 1 {
		t.Fatalf("action command produced %d messages, want 1", len(msgs))
	}
	next, cmd = pending.Update(msgs[0])
	failed := next.(model)
	if cmd != nil {
		t.Fatal("failed action started a refresh command")
	}
	if failed.confirm == nil || failed.confirm.err != "Pause failed: owner access required" {
		t.Fatalf("failed modal = %+v, want friendly owner error", failed.confirm)
	}
	if !strings.Contains(failed.View(), "Pause failed: owner access required") {
		t.Fatal("call error is held in state but not rendered in the modal")
	}
}

func TestPauseKeyRequiresFocusedSelectedAgent(t *testing.T) {
	m := modelWithAgent(false)
	m.focus = 1
	next, cmd := m.Update(pauseKey)
	if cmd != nil || next.(model).confirm != nil {
		t.Fatal("p opened an agent action while another pane was focused")
	}

	empty := newModel()
	next, cmd = empty.Update(pauseKey)
	if cmd != nil || next.(model).confirm != nil {
		t.Fatal("p opened an agent action without a selected row")
	}
}

func TestPauseModalSwallowsOtherKeys(t *testing.T) {
	m := modelWithAgent(false)
	next, _ := m.Update(pauseKey)
	shown := next.(model)
	next, cmd := shown.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	got := next.(model)
	if cmd != nil {
		t.Fatal("q escaped the modal and returned a quit command")
	}
	if got.confirm == nil {
		t.Fatal("an unrelated key dismissed the pause modal")
	}
}

func TestPauseModalUpdateDoesNotMutateInputModel(t *testing.T) {
	m := modelWithAgent(false)
	next, _ := m.Update(pauseKey)
	shown := next.(model)
	_, _ = shown.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})

	if shown.confirm.pending || shown.confirm.actionID != 0 {
		t.Fatal("Update mutated the input model through shared confirmation state")
	}
}

func TestLateActionResultDoesNotCloseNewModal(t *testing.T) {
	m := modelWithAgent(false)
	m.confirm = &confirmState{agent: "scanner", pause: true, pending: true, actionID: 2}

	next, _ := m.Update(agentActionMsg{
		actionID: 1,
		agent:    "scanner",
		pause:    true,
		result: client.AgentActionResult{
			Agent: "scanner",
			State: client.AgentStatePaused,
		},
	})
	got := next.(model)
	if got.confirm == nil || got.confirm.actionID != 2 {
		t.Fatal("an older response closed the newer confirmation for the same agent")
	}
}

func TestPauseModalBoundsLongErrors(t *testing.T) {
	m := modelWithAgent(false)
	m.width, m.height = minWidth, minHeight
	m.confirm = &confirmState{
		agent: "scanner",
		pause: true,
		err:   "Pause failed: " + strings.Repeat("dashboard unavailable ", 20),
	}

	view := m.View()
	for i, line := range strings.Split(view, "\n") {
		if got := lipgloss.Width(line); got > minWidth {
			t.Fatalf("modal error line %d is %d cells wide, want <= %d", i, got, minWidth)
		}
	}
	if got := lipgloss.Height(view); got != minHeight {
		t.Fatalf("modal error frame is %d rows high, want %d", got, minHeight)
	}
}

// TestKickSelectedAgentThroughTeatest drives the complete T21 operator path:
// the roster supplies the selection, uppercase K runs a Bubble Tea command,
// and the dashboard receives exactly one bodyless kick request.
func TestKickSelectedAgentThroughTeatest(t *testing.T) {
	type observedRequest struct {
		method string
		path   string
		body   []byte
	}
	request := make(chan observedRequest, 1)
	var kickCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/agents":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"scanner","enabled":true,"backend":"claude","model":"claude-opus-4-5"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/kick/scanner":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read kick body: %v", err)
			}
			kickCount.Add(1)
			request <- observedRequest{method: r.Method, path: r.URL.Path, body: body}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"queued","agent":"scanner"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	pinDashboard(t, server.URL)

	tm := teatest.NewTestModel(t, newModel(), teatest.WithInitialTermSize(100, 30))
	teatest.WaitFor(t, tm.Output(), func(out []byte) bool {
		return strings.Contains(string(out), "scanner")
	}, teatest.WithDuration(finalWait))
	tm.Send(kickKey)

	select {
	case got := <-request:
		if got.method != http.MethodPost || got.path != "/api/kick/scanner" {
			t.Errorf("kick request = %s %s, want POST /api/kick/scanner", got.method, got.path)
		}
		if len(got.body) != 0 {
			t.Errorf("kick request body = %q, want no body", got.body)
		}
	case <-time.After(finalWait):
		t.Fatal("uppercase K did not reach the dashboard")
	}
	teatest.WaitFor(t, tm.Output(), func(out []byte) bool {
		return strings.Contains(string(out), "kick queued for scanner")
	}, teatest.WithDuration(finalWait))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))
	if got := kickCount.Load(); got != 1 {
		t.Errorf("kick request count = %d, want exactly 1", got)
	}
	if final := tm.FinalModel(t).(model); final.kickPending != "" {
		t.Errorf("kick remained pending for %q after the response", final.kickPending)
	}
}

func TestKickKeyRequiresFocusedSelectedAgent(t *testing.T) {
	m := modelWithAgent(false)
	m.focus = 1
	next, cmd := m.Update(kickKey)
	if cmd != nil || next.(model).kickPending != "" {
		t.Fatal("K queued a kick while another pane was focused")
	}

	empty := newModel()
	next, cmd = empty.Update(kickKey)
	if cmd != nil || next.(model).kickPending != "" {
		t.Fatal("K queued a kick before the roster supplied a selection")
	}

	loadedEmpty, _ := empty.Update(panes.AgentsMsg{Agents: []client.Agent{}})
	next, cmd = loadedEmpty.(model).Update(kickKey)
	if cmd != nil || next.(model).kickPending != "" {
		t.Fatal("K queued a kick from a loaded but empty roster")
	}
}

func TestLowercaseKRemainsAgentNavigation(t *testing.T) {
	m := newModel()
	next, _ := m.Update(panes.AgentsMsg{Agents: []client.Agent{
		{Name: "scanner", Enabled: true},
		{Name: "quality", Enabled: true},
	}})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)
	name, _, _ := m.panes[0].(panes.Agents).SelectedAgent()
	if name != "quality" {
		t.Fatalf("j selected %q, want quality", name)
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	got := next.(model)
	name, _, _ = got.panes[0].(panes.Agents).SelectedAgent()
	if name != "scanner" {
		t.Errorf("lowercase k selected %q, want scanner", name)
	}
	if cmd != nil || got.kickPending != "" {
		t.Fatal("lowercase k queued a kick instead of only navigating")
	}
}

func TestRepeatedKickKeyWhilePendingQueuesOneCommand(t *testing.T) {
	m := modelWithAgent(false)
	next, first := m.Update(kickKey)
	if first == nil {
		t.Fatal("first K did not return a kick command")
	}
	pending := next.(model)
	if pending.kickPending != "scanner" {
		t.Fatalf("kick pending for %q, want scanner", pending.kickPending)
	}

	next, second := pending.Update(kickKey)
	if second != nil {
		t.Fatal("repeated K returned a second command while the first was pending")
	}
	if got := next.(model).kickPending; got != "scanner" {
		t.Errorf("repeated K changed pending target to %q", got)
	}
}

func TestKickResultStatusText(t *testing.T) {
	apiFailure := &client.APIError{
		StatusCode: http.StatusBadGateway,
		Method:     http.MethodPost,
		Path:       "/api/kick/scanner",
		Body:       `{"error":"agent tmux session is unavailable"}`,
	}
	tests := []struct {
		name   string
		result client.KickResult
		err    error
		want   string
	}{
		{name: "queued", result: client.KickResult{Status: "queued", Agent: "scanner"}, want: "kick queued for scanner"},
		{name: "in flight", result: client.KickResult{Status: "in-flight", Agent: "scanner"}, want: "kick already in flight for scanner (request deduplicated)"},
		{name: "owner required", err: &client.APIError{StatusCode: http.StatusForbidden}, want: "Kick failed: owner access required"},
		{name: "API failure", err: apiFailure, want: "agent tmux session is unavailable"},
		{name: "transport failure", err: errors.New("dial tcp: connection refused"), want: "dial tcp: connection refused"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := modelWithAgent(false)
			m.width, m.height = 100, 30
			m.kickPending = "scanner"
			next, cmd := m.Update(kickResultMsg{agent: "scanner", result: tc.result, err: tc.err})
			got := next.(model)
			if cmd != nil {
				t.Fatal("kick result returned a command; status rendering must remain in the TUI")
			}
			if got.kickPending != "" {
				t.Errorf("kick remained pending for %q", got.kickPending)
			}
			if !strings.Contains(got.footerStatus, tc.want) {
				t.Errorf("kick status = %q, want it to contain %q", got.footerStatus, tc.want)
			}
			if !strings.Contains(got.View(), tc.want) {
				t.Errorf("footer does not contain %q", tc.want)
			}
		})
	}
}

func TestFooterAdvertisesKickBinding(t *testing.T) {
	if !strings.Contains(footerText, "K kick") {
		t.Errorf("footerText = %q, want it to advertise uppercase K kick", footerText)
	}
}
