package tui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// agentsFixture is the /api/agents body the poll tests serve. Three agents so
// a decoded result is distinguishable from a zero value at a glance.
const agentsFixture = `[
  {"name":"scanner","id":"agt_1","displayName":"Scanner","enabled":true,"managed":false,"backend":"claude","model":"claude-opus-4-5"},
  {"name":"quality","id":"agt_2","displayName":"Quality","enabled":true,"managed":true,"backend":"copilot","model":"gpt-5"},
  {"name":"reviewer","id":"agt_3","displayName":"Reviewer","enabled":false,"managed":false,"backend":"claude","model":"claude-sonnet-4-5"}
]`

// closedDashboard is an address nothing listens on, used by the tests that
// must not reach a dashboard at all.
//
// Every test in this package pins the client's environment, successful or not.
// The model builds its client from HIVE_DASHBOARD_URL, so a suite that left it
// alone would poll whatever the developer running the tests happens to have on
// localhost:3001 — which is exactly the machine a hive developer is working on.
// The design doc's testing convention is that no task needs a running Hive.
const closedDashboard = "http://127.0.0.1:1"

// TestMain pins the dashboard URL for EVERY test in this package.
//
// The model builds its client from the environment at construction, and T12 is
// the change that made the model start issuing requests on its own. Without
// this, running the suite on a developer's machine — the machine most likely to
// have a hive on localhost:3001 — would poll their real dashboard, and the
// frame-level tests would render whatever it returned. The design doc's testing
// convention is that no task requires a running Hive; this is what keeps that
// true now that the app polls. Tests that need a live server override it with
// t.Setenv, which restores this value afterwards.
func TestMain(m *testing.M) {
	if err := os.Setenv(client.BaseURLEnv, closedDashboard); err != nil {
		panic(err)
	}
	if err := os.Setenv(client.TokenEnv, "test-token"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// pinDashboard points the client at url for the duration of the test, and
// gives it a token so the request shape does not depend on the developer's
// own HIVE_DASHBOARD_TOKEN either.
func pinDashboard(t *testing.T, url string) {
	t.Helper()
	t.Setenv(client.BaseURLEnv, url)
	t.Setenv(client.TokenEnv, "test-token")
}

// pollTestModel is a model pointed at url with a poll interval short enough
// that a tick can be run to completion inside a test.
//
// The interval is what makes these tests fast without sleeping a real one: the
// AC asks for tick scheduling covered without waiting out the cadence, and the
// interval being a model field rather than a bare constant read is what allows
// it. It is deliberately not zero — a zero-duration tea.Tick is a hot loop, and
// a test that passed with one would hide exactly the bug
// TestNewModelPollsOnAnInterval guards.
func pollTestModel(t *testing.T, url string) model {
	t.Helper()
	pinDashboard(t, url)
	m := newModel()
	m.interval = time.Millisecond
	return m
}

// drain runs a tea.Cmd to completion and returns every message it produced,
// flattening tea.Batch.
//
// Batched commands run concurrently under bubbletea with no ordering
// guarantee, so callers assert on MEMBERSHIP, never on position. Running them
// sequentially here is safe precisely because nothing may depend on the order.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, c := range batch {
		out = append(out, drain(c)...)
	}
	return out
}

// runTick injects a tick into m and returns every message the resulting
// command produced. The tick is INJECTED, never waited for — that is the AC's
// "does not sleep real intervals", and it is also what makes these assertions
// deterministic rather than timing-dependent.
func runTick(m model) []tea.Msg {
	_, cmd := m.Update(tickMsg{})
	return drain(cmd)
}

// findAgentsMsg returns the delivered agent list, or nil if no AgentsMsg was
// produced.
func findAgentsMsg(msgs []tea.Msg) *panes.AgentsMsg {
	for _, m := range msgs {
		if a, ok := m.(panes.AgentsMsg); ok {
			return &a
		}
	}
	return nil
}

func hasTick(msgs []tea.Msg) bool {
	for _, m := range msgs {
		if _, ok := m.(tickMsg); ok {
			return true
		}
	}
	return false
}

func findFetchErr(msgs []tea.Msg) *fetchErrMsg {
	for _, m := range msgs {
		if e, ok := m.(fetchErrMsg); ok {
			return &e
		}
	}
	return nil
}

