package tui

import (
	"encoding/json"
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

var acmmKey = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("A")}

// acmmPacksJSON is the pack list the fixture server answers GET /api/packs
// with. It is a bare ARRAY — the endpoint's actual shape, per client.ACMM —
// with the current level carried as the `current` flag rather than as a field
// of its own.
const acmmPacksJSON = `[
  {"level":2,"name":"Assisted","description":"Agents propose, humans merge.","agentCount":4,
   "governor":{"modes":"advisory","mergePolicy":"human"},"current":false},
  {"level":4,"name":"Supervised","description":"Agents merge green PRs.","agentCount":9,
   "governor":{"modes":"supervised","mergePolicy":"lgtm"},"current":true},
  {"level":5,"name":"Autonomous","description":"Agents own the merge path.","agentCount":12,
   "governor":{"modes":"autonomous","mergePolicy":"auto"},"current":false}
]`

// acmmStatus is the same list as a decoded value, for tests that inject the
// response rather than serving it.
func acmmStatus(t *testing.T) client.ACMMStatus {
	t.Helper()
	var packs []client.Pack
	if err := json.Unmarshal([]byte(acmmPacksJSON), &packs); err != nil {
		t.Fatalf("acmmPacksJSON does not decode: %v", err)
	}
	status := client.ACMMStatus{Packs: packs}
	if current, ok := status.Current(); ok {
		status.Level = current.Level
	}
	return status
}

// acmmTestModel is a sized model with a fleet snapshot already delivered, so
// the tests below can assert that a key leaking out of the overlay would have
// found an agent to act on.
func acmmTestModel(t *testing.T) model {
	t.Helper()
	m := newModel()
	m.width, m.height = 100, 30
	next, _ := m.Update(panes.AgentsMsg{Agents: []client.Agent{
		{Name: "scanner", DisplayName: "Scanner", Enabled: true, Backend: "claude", Model: "claude-opus-4-5"},
	}})
	return next.(model)
}

// openACMM opens the overlay and delivers the pack list, leaving the model in
// the state an operator navigates.
func openACMM(t *testing.T) model {
	t.Helper()
	m := acmmTestModel(t)
	next, _ := m.Update(acmmKey)
	m = next.(model)
	if m.acmm == nil {
		t.Fatal("A did not open the ACMM overlay")
	}
	next, _ = m.Update(acmmPacksMsg{overlayID: m.acmmID, status: acmmStatus(t)})
	m = next.(model)
	if m.acmm.Loading() {
		t.Fatal("the pack list did not reach the overlay")
	}
	return m
}

// typeACMM delivers text to the overlay one rune per key press, the way a
// terminal does. Typing the phrase as a single multi-rune KeyMsg would not
// exercise what an operator actually produces.
func typeACMM(t *testing.T, m model, text string) model {
	t.Helper()
	for _, r := range text {
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
		if cmd != nil {
			t.Fatalf("typing %q issued a command", string(r))
		}
	}
	return m
}

// TestACMMKeyIsGlobal. Unlike p/m/K/a, the ACMM level is a property of the hive
// rather than of a selected agent, so `A` must open from ANY pane and before
// any fleet snapshot has arrived.
func TestACMMKeyIsGlobal(t *testing.T) {
	t.Run("with no fleet snapshot", func(t *testing.T) {
		m := newModel()
		m.width, m.height = 100, 30
		next, cmd := m.Update(acmmKey)
		if next.(model).acmm == nil {
			t.Error("A did not open the overlay before the first fleet snapshot")
		}
		if cmd == nil {
			t.Error("A did not issue the pack list request")
		}
	})

	for focus := 0; focus < paneCount; focus++ {
		t.Run(fmt.Sprintf("focus=%d", focus), func(t *testing.T) {
			m := acmmTestModel(t)
			m.focus = focus
			next, cmd := m.Update(acmmKey)
			got := next.(model)
			if got.acmm == nil {
				t.Errorf("A did not open the overlay with pane %d focused", focus)
			}
			if cmd == nil {
				t.Errorf("A did not issue the pack list request with pane %d focused", focus)
			}
			if got.focus != focus {
				t.Errorf("A moved focus from %d to %d", focus, got.focus)
			}
		})
	}
}

