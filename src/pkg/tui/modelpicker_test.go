package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

var (
	modelKey = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")}
	enterKey = tea.KeyMsg{Type: tea.KeyEnter}
	escKey   = tea.KeyMsg{Type: tea.KeyEsc}
)

// runCmd runs a command to completion and feeds its message back into the
// model, which is what bubbletea's runtime does. Returning the model lets a
// test drive open → fetch → respond without a program or a terminal.
func runCmd(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if msg == nil {
		return m
	}
	next, _ := m.Update(msg)
	return next.(model)
}

func modelPickerModel(t *testing.T, agents ...client.Agent) model {
	t.Helper()
	if len(agents) == 0 {
		agents = []client.Agent{{Name: "scanner", DisplayName: "Scanner", Enabled: true, Backend: "claude", Model: "claude-opus-4-5"}}
	}
	m := newModel()
	m.width, m.height = 100, 30
	next, _ := m.Update(panes.AgentsMsg{Agents: agents})
	return next.(model)
}

// TestModelKeyOpensOnlyForASelectedAgentInTheFocusedPane. `m` addressed at a
// pane that has no agent — or at a different pane entirely — must be a no-op
// rather than an overlay pointed at nothing.
func TestModelKeyOpensOnlyForASelectedAgentInTheFocusedPane(t *testing.T) {
	t.Run("no fleet snapshot yet", func(t *testing.T) {
		m := newModel()
		m.width, m.height = 100, 30
		next, cmd := m.Update(modelKey)
		if next.(model).picker != nil {
			t.Error("m opened a picker before any agent existed to change")
		}
		if cmd != nil {
			t.Error("m issued a request with no agent selected")
		}
	})

	t.Run("another pane focused", func(t *testing.T) {
		m := modelPickerModel(t)
		m.focus = 1
		next, cmd := m.Update(modelKey)
		if next.(model).picker != nil {
			t.Error("m opened the picker while the Agents pane was not focused")
		}
		if cmd != nil {
			t.Error("m issued a catalogue request from an unfocused Agents pane")
		}
	})

	t.Run("agent with no backend", func(t *testing.T) {
		// Models() requires a backend; asking with an empty one would 404 on
		// the routing table and be reported as a broken catalogue instead of a
		// missing configuration.
		m := modelPickerModel(t, client.Agent{Name: "scanner", Enabled: true})
		next, cmd := m.Update(modelKey)
		got := next.(model)
		if got.picker != nil {
			t.Error("m opened a picker for an agent with no configured backend")
		}
		if cmd != nil {
			t.Error("m issued a catalogue request with an empty backend")
		}
		if !strings.Contains(got.footerStatus, "no configured backend") {
			t.Errorf("footer does not explain why nothing opened: %q", got.footerStatus)
		}
	})
}