// agentsServer serves the fixture, or a 500 once fail is set. The counter lets
// a test assert that a second tick really issued a second request rather than
// replaying a cached result.
//
// It counts /api/agents ONLY. Since T13b the model also subscribes to
// /api/events at startup, and that request lands on this handler too — so a
// counter that counted every path would make "the poll fetched twice" and "the
// poll fetched once and the stream connected" indistinguishable, and would
// depend on when an asynchronous stream goroutine happened to dial.
type agentsServer struct {
	*httptest.Server
	fail     atomic.Bool
	requests atomic.Int64
}

func newAgentsServer(t *testing.T) *agentsServer {
	t.Helper()
	s := &agentsServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.requests.Add(1)
		if s.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(agentsFixture))
	}))
	t.Cleanup(s.Close)
	return s
}

// TestNewModelPollsOnAnInterval guards the cheapest catastrophic mistake in
// this file: an interval left at its zero value.
//
// tea.Tick(0, …) fires immediately and forever, so a model that forgot to set
// the field would spin the CPU and hammer the dashboard as fast as the network
// allows — while every other test in this package still passed, because they
// all override the interval themselves.
func TestNewModelPollsOnAnInterval(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m := newModel()

	if m.interval <= 0 {
		t.Fatalf("newModel().interval = %v, want a positive cadence (a zero tick is a hot loop)", m.interval)
	}
	if m.interval != pollInterval {
		t.Errorf("newModel().interval = %v, want pollInterval (%v)", m.interval, pollInterval)
	}
	if m.api == nil {
		t.Fatal("newModel() built no client; poll would nil-panic on the first tick")
	}
}

// TestInitPollsImmediatelyAndArmsTick pins that startup does not wait out a
// full interval before showing anything. Without the immediate fetch every
// pane would sit on "waiting for data" for five seconds against a perfectly
// healthy dashboard, which an operator reads as the TUI being broken.
func TestInitPollsImmediatelyAndArmsTick(t *testing.T) {
	server := newAgentsServer(t)
	m := pollTestModel(t, server.URL)

	msgs := drain(m.Init())

	got := findAgentsMsg(msgs)
	if got == nil {
		t.Fatalf("Init() issued no agents fetch; produced %d messages", len(msgs))
	}
	if len(got.Agents) != 3 {
		t.Errorf("Init() delivered %d agents, want 3", len(got.Agents))
	}
	if !hasTick(msgs) {
		t.Error("Init() armed no tick; the poll loop would never run a second time")
	}
	if n := server.requests.Load(); n != 1 {
		t.Errorf("Init() made %d requests, want exactly 1", n)
	}
}

// TestTickFetchesAndRearms is the AC's tick-scheduling test: the tick message
// is INJECTED rather than waited for, so nothing here sleeps an interval.
//
// One tick must do both things — issue the fetches and arm the next tick. A
// handler that only fetched would poll exactly once and then go quiet, and a
// static frame is indistinguishable from a live one showing unchanged data.
func TestTickFetchesAndRearms(t *testing.T) {
	server := newAgentsServer(t)
	m := pollTestModel(t, server.URL)

	next, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("a tick produced no command at all")
	}
	if _, ok := next.(model); !ok {
		t.Fatalf("a tick returned %T, want the root model", next)
	}

	msgs := drain(cmd)
	if got := findAgentsMsg(msgs); got == nil || len(got.Agents) != 3 {
		t.Errorf("a tick did not deliver the agent list: %+v", got)
	}
	if !hasTick(msgs) {
		t.Error("a tick did not arm the next one; the loop stops after one poll")
	}
}

// TestTickRearmsWhenTheFetchFails is the half of the error policy that keeps
// the loop alive. A dashboard that is down must not be able to stop the clock:
// if the re-arm were chained off a successful fetch, one 500 would end polling
// for the rest of the session and the TUI would never notice the dashboard
// coming back.
func TestTickRearmsWhenTheFetchFails(t *testing.T) {
	server := newAgentsServer(t)
	server.fail.Store(true)
	m := pollTestModel(t, server.URL)

	msgs := runTick(m)

	if !hasTick(msgs) {
		t.Fatal("a failed fetch stopped the tick loop")
	}
	if findFetchErr(msgs) == nil {
		t.Error("a 500 produced no fetchErrMsg")
	}
	if findAgentsMsg(msgs) != nil {
		t.Error("a failed fetch produced an AgentsMsg; panes must never see a zero-valued result")
	}
}

