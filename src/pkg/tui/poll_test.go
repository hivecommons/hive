package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

// eventsFixture is the newest-first /api/audit snapshot the Events wiring
// tests serve. The ordering is intentionally visible in both timestamps and
// action names so an app-level sort or reversal cannot pass unnoticed.
const eventsFixture = `{"entries":[
  {"ts":"2026-09-01T12:04:05Z","user":"operator","action":"newest","agent":"scanner"},
  {"ts":"2026-09-01T12:04:04Z","user":"governor","action":"anchored","agent":"quality"},
  {"ts":"2026-09-01T12:04:03Z","user":"operator","action":"oldest","agent":"reviewer"}
]}`

const emptyEventsFixture = `{"entries":[]}`

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
	// Clear the session cookie for the same reason the two above are pinned:
	// a developer with a real HIVE_DASHBOARD_COOKIE exported would otherwise
	// have every fixture server in this package receive their live session,
	// and the one test that asserts on the header would pass against their
	// value rather than the one it set. Empty is the correct default here —
	// New() omits the header entirely for it.
	if err := os.Setenv(client.CookieEnv, ""); err != nil {
		panic(err)
	}
	// Same containment for the terminal credential override: a developer
	// with a real HIVE_TTYD_CREDENTIAL exported must not have the attach
	// tests present it to their fixture servers.
	if err := os.Setenv(client.TtydCredentialEnv, ""); err != nil {
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

// pollTestModel is a model pointed at url with BOTH poll intervals short
// enough that a tick can be run to completion inside a test.
//
// The intervals are what make these tests fast without sleeping a real one:
// the AC asks for tick scheduling covered without waiting out the cadence, and
// each interval being a model field rather than a bare constant read is what
// allows it. Neither is zero — a zero-duration tea.Tick is a hot loop, and a
// test that passed with one would hide exactly the bug
// TestNewModelPollsOnAnInterval guards.
//
// Both are shortened, not just the reconcile one: since T32 the two loops have
// separate timers, and leaving the activity chain at its production 5s would
// let a test that means to exercise it silently exercise nothing.
func pollTestModel(t *testing.T, url string) model {
	t.Helper()
	pinDashboard(t, url)
	m := newModel()
	m.reconcileInterval = time.Millisecond
	m.activityInterval = time.Millisecond
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

// runTick injects a RECONCILIATION tick into m and returns every message the
// resulting command produced. The tick is INJECTED, never waited for — that is
// the AC's "does not sleep real intervals", and it is also what makes these
// assertions deterministic rather than timing-dependent.
func runTick(m model) []tea.Msg {
	_, cmd := m.Update(reconcileTickMsg{})
	return drain(cmd)
}

// runActivityTick is the same for the activity chain. It is a separate helper
// rather than a parameter because since T32 the two ticks fetch DIFFERENT
// endpoints, so a test that used the wrong one would not fail loudly — it
// would assert against a batch that never contained what it was looking for.
func runActivityTick(m model) []tea.Msg {
	_, cmd := m.Update(activityTickMsg{})
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
		if _, ok := m.(reconcileTickMsg); ok {
			return true
		}
	}
	return false
}

func hasActivityTick(msgs []tea.Msg) bool {
	for _, m := range msgs {
		if _, ok := m.(activityTickMsg); ok {
			return true
		}
	}
	return false
}

// countMsg counts messages of one type in a drained batch. "Exactly one chain
// of each class" is a COUNTING claim, so the tests that make it need a counter
// rather than the boolean has* helpers above: two armed ticks and one armed
// tick both look like `true`, and two is the failure.
func countMsg[T tea.Msg](msgs []tea.Msg) int {
	n := 0
	for _, m := range msgs {
		if _, ok := m.(T); ok {
			n++
		}
	}
	return n
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
// all override the intervals themselves.
//
// BOTH are checked. T32 added the second field, and a constructor that set
// only the one it inherited would leave the activity chain hot-looping on
// /api/tokens, /api/cost and /api/audit from the moment the TUI started.
func TestNewModelPollsOnAnInterval(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m := newModel()

	for _, c := range []struct {
		name string
		got  time.Duration
	}{
		{"reconcileInterval", m.reconcileInterval},
		{"activityInterval", m.activityInterval},
	} {
		if c.got <= 0 {
			t.Fatalf("newModel().%s = %v, want a positive cadence (a zero tick is a hot loop)", c.name, c.got)
		}
		if c.got != pollInterval {
			t.Errorf("newModel().%s = %v, want pollInterval (%v)", c.name, c.got, pollInterval)
		}
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
	// BOTH chains must be armed, and exactly once each. Init is the only place
	// either is started, so a missing arm here is a loop that never runs at
	// all — and since T32 there are two loops to forget.
	if n := countMsg[reconcileTickMsg](msgs); n != 1 {
		t.Errorf("Init() armed %d reconciliation ticks, want exactly 1", n)
	}
	if n := countMsg[activityTickMsg](msgs); n != 1 {
		t.Errorf("Init() armed %d activity ticks, want exactly 1; the Tokens and Events panes would never refresh", n)
	}
	if n := server.requests.Load(); n != 1 {
		t.Errorf("Init() made %d requests, want exactly 1", n)
	}
}

// TestInitFetchesBothClassesImmediately is the AC's "neither loop waits one
// interval before its first data" clause.
//
// It is a separate test from the arming one above because they fail
// differently: an Init that armed both chains but fetched only the reconcile
// class would still fill the Tokens and Events panes — five seconds late,
// which is invisible to a test that only checks the chains exist.
func TestInitFetchesBothClassesImmediately(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	msgs := drain(m.Init())

	if findAgentsMsg(msgs) == nil {
		t.Error("Init() delivered no agents; the reconciliation class did not fetch at startup")
	}
	if countMsg[tokenUsageMsg](msgs) == 0 {
		t.Error("Init() delivered no token counts; the Tokens pane would say `waiting for data` for a whole interval")
	}
	if countMsg[panes.EventsMsg](msgs) == 0 {
		t.Error("Init() delivered no audit rows; the Events pane would say `waiting for data` for a whole interval")
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

	next, cmd := m.Update(reconcileTickMsg{})
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

// ── T29: governor + header wiring ────────────────────────────────────────────

// governorStatusFixture is the /api/status body the T29 tests serve, trimmed
// to the keys client.GovernorStatus reads. The mode is deliberately lowercase,
// as buildGovernor sends it, so a test can tell a case-folding header from one
// that echoes the wire.
const governorStatusFixture = `{
  "governor": {
    "active": true, "mode": "surge", "issues": 12, "prs": 3,
    "thresholds": {"quiet": 1, "busy": 5, "surge": 20},
    "nextKick": "9/1 12:05 PM UTC"
  },
  "acmmLevel": 4,
  "acmmLevelConfigured": true
}`

// governorConfigFixture is the /api/config/governor body. Only the one nested
// key GovernorEvalInterval reads is present; the real response is far larger.
const governorConfigFixture = `{"general_advanced": {"eval_interval_s": 300}}`

// hiveIDFixture is the /api/hive-id body (T6b, #5412).
const hiveIDFixture = `{"id": "acme-prod"}`

// governorEvalInterval is what governorConfigFixture decodes to.
const governorEvalInterval = 300 * time.Second

// dashboardServer serves every endpoint the poll reads, with per-path controls
// for the failure and replacement cases under test.
//
// FAILING BY PATH IS THE WHOLE POINT. T29's core invariant is that these reads
// fail independently, and a server that could only be all-up or all-down could
// not express the case that matters: /api/status fine, /api/config/governor
// forbidden. Each path also counts its requests, so a test can prove a second
// tick re-read rather than replaying a cache.
//
// T30 adds /api/tokens and /api/cost on the same terms, and the same reasoning
// applies twice over: the Tokens pane's whole failure contract is that the two
// halves are independently losable, and only a per-path switch can serve the
// "counts fine, estimate forbidden" shape that contract is about.
// T31 adds the mutable /api/audit body and status plus request counters that
// distinguish its poll-shaped activity feed from the /api/events SSE stream.
type dashboardServer struct {
	*httptest.Server
	failStatus          atomic.Bool
	failConfig          atomic.Bool
	failHiveID          atomic.Bool
	failTokens          atomic.Bool
	failCost            atomic.Bool
	hiveID              atomic.Value // string body for /api/hive-id
	tokens              atomic.Value // string body for /api/tokens
	cost                atomic.Value // string body for /api/cost
	audit               atomic.Value // string body for /api/audit
	auditStatus         atomic.Int64
	auditRequests       atomic.Int64
	eventStreamRequests atomic.Int64
}

func newDashboardServer(t *testing.T) *dashboardServer {
	t.Helper()
	s := &dashboardServer{}
	s.hiveID.Store(hiveIDFixture)
	s.tokens.Store(tokensFixture)
	s.cost.Store(costFixture)
	s.audit.Store(eventsFixture)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden"}`))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/agents":
			_, _ = w.Write([]byte(agentsFixture))
		case "/api/status":
			if s.failStatus.Load() {
				fail()
				return
			}
			_, _ = w.Write([]byte(governorStatusFixture))
		case "/api/config/governor":
			if s.failConfig.Load() {
				fail()
				return
			}
			_, _ = w.Write([]byte(governorConfigFixture))
		case "/api/hive-id":
			if s.failHiveID.Load() {
				fail()
				return
			}
			body, _ := s.hiveID.Load().(string)
			_, _ = w.Write([]byte(body))
		case "/api/tokens":
			if s.failTokens.Load() {
				fail()
				return
			}
			body, _ := s.tokens.Load().(string)
			_, _ = w.Write([]byte(body))
		case "/api/cost":
			if s.failCost.Load() {
				fail()
				return
			}
			body, _ := s.cost.Load().(string)
			_, _ = w.Write([]byte(body))
		case "/api/audit":
			s.auditRequests.Add(1)
			if status := int(s.auditStatus.Load()); status != 0 {
				w.WriteHeader(status)
			}
			body, _ := s.audit.Load().(string)
			_, _ = w.Write([]byte(body))
		case "/api/events":
			s.eventStreamRequests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// applyAll feeds every message a poll produced back into the model, the way
// bubbletea's runtime would, and returns the settled model.
//
// It exists because T29's behaviour is a two-stage pipeline — a fetch produces
// an app-level message, and Update turns cached app state into a pane message
// — so a test that only inspected the poll's output would be asserting on the
// wrong half.
func applyAll(m model, msgs []tea.Msg) model {
	for _, msg := range msgs {
		next, _ := m.Update(msg)
		m = next.(model)
	}
	return m
}

// pollAndApply runs one poll against the server and settles every result.
func pollAndApply(t *testing.T, m model) model {
	t.Helper()
	return applyAll(m, drain(m.poll()))
}

// deliveredGovernor is the frame the model would hand the panes, or nil when
// no successful status read has happened yet.
//
// It reads model state rather than intercepting a message because broadcast
// delivers INTO the panes and returns only their commands — the frame itself
// never appears in any Cmd's output. governorLoaded is the same distinction
// the app makes: no status read yet is not the same fact as a status read that
// reported an inactive governor.
func deliveredGovernor(m model) *panes.GovernorMsg {
	if !m.governorLoaded {
		return nil
	}
	msg := m.governorMsg()
	return &msg
}

// TestPollPopulatesGovernorAndHeaderWithoutSSE is the first acceptance
// criterion: startup polling alone fills the Governor pane and both header
// fields, with no stream event involved.
func TestPollPopulatesGovernorAndHeaderWithoutSSE(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	got := deliveredGovernor(settled)
	if got == nil {
		t.Fatal("a successful poll delivered no GovernorMsg; the pane would stay on its waiting placeholder")
	}
	if got.Status.Mode != "surge" {
		t.Errorf("GovernorMsg.Status.Mode = %q, want %q", got.Status.Mode, "surge")
	}
	if got.EvalInterval != governorEvalInterval {
		t.Errorf("GovernorMsg.EvalInterval = %v, want %v", got.EvalInterval, governorEvalInterval)
	}

	want := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want %q", settled.headerText(), want)
	}
}

// TestGovernorMsgAlwaysCarriesTheCachedInterval is the bug this task exists to
// close, stated directly: a stream event carries no evaluation interval, so a
// GovernorMsg built from one alone reverts `next eval` to unknown.
func TestGovernorMsgAlwaysCarriesTheCachedInterval(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)
	m = pollAndApply(t, m)

	if m.governorInterval != governorEvalInterval {
		t.Fatalf("poll cached interval %v, want %v", m.governorInterval, governorEvalInterval)
	}

	// Now let the stream deliver a full status event, as it would a moment
	// later. Before T29 this is the message that blanked the interval.
	m = connectedStream(t, m)
	// The command is deliberately NOT drained: handleSSEEvent re-arms the
	// stream pump, and running that Cmd would block forever on a test stream
	// nothing writes to. The frame is read from the settled model instead.
	next, _ := m.Update(sseEventMsg{
		gen:   m.sseGen,
		event: sseEvent(client.SSEEventTypeMessage, statusFixture),
	})
	m = next.(model)

	got := deliveredGovernor(m)
	if got == nil {
		t.Fatal("a full SSE status event delivered no GovernorMsg")
	}
	if got.EvalInterval != governorEvalInterval {
		t.Errorf("SSE-sourced GovernorMsg.EvalInterval = %v, want the cached %v; a zero here is the `next eval` regression",
			got.EvalInterval, governorEvalInterval)
	}
	// The stream's own mode must land too — that is the "updates immediately"
	// half of the same criterion.
	if got.Status.Mode != "busy" {
		t.Errorf("GovernorMsg.Status.Mode = %q, want the streamed %q", got.Status.Mode, "busy")
	}
	if m.governorInterval != governorEvalInterval {
		t.Errorf("model interval = %v after an SSE event, want it retained", m.governorInterval)
	}

	// The pane-message builder itself must carry the interval too. This is the
	// exact call site that shipped `panes.GovernorMsg{Status: status}` with no
	// interval, so asserting on it directly is what stops the regression
	// reappearing there specifically.
	direct := findGovernorMsg(m.paneMsgs(sseEvent(client.SSEEventTypeMessage, statusFixture)))
	if direct == nil {
		t.Fatal("paneMsgs produced no governor frame for a full status event")
	}
	if direct.EvalInterval != governorEvalInterval {
		t.Errorf("paneMsgs GovernorMsg.EvalInterval = %v, want the cached %v", direct.EvalInterval, governorEvalInterval)
	}
}

// connectedStream puts m into the connected state with a live stream attached,
// so sseEventMsg is not dropped by the generation guard.
func connectedStream(t *testing.T, m model) model {
	t.Helper()
	stream := &sseStream{
		events: make(chan client.SSEEvent),
		errs:   make(chan error),
		cancel: func() {},
		gen:    m.sseGen,
	}
	m.sse = stream
	m.sseConnected = true
	return m
}

// TestAgentOnlySSEEventPreservesGovernorAndHeader pins that the light
// agent-status push, which carries no governor object, leaves the cached mode
// and the header alone rather than clearing them.
func TestAgentOnlySSEEventPreservesGovernorAndHeader(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)
	m = pollAndApply(t, m)
	before := m.headerText()

	m = connectedStream(t, m)
	next, _ := m.Update(sseEventMsg{
		gen:   m.sseGen,
		event: sseEvent(client.SSEEventTypeAgentStatus, agentStatusFixture),
	})
	got := next.(model)

	if got.governorStatus.Mode != "surge" {
		t.Errorf("governor mode = %q after an agent-only event, want the cached %q", got.governorStatus.Mode, "surge")
	}
	if got.governorInterval != governorEvalInterval {
		t.Errorf("interval = %v after an agent-only event, want it retained", got.governorInterval)
	}
	// The header's ws field legitimately flips to connected; the data fields
	// must not move.
	want := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsConnected)
	if got.headerText() != want {
		t.Errorf("header = %q after an agent-only event, want %q (was %q)", got.headerText(), want, before)
	}
}

// TestForbiddenConfigReadKeepsLiveGovernorMode is the failure-isolation
// criterion in its most concrete form: a read-only token that cannot see
// /api/config/governor must still get a live mode in the header and a loaded
// Governor pane.
func TestForbiddenConfigReadKeepsLiveGovernorMode(t *testing.T) {
	server := newDashboardServer(t)
	server.failConfig.Store(true)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	got := deliveredGovernor(settled)
	if got == nil {
		t.Fatal("a forbidden config read suppressed the whole governor frame")
	}
	if got.Status.Mode != "surge" {
		t.Errorf("Status.Mode = %q, want the live %q despite the config failure", got.Status.Mode, "surge")
	}
	// The interval is honestly unknown, which the pane renders as a dash.
	if got.EvalInterval != 0 {
		t.Errorf("EvalInterval = %v, want zero when the config read failed", got.EvalInterval)
	}
	want := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want %q; a config failure must not blank the mode", settled.headerText(), want)
	}
}

// TestFailedStatusReadKeepsHiveIdentity is the mirror case: /api/status down
// must not take the header's identity with it.
func TestFailedStatusReadKeepsHiveIdentity(t *testing.T) {
	server := newDashboardServer(t)
	server.failStatus.Store(true)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	if deliveredGovernor(settled) != nil {
		t.Error("a failed status read still delivered a governor frame; the pane would show invented values")
	}
	want := fmt.Sprintf(headerFormat, "acme-prod", headerUnknown, wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want %q; the identity read succeeded", settled.headerText(), want)
	}
}

// TestEmptyHiveIDDoesNotBlockTheGovernorPane pins the other half of the
// issue's isolation clause: an unnamed hive renders an honest dash and the
// Governor pane still loads.
func TestEmptyHiveIDDoesNotBlockTheGovernorPane(t *testing.T) {
	server := newDashboardServer(t)
	server.hiveID.Store(`{"id": ""}`)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	if deliveredGovernor(settled) == nil {
		t.Fatal("an empty hive id stopped the Governor pane loading")
	}
	want := fmt.Sprintf(headerFormat, headerUnknown, "SURGE", wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want %q for a hive with no configured identity", settled.headerText(), want)
	}
}

// TestInactiveGovernorRendersADash pins that an inactive governor is a dash in
// the header rather than an empty or invented mode.
func TestInactiveGovernorRendersADash(t *testing.T) {
	m := newModel()
	m.hiveID = "acme-prod"
	m.governorStatus = client.GovernorStatus{
		GovernorState: client.GovernorState{Active: false, Mode: "busy"},
	}

	want := fmt.Sprintf(headerFormat, "acme-prod", headerUnknown, wsNotConnected)
	if m.headerText() != want {
		t.Errorf("header = %q, want %q; an inactive governor has no mode to report", m.headerText(), want)
	}
}

// TestMalformedResponsesRenderDashes pins that a body the client cannot decode
// is a failed read — dashes — rather than a zero value rendered as fact.
func TestMalformedResponsesRenderDashes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"this": "is not the shape you asked for"`))
	}))
	t.Cleanup(server.Close)
	m := pollTestModel(t, server.URL)

	settled := applyAll(m, drain(m.poll()))

	if deliveredGovernor(settled) != nil {
		t.Error("a malformed status body still produced a governor frame")
	}
	want := fmt.Sprintf(headerFormat, headerUnknown, headerUnknown, wsNotConnected)
	if settled.headerText() != want {
		t.Errorf("header = %q, want all dashes for undecodable responses", settled.headerText())
	}
}

// TestHeaderSurvivesStreamDropAndRecovers walks the four states the AC asks to
// be pinned — startup, connected, degraded, recovered — as one sequence,
// because the property under test is precisely that the data fields do NOT
// move while the ws field does.
func TestHeaderSurvivesStreamDropAndRecovers(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	// 1. Startup: nothing fetched yet. Every field honest about that.
	startup := fmt.Sprintf(headerFormat, headerUnknown, headerUnknown, wsNotConnected)
	if m.headerText() != startup {
		t.Errorf("startup header = %q, want %q", m.headerText(), startup)
	}

	// 2. Connected: the poll has data and the stream is up.
	m = pollAndApply(t, m)
	m = connectedStream(t, m)
	connected := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsConnected)
	if m.headerText() != connected {
		t.Errorf("connected header = %q, want %q", m.headerText(), connected)
	}

	// 3. Degraded: the stream drops. ONLY ws changes — this is the "do not
	// treat an SSE connection as the data value" clause. A header that
	// derived identity or mode from the connection blanks them here.
	next, _ := m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})
	m = next.(model)
	degraded := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsNotConnected)
	if m.headerText() != degraded {
		t.Errorf("degraded header = %q, want %q; identity and mode must survive a stream drop", m.headerText(), degraded)
	}

	// 4. Recovered: the fallback poll keeps refreshing while the stream is
	// gone. The server now reports a different mode, and the header must
	// follow it without any stream event.
	if m.reconcileInterval != pollInterval {
		t.Errorf("interval = %v after a drop, want the fallback cadence %v", m.reconcileInterval, pollInterval)
	}
	m = pollAndApply(t, m)
	recovered := fmt.Sprintf(headerFormat, "acme-prod", "SURGE", wsNotConnected)
	if m.headerText() != recovered {
		t.Errorf("recovered header = %q, want %q", m.headerText(), recovered)
	}
}

// TestPollFallbackKeepsRefreshingGovernorAfterADrop is the fourth acceptance
// criterion. It changes what the server returns between ticks, so a model that
// merely retained its cache — rather than re-reading — fails.
func TestPollFallbackKeepsRefreshingGovernorAfterADrop(t *testing.T) {
	mode := atomic.Value{}
	mode.Store("quiet")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			current, _ := mode.Load().(string)
			_, _ = fmt.Fprintf(w, `{"governor":{"active":true,"mode":%q,"issues":1,"prs":0},
			  "acmmLevel":2,"acmmLevelConfigured":true}`, current)
		case "/api/config/governor":
			_, _ = w.Write([]byte(governorConfigFixture))
		case "/api/hive-id":
			_, _ = w.Write([]byte(hiveIDFixture))
		default:
			_, _ = w.Write([]byte(agentsFixture))
		}
	}))
	t.Cleanup(server.Close)

	m := pollTestModel(t, server.URL)
	m = connectedStream(t, m)
	m = pollAndApply(t, m)
	if m.governorStatus.Mode != "quiet" {
		t.Fatalf("mode = %q before the drop, want %q", m.governorStatus.Mode, "quiet")
	}

	next, _ := m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})
	m = next.(model)

	// The hive gets busier while there is no stream to say so.
	mode.Store("surge")
	m = pollAndApply(t, m)

	if m.governorStatus.Mode != "surge" {
		t.Errorf("mode = %q after a fallback poll, want %q; the fallback stopped refreshing the governor",
			m.governorStatus.Mode, "surge")
	}
	if m.governorInterval != governorEvalInterval {
		t.Errorf("interval = %v after a fallback poll, want %v", m.governorInterval, governorEvalInterval)
	}
}

// TestPollIssuesEveryRead pins that the batch actually contains every fetch the
// app has wired. A regression that dropped one would otherwise only show up as
// a field (or, since T30, a whole pane) that quietly never updates.
func TestPollIssuesEveryRead(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	msgs := drain(m.poll())

	var agents, governor, interval, hiveID, tokens, cost, events bool
	for _, msg := range msgs {
		switch msg.(type) {
		case panes.AgentsMsg:
			agents = true
		case governorStatusMsg:
			governor = true
		case governorIntervalMsg:
			interval = true
		case hiveIDMsg:
			hiveID = true
		case tokenUsageMsg:
			tokens = true
		case costSummaryMsg:
			cost = true
		case panes.EventsMsg:
			events = true
		}
	}
	if !agents {
		t.Error("poll did not fetch agents")
	}
	if !governor {
		t.Error("poll did not fetch governor status")
	}
	if !interval {
		t.Error("poll did not fetch the governor eval interval")
	}
	if !hiveID {
		t.Error("poll did not fetch the hive id")
	}
	if !tokens {
		t.Error("poll did not fetch token usage; the Tokens pane would stay on its placeholder")
	}
	if !cost {
		t.Error("poll did not fetch the cost estimate; every cost column would render a dash")
	}
	if !events {
		t.Error("poll did not fetch audit events; the Events pane would stay on its placeholder")
	}
	if got := server.auditRequests.Load(); got != 1 {
		t.Errorf("poll made %d /api/audit requests, want exactly 1", got)
	}
	if got := server.eventStreamRequests.Load(); got != 0 {
		t.Errorf("poll made %d /api/events requests for activity rows, want none", got)
	}
}

// paneEventsIndex is the Events pane's slot in the model's pane array, fixed by
// the frame layout in app.go.
const paneEventsIndex = 3

const eventsPlaceholder = "waiting for data"

// TestStartupPollFillsTheEventsPane drives the client response through the root
// broadcast and into the real pane. Client and pane tests in isolation cannot
// catch the missing app command that left this pane waiting forever.
func TestStartupPollFillsTheEventsPane(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	before := m.panes[paneEventsIndex].View(60, 12)
	if !strings.Contains(before, eventsPlaceholder) {
		t.Fatalf("the Events pane did not start on its placeholder; this test would pass vacuously:\n%s", before)
	}

	m = pollAndApply(t, m)
	view := m.panes[paneEventsIndex].View(60, 12)
	if strings.Contains(view, eventsPlaceholder) {
		t.Fatalf("the Events pane still shows %q after a successful audit poll:\n%s", eventsPlaceholder, view)
	}
	newest := strings.Index(view, "newest")
	oldest := strings.Index(view, "oldest")
	if newest < 0 || oldest < 0 || newest >= oldest {
		t.Errorf("audit rows are not rendered newest first:\n%s", view)
	}
}

// TestSuccessfulEmptyAuditLoadsTheEventsPane distinguishes a healthy idle hive
// from a fetch that never completed.
func TestSuccessfulEmptyAuditLoadsTheEventsPane(t *testing.T) {
	server := newDashboardServer(t)
	server.audit.Store(emptyEventsFixture)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	view := m.panes[paneEventsIndex].View(60, 12)
	if !strings.Contains(view, "no events yet") || strings.Contains(view, eventsPlaceholder) {
		t.Errorf("successful empty audit response did not render the loaded empty state:\n%s", view)
	}
}

// TestAuditRefreshPreservesTheEventsScrollAnchor proves the root passes each
// replacement snapshot through to the pane unchanged and lets the pane retain
// the operator's anchored row as newer activity arrives.
func TestAuditRefreshPreservesTheEventsScrollAnchor(t *testing.T) {
	server := newDashboardServer(t)
	m := pollAndApply(t, pollTestModel(t, server.URL))
	m.focus = paneEventsIndex

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = next.(model)
	if view := m.panes[paneEventsIndex].View(60, 3); !strings.Contains(view, "anchored") {
		t.Fatalf("test did not scroll to the anchor row:\n%s", view)
	}

	server.audit.Store(`{"entries":[
  {"ts":"2026-09-01T12:04:06Z","user":"system","action":"new arrival"},
  {"ts":"2026-09-01T12:04:05Z","user":"operator","action":"newest","agent":"scanner"},
  {"ts":"2026-09-01T12:04:04Z","user":"governor","action":"anchored","agent":"quality"},
  {"ts":"2026-09-01T12:04:03Z","user":"operator","action":"oldest","agent":"reviewer"}
]}`)
	m = pollAndApply(t, m)

	view := m.panes[paneEventsIndex].View(60, 3)
	if !strings.Contains(view, "anchored") || strings.Contains(view, "new arrival") {
		t.Errorf("audit refresh moved the anchored viewport to the newest row:\n%s", view)
	}
}

// TestAuditFailuresPreservePriorEvents covers every failure class called out by
// the issue. None may broadcast an empty EventsMsg, terminate the model, erase
// the last good rows, or reset the pane's scroll position.
func TestAuditFailuresPreservePriorEvents(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		close  bool
	}{
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":"forbidden"}`},
		{name: "server error", status: http.StatusInternalServerError, body: `{"error":"audit unavailable"}`},
		{name: "malformed JSON", body: `{"entries":`},
		{name: "transport", close: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newDashboardServer(t)
			m := pollAndApply(t, pollTestModel(t, server.URL))
			m.focus = paneEventsIndex
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
			m = next.(model)
			before := m.panes[paneEventsIndex].View(60, 3)

			if tc.close {
				server.Close()
			} else {
				server.auditStatus.Store(int64(tc.status))
				server.audit.Store(tc.body)
			}

			msg := m.fetchEvents()()
			fetchErr, ok := msg.(fetchErrMsg)
			if !ok {
				t.Fatalf("failed audit fetch produced %T, want fetchErrMsg", msg)
			}
			if fetchErr.source != "events" {
				t.Errorf("failure source = %q, want events", fetchErr.source)
			}

			next, cmd := m.Update(fetchErr)
			if cmd != nil {
				t.Error("nonfatal audit failure produced a command")
			}
			m = next.(model)
			if after := m.panes[paneEventsIndex].View(60, 3); after != before {
				t.Errorf("audit failure changed the anchored Events pane.\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// TestIntervalBeforeStatusDeliversNoFrame pins the ordering guard: config can
// answer before live state does, and a GovernorMsg then would carry a zero
// status the pane cannot tell from an inactive governor.
func TestIntervalBeforeStatusDeliversNoFrame(t *testing.T) {
	m := newModel()

	next, _ := m.Update(governorIntervalMsg{interval: governorEvalInterval})
	m = next.(model)

	if got := deliveredGovernor(m); got != nil {
		t.Errorf("an interval arriving before any status delivered a frame with status %+v", got.Status)
	}
	if m.governorInterval != governorEvalInterval {
		t.Errorf("interval = %v, want it cached for the first status read", m.governorInterval)
	}

	// The first status read then delivers both halves at once.
	next, _ = m.Update(governorStatusMsg{status: client.GovernorStatus{
		GovernorState: client.GovernorState{Active: true, Mode: "busy"},
	}})
	m = next.(model)
	got := deliveredGovernor(m)
	if got == nil {
		t.Fatal("the first status read delivered no frame")
	}
	if got.EvalInterval != governorEvalInterval {
		t.Errorf("EvalInterval = %v, want the cached %v", got.EvalInterval, governorEvalInterval)
	}
}

// TestHeaderIsClippedNotWrappedAtTheMinimumWidth guards the frame's height
// against a long hive identity.
//
// lipgloss's Width() WRAPS rather than truncates, so a header wider than the
// terminal silently becomes two lines and pushes the whole frame one row past
// the terminal's height — the same cliff the footer strip sits on. T29 is what
// makes this reachable for the header: before it, `hive: —` was a constant two
// cells wide, and no input could overflow. Now the field carries a
// server-supplied string of unbounded length, and identities of this shape are
// ordinary rather than adversarial.
//
// The assertion is on the RENDERED FRAME's line count, not on the header
// string, because the bug is a layout overflow — a test that only measured the
// text would pass while the frame was a row too tall.
func TestHeaderIsClippedNotWrappedAtTheMinimumWidth(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m := newModel()
	m.width, m.height = minWidth, minHeight
	m.hiveID = "acme-production-us-east-1-primary"
	m.governorStatus = client.GovernorStatus{
		GovernorState: client.GovernorState{Active: true, Mode: "surge"},
	}

	if lipgloss.Width(m.headerText()) <= minWidth {
		t.Fatalf("the fixture no longer overflows (%d <= %d); this test would pass vacuously",
			lipgloss.Width(m.headerText()), minWidth)
	}

	frame := m.View()
	lines := strings.Split(frame, "\n")
	if len(lines) != minHeight {
		t.Errorf("frame is %d lines at height %d; a wrapped header cost a row", len(lines), minHeight)
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got > minWidth {
			t.Errorf("line %d is %d columns wide, want at most %d", i, got, minWidth)
		}
	}
}

// --- T30: Tokens pane wiring (#5419) ---------------------------------------

// paneTokensIndex is the Tokens pane's slot in the model's pane array, fixed by
// newModel's construction order and by the design doc's §3 grid.
//
// It is a test-local constant rather than something the app exports: the app
// broadcasts to every pane and never addresses one, so an index is a fact these
// tests need and production code deliberately does not.
const paneTokensIndex = 2

// tokensPlaceholder is the pre-data text every pane shows (panes.placeholder,
// which is unexported). Asserting on the literal is what makes "the pane is
// still waiting" a statement about what an OPERATOR sees rather than about an
// internal flag.
const tokensPlaceholder = "waiting for data"

// tokensFixture is the /api/tokens body the T30 tests serve.
//
// IT IS DELIBERATELY NOT SELF-CONSISTENT. total_input/total_output exceed the
// sum of by_agent_detail by exactly one unattributed session's worth
// (1000 in / 100 out), because AggregateSummary counts every scanned session
// including ones it could not attribute to a configured agent. That gap is the
// whole point of the fixture: an implementation that re-sums the visible rows
// instead of reading the server's totals produces 33000/3300 here and fails,
// where a fixture whose numbers added up would let it pass.
//
// Three agents, so an ordering bug is visible, and by_agent_detail is a JSON
// object — a Go map after decoding — so every test built on this fixture is
// also exercising the map-iteration determinism requirement.
const tokensFixture = `{
  "total_tokens": 37400,
  "total_input": 34000,
  "total_output": 3400,
  "by_agent": {"scanner": 22000, "quality": 8800, "reviewer": 2200},
  "by_agent_detail": {
    "scanner":  {"input": 20000, "output": 2000, "messages": 40, "sessions": 4},
    "quality":  {"input":  8000, "output":  800, "messages": 16, "sessions": 2},
    "reviewer": {"input":  2000, "output":  200, "messages":  4, "sessions": 1}
  },
  "session_count": 8
}`

// unattributedInput and unattributedOutput are the fleet-total surplus above.
const (
	fixtureTotalInput  = 34000
	fixtureTotalOutput = 3400
	// The sum of the three by_agent_detail rows, which is what a re-summing
	// implementation would report instead.
	fixtureAgentSumInput  = 30000
	fixtureAgentSumOutput = 3000
)

// costFixture is the /api/cost body: every agent priced, no unpriced models.
// This is the fully-known case, where both the per-agent and the fleet cost
// are real figures.
const costFixture = `{
  "estimated": {
    "total_usd": 12.5,
    "unpriced_models": [],
    "by_agent": [
      {"name": "scanner",  "usd": 8.25, "source": "estimated", "input": 20000, "output": 2000},
      {"name": "quality",  "usd": 3.00, "source": "estimated", "input":  8000, "output":  800},
      {"name": "reviewer", "usd": 1.25, "source": "estimated", "input":  2000, "output":  200}
    ]
  }
}`

// mixedCostFixture prices one agent and leaves two unpriced, with a non-empty
// unpriced_models list.
//
// The unpriced entries carry a NON-ZERO usd on the wire on purpose. The
// dashboard emits a tier estimate for models it cannot price exactly
// (pkg/tokens/pricing.go), so "unpriced" does not mean "zero" — an
// implementation that decided knownness by testing usd != 0 would show these as
// real spend, and one that tested usd == 0 would still show them. Only Source
// separates them, which is what these rows force a reader to consult.
const mixedCostFixture = `{
  "estimated": {
    "total_usd": 9.75,
    "unpriced_models": ["glm-4.6", "qwen3-coder"],
    "by_agent": [
      {"name": "scanner",  "usd": 8.25, "source": "estimated", "input": 20000, "output": 2000},
      {"name": "quality",  "usd": 1.10, "source": "unpriced",  "input":  8000, "output":  800},
      {"name": "reviewer", "usd": 0.40, "source": "unpriced",  "input":  2000, "output":  200}
    ]
  }
}`

// emptyTokensFixture is a hive that has burned nothing: a successful read whose
// every count is zero. It is NOT the same as a failed read, and the pane must
// render it as a loaded zero-usage table.
const emptyTokensFixture = `{
  "total_tokens": 0, "total_input": 0, "total_output": 0,
  "by_agent": {}, "by_agent_detail": {}, "session_count": 0
}`

// deliveredTokens is the frame the model would hand the panes, or nil when no
// successful token read has happened yet.
//
// It reads model state for the same reason deliveredGovernor does: broadcast
// delivers INTO the panes and returns only their commands, so the frame itself
// never appears in any Cmd's output. tokensLoaded is the same distinction the
// app makes — no count read yet is not the same fact as a hive that has spent
// nothing.
func deliveredTokens(m model) *panes.TokensMsg {
	if !m.tokensLoaded {
		return nil
	}
	msg := m.tokensMsg()
	return &msg
}

// tokenRowsByAgent indexes a delivered frame by agent name so a test can assert
// on one row without depending on the order rows were built in.
func tokenRowsByAgent(msg panes.TokensMsg) map[string]panes.TokenRow {
	out := make(map[string]panes.TokenRow, len(msg.Agents))
	for _, row := range msg.Agents {
		out[row.Agent] = row
	}
	return out
}

// TestStartupPollFillsTheTokensPane is the first acceptance criterion: a
// startup poll alone replaces the pane's placeholder with real rows and totals.
//
// It asserts on the RENDERED PANE, not just the message, because the message
// being correct while the pane never receives it is exactly the bug this task
// exists to fix — the clients and the pane were both already tested in
// isolation and the operator still saw "waiting for data" forever.
func TestStartupPollFillsTheTokensPane(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	before := m.panes[paneTokensIndex].View(60, 12)
	if !strings.Contains(before, tokensPlaceholder) {
		t.Fatalf("the Tokens pane did not start on its placeholder; this test would pass vacuously.\n%s", before)
	}

	settled := pollAndApply(t, m)

	view := settled.panes[paneTokensIndex].View(60, 12)
	if strings.Contains(view, tokensPlaceholder) {
		t.Fatalf("the Tokens pane still shows %q after a successful poll:\n%s", tokensPlaceholder, view)
	}
	for _, agent := range []string{"scanner", "quality", "reviewer"} {
		if !strings.Contains(view, agent) {
			t.Errorf("rendered Tokens pane is missing agent %q:\n%s", agent, view)
		}
	}
	if !strings.Contains(view, "total") {
		t.Errorf("rendered Tokens pane is missing its totals row:\n%s", view)
	}
}

// TestTokenRowsCarryTheTokenEndpointCounts pins that input/output come from
// /api/tokens and not from the cost payload's own per-agent counts.
func TestTokenRowsCarryTheTokenEndpointCounts(t *testing.T) {
	server := newDashboardServer(t)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	frame := deliveredTokens(m)
	if frame == nil {
		t.Fatal("a successful poll delivered no TokensMsg")
	}
	if len(frame.Agents) != 3 {
		t.Fatalf("delivered %d rows, want 3 (one per by_agent_detail key)", len(frame.Agents))
	}

	rows := tokenRowsByAgent(*frame)
	for _, want := range []panes.TokenRow{
		{Agent: "scanner", TokenCounts: panes.TokenCounts{Input: 20000, Output: 2000, CostUSD: 8.25, CostKnown: true}},
		{Agent: "quality", TokenCounts: panes.TokenCounts{Input: 8000, Output: 800, CostUSD: 3.00, CostKnown: true}},
		{Agent: "reviewer", TokenCounts: panes.TokenCounts{Input: 2000, Output: 200, CostUSD: 1.25, CostKnown: true}},
	} {
		if got := rows[want.Agent]; got != want {
			t.Errorf("row %q = %+v, want %+v", want.Agent, got, want)
		}
	}
}

// TestFleetTotalsComeFromTheServerNotTheRows is the unattributed-usage
// criterion. tokensFixture's totals exceed the sum of its agent rows, so a
// re-summing implementation reports the smaller number and fails here.
func TestFleetTotalsComeFromTheServerNotTheRows(t *testing.T) {
	server := newDashboardServer(t)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	frame := deliveredTokens(m)
	if frame == nil {
		t.Fatal("a successful poll delivered no TokensMsg")
	}

	// Guard the fixture itself: if the numbers ever balance, this test proves
	// nothing and should say so rather than passing quietly.
	var sumIn, sumOut int64
	for _, row := range frame.Agents {
		sumIn += row.Input
		sumOut += row.Output
	}
	if sumIn != fixtureAgentSumInput || sumOut != fixtureAgentSumOutput {
		t.Fatalf("fixture rows sum to %d/%d, want %d/%d", sumIn, sumOut, fixtureAgentSumInput, fixtureAgentSumOutput)
	}
	if sumIn == fixtureTotalInput {
		t.Fatal("the fixture has no unattributed usage; a re-summing implementation would pass vacuously")
	}

	if frame.Total.Input != fixtureTotalInput {
		t.Errorf("Total.Input = %d, want the server's %d (a re-sum of rows gives %d)",
			frame.Total.Input, fixtureTotalInput, sumIn)
	}
	if frame.Total.Output != fixtureTotalOutput {
		t.Errorf("Total.Output = %d, want the server's %d (a re-sum of rows gives %d)",
			frame.Total.Output, fixtureTotalOutput, sumOut)
	}
}

// TestFullyPricedSummaryKnowsTheFleetCost: no unpriced models, so total_usd is
// the complete spend and is presented as such.
func TestFullyPricedSummaryKnowsTheFleetCost(t *testing.T) {
	server := newDashboardServer(t)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	frame := deliveredTokens(m)
	if frame == nil {
		t.Fatal("a successful poll delivered no TokensMsg")
	}
	if !frame.Total.CostKnown {
		t.Fatal("Total.CostKnown = false for a summary with no unpriced models; the fleet cost renders a dash for no reason")
	}
	if frame.Total.CostUSD != 12.5 {
		t.Errorf("Total.CostUSD = %v, want the summary's total_usd 12.5", frame.Total.CostUSD)
	}
}

// TestMixedSummaryPricesOnlyTheEstimatedRows is the load-bearing case: unpriced
// entries carry a non-zero usd on the wire, and only Source distinguishes them.
func TestMixedSummaryPricesOnlyTheEstimatedRows(t *testing.T) {
	server := newDashboardServer(t)
	server.cost.Store(mixedCostFixture)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	frame := deliveredTokens(m)
	if frame == nil {
		t.Fatal("a successful poll delivered no TokensMsg")
	}
	rows := tokenRowsByAgent(*frame)

	scanner := rows["scanner"]
	if !scanner.CostKnown || scanner.CostUSD != 8.25 {
		t.Errorf("scanner cost = {%v %v}, want {8.25 true}: source is \"estimated\"", scanner.CostUSD, scanner.CostKnown)
	}
	for _, agent := range []string{"quality", "reviewer"} {
		row := rows[agent]
		if row.CostKnown {
			t.Errorf("%s CostKnown = true for an entry whose source is \"unpriced\"; the pane would render $%.2f as real spend",
				agent, row.CostUSD)
		}
		if row.CostUSD != 0 {
			t.Errorf("%s CostUSD = %v on an unknown row; an unknown cost must not carry a figure a later change could render",
				agent, row.CostUSD)
		}
		// The counts are unaffected by the missing price.
		if row.Input == 0 || row.Output == 0 {
			t.Errorf("%s lost its token counts to an unpriced model: %+v", agent, row)
		}
	}

	if frame.Total.CostKnown {
		t.Errorf("Total.CostKnown = true with unpriced_models present; a lower bound (%v) was presented as the fleet's complete spend",
			frame.Total.CostUSD)
	}
}

// TestUnpricedZeroIsNotRenderedAsFree drives the mixed case all the way to the
// rendered pane, where the distinction is visible to an operator.
//
// The wire fixture is a $0.00 unpriced entry — the exact shape
// client.CostAgentEntry warns about — so an implementation that trusted the
// float would render "$0.00" and claim the agent is free.
func TestUnpricedZeroIsNotRenderedAsFree(t *testing.T) {
	server := newDashboardServer(t)
	server.cost.Store(`{"estimated": {
	  "total_usd": 8.25,
	  "unpriced_models": ["glm-4.6"],
	  "by_agent": [
	    {"name": "scanner",  "usd": 8.25, "source": "estimated"},
	    {"name": "quality",  "usd": 0,    "source": "unpriced"},
	    {"name": "reviewer", "usd": 0,    "source": "unpriced"}
	  ]
	}}`)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	rows := tokenRowsByAgent(*deliveredTokens(m))
	if rows["quality"].CostKnown {
		t.Error("a $0 unpriced entry was taken as a known cost; the pane renders $0.00 for an agent whose price is simply unknown")
	}

	view := m.panes[paneTokensIndex].View(60, 12)
	if strings.Contains(view, "$0.00") {
		t.Errorf("the rendered pane shows $0.00 for an unpriced agent:\n%s", view)
	}
	if !strings.Contains(view, "—") {
		t.Errorf("the rendered pane shows no em dash for the unknown costs:\n%s", view)
	}
	if !strings.Contains(view, "$8.25") {
		t.Errorf("the rendered pane lost the one cost that IS known:\n%s", view)
	}
}

// TestCostFailureStillRefreshesCounts is the failure-isolation criterion: the
// optional half is gone, the primary half is not held behind it.
func TestCostFailureStillRefreshesCounts(t *testing.T) {
	server := newDashboardServer(t)
	server.failCost.Store(true)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	frame := deliveredTokens(m)
	if frame == nil {
		t.Fatal("a cost failure suppressed the whole frame; the pane stays on its placeholder despite a healthy /api/tokens")
	}
	if len(frame.Agents) != 3 {
		t.Errorf("delivered %d rows with cost down, want the 3 the token read returned", len(frame.Agents))
	}
	if frame.Total.Input != fixtureTotalInput {
		t.Errorf("Total.Input = %d with cost down, want the fresh %d", frame.Total.Input, fixtureTotalInput)
	}
	for _, row := range frame.Agents {
		if row.CostKnown {
			t.Errorf("row %q reports a known cost of %v after /api/cost failed", row.Agent, row.CostUSD)
		}
	}
	if frame.Total.CostKnown {
		t.Errorf("Total.CostKnown = true after /api/cost failed (%v)", frame.Total.CostUSD)
	}

	view := m.panes[paneTokensIndex].View(60, 12)
	if !strings.Contains(view, "scanner") {
		t.Errorf("the pane did not render fresh counts with cost down:\n%s", view)
	}
}

// TestTokenFailurePreservesThePriorPane is the other half of that contract: the
// primary read is the one whose failure holds the pane.
func TestTokenFailurePreservesThePriorPane(t *testing.T) {
	server := newDashboardServer(t)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	good := m.panes[paneTokensIndex].View(60, 12)
	if strings.Contains(good, tokensPlaceholder) {
		t.Fatalf("no data to preserve; this test would pass vacuously:\n%s", good)
	}

	// The dashboard's token endpoint goes away. Everything else still answers.
	server.failTokens.Store(true)
	m = pollAndApply(t, m)

	after := m.panes[paneTokensIndex].View(60, 12)
	if after != good {
		t.Errorf("a failed /api/tokens changed the pane.\nbefore:\n%s\nafter:\n%s", good, after)
	}
}

// TestStaleCostIsClearedWhenTheEstimateGoesAway pins the arrival-order property
// the fetchErrMsg handler exists for.
//
// A cost failure invalidates the cached estimate AND rebuilds the frame. Without
// the rebuild, a token result that happened to be applied before the error would
// leave the previous poll's dollar figures sitting on this poll's counts — a
// stale price on fresh usage, which is the one thing worse than a dash.
func TestStaleCostIsClearedWhenTheEstimateGoesAway(t *testing.T) {
	server := newDashboardServer(t)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	if !deliveredTokens(m).Total.CostKnown {
		t.Fatal("the first poll produced no known cost; this test would pass vacuously")
	}

	// Apply the messages in the order that hides the bug: the fresh counts
	// first, then the cost failure.
	server.failCost.Store(true)
	server.tokens.Store(`{
	  "total_input": 99000, "total_output": 9900,
	  "by_agent_detail": {"scanner": {"input": 99000, "output": 9900}}
	}`)
	msgs := drain(m.poll())
	var errs []tea.Msg
	for _, msg := range msgs {
		if _, isErr := msg.(fetchErrMsg); isErr {
			errs = append(errs, msg)
			continue
		}
		next, _ := m.Update(msg)
		m = next.(model)
	}
	m = applyAll(m, errs)

	frame := deliveredTokens(m)
	if frame.Total.Input != 99000 {
		t.Errorf("Total.Input = %d, want the refreshed 99000", frame.Total.Input)
	}
	if frame.Total.CostKnown {
		t.Errorf("Total.CostKnown = true (%v) after /api/cost failed; a stale estimate is sitting on fresh counts", frame.Total.CostUSD)
	}
	for _, row := range frame.Agents {
		if row.CostKnown {
			t.Errorf("row %q kept a stale cost of %v after /api/cost failed", row.Agent, row.CostUSD)
		}
	}

	view := m.panes[paneTokensIndex].View(60, 12)
	if strings.Contains(view, "$") {
		t.Errorf("the rendered pane still shows a dollar figure after the estimate went away:\n%s", view)
	}
}

// TestEmptyUsageIsALoadedZeroFrame: a successful read of an idle hive is data,
// not an absence of data.
func TestEmptyUsageIsALoadedZeroFrame(t *testing.T) {
	server := newDashboardServer(t)
	server.tokens.Store(emptyTokensFixture)
	server.cost.Store(`{"estimated": {"total_usd": 0, "unpriced_models": [], "by_agent": []}}`)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	frame := deliveredTokens(m)
	if frame == nil {
		t.Fatal("an empty-but-successful token read delivered no frame; the pane would wait forever on a healthy idle hive")
	}
	if len(frame.Agents) != 0 {
		t.Errorf("delivered %d rows for an empty summary, want 0", len(frame.Agents))
	}
	if frame.Total.Input != 0 || frame.Total.Output != 0 {
		t.Errorf("Total = %+v, want zeroes", frame.Total)
	}

	view := m.panes[paneTokensIndex].View(60, 12)
	if strings.Contains(view, tokensPlaceholder) {
		t.Errorf("an idle hive still renders %q rather than a loaded zero table:\n%s", tokensPlaceholder, view)
	}
	if !strings.Contains(view, "no usage recorded") {
		t.Errorf("the loaded zero state did not render the pane's empty row:\n%s", view)
	}
	// A zero-usage hive with a fully priced (empty) summary genuinely spent
	// $0.00, and that is a fact rather than an unknown.
	if !frame.Total.CostKnown {
		t.Error("Total.CostKnown = false for an empty fully-priced summary; a real $0.00 was reported as unknown")
	}
}

// TestCostBeforeTokensDeliversNoFrame is the ordering guard, the mirror of
// TestIntervalBeforeStatusDeliversNoFrame: the two fetches race, and a cost
// result arriving first must not produce a loaded pane with no rows — which the
// pane cannot tell from a hive that has genuinely spent nothing.
func TestCostBeforeTokensDeliversNoFrame(t *testing.T) {
	m := newModel()

	next, _ := m.Update(costSummaryMsg{summary: client.CostSummary{
		TotalUSD: 12.5,
		ByAgent:  []client.CostAgentEntry{{Name: "scanner", USD: 8.25, Source: "estimated"}},
	}})
	m = next.(model)

	if got := deliveredTokens(m); got != nil {
		t.Errorf("a cost read arriving before any token read delivered a frame: %+v", *got)
	}
	// The pane is the real assertion: deliveredTokens reads the same
	// tokensLoaded flag the guard does, so it cannot observe a frame that WAS
	// broadcast. Only the pane can, and a loaded pane here is a hive reported
	// as having spent nothing when nothing has been read.
	if view := m.panes[paneTokensIndex].View(60, 12); !strings.Contains(view, tokensPlaceholder) {
		t.Errorf("a cost read alone loaded the pane; an idle-looking zero table is showing before any count was read:\n%s", view)
	}
	if !m.costLoaded {
		t.Error("the estimate was not cached for the first token read")
	}

	// The first token read then delivers both halves at once.
	next, _ = m.Update(tokenUsageMsg{usage: client.TokenUsage{
		TotalInput:    20000,
		TotalOutput:   2000,
		ByAgentDetail: map[string]*client.TokenBucket{"scanner": {Input: 20000, Output: 2000}},
	}})
	m = next.(model)

	frame := deliveredTokens(m)
	if frame == nil {
		t.Fatal("the first token read delivered no frame")
	}
	if len(frame.Agents) != 1 || !frame.Agents[0].CostKnown || frame.Agents[0].CostUSD != 8.25 {
		t.Errorf("the frame did not join the cached estimate: %+v", frame.Agents)
	}
}

// TestRepeatedPollsRenderDeterministically is the map-nondeterminism criterion.
//
// by_agent_detail and the cost by_agent list are decoded into a Go map, whose
// iteration order is randomized per range. Polling repeatedly and comparing the
// RENDERED pane is what catches a projection that let that order through: the
// pane sorts what it is handed, so this also pins that the wiring did not
// bypass that sort by delivering pre-ordered rows the pane would keep.
//
// The rows are given EQUAL totals so the sort's tie-break is the thing under
// test. With distinct magnitudes the descending sort alone would mask a lost
// tie-break, and equal-spend agents swapping places every 5 seconds is exactly
// the flicker the contract forbids.
func TestRepeatedPollsRenderDeterministically(t *testing.T) {
	server := newDashboardServer(t)
	server.tokens.Store(`{
	  "total_input": 3000, "total_output": 300,
	  "by_agent_detail": {
	    "alpha":   {"input": 1000, "output": 100},
	    "bravo":   {"input": 1000, "output": 100},
	    "charlie": {"input": 1000, "output": 100},
	    "delta":   {"input": 1000, "output": 100},
	    "echo":    {"input": 1000, "output": 100}
	  }
	}`)
	server.cost.Store(`{"estimated": {
	  "total_usd": 5, "unpriced_models": [],
	  "by_agent": [
	    {"name": "alpha", "usd": 1, "source": "estimated"},
	    {"name": "bravo", "usd": 1, "source": "estimated"},
	    {"name": "charlie", "usd": 1, "source": "estimated"},
	    {"name": "delta", "usd": 1, "source": "estimated"},
	    {"name": "echo", "usd": 1, "source": "estimated"}
	  ]
	}}`)

	m := pollAndApply(t, pollTestModel(t, server.URL))
	want := m.panes[paneTokensIndex].View(60, 14)

	// One poll can only observe one iteration order. Repeating is what makes a
	// leak show up rather than passing on a lucky shuffle.
	const repeats = 30
	for i := 0; i < repeats; i++ {
		m = pollAndApply(t, m)
		if got := m.panes[paneTokensIndex].View(60, 14); got != want {
			t.Fatalf("poll %d rendered a different pane; map iteration order leaked into the frame.\nfirst:\n%s\nnow:\n%s", i+1, want, got)
		}
	}
}

// TestTokenPollSurvivesAnUnreachableDashboard: neither fetch may panic or hang
// when nothing is listening, and both must report as failures rather than as
// empty successes.
func TestTokenPollSurvivesAnUnreachableDashboard(t *testing.T) {
	m := pollTestModel(t, closedDashboard)

	msgs := drain(m.poll())

	var sources []string
	for _, msg := range msgs {
		if err, ok := msg.(fetchErrMsg); ok {
			sources = append(sources, err.source)
		}
		if _, ok := msg.(tokenUsageMsg); ok {
			t.Error("an unreachable dashboard produced a tokenUsageMsg; an empty success would render as a zero-usage hive")
		}
		if _, ok := msg.(costSummaryMsg); ok {
			t.Error("an unreachable dashboard produced a costSummaryMsg")
		}
	}
	for _, want := range []string{"tokens", costFetchSource} {
		found := false
		for _, got := range sources {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no fetchErrMsg with source %q; sources were %v", want, sources)
		}
	}

	settled := applyAll(m, msgs)
	if deliveredTokens(settled) != nil {
		t.Error("a fully failed poll delivered a frame")
	}
	view := settled.panes[paneTokensIndex].View(60, 12)
	if !strings.Contains(view, tokensPlaceholder) {
		t.Errorf("the pane left its placeholder without ever receiving data:\n%s", view)
	}
}

// TestCostForAnUnknownAgentCreatesNoRow: cost enriches rows, it never invents
// them. An agent priced by /api/cost but absent from by_agent_detail has no
// counts to show, and a row of dashes carrying a dollar figure would read as
// spend without tokens.
func TestCostForAnUnknownAgentCreatesNoRow(t *testing.T) {
	server := newDashboardServer(t)
	server.tokens.Store(`{
	  "total_input": 1000, "total_output": 100,
	  "by_agent_detail": {"scanner": {"input": 1000, "output": 100}}
	}`)
	server.cost.Store(`{"estimated": {
	  "total_usd": 9, "unpriced_models": [],
	  "by_agent": [
	    {"name": "scanner", "usd": 4, "source": "estimated"},
	    {"name": "ghost",   "usd": 5, "source": "estimated"}
	  ]
	}}`)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	frame := deliveredTokens(m)
	if len(frame.Agents) != 1 {
		t.Fatalf("delivered %d rows, want only the 1 with token counts: %+v", len(frame.Agents), frame.Agents)
	}
	if frame.Agents[0].Agent != "scanner" {
		t.Errorf("row is %q, want scanner", frame.Agents[0].Agent)
	}
}

// TestNullAgentBucketIsSkipped: by_agent_detail is a map of POINTERS, so a null
// value decodes to a nil bucket. A row of zeroes attributed to a real agent
// would read as an idle agent rather than as missing data.
func TestNullAgentBucketIsSkipped(t *testing.T) {
	server := newDashboardServer(t)
	server.tokens.Store(`{
	  "total_input": 1000, "total_output": 100,
	  "by_agent_detail": {"scanner": {"input": 1000, "output": 100}, "broken": null}
	}`)
	m := pollAndApply(t, pollTestModel(t, server.URL))

	for _, row := range deliveredTokens(m).Agents {
		if row.Agent == "broken" {
			t.Errorf("a null bucket produced a row: %+v", row)
		}
	}
}

// TestTokenFrameDoesNotOverflowTheMinimumTerminal guards the frame's geometry
// against the values THIS TASK newly lets reach it.
//
// It is the same cliff #5486 and #5487 hit: lipgloss's Width() WRAPS rather
// than truncates, so an over-wide row silently becomes two lines and pushes the
// frame a row past the terminal's height. Before T30 the Tokens pane was
// unreachable from real data — nothing sent it a TokensMsg — so no
// server-supplied string could overflow it. Now agent names and dollar amounts
// both arrive from the wire with no length bound.
//
// The assertion is that the EXISTING pane already clips (panes.truncate for the
// name column, the grid's MaxWidth/MaxHeight for the rest), so this task needs
// no clipping of its own. It is a regression guard, not a fix: if a later change
// to the pane's layout removes that clipping, this fails at the wiring that
// made the values reachable.
func TestTokenFrameDoesNotOverflowTheMinimumTerminal(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m := newModel()
	m.width, m.height = minWidth, minHeight

	longName := strings.Repeat("agent-with-an-absurdly-long-name-", 8)
	m.costLoaded = true
	m.costSummary = client.CostSummary{
		TotalUSD: 987654321098765,
		ByAgent:  []client.CostAgentEntry{{Name: longName, USD: 123456789012345, Source: "estimated"}},
	}
	next, _ := m.Update(tokenUsageMsg{usage: client.TokenUsage{
		TotalInput:    9876543210,
		TotalOutput:   1234567890,
		ByAgentDetail: map[string]*client.TokenBucket{longName: {Input: 9876543210, Output: 1234567890}},
	}})
	m = next.(model)

	frame := m.View()
	lines := strings.Split(frame, "\n")
	if len(lines) != minHeight {
		t.Errorf("frame is %d lines at height %d; a wrapped token row cost a row", len(lines), minHeight)
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got > minWidth {
			t.Errorf("line %d is %d columns wide, want at most %d", i, got, minWidth)
		}
	}
}

// ── T32: split reconcile and activity cadences ───────────────────────────────

// streamHealthyModel is a model in the state a healthy stream leaves behind,
// built so the ACTIVITY chain can still be drained.
//
// pollTestModel shortens both intervals; this puts the reconcile one back to
// the stretched production value, because these tests are about what that
// stretch does and does not reach. The consequence is that a reconcile tick
// armed by this model carries a real 60s timer — drainUntil it, never drain().
func streamHealthyModel(t *testing.T, url string) model {
	t.Helper()
	m := pollTestModel(t, url)
	m = connectedStream(t, m)
	m.reconcileInterval = sseReconcileInterval
	return m
}

// activityMsgCount is how many of the three activity reads a drained batch
// delivered. It counts rather than reporting presence because the failures it
// guards are "one of them went missing" and "all three were issued twice".
func activityMsgCount(msgs []tea.Msg) int {
	return countMsg[tokenUsageMsg](msgs) +
		countMsg[costSummaryMsg](msgs) +
		countMsg[panes.EventsMsg](msgs)
}

// findEventsMsg returns the delivered audit snapshot, or nil if no EventsMsg
// was produced.
func findEventsMsg(msgs []tea.Msg) *panes.EventsMsg {
	for _, m := range msgs {
		if e, ok := m.(panes.EventsMsg); ok {
			return &e
		}
	}
	return nil
}

// reconcileMsgCount is the same for the four reconciliation reads.
func reconcileMsgCount(msgs []tea.Msg) int {
	return countMsg[panes.AgentsMsg](msgs) +
		countMsg[governorStatusMsg](msgs) +
		countMsg[governorIntervalMsg](msgs) +
		countMsg[hiveIDMsg](msgs)
}

// TestEachTickFetchesOnlyItsOwnClass is the split itself, asserted from both
// sides.
//
// Testing only that each batch contains what it should would pass on a model
// that never split them at all — one loop fetching everything satisfies every
// "contains" clause. What distinguishes the split is the ABSENCE: a
// reconciliation tick must not read /api/audit, and an activity tick must not
// read /api/agents. Without that, the two loops are one loop running twice.
func TestEachTickFetchesOnlyItsOwnClass(t *testing.T) {
	t.Run("reconcile", func(t *testing.T) {
		server := newDashboardServer(t)
		m := pollTestModel(t, server.URL)

		msgs := runTick(m)

		if got := reconcileMsgCount(msgs); got != 4 {
			t.Errorf("a reconciliation tick delivered %d of its 4 reads; a wired endpoint went missing", got)
		}
		if got := activityMsgCount(msgs); got != 0 {
			t.Errorf("a reconciliation tick delivered %d activity reads, want 0 — those belong to the fixed loop", got)
		}
		if n := server.auditRequests.Load(); n != 0 {
			t.Errorf("a reconciliation tick read /api/audit %d times, want 0", n)
		}
		if !hasTick(msgs) {
			t.Error("a reconciliation tick did not arm the next one")
		}
		if hasActivityTick(msgs) {
			t.Error("a reconciliation tick armed an activity chain; the two would multiply on every tick")
		}
	})

	t.Run("activity", func(t *testing.T) {
		server := newDashboardServer(t)
		m := pollTestModel(t, server.URL)

		msgs := runActivityTick(m)

		if got := activityMsgCount(msgs); got != 3 {
			t.Errorf("an activity tick delivered %d of its 3 reads; a wired endpoint went missing", got)
		}
		if got := reconcileMsgCount(msgs); got != 0 {
			t.Errorf("an activity tick delivered %d reconciliation reads, want 0 — those belong to the stretching loop", got)
		}
		if n := server.auditRequests.Load(); n != 1 {
			t.Errorf("an activity tick read /api/audit %d times, want exactly 1", n)
		}
		if !hasActivityTick(msgs) {
			t.Error("an activity tick did not arm the next one; the Tokens and Events panes would freeze")
		}
		if hasTick(msgs) {
			t.Error("an activity tick armed a reconciliation chain; the two would multiply on every tick")
		}
	})
}

// TestPollStillIssuesBothClasses pins that splitting the batch did not split
// the one-shot refresh.
//
// poll() is what Init and every action handler use — a pause, a model apply,
// an ACMM apply, returning from tmux. Those writes move the roster AND append
// the operator's own action to the audit log, so a poll() that had quietly
// become reconcile-only would show the effect of an action while the Events
// pane sat there not recording it.
func TestPollStillIssuesBothClasses(t *testing.T) {
	server := newDashboardServer(t)
	m := pollTestModel(t, server.URL)

	msgs := drain(m.poll())

	if got := reconcileMsgCount(msgs); got != 4 {
		t.Errorf("poll() delivered %d reconciliation reads, want 4", got)
	}
	if got := activityMsgCount(msgs); got != 3 {
		t.Errorf("poll() delivered %d activity reads, want 3", got)
	}
	if hasTick(msgs) || hasActivityTick(msgs) {
		t.Error("poll() armed a tick chain; it is the one-shot refresh, and arming from an action handler would fork a second loop per keypress")
	}
}

// TestHealthyStreamStretchesOnlyTheReconcileLoop is the bug T32 exists to fix,
// stated as directly as the model allows.
//
// Before this change all seven reads hung off one timer, so the SSE event that
// proved the stream healthy also stretched /api/tokens, /api/cost and
// /api/audit to 60s. The Tokens and Events panes were therefore at their
// stalest precisely when the header said `ws: connected` — the frame looking
// its most alive at the moment half of it stopped moving.
func TestHealthyStreamStretchesOnlyTheReconcileLoop(t *testing.T) {
	m := pollTestModel(t, closedDashboard)
	m = connectedStream(t, m)
	m.reconcileInterval = pollInterval
	m.activityInterval = pollInterval
	beforeGen := m.activityGen

	// The returned command is the stream reader re-arming on channels nothing
	// ever writes to, so it is deliberately not drained.
	next, _ := m.Update(sseEventMsg{
		gen:   m.sseGen,
		event: sseEvent(client.SSEEventTypeMessage, statusFixture),
	})
	got := next.(model)

	if got.reconcileInterval != sseReconcileInterval {
		t.Errorf("reconcileInterval = %v after a healthy stream event, want %v",
			got.reconcileInterval, sseReconcileInterval)
	}
	if got.activityInterval != pollInterval {
		t.Errorf("activityInterval = %v after a healthy stream event, want it untouched at %v; "+
			"no stream event carries token counts or audit rows, so stretching this loop only makes the panes staler",
			got.activityInterval, pollInterval)
	}
	if got.activityGen != beforeGen {
		t.Error("a stream event retired the activity chain; nothing re-arms it, so the Tokens and Events panes would go dark")
	}
}

// TestActivityLoopKeepsRefreshingWhileTheStreamIsHealthy is the same claim
// made behaviourally rather than by reading a field.
//
// The audit body changes between the two ticks, so a model that had attached
// the activity reads to the stretched timer — or that simply stopped issuing
// them once connected — delivers the first snapshot twice and fails.
func TestActivityLoopKeepsRefreshingWhileTheStreamIsHealthy(t *testing.T) {
	server := newDashboardServer(t)
	m := streamHealthyModel(t, server.URL)

	first := runActivityTick(m)
	if activityMsgCount(first) != 3 {
		t.Fatalf("the first activity tick under a healthy stream delivered %d of 3 reads", activityMsgCount(first))
	}

	// The hive does something while the stream is up and says nothing about it.
	server.audit.Store(`{"entries":[{"ts":"2026-09-01T13:00:00Z","user":"operator","action":"while-connected","agent":"scanner"}]}`)

	second := runActivityTick(m)
	events := findEventsMsg(second)
	if events == nil {
		t.Fatal("the second activity tick delivered no audit rows; the loop stopped under a healthy stream")
	}
	if len(events.Events) != 1 || events.Events[0].Action != "while-connected" {
		t.Errorf("the second activity tick replayed the first snapshot: %+v", events.Events)
	}
	if n := server.auditRequests.Load(); n != 2 {
		t.Errorf("/api/audit was read %d times across two activity ticks, want 2", n)
	}
	if !hasActivityTick(second) {
		t.Error("the activity chain did not re-arm while the stream was healthy")
	}
}

// TestStreamDropDoesNotRetireTheActivityChain is the failure a shared
// generation counter would produce, and the reason activityGen exists.
//
// The drop path bumps the reconcile generation to retire the stretched 60s
// chain, and re-arms a reconcile chain only. Had both classes stamped their
// ticks with that one counter, the same bump would retire the activity tick
// already in flight — and nothing would ever arm another. The Tokens and
// Events panes would go dark for the rest of the session at the first stream
// blip, which is the very failure this task is about, reached from the other
// side.
func TestStreamDropDoesNotRetireTheActivityChain(t *testing.T) {
	server := newDashboardServer(t)
	m := streamHealthyModel(t, server.URL)
	inFlightGen, beforeInterval := m.activityGen, m.activityInterval

	next, _ := m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})
	got := next.(model)

	if got.activityGen != inFlightGen {
		t.Errorf("activityGen moved from %d to %d on a stream drop; the in-flight activity tick is now stale",
			inFlightGen, got.activityGen)
	}
	if got.activityInterval != beforeInterval {
		t.Errorf("activityInterval = %v after a drop, want it untouched at %v", got.activityInterval, beforeInterval)
	}

	// The proof, rather than the bookkeeping: the tick that was ALREADY ARMED
	// when the stream dropped — carrying the pre-drop generation — must still
	// fetch and still re-arm on the post-drop model.
	_, cmd := got.Update(activityTickMsg{gen: inFlightGen})
	if cmd == nil {
		t.Fatal("the activity chain died with the stream; nothing would ever refresh Tokens or Events again")
	}
	msgs := drain(cmd)
	if activityMsgCount(msgs) != 3 {
		t.Errorf("the activity tick armed before the drop delivered %d of 3 reads afterwards", activityMsgCount(msgs))
	}
	if !hasActivityTick(msgs) {
		t.Error("the surviving activity tick did not arm its successor")
	}
}

// TestStreamDropDoesNotDuplicateActivityRequests is the AC's "does not
// duplicate activity requests" clause.
//
// The fallback must fetch immediately — the reconcile data really is stale the
// moment the stream stops — but the activity loop never stretched, so it has
// been reading these three endpoints every 5s throughout and is mid-interval
// right now. A full poll() here would issue a second copy of each, on the
// exact code path a flapping dashboard walks over and over.
func TestStreamDropDoesNotDuplicateActivityRequests(t *testing.T) {
	server := newDashboardServer(t)
	m := streamHealthyModel(t, server.URL)

	_, cmd := m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})

	// Waiting for the reconnect message is what makes the counter assertion
	// trustworthy: it is a real sseBackoffMin timer, so by the time it lands a
	// duplicate audit fetch against a local test server has had a full second
	// to complete and be counted.
	msgs := drainUntil(cmd, finalWait, func(msgs []tea.Msg) bool {
		return findAgentsMsg(msgs) != nil && hasMsg[sseReconnectMsg](msgs)
	})

	if findAgentsMsg(msgs) == nil {
		t.Fatal("the drop issued no immediate reconciliation fetch; the frame would sit stale for a whole interval")
	}
	if got := activityMsgCount(msgs); got != 0 {
		t.Errorf("the drop issued %d activity reads, want 0 — the 5s loop is already doing that", got)
	}
	if n := server.auditRequests.Load(); n != 0 {
		t.Errorf("the drop read /api/audit %d times, want 0", n)
	}
	if hasActivityTick(msgs) {
		t.Error("the drop armed a second activity chain; every reconnect would double the activity fetch rate")
	}
}