// TestModelKeyRequestsTheSelectedAgentsBackend: the catalogue is
// backend-qualified, so opening the overlay for the second row must ask about
// THAT row's backend.
func TestModelKeyRequestsTheSelectedAgentsBackend(t *testing.T) {
	var requested atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"backend":"copilot","models":["gpt-5"]}`)
	}))
	t.Cleanup(server.Close)
	pinDashboard(t, server.URL)

	m := modelPickerModel(t,
		client.Agent{Name: "scanner", Enabled: true, Backend: "claude", Model: "claude-opus-4-5"},
		client.Agent{Name: "quality", Enabled: true, Backend: "copilot", Model: "gpt-5"},
	)
	m.api = client.New()
	// Move the selection to the second row through the pane's own binding.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)

	next, cmd := m.Update(modelKey)
	m = next.(model)
	if m.picker == nil {
		t.Fatal("m did not open the picker for a selected agent")
	}
	if got := m.picker.Agent(); got != "quality" {
		t.Errorf("picker agent = %q, want the selected row", got)
	}
	if got := m.picker.Backend(); got != "copilot" {
		t.Errorf("picker backend = %q, want the selected agent's backend", got)
	}
	m = runCmd(t, m, cmd)
	if got, _ := requested.Load().(string); got != "/api/inference/models/copilot" {
		t.Errorf("catalogue request path = %q, want the selected agent's backend", got)
	}
	if m.picker == nil || m.picker.Loading() {
		t.Error("the catalogue response did not reach the open overlay")
	}
}

// TestModelPickerConsumesEveryGlobalBinding is the containment guarantee.
// While the overlay is up, no key may reach quit, focus, pause, kick, attach
// or the ACMM binding underneath it — an operator scrolling a model list must
// not be able to quit the program or restart a different agent by typo.
func TestModelPickerConsumesEveryGlobalBinding(t *testing.T) {
	keys := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyCtrlC},
		{Type: tea.KeyTab},
		{Type: tea.KeyShiftTab},
		{Type: tea.KeyRunes, Runes: []rune("p")},
		{Type: tea.KeyRunes, Runes: []rune("K")},
		{Type: tea.KeyRunes, Runes: []rune("a")},
		{Type: tea.KeyRunes, Runes: []rune("A")},
		{Type: tea.KeyRunes, Runes: []rune("?")},
		{Type: tea.KeyRunes, Runes: []rune("y")},
		{Type: tea.KeyRunes, Runes: []rune("n")},
	}
	for _, key := range keys {
		t.Run(key.String(), func(t *testing.T) {
			m := modelPickerModel(t)
			next, _ := m.Update(modelKey)
			m = next.(model)
			if m.picker == nil {
				t.Fatal("the picker did not open")
			}
			openFocus := m.focus

			next, cmd := m.Update(key)
			got := next.(model)

			if got.picker == nil {
				t.Fatalf("%s closed the model picker; only esc may", key.String())
			}
			if cmd != nil {
				t.Errorf("%s produced a command while the picker was modal", key.String())
			}
			if got.focus != openFocus {
				t.Errorf("%s moved pane focus behind the modal", key.String())
			}
			if got.confirm != nil {
				t.Errorf("%s opened the pause dialog behind the modal", key.String())
			}
			if got.helpVisible {
				t.Errorf("%s opened help behind the modal", key.String())
			}
			if got.kickPending != "" {
				t.Errorf("%s queued a kick behind the modal", key.String())
			}
			if got.attachPending {
				t.Errorf("%s started an attach behind the modal", key.String())
			}
		})
	}
}

// TestModelPickerEscClosesWithoutRequest.
func TestModelPickerEscClosesWithoutRequest(t *testing.T) {
	m := modelPickerModel(t)
	next, _ := m.Update(modelKey)
	m = next.(model)
	next, cmd := m.Update(escKey)
	if next.(model).picker != nil {
		t.Error("esc did not close the picker")
	}
	if cmd != nil {
		t.Error("esc issued a request")
	}
}

// TestModelPickerEnterIssuesExactlyOneRequestWhilePending is the acceptance
// criterion this whole modal exists to satisfy: POST /api/model restarts the
// agent's session, so repeated enter presses before the first response must
// not restart it repeatedly.
func TestModelPickerEnterIssuesExactlyOneRequestWhilePending(t *testing.T) {
	var sets atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/inference/models/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"backend":"claude","models":["claude-opus-4-5","claude-sonnet-4-5"]}`)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/model/"):
			sets.Add(1)
			// Block so the request is genuinely in flight while the extra
			// enter presses are delivered.
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"status":"ok","agent":"scanner","model":"claude-sonnet-4-5"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	pinDashboard(t, server.URL)

	m := modelPickerModel(t)
	m.api = client.New()
	next, cmd := m.Update(modelKey)
	m = runCmd(t, next.(model), cmd)
	if m.picker == nil || m.picker.Loading() {
		t.Fatal("the catalogue did not load")
	}

	// Select the second model, then press enter repeatedly.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)

	next, applyCmd := m.Update(enterKey)
	m = next.(model)
	if applyCmd == nil {
		t.Fatal("the first enter did not issue a model-set request")
	}
	if !m.picker.Pending() {
		t.Fatal("the first enter did not put the overlay in a pending state")
	}
	// Issue the (blocking) request the way bubbletea would: on its own
	// goroutine, so the model can keep processing keys while it is in flight.
	done := make(chan tea.Msg, 1)
	go func() { done <- applyCmd() }()

	for i := 0; i < 5; i++ {
		next, extra := m.Update(enterKey)
		m = next.(model)
		if extra != nil {
			t.Fatalf("enter press %d issued a second model-set request while one was pending", i+2)
		}
		if m.picker == nil {
			t.Fatalf("enter press %d closed the pending overlay", i+2)
		}
	}
	// esc must not escape a pending request either: the restart is already
	// under way and hiding it would leave the operator with no result.
	next, escCmd := m.Update(escKey)
	m = next.(model)
	if m.picker == nil {
		t.Error("esc closed the overlay while a session-restarting request was in flight")
	}
	if escCmd != nil {
		t.Error("esc issued a command while pending")
	}

	release <- struct{}{}
	select {
	case msg := <-done:
		next, _ := m.Update(msg)
		m = next.(model)
	case <-time.After(finalWait):
		t.Fatal("the model-set request never answered")
	}
	if got := sets.Load(); got != 1 {
		t.Fatalf("model-set requests = %d, want exactly 1 — the agent restarted %d times", got, got)
	}
	if m.picker != nil {
		t.Error("a successful apply left the overlay open")
	}
}

