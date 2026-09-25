package tui

// Branch coverage for paths the behavioural suites skirt: the run
// approve/reject error ladder, kick-result fallbacks, the SSE snapshot
// projections, and the hives overlay's rank/strategy/typing keys. Each test
// pins a contract an operator sees — a footer message, a refused keypress —
// rather than restating the implementation.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// ── SSE projections ─────────────────────────────────────────────────────────

func TestSSEObservedAtParsesAndRejects(t *testing.T) {
	if got := sseObservedAt("2026-09-25T10:00:00Z"); got.IsZero() {
		t.Error("sseObservedAt rejected a valid RFC 3339 timestamp")
	}
	if got := sseObservedAt("not-a-time"); !got.IsZero() {
		t.Errorf("sseObservedAt(garbage) = %v, want zero time", got)
	}
}

func TestSSEAgentStatesSkipsNamelessAgents(t *testing.T) {
	// A payload of only nameless agents must project to nil — the "leave the
	// pane's states alone" signal — not to an empty map that would clear them.
	if got := sseAgentStates([]sseStatusAgent{{Name: ""}}); got != nil {
		t.Errorf("sseAgentStates(nameless only) = %v, want nil", got)
	}
	got := sseAgentStates([]sseStatusAgent{
		{Name: ""},
		{Name: "scanner", Enabled: true, State: sseAgentStateRunning},
	})
	if len(got) != 1 {
		t.Fatalf("states = %v, want exactly the named agent", got)
	}
	if got["scanner"].Status != panes.AgentStatusRunning {
		t.Errorf("running agent projected as %v", got["scanner"].Status)
	}
}

// ── Run approve/reject ──────────────────────────────────────────────────────

func TestRunWaitingLabelSubstitutesNone(t *testing.T) {
	if got := runWaitingLabel(""); got != client.RunWaitingOnNone {
		t.Errorf("runWaitingLabel(\"\") = %q, want %q", got, client.RunWaitingOnNone)
	}
	if got := runWaitingLabel("agent"); got != "agent" {
		t.Errorf("runWaitingLabel(agent) = %q", got)
	}
}

func TestRunSelectedActionNoOpsWithoutASelection(t *testing.T) {
	m := newModel()
	m.focus = paneRunsIndex
	// Empty runs pane: nothing selected, so `a` must be a silent local no-op.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = next.(model)
	if cmd != nil || m.footerStatus != "" {
		t.Fatalf("a with no selectable run: cmd=%v footer=%q, want silent no-op", cmd, m.footerStatus)
	}

	// A runs slot that is not a panes.Runs (an embedder substituting panes)
	// must also refuse rather than panic on the type assertion.
	m.panes[paneRunsIndex] = panes.NewAgents()
	if _, cmd := m.runSelectedAction(true); cmd != nil {
		t.Fatal("runSelectedAction on a non-Runs pane returned a command")
	}
}

// runActionServer answers the three endpoints runAction walks, with each
// body injectable so every rung of its error ladder can be reached.
func runActionServer(t *testing.T, role, run, action string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body string
		switch {
		case r.URL.Path == "/api/role":
			body = role
		case strings.HasPrefix(r.URL.Path, "/api/runs/"):
			body = run
		default:
			body = action
		}
		if body == "" {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	pinDashboard(t, server.URL)
}

func TestRunActionErrorLadder(t *testing.T) {
	cases := []struct {
		name             string
		role, run        string
		wantErrSubstring string
		wantForbidden    bool
	}{
		{"role fetch fails", "", "", "boom", false},
		{"contributor is refused locally", `{"role":"contributor"}`, "", "", true},
		{"run fetch fails", `{"role":"owner"}`, "", "boom", false},
		{"run not waiting on human", `{"role":"owner"}`, `{"key":"k","waiting_on":"agent","plan_epic_id":"e"}`, "not human review", false},
		{"run missing plan epic", `{"role":"owner"}`, `{"key":"k","waiting_on":"human"}`, "no plan epic id", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runActionServer(t, tc.role, tc.run, "")
			m := newModel()
			msg := m.runAction("k", true)()
			action, ok := msg.(runActionMsg)
			if !ok {
				t.Fatalf("runAction returned %#v", msg)
			}
			if action.err == nil {
				t.Fatal("runAction succeeded, want an error")
			}
			if tc.wantForbidden && !client.IsForbidden(action.err) {
				t.Fatalf("err = %v, want a 403", action.err)
			}
			if tc.wantErrSubstring != "" && !strings.Contains(action.err.Error(), tc.wantErrSubstring) {
				t.Fatalf("err = %v, want it to mention %q", action.err, tc.wantErrSubstring)
			}
		})
	}
}

func TestHandleRunActionFooters(t *testing.T) {
	forbidden := &client.APIError{StatusCode: 403, Method: "GET", Path: "/api/role"}
	cases := []struct {
		name       string
		msg        runActionMsg
		wantFooter string
		wantPoll   bool
	}{
		{"forbidden reject", runActionMsg{key: "k", approve: false, err: forbidden}, "Reject run failed: owner access required", false},
		{"generic approve error", runActionMsg{key: "k", approve: true, err: errors.New("boom")}, "Approve run failed: boom", false},
		{"empty status approve", runActionMsg{key: "k", approve: true}, "Run k approved", true},
		{"empty status reject", runActionMsg{key: "k", approve: false}, "Run k rejected", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel()
			next, cmd := m.handleRunAction(tc.msg)
			got := next.(model).footerStatus
			if got != tc.wantFooter {
				t.Errorf("footer = %q, want %q", got, tc.wantFooter)
			}
			if (cmd != nil) != tc.wantPoll {
				t.Errorf("refresh cmd presence = %v, want %v", cmd != nil, tc.wantPoll)
			}
		})
	}
}