// TestACMMOverlayFetchesPacks drives the real client against a fixture server
// and asserts both the request the overlay makes and that the response reaches
// it — including the current-level marker, which is what an operator reads
// before choosing anything.
func TestACMMOverlayFetchesPacks(t *testing.T) {
	var requested atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.Method + " " + r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, acmmPacksJSON)
	}))
	t.Cleanup(server.Close)
	pinDashboard(t, server.URL)

	m := acmmTestModel(t)
	m.api = client.New()
	next, cmd := m.Update(acmmKey)
	m = runCmd(t, next.(model), cmd)

	if got, _ := requested.Load().(string); got != "GET /api/packs" {
		t.Errorf("overlay requested %q, want %q", got, "GET /api/packs")
	}
	if m.acmm == nil || m.acmm.Loading() {
		t.Fatal("the pack list did not reach the overlay")
	}
	view := m.View()
	for _, want := range []string{"current L4", "Assisted", "Supervised", "Autonomous", "(current)"} {
		if !strings.Contains(view, want) {
			t.Errorf("the frame omits %q", want)
		}
	}
}

// TestACMMOverlayConsumesEveryGlobalBinding is the acceptance criterion that
// the overlay owns all input. It runs in three states — loading, confirming and
// pending — because they are three different branches of updateACMM and a leak
// in any one of them is a fleet-wide action fired by a key the operator thought
// was text.
func TestACMMOverlayConsumesEveryGlobalBinding(t *testing.T) {
	keys := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyCtrlC},
		{Type: tea.KeyTab},
		{Type: tea.KeyShiftTab},
		{Type: tea.KeyRunes, Runes: []rune("p")},
		{Type: tea.KeyRunes, Runes: []rune("m")},
		{Type: tea.KeyRunes, Runes: []rune("K")},
		{Type: tea.KeyRunes, Runes: []rune("a")},
		{Type: tea.KeyRunes, Runes: []rune("A")},
		{Type: tea.KeyRunes, Runes: []rune("?")},
		{Type: tea.KeyRunes, Runes: []rune("y")},
		{Type: tea.KeyRunes, Runes: []rune("n")},
	}

	states := map[string]func(*testing.T) model{
		"loading": func(t *testing.T) model {
			m := acmmTestModel(t)
			next, _ := m.Update(acmmKey)
			return next.(model)
		},
		"list": openACMM,
		"confirming": func(t *testing.T) model {
			m := openACMM(t)
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
			next, _ = next.(model).Update(enterKey)
			m = next.(model)
			if !m.acmm.Confirming() {
				t.Fatal("the confirmation did not open")
			}
			return m
		},
	}

	for state, build := range states {
		for _, key := range keys {
			t.Run(state+"/"+key.String(), func(t *testing.T) {
				m := build(t)
				openFocus := m.focus
				openID := m.acmmID

				next, cmd := m.Update(key)
				got := next.(model)

				if got.acmm == nil {
					t.Fatalf("%s closed the ACMM overlay; only esc may", key.String())
				}
				if cmd != nil {
					t.Errorf("%s produced a command while the overlay was modal", key.String())
				}
				if got.acmmID != openID {
					t.Errorf("%s reopened the overlay behind itself", key.String())
				}
				if got.focus != openFocus {
					t.Errorf("%s moved pane focus behind the modal", key.String())
				}
				if got.confirm != nil {
					t.Errorf("%s opened the pause dialog behind the modal", key.String())
				}
				if got.picker != nil {
					t.Errorf("%s opened the model picker behind the modal", key.String())
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
}

// TestACMMConfirmationTreatsLettersAsText is the other half of the consumption
// rule, and the half a "did it leak?" assertion cannot see: while the
// confirmation is open the letters of the phrase must land IN it. `A`, `p` and
// `a` all appear in "APPLY L5", so a branch that swallowed them to be safe
// would leave an operator unable to type the phrase at all.
func TestACMMConfirmationTreatsLettersAsText(t *testing.T) {
	m := openACMM(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	next, _ = next.(model).Update(enterKey)
	m = next.(model)

	m = typeACMM(t, m, panes.ConfirmPhrase(5))
	if got := m.acmm.Typed(); got != panes.ConfirmPhrase(5) {
		t.Fatalf("Typed() = %q, want %q — the phrase's own letters did not reach the field", got, panes.ConfirmPhrase(5))
	}

	// j and k are navigation over the LIST and text inside the confirmation.
	before, _ := m.acmm.SelectedPack()
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)
	if after, _ := m.acmm.SelectedPack(); after.Level != before.Level {
		t.Errorf("j moved the cursor from L%d to L%d during a confirmation", before.Level, after.Level)
	}
	if got := m.acmm.Typed(); got != panes.ConfirmPhrase(5)+"j" {
		t.Errorf("Typed() = %q; j was not treated as text inside the confirmation", got)
	}

	// Backspace is an edit, not a leak.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = next.(model)
	if got := m.acmm.Typed(); got != panes.ConfirmPhrase(5) {
		t.Errorf("Typed() = %q after backspace, want %q", got, panes.ConfirmPhrase(5))
	}
}

// TestACMMNonRuneKeysAreNotTyped. A function or control key cannot appear in
// the phrase, so letting one accumulate invisibly would leave the operator
// staring at a field that looks right and will not match.
func TestACMMNonRuneKeysAreNotTyped(t *testing.T) {
	m := openACMM(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	next, _ = next.(model).Update(enterKey)
	m = next.(model)

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyF5},
		{Type: tea.KeyCtrlW},
		{Type: tea.KeyLeft},
		{Type: tea.KeyRight},
		{Type: tea.KeyHome},
		{Type: tea.KeyRunes, Runes: []rune("r"), Alt: true},
	} {
		next, cmd := m.Update(key)
		m = next.(model)
		if cmd != nil {
			t.Errorf("%s issued a command from inside the confirmation", key.String())
		}
		if got := m.acmm.Typed(); got != "" {
			t.Fatalf("%s put %q in the confirmation field", key.String(), got)
		}
	}
}

// TestACMMEnterOnANonCurrentLevelDoesNotApply is the safety property: the first
// enter opens a confirmation and issues NOTHING.
func TestACMMEnterOnANonCurrentLevelDoesNotApply(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, acmmPacksJSON)
	}))
	t.Cleanup(server.Close)
	pinDashboard(t, server.URL)

	m := acmmTestModel(t)
	m.api = client.New()
	next, cmd := m.Update(acmmKey)
	m = runCmd(t, next.(model), cmd)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)
	next, applyCmd := m.Update(enterKey)
	m = next.(model)

	if applyCmd != nil {
		t.Fatal("the first enter issued a request; enter must only open the confirmation")
	}
	if !m.acmm.Confirming() {
		t.Fatal("the first enter did not open the confirmation")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("PUT requests = %d, want 0 — enter applied a level without confirmation", got)
	}
	view := m.View()
	if !strings.Contains(view, panes.ConfirmPhrase(5)) {
		t.Errorf("the frame does not name the phrase to type:\n%s", view)
	}
}