// TestModelPickerSuccessUpdatesTheRowImmediately: the fleet poll is on a five
// second cadence, so a successful change that waited for it would leave the
// old model on screen for seconds after the agent had already restarted onto
// the new one.
func TestModelPickerSuccessUpdatesTheRowImmediately(t *testing.T) {
	m := modelPickerModel(t)
	next, _ := m.Update(modelKey)
	m = next.(model)
	next, _ = m.Update(modelListMsg{
		pickerID: m.pickerID,
		list:     client.ModelList{Backend: "claude", Models: []client.ModelOption{"claude-opus-4-5", "claude-sonnet-4-5"}},
	})
	m = next.(model)

	next, _ = m.Update(modelSetMsg{
		pickerID: m.pickerID,
		agent:    "scanner",
		model:    "claude-sonnet-4-5",
		result:   client.ModelSetResult{Status: "ok", Agent: "scanner", Model: "claude-sonnet-4-5"},
	})
	m = next.(model)

	if m.picker != nil {
		t.Error("a successful apply left the overlay open")
	}
	if !strings.Contains(m.View(), "claude-sonnet-4-5") {
		t.Error("the Agents row still shows the previous model after a successful change")
	}
	if !strings.Contains(m.footerStatus, "session restarted") {
		t.Errorf("the footer does not report the restart: %q", m.footerStatus)
	}
}

// TestModelPickerAppliesTheServersModelNotTheRequestedOne: the response is
// authoritative. A gateway that canonicalizes an alias must be reflected as it
// resolved it, not as the operator typed it.
func TestModelPickerAppliesTheServersModelNotTheRequestedOne(t *testing.T) {
	m := modelPickerModel(t)
	next, _ := m.Update(modelKey)
	m = next.(model)
	next, _ = m.Update(modelListMsg{pickerID: m.pickerID, list: client.ModelList{
		Backend: "claude", Models: []client.ModelOption{"opus"},
	}})
	m = next.(model)
	next, _ = m.Update(modelSetMsg{
		pickerID: m.pickerID,
		agent:    "scanner",
		model:    "opus",
		result:   client.ModelSetResult{Status: "ok", Agent: "scanner", Model: "claude-opus-4-5"},
	})
	m = next.(model)

	agents := m.panes[0].(panes.Agents)
	_, _, _, gotModel, ok := agents.SelectedAgentDetail()
	if !ok || gotModel != "claude-opus-4-5" {
		t.Errorf("row model = %q, want the server's canonical id", gotModel)
	}
}