// TestConnectDropCyclesLeaveOneChainOfEachClass is the AC's repeated-cycle
// clause, and it is the one a per-cycle leak actually shows up in.
//
// Every individual handler can look correct while the pair leaks: an extra
// chain armed once per cycle is invisible in a single connect/drop test and
// doubles the request rate every time a flaky dashboard blips. After three
// full cycles there must still be exactly one live chain of each class — and
// every retired reconcile generation must be dead, not merely outnumbered.
func TestConnectDropCyclesLeaveOneChainOfEachClass(t *testing.T) {
	m := pollTestModel(t, closedDashboard)

	const cycles = 3
	for range cycles {
		m = connectedStream(t, m)
		next, _ := m.Update(sseEventMsg{
			gen:   m.sseGen,
			event: sseEvent(client.SSEEventTypeMessage, statusFixture),
		})
		m = next.(model)
		if m.reconcileInterval != sseReconcileInterval {
			t.Fatalf("cycle setup: reconcileInterval = %v, want the stream to have stretched it", m.reconcileInterval)
		}

		next, _ = m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})
		m = next.(model)
	}

	if m.activityGen != 0 {
		t.Errorf("activityGen = %d after %d connect/drop cycles, want 0; the fixed loop has no cadence to change",
			m.activityGen, cycles)
	}
	if m.reconcileGen != cycles {
		t.Errorf("reconcileGen = %d after %d cycles, want one retirement per stretch undone", m.reconcileGen, cycles)
	}

	// Every retired reconcile chain is dead.
	for gen := uint64(0); gen < m.reconcileGen; gen++ {
		if _, cmd := m.Update(reconcileTickMsg{gen: gen}); cmd != nil {
			t.Errorf("a tick from retired reconcile generation %d re-armed itself", gen)
		}
	}

	// Exactly one live chain of each class. The reconcile interval is back at
	// its production 5s after the last drop; shortening it here is what lets
	// the re-arm be drained without the test sleeping it.
	m.reconcileInterval = time.Millisecond
	_, reconcileCmd := m.Update(reconcileTickMsg{gen: m.reconcileGen})
	if n := countMsg[reconcileTickMsg](drain(reconcileCmd)); n != 1 {
		t.Errorf("the live reconcile tick armed %d successors, want exactly 1", n)
	}

	_, activityCmd := m.Update(activityTickMsg{gen: m.activityGen})
	if n := countMsg[activityTickMsg](drain(activityCmd)); n != 1 {
		t.Errorf("the live activity tick armed %d successors, want exactly 1", n)
	}
}

// TestActivityTickRearmsWhenTheFetchFails is the activity half of the error
// policy that keeps a loop alive, and it matters more here than for
// reconciliation: this loop is the ONLY source the Tokens and Events panes
// have, so an unreachable dashboard that stopped the clock would leave them
// frozen even after it came back.
func TestActivityTickRearmsWhenTheFetchFails(t *testing.T) {
	m := pollTestModel(t, closedDashboard)

	done := make(chan []tea.Msg, 1)
	go func() { done <- runActivityTick(m) }()

	select {
	case msgs := <-done:
		if findFetchErr(msgs) == nil {
			t.Error("an unreachable dashboard produced no fetchErrMsg on the activity loop")
		}
		if activityMsgCount(msgs) != 0 {
			t.Error("a failed activity fetch produced pane data; panes must never see a zero-valued result")
		}
		if !hasActivityTick(msgs) {
			t.Error("a failed activity fetch stopped the activity loop")
		}
	case <-time.After(finalWait):
		t.Fatal("an activity tick against an unreachable dashboard did not return")
	}
}