// TestACMMEnterOnTheCurrentLevelIsAnExplicitNoOp.
func TestACMMEnterOnTheCurrentLevelIsAnExplicitNoOp(t *testing.T) {
	m := openACMM(t)
	// The cursor opens on the level in force.
	if pack, _ := m.acmm.SelectedPack(); pack.Level != 4 {
		t.Fatalf("the cursor opened on L%d, want the current L4", pack.Level)
	}
	next, cmd := m.Update(enterKey)
	m = next.(model)
	if cmd != nil {
		t.Error("enter on the current level issued a request")
	}
	if m.acmm.Confirming() {
		t.Error("enter on the current level opened a confirmation")
	}
	if !strings.Contains(m.View(), "already the level in force") {
		t.Error("the frame does not explain why nothing happened")
	}
}

// TestACMMIncompleteConfirmationCannotApply walks the phrase one rune at a time
// and presses enter after every one. Only the last press may produce a command.
func TestACMMIncompleteConfirmationCannotApply(t *testing.T) {
	m := openACMM(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	next, _ = next.(model).Update(enterKey)
	m = next.(model)

	phrase := panes.ConfirmPhrase(5)
	for i, r := range phrase {
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
		if cmd != nil {
			t.Fatalf("typing produced a command at rune %d", i)
		}
		next, cmd = m.Update(enterKey)
		m = next.(model)
		complete := i == len(phrase)-1
		if complete && cmd == nil {
			t.Fatalf("the complete phrase %q did not apply", phrase)
		}
		if !complete && cmd != nil {
			t.Fatalf("the incomplete phrase %q applied", phrase[:i+1])
		}
	}
}

// TestACMMExactPhraseIssuesExactlyOnePUTWhilePending is the acceptance
// criterion this whole modal exists to satisfy. Setting a level replaces the
// roster, pauses and resumes agents and rewrites the governor, so repeated
// enter presses before the first response must not reconcile the fleet twice.
//
// The server BLOCKS so the request is genuinely in flight while the extra
// presses are delivered — the same approach T17 used for the model set.
func TestACMMExactPhraseIssuesExactlyOnePUTWhilePending(t *testing.T) {
	var puts atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/packs":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, acmmPacksJSON)
		case r.Method == http.MethodPut && r.URL.Path == "/api/packs/level":
			puts.Add(1)
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"ok":true,"level":5,"packAgents":["scanner"],"packUpdated":[],"paused":[],"resumed":["scanner"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	pinDashboard(t, server.URL)

	m := acmmTestModel(t)
	m.api = client.New()
	next, cmd := m.Update(acmmKey)
	m = runCmd(t, next.(model), cmd)
	if m.acmm == nil || m.acmm.Loading() {
		t.Fatal("the pack list did not load")
	}

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	next, _ = next.(model).Update(enterKey)
	m = typeACMM(t, next.(model), panes.ConfirmPhrase(5))

	next, applyCmd := m.Update(enterKey)
	m = next.(model)
	if applyCmd == nil {
		t.Fatal("the exact phrase did not issue a PUT")
	}
	if !m.acmm.Pending() {
		t.Fatal("the accepted confirmation did not put the overlay in a pending state")
	}
	// Run the blocking request the way bubbletea would: on its own goroutine,
	// so the model keeps processing keys while it is in flight.
	done := make(chan tea.Msg, 1)
	go func() { done <- applyCmd() }()

	for i := 0; i < 5; i++ {
		next, extra := m.Update(enterKey)
		m = next.(model)
		if extra != nil {
			t.Fatalf("enter press %d issued a second PUT while one was pending", i+2)
		}
		if m.acmm == nil {
			t.Fatalf("enter press %d closed the pending overlay", i+2)
		}
	}
	// esc must not escape a pending request either: the reconciliation is
	// already under way and hiding it would leave the operator with no result.
	next, escCmd := m.Update(escKey)
	m = next.(model)
	if m.acmm == nil {
		t.Error("esc closed the overlay while a fleet-wide write was in flight")
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
		t.Fatal("the apply never answered")
	}
	if got := puts.Load(); got != 1 {
		t.Fatalf("PUT requests = %d, want exactly 1 — the fleet was reconciled %d times", got, got)
	}
}

// TestACMMSuccessRendersTheReceiptAndRefreshes. The overlay stays open holding
// the full reconciliation so it is read rather than flashing past, and the poll
// is re-issued because the roster, every agent's run state and the governor's
// configuration have all just been rewritten.
func TestACMMSuccessRendersTheReceiptAndRefreshes(t *testing.T) {
	m := openACMM(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	next, _ = next.(model).Update(enterKey)
	m = typeACMM(t, next.(model), panes.ConfirmPhrase(5))
	next, _ = m.Update(enterKey)
	m = next.(model)

	next, cmd := m.Update(acmmApplyMsg{
		overlayID: m.acmmID,
		level:     5,
		result: client.ACMMLevelResult{
			OK: true, Level: 5,
			PackAgents:  []string{"scanner", "reviewer"},
			PackUpdated: []string{"reviewer"},
			Paused:      []string{"outreach"},
			Resumed:     nil,
			GovernorChanges: &client.GovernorChanges{
				EvalIntervalS: &client.GovernorIntervalChange{From: 300, To: 120},
			},
		},
	})
	m = next.(model)

	if cmd == nil {
		t.Error("a successful apply did not refresh; the panes still describe the previous level")
	}
	if m.acmm == nil {
		t.Fatal("the overlay closed before the receipt could be read")
	}
	if !m.acmm.Done() {
		t.Fatal("the overlay is not showing a receipt")
	}
	view := m.View()
	for _, want := range []string{
		"Level is now L5",
		"scanner", "reviewer", // roster
		"outreach",   // paused
		"none",       // resumed, empty
		"300", "120", // governor
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the receipt frame omits %q", want)
		}
	}
	if !strings.Contains(m.footerStatus, "L5") {
		t.Errorf("footerStatus = %q, want the new level", m.footerStatus)
	}

	// Dismissal is the operator's, and only esc/enter do it.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if next.(model).acmm == nil {
		t.Error("q dismissed the receipt; it must be consumed like every other key")
	}
	next, cmd = m.Update(escKey)
	if next.(model).acmm != nil {
		t.Error("esc did not dismiss the receipt")
	}
	if cmd != nil {
		t.Error("dismissing the receipt issued a command")
	}
}

// TestACMMEnterOnAReceiptCannotReapply. The receipt is reached by pressing
// enter, and enter also dismisses it — so the state must have cleared its
// confirmation, or a double-tap would apply the level a second time.
func TestACMMEnterOnAReceiptCannotReapply(t *testing.T) {
	m := openACMM(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	next, _ = next.(model).Update(enterKey)
	m = typeACMM(t, next.(model), panes.ConfirmPhrase(5))
	next, _ = m.Update(enterKey)
	m = next.(model)
	next, _ = m.Update(acmmApplyMsg{overlayID: m.acmmID, level: 5,
		result: client.ACMMLevelResult{OK: true, Level: 5}})
	m = next.(model)

	next, cmd := m.Update(enterKey)
	if cmd != nil {
		t.Fatal("enter on the receipt issued a second apply")
	}
	if next.(model).acmm != nil {
		t.Error("enter did not dismiss the receipt")
	}
}

// TestACMMApplyFailures is the three-way distinction the acceptance criteria
// demand, asserted at the app layer because the REFRESH decision differs
// between them and lives here.
func TestACMMApplyFailures(t *testing.T) {
	apply := func(t *testing.T, err error) (model, tea.Cmd) {
		t.Helper()
		m := openACMM(t)
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		next, _ = next.(model).Update(enterKey)
		m = typeACMM(t, next.(model), panes.ConfirmPhrase(5))
		next, _ = m.Update(enterKey)
		m = next.(model)
		next, cmd := m.Update(acmmApplyMsg{overlayID: m.acmmID, level: 5, err: err})
		return next.(model), cmd
	}

	t.Run("forbidden", func(t *testing.T) {
		m, cmd := apply(t, &client.APIError{StatusCode: http.StatusForbidden,
			Method: http.MethodPut, Path: "/api/packs/level"})
		if m.acmm == nil {
			t.Fatal("a failed apply closed the overlay")
		}
		if !strings.Contains(m.View(), "owner access required") {
			t.Error("the 403 does not name the access required")
		}
		if cmd != nil {
			t.Error("a clean 403 refresh: nothing changed, so nothing needs re-reading")
		}
	})

	t.Run("ordinary failure", func(t *testing.T) {
		m, cmd := apply(t, &client.APIError{StatusCode: http.StatusBadRequest,
			Method: http.MethodPut, Path: "/api/packs/level", Body: "level out of range"})
		if !strings.Contains(m.View(), "level out of range") {
			t.Error("the server's error was not preserved in the frame")
		}
		if m.acmm.PartiallyReconciled() {
			t.Error("a 400 was flagged as potentially partial")
		}
		if cmd != nil {
			t.Error("a 400 triggered a refresh; the request was rejected, nothing changed")
		}
	})
}

// TestACMM500WarnsAndRefreshes is the subtlest requirement in the task.
//
// PUT /api/packs/level answers 500 when the level was PERSISTED but the roster
// could not be reconciled to it. Every other failure means "nothing happened";
// this one means "something happened and we do not know how much". So it is the
// one failure that must ALSO refresh — the panes underneath may be describing a
// hive that has already moved, and leaving them alone would make the frame
// agree with the false reading that nothing changed.
func TestACMM500WarnsAndRefreshes(t *testing.T) {
	m := openACMM(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	next, _ = next.(model).Update(enterKey)
	m = typeACMM(t, next.(model), panes.ConfirmPhrase(5))
	next, _ = m.Update(enterKey)
	m = next.(model)

	next, cmd := m.Update(acmmApplyMsg{overlayID: m.acmmID, level: 5,
		err: &client.APIError{StatusCode: http.StatusInternalServerError,
			Method: http.MethodPut, Path: "/api/packs/level",
			Body: "pack applied but roster reconciliation failed"}})
	m = next.(model)

	if cmd == nil {
		t.Fatal("a 500 did not refresh; the level may already be set and the panes are stale")
	}
	if m.acmm == nil {
		t.Fatal("a 500 closed the overlay")
	}
	if !m.acmm.PartiallyReconciled() {
		t.Fatal("a 500 was not flagged as potentially partial")
	}
	view := m.View()
	if !strings.Contains(view, "PARTIALLY RECONCILED") {
		t.Errorf("the 500 frame does not warn about partial reconciliation:\n%s", view)
	}
	if !strings.Contains(view, "roster reconciliation failed") {
		t.Error("the server's own explanation was dropped")
	}
	for _, forbidden := range []string{"nothing changed", "was not applied"} {
		if strings.Contains(strings.ToLower(view), forbidden) {
			t.Errorf("the 500 frame claims %q, which may be false", forbidden)
		}
	}
}

// TestACMMIgnoresResponsesForSupersededOverlays. A close-and-reopen must not be
// populated — or worse, receipted — by a response belonging to the overlay
// before it. Same pickerID pattern T17 established.
func TestACMMIgnoresResponsesForSupersededOverlays(t *testing.T) {
	t.Run("stale pack list", func(t *testing.T) {
		m := acmmTestModel(t)
		next, _ := m.Update(acmmKey)
		m = next.(model)
		staleID := m.acmmID

		next, _ = m.Update(escKey)
		next, _ = next.(model).Update(acmmKey)
		m = next.(model)
		if m.acmmID == staleID {
			t.Fatal("reopening the overlay reused the previous id")
		}

		next, _ = m.Update(acmmPacksMsg{overlayID: staleID, status: acmmStatus(t)})
		if !next.(model).acmm.Loading() {
			t.Error("a pack list for a superseded overlay populated the new one")
		}
	})

	t.Run("stale apply result", func(t *testing.T) {
		m := openACMM(t)
		staleID := m.acmmID
		next, _ := m.Update(escKey)
		next, _ = next.(model).Update(acmmKey)
		m = next.(model)

		next, _ = m.Update(acmmApplyMsg{overlayID: staleID, level: 5,
			result: client.ACMMLevelResult{OK: true, Level: 5}})
		if next.(model).acmm.Done() {
			t.Error("an apply result for a superseded overlay produced a receipt in the new one")
		}
	})

	t.Run("a result for a closed overlay still refreshes", func(t *testing.T) {
		// The write HAPPENED. Closing the overlay does not un-apply it, so the
		// panes are stale whether or not anything is left to render it into.
		m := openACMM(t)
		staleID := m.acmmID
		next, _ := m.Update(escKey)
		m = next.(model)
		next, cmd := m.Update(acmmApplyMsg{overlayID: staleID, level: 5,
			result: client.ACMMLevelResult{OK: true, Level: 5}})
		if cmd == nil {
			t.Error("a successful apply for a closed overlay did not refresh the panes")
		}
		if next.(model).acmm != nil {
			t.Error("a stale result reopened the overlay")
		}
	})
}

// TestACMMEscBacksOutOfTheConfirmationThenCloses. Two escapes, two different
// meanings: the first abandons a mistyped phrase without losing the list, the
// second closes the overlay.
func TestACMMEscBacksOutOfTheConfirmationThenCloses(t *testing.T) {
	m := openACMM(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	next, _ = next.(model).Update(enterKey)
	m = typeACMM(t, next.(model), "APPLY L")

	next, cmd := m.Update(escKey)
	m = next.(model)
	if cmd != nil {
		t.Error("esc issued a command")
	}
	if m.acmm == nil {
		t.Fatal("the first esc closed the whole overlay rather than the confirmation")
	}
	if m.acmm.Confirming() {
		t.Fatal("esc did not leave the confirmation")
	}
	if m.acmm.Typed() != "" {
		t.Errorf("esc left %q in the field", m.acmm.Typed())
	}

	next, cmd = m.Update(escKey)
	if next.(model).acmm != nil {
		t.Error("the second esc did not close the overlay")
	}
	if cmd != nil {
		t.Error("closing the overlay issued a command")
	}
}

// TestFooterAdvertisesACMM. The footer names only bindings that EXIST; `A`
// earns its place here because this task wired it.
func TestFooterAdvertisesACMM(t *testing.T) {
	if !strings.Contains(footerText, "A acmm") {
		t.Errorf("footer %q does not advertise the ACMM binding", footerText)
	}
	m := acmmTestModel(t)
	if !strings.Contains(m.View(), "A acmm") {
		t.Error("the rendered frame does not advertise the ACMM binding")
	}
}

// TestFooterStillFitsTheMinimumTerminalWithACMM re-runs T17's clipping guard
// with `A acmm` added. The strip grows by 7 columns here, which is exactly the
// kind of change that broke the frame when `m model` was added: lipgloss's
// Width() WRAPS rather than truncates, so an over-long footer becomes a second
// line and pushes the frame one row past the terminal's height.
//
// The clipping is done in app.go's View (MaxWidth on an Inline style, applied
// BEFORE Width pads); this asserts it still holds at the longer strip rather
// than re-implementing it.
func TestFooterStillFitsTheMinimumTerminalWithACMM(t *testing.T) {
	if len(footerText) <= minWidth {
		t.Skipf("footer is %d columns, still within the %d-column minimum", len(footerText), minWidth)
	}
	m := acmmTestModel(t)
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

// TestACMMOverlayFitsTheMinimumTerminal: the overlay itself must not grow the
// frame either, at the smallest size the grid is drawn in.
func TestACMMOverlayFitsTheMinimumTerminal(t *testing.T) {
	m := openACMM(t)
	m.width, m.height = minWidth, minHeight
	view := m.View()
	if got := lipgloss.Width(view); got != minWidth {
		t.Errorf("frame width = %d, want %d", got, minWidth)
	}
	if got := lipgloss.Height(view); got != minHeight {
		t.Errorf("frame height = %d, want %d", got, minHeight)
	}
}