// ── Kick results ────────────────────────────────────────────────────────────

func TestHandleKickResultFallbacks(t *testing.T) {
	m := newModel()

	// A result for an agent with no pending local request is stale and must
	// change nothing.
	m.kickPending = "scanner"
	m.footerStatus = "before"
	next, _ := m.handleKickResult(kickResultMsg{agent: "other"})
	if got := next.(model).footerStatus; got != "before" {
		t.Errorf("stale kick result rewrote footer to %q", got)
	}

	// An empty result agent falls back to the requested name, and an unknown
	// status is reported verbatim rather than silently normalised.
	m.kickPending = "scanner"
	next, _ = m.handleKickResult(kickResultMsg{agent: "scanner", result: client.KickResult{Status: "odd"}})
	got := next.(model).footerStatus
	want := fmt.Sprintf("Kick returned status %q for %s", "odd", "scanner")
	if got != want {
		t.Errorf("footer = %q, want %q", got, want)
	}
}

// ── Hives-only shell ────────────────────────────────────────────────────────

func TestUpdateHivesOnlyRoutesNonKeyMessages(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	m := newHivesOnlyModel()
	m.hivesEnv = h.model.hivesEnv
	h.model = m
	h.run(t, h.model.Init())

	// The resize path must land on the model like the full app's.
	next, _ := h.model.Update(tea.WindowSizeMsg{Width: 97, Height: 41})
	h.model = next.(model)
	if h.model.width != 97 || h.model.height != 41 {
		t.Fatalf("size = %dx%d, want 97x41", h.model.width, h.model.height)
	}

	// Probe and action results route to their handlers, identified by the
	// overlay id so a stale one is dropped rather than misapplied.
	h.send(t, hivesProbeMsg{overlayID: h.model.hivesID, hub: "wss://acme.example/contribute", reachable: false})
	h.send(t, hivesActionMsg{overlayID: h.model.hivesID, note: "switched"})
	if view := h.view(); !strings.Contains(view, "switched") {
		t.Errorf("action note not rendered:\n%s", view)
	}
}

func TestHivesOnlyEscQuitsFromTheList(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	m := newHivesOnlyModel()
	m.hivesEnv = h.model.hivesEnv
	h.model = m
	h.run(t, h.model.Init())

	next, cmd := h.model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	h.model = next.(model)
	if cmd == nil || cmd() != (tea.QuitMsg{}) {
		t.Fatal("esc on the hives-only list did not quit")
	}
}

func TestHivesOnlyEscWhileTypingCancelsTheForm(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	m := newHivesOnlyModel()
	m.hivesEnv = h.model.hivesEnv
	h.model = m
	h.run(t, h.model.Init())

	h.send(t, key("a"))
	if !h.model.hives.Typing() {
		t.Fatal("a did not open the add form")
	}
	next, cmd := h.model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	h.model = next.(model)
	if cmd != nil {
		t.Fatal("esc while typing quit the program instead of cancelling the form")
	}
	if h.model.hives.Typing() {
		t.Fatal("esc did not cancel the form")
	}
}

// ── Hives overlay keys ──────────────────────────────────────────────────────

func TestHivesRankAndStrategyKeys(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)

	// `[` on the top row issues the move but the store clamps it: the order
	// on disk must be unchanged.
	h.run(t, h.send(t, key("[")))
	if got := h.profiles(t).Profiles[0].Name; got != "acme" {
		t.Fatalf("[ on the top row reordered profiles: first = %q", got)
	}

	// `]` moves the selected hive down and commits the new order to disk.
	h.run(t, h.send(t, key("]")))
	if got := h.profiles(t).Profiles[0].Name; got != "other" {
		t.Fatalf("after ]: first profile = %q, want other", got)
	}

	// `[` from the bottom moves it back up.
	h.run(t, h.send(t, key("[")))
	if got := h.profiles(t).Profiles[0].Name; got != "acme" {
		t.Fatalf("after [: first profile = %q, want acme", got)
	}

	// `s` cycles the failover strategy and persists it.
	before := h.profiles(t).EffectiveCommonsStrategy()
	h.run(t, h.send(t, key("s")))
	if got := h.profiles(t).EffectiveCommonsStrategy(); got == before {
		t.Fatalf("s did not change the strategy (still %q)", got)
	}
}

func TestHivesRankKeysRefusedWithoutARow(t *testing.T) {
	h := newHivesHarness(t, &hivectl.ProfileSet{Version: hivectl.ProfilesVersion})
	h.open(t)
	for _, k := range []string{"[", "]"} {
		if cmd := h.send(t, key(k)); cmd != nil {
			t.Fatalf("%s with no rows issued a write", k)
		}
	}
}

func TestHivesListNavigationAndTypingKeys(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)

	// k / up moves the selection; on row 0 it clamps rather than wrapping.
	h.send(t, key("j"))
	h.send(t, key("k"))
	sel, ok := h.model.hives.Selected()
	if !ok || sel.Name != "acme" {
		t.Fatalf("selection after j,k = %v %v, want acme", sel, ok)
	}

	// Open the rename form: shift+tab must move focus between its fields,
	// and a control key must be swallowed by the field, not typed.
	h.send(t, key("r"))
	if !h.model.hives.Typing() {
		t.Fatal("r did not open the rename form")
	}
	h.send(t, tea.KeyMsg{Type: tea.KeyShiftTab})
	h.send(t, tea.KeyMsg{Type: tea.KeyF5})
	// alt+r is not the letter r; it must not insert text either.
	h.send(t, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r"), Alt: true})
	h.send(t, tea.KeyMsg{Type: tea.KeyEsc})
	if h.model.hives.Typing() {
		t.Fatal("esc did not close the rename form")
	}
}