// TestFetchErrorLeavesPriorPaneDataIntact is the AC's error test, end to end
// through the real panes: poll successfully, then fail, and the frame must
// still show the fleet from the last good poll.
func TestFetchErrorLeavesPriorPaneDataIntact(t *testing.T) {
	server := newAgentsServer(t)
	m := pollTestModel(t, server.URL)

	sized, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = sized.(model)

	// First poll succeeds and reaches the panes.
	for _, msg := range runTick(m) {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	loaded := m.View()
	if !strings.Contains(loaded, "3 agents") {
		t.Fatalf("a successful poll did not reach the AGENTS pane:\n%s", loaded)
	}

	// Second poll fails.
	server.fail.Store(true)
	failed := runTick(m)
	if findFetchErr(failed) == nil {
		t.Fatal("the second poll did not fail as arranged")
	}
	for _, msg := range failed {
		next, _ := m.Update(msg)
		m = next.(model)
	}

	if got := m.View(); got != loaded {
		t.Errorf("a failed poll changed the frame; prior data must survive\nbefore:\n%s\nafter:\n%s", loaded, got)
	}
	if n := server.requests.Load(); n != 2 {
		t.Errorf("server saw %d requests, want 2 — the second tick must really re-fetch", n)
	}
}

// TestFetchErrMsgNeverReachesAPane pins the MECHANISM behind the test above.
//
// The previous test would also pass if the error reached the panes and every
// pane happened to ignore it — which is true of the stubs today and will stop
// being true as each pane grows. The contract is that the app swallows the
// error, so no pane ever has to decide what to do with one.
func TestFetchErrMsgNeverReachesAPane(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m, fakes := modelWithFakes()

	if _, cmd := m.Update(fetchErrMsg{source: "agents", err: http.ErrServerClosed}); cmd != nil {
		t.Error("a swallowed error produced a command")
	}
	for i, f := range fakes {
		if f.updates != 0 {
			t.Errorf("pane %d saw the fetch error; the app must swallow it", i)
		}
	}
}

// TestPollResultsBroadcastToEveryPane pins the other side of the routing
// contract: a poll result is not addressed to whichever pane happens to be
// focused, so every pane sees it and each decides whether the message is its
// own.
func TestPollResultsBroadcastToEveryPane(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m, fakes := modelWithFakes()
	m.focus = 2

	if _, cmd := m.Update(panes.AgentsMsg{}); cmd == nil {
		t.Error("the panes' commands were dropped")
	}
	for i, f := range fakes {
		if f.updates != 1 {
			t.Errorf("pane %d saw %d updates, want 1", i, f.updates)
		}
	}
}

// TestFetchErrMsgReportsSourceAndCause: the error is not displayed yet, so the
// only thing keeping it useful for the task that displays it is that it
// carries which fetch failed and why. A message that collapsed to "poll
// failed" would make that task start by re-plumbing this one.
func TestFetchErrMsgReportsSourceAndCause(t *testing.T) {
	err := fetchErrMsg{source: "agents", err: http.ErrServerClosed}
	got := err.Error()
	if !strings.Contains(got, "agents") {
		t.Errorf("Error() = %q, want it to name the failing fetch", got)
	}
	if !strings.Contains(got, http.ErrServerClosed.Error()) {
		t.Errorf("Error() = %q, want it to carry the underlying cause", got)
	}
}

// TestPollSurvivesAnUnreachableDashboard is the case an operator actually
// hits: the TUI started before the dashboard did. It must produce an error and
// a re-armed tick, not a panic and not a hang.
func TestPollSurvivesAnUnreachableDashboard(t *testing.T) {
	m := pollTestModel(t, closedDashboard)

	done := make(chan []tea.Msg, 1)
	go func() { done <- runTick(m) }()

	select {
	case msgs := <-done:
		if findFetchErr(msgs) == nil {
			t.Error("an unreachable dashboard produced no fetchErrMsg")
		}
		if !hasTick(msgs) {
			t.Error("an unreachable dashboard stopped the tick loop")
		}
	case <-time.After(finalWait):
		t.Fatal("a poll against an unreachable dashboard did not return")
	}
}