// TestModelPickerFailurePreservesTheRowAndOffersRetry covers the failure
// classes the acceptance criteria names — 400 validation, 403, and a transport
// error — at the app level: the overlay stays open, the row is untouched, and
// the operator can retry or cancel.
func TestModelPickerFailurePreservesTheRowAndOffersRetry(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "400 validation",
			err:  &client.APIError{StatusCode: http.StatusBadRequest, Method: http.MethodPost, Path: "/api/model/scanner/bogus", Body: "unsupported model"},
			want: "unsupported model",
		},
		{
			name: "403 forbidden",
			err:  &client.APIError{StatusCode: http.StatusForbidden, Method: http.MethodPost, Path: "/api/model/scanner/claude-sonnet-4-5"},
			want: "owner access required",
		},
		{
			name: "transport error",
			err:  fmt.Errorf("dial tcp: connection refused"),
			want: "connection refused",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := modelPickerModel(t)
			next, _ := m.Update(modelKey)
			m = next.(model)
			next, _ = m.Update(modelListMsg{pickerID: m.pickerID, list: client.ModelList{
				Backend: "claude", Models: []client.ModelOption{"claude-opus-4-5", "claude-sonnet-4-5"},
			}})
			m = next.(model)
			next, _ = m.Update(enterKey)
			m = next.(model)

			next, _ = m.Update(modelSetMsg{pickerID: m.pickerID, agent: "scanner", model: "claude-opus-4-5", err: tc.err})
			m = next.(model)

			if m.picker == nil {
				t.Fatal("a failed apply closed the overlay, so the operator never sees why")
			}
			if m.picker.Pending() {
				t.Error("a failed apply left the overlay pending, so retry is impossible")
			}
			flatView := strings.Join(strings.Fields(m.View()), " ")
			if !strings.Contains(flatView, tc.want) {
				t.Errorf("the overlay does not report %q:\n%s", tc.want, m.View())
			}
			// The row keeps the model the agent still actually runs.
			agents := m.panes[0].(panes.Agents)
			_, _, _, gotModel, _ := agents.SelectedAgentDetail()
			if gotModel != "claude-opus-4-5" {
				t.Errorf("a failed change rewrote the row to %q", gotModel)
			}
			// Retry is accepted exactly once.
			next, retry := m.Update(enterKey)
			m = next.(model)
			if retry == nil {
				t.Fatal("enter after a failure did not retry")
			}
			if _, again := m.Update(enterKey); again != nil {
				t.Error("the retry is not single-shot")
			}
		})
	}
}

// TestModelPickerCatalogueStatesReachTheOverlay walks the catalogue outcomes
// through the app so the wiring — not only the pane — is covered for each.
func TestModelPickerCatalogueStatesReachTheOverlay(t *testing.T) {
	tests := []struct {
		name string
		msg  func(id uint64) modelListMsg
		want string
		// selectable is whether enter should have anything to apply.
		selectable bool
	}{
		{
			name: "success",
			msg: func(id uint64) modelListMsg {
				return modelListMsg{pickerID: id, list: client.ModelList{Backend: "claude", Models: []client.ModelOption{"claude-opus-4-5"}}}
			},
			want:       "claude-opus-4-5",
			selectable: true,
		},
		{
			name: "empty successful catalogue",
			msg: func(id uint64) modelListMsg {
				return modelListMsg{pickerID: id, list: client.ModelList{Backend: "claude"}}
			},
			want: "no models reported",
		},
		{
			name: "fallback",
			msg: func(id uint64) modelListMsg {
				return modelListMsg{pickerID: id, list: client.ModelList{Backend: "claude", Models: []client.ModelOption{"claude-opus-4-5"}, Fallback: true}}
			},
			want:       "Unverified",
			selectable: true,
		},
		{
			name: "partial",
			msg: func(id uint64) modelListMsg {
				return modelListMsg{pickerID: id, list: client.ModelList{Backend: "claude", Models: []client.ModelOption{"claude-opus-4-5"}, Partial: true}}
			},
			want:       "Incomplete",
			selectable: true,
		},
		{
			name: "404 is not an error",
			msg: func(id uint64) modelListMsg {
				return modelListMsg{pickerID: id, err: &client.APIError{StatusCode: http.StatusNotFound, Method: http.MethodGet, Path: "/api/inference/models/watsonx"}}
			},
			want: "no model catalogue for this backend",
		},
		{
			name: "403",
			msg: func(id uint64) modelListMsg {
				return modelListMsg{pickerID: id, err: &client.APIError{StatusCode: http.StatusForbidden, Method: http.MethodGet, Path: "/api/inference/models/claude"}}
			},
			want: "owner access required",
		},
		{
			name: "transport error",
			msg: func(id uint64) modelListMsg {
				return modelListMsg{pickerID: id, err: fmt.Errorf("dial tcp: connection refused")}
			},
			want: "Catalogue unavailable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := modelPickerModel(t)
			next, _ := m.Update(modelKey)
			m = next.(model)
			if !m.picker.Loading() {
				t.Fatal("the overlay did not open in a loading state")
			}
			if !strings.Contains(m.View(), "Loading models") {
				t.Errorf("the loading state is not visible:\n%s", m.View())
			}

			next, _ = m.Update(tc.msg(m.pickerID))
			m = next.(model)
			if m.picker == nil {
				t.Fatal("the catalogue response closed the overlay")
			}
			if m.picker.Loading() {
				t.Error("the overlay is still loading after a response")
			}
			flatView := strings.Join(strings.Fields(m.View()), " ")
			if !strings.Contains(flatView, tc.want) {
				t.Errorf("the overlay does not show %q:\n%s", tc.want, m.View())
			}
			// The restart warning is guidance BEFORE the key press, so it is
			// present in every state enter can be pressed from.
			if !strings.Contains(flatView, "Applying restarts the agent's session.") {
				t.Errorf("the overlay omits the restart warning:\n%s", m.View())
			}

			_, cmd := m.Update(enterKey)
			if tc.selectable && cmd == nil {
				t.Error("enter did not apply from a selectable catalogue")
			}
			if !tc.selectable && cmd != nil {
				t.Error("enter issued a request with nothing selectable")
			}
		})
	}
}

// TestModelPickerIgnoresResponsesForSupersededOverlays: an operator can close
// an overlay and open another before the first request answers. A late
// response must not populate — or close — the newer one.
func TestModelPickerIgnoresResponsesForSupersededOverlays(t *testing.T) {
	m := modelPickerModel(t)
	next, _ := m.Update(modelKey)
	m = next.(model)
	stale := m.pickerID

	next, _ = m.Update(escKey)
	m = next.(model)
	next, _ = m.Update(modelKey)
	m = next.(model)
	if m.pickerID == stale {
		t.Fatal("reopening the overlay reused the previous request identity")
	}

	next, _ = m.Update(modelListMsg{pickerID: stale, list: client.ModelList{
		Backend: "claude", Models: []client.ModelOption{"stale-model"},
	}})
	m = next.(model)
	if !m.picker.Loading() {
		t.Error("a superseded catalogue response populated the current overlay")
	}
	if strings.Contains(m.View(), "stale-model") {
		t.Error("a superseded catalogue reached the screen")
	}

	next, _ = m.Update(modelSetMsg{pickerID: stale, agent: "scanner", model: "stale-model",
		result: client.ModelSetResult{Status: "ok", Agent: "scanner", Model: "stale-model"}})
	m = next.(model)
	if m.picker == nil {
		t.Error("a superseded apply response closed the current overlay")
	}
}

// TestFooterAdvertisesModel is the honesty rule app.go's footerText documents:
// a binding is listed only once pressing it does something. It is asserted
// from the rendered frame rather than the constant, because the frame is what
// the operator reads.
func TestFooterAdvertisesModel(t *testing.T) {
	m := modelPickerModel(t)
	if !strings.Contains(m.View(), "m model") {
		t.Errorf("the footer does not advertise the now-wired m binding:\n%s", m.View())
	}
}

// TestFooterIsClippedNotWrappedAtTheMinimumWidth pins what adding `m model`
// broke. The strip is now 68 columns against a 60-column minimum terminal, and
// lipgloss's Width() WRAPS rather than truncates — so an over-long footer
// became a second line and pushed the frame one row past the terminal's
// height. Any future binding added to the strip will fail here rather than
// silently corrupting the frame on a small terminal.
func TestFooterIsClippedNotWrappedAtTheMinimumWidth(t *testing.T) {
	if len(footerText) <= minWidth {
		t.Skipf("footer is %d columns, still within the %d-column minimum; this test guards the case where it is not",
			len(footerText), minWidth)
	}
	m := modelPickerModel(t)
	m.width, m.height = minWidth, minHeight
	view := m.View()
	if got := lipgloss.Height(view); got != minHeight {
		t.Errorf("frame at %dx%d renders %d lines, want exactly %d — the footer wrapped",
			minWidth, minHeight, got, minHeight)
	}
	if got := lipgloss.Width(view); got != minWidth {
		t.Errorf("frame width = %d, want %d", got, minWidth)
	}
}
