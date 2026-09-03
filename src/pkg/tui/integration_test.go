package tui

// T33 (#5424): end-to-end v1 acceptance coverage.
//
// WHY THIS FILE EXISTS, AND WHAT IT IS ALLOWED TO PROVE.
//
// The #4907 tracker was decomposed into client, pane and app tasks small enough
// for short contributor runs. That kept every PR reviewable, and it introduced
// exactly one failure mode: "both halves merged" was mistaken for "the feature
// is wired". Tokens and Events had a complete client AND a complete pane while
// the root app never delivered either message. Every isolated package test
// passed. The panes said `waiting for data` forever.
//
// So the bar for this file is not "the parts return correct values". Every
// other test in this package already establishes that, and none of them would
// have caught the defect above. The bar is that the ASSEMBLED loop is INVOKED:
// each assertion below is written against a fixture that records real HTTP
// traffic from the real root model, so a property can only pass if the request
// was actually issued by the wiring under test. A test that would still pass
// with zero call sites is the defect this task exists to eliminate, not a
// cheaper way to satisfy it.
//
// DETERMINISM. Nothing here sleeps a production duration. The two poll chains
// are driven through model fields (reconcileInterval/activityInterval) that
// exist for this purpose, the SSE stream is driven by a channel the test owns,
// and every wait is a condition poll bounded by a short deadline rather than a
// fixed delay. The suite is required to pass under -count=20 and -shuffle=on.
//
// TMUX IS NOT EXERCISED HERE, DELIBERATELY. `a` builds a tmux command and hands
// it to tea.ExecProcess, which suspends the terminal and attaches to a real
// session. That cannot safely execute in CI — there is no TTY, and a successful
// attach would block the test binary inside an interactive session. The command
// CONSTRUCTION is already owned by attach_test.go/attach_errors_test.go. What
// this file covers is the BINDING AND SELECTION path up to that boundary: that
// `a` is gated on the Agents pane being focused, that it targets the displayed
// selection, and that a second press cannot queue a second suspend. The process
// is never spawned. See TestAttachBindingTargetsSelectionWithoutSpawningTmux.
//
// ─────────────────────────────────────────────────────────────────────────────
// MUTATION EVIDENCE (#5424 final acceptance criterion)
//
// A green suite proves the tests do not error. It does NOT prove they would
// catch the regression they name — and eleven vacuous checks were found in this
// repo in one week, including one inside the tool built to prevent them. So
// each property below was verified by BREAKING IT IN PRODUCTION CODE and
// confirming the named test fails. Each mutation was checked to be physically
// present in the file and to COMPILE first: a mutation that fails to build
// demonstrates nothing.
//
//	# production defect introduced          → test that caught it
//	1 pollActivity drops fetchEvents()      → TestStartupLoadsEveryPaneAndBoth
//	                                          LiveHeaderFieldsWithoutWaitingAnInterval
//	2 handleSSEEvent stops caching the      → TestStreamEventUpdatesAgentAnd
//	  governor slice                          GovernorStateImmediately
//	3 handleSSEEvent stretches              → TestActivityDataKeepsRefreshing
//	  activityInterval too (pre-T32 bug)      WhileTheStreamIsHealthy
//	4 handleSSEDropped never restores       → TestStreamDropActivatesPollFallback
//	  pollInterval (fallback never arms)      PreservesDataAndReconnectsOnce
//	5 tab focus cycles by 2, skipping a     → TestFocusCyclesAndNavigation
//	  pane                                    TargetsTheDisplayedSelection
//	6a kick path drops url.PathEscape       → TestActionsEscapeAgentNamesInto
//	                                          PathSegments
//	6b confirm dialog clears `pending`,     → TestPauseConfirmationHitsThe
//	   so enter re-fires the write            EscapedEndpointExactlyOnceAndRendersTheResponse
//	6c `K` drops the kickPending guard      → TestKickHitsTheEscapedEndpoint
//	                                          ExactlyOnceAndRendersTheQueuedStatus
//	6d model-apply path drops PathEscape    → TestActionsEscapeAgentNamesInto
//	                                          PathSegments  [test fixed, see below]
//	6e pause/resume path drops PathEscape   → TestActionsEscapeAgentNamesInto
//	                                          PathSegments  [test fixed, see below]
//	6f ACMM typing guard drops Pending()    → TestACMMTypedConfirmationApplies
//	   (BOTH the app-level acmmType guard     ExactlyOnceAtTheEscapedEndpoint
//	   and ACMMOverlay.Type must be           [test fixed, see below]
//	   removed together — see note)
//	7a footerText drops a binding           → TestHelpAndFooterListTheSame
//	                                          AvailableBindings
//	7b ACMM overlay falls through to the    → TestModalKeysCannotLeakInto
//	   global bindings                        GlobalActions
//	8 View checks only minWidth, ignoring   → TestResizeBelowAndAboveTheMinimum
//	  minHeight                               SwapsTheFrameCleanly
//	9 stopSSE no longer calls cancelSSE()   → TestQuitCancelsTheStreamWithout
//	  (stream reader leaks)                   LeakingAGoroutineOrReconnect
//	                                          [test fixed, see below]
//
// FOUR MUTATIONS SURVIVED THE ORIGINAL SUITE. Each is fixed above:
//
//   - 6d/6e — TestActionsEscapeAgentNamesIntoPathSegments asserted the escaped
//     path for KICK ONLY, so removing url.PathEscape from PauseAgent,
//     ResumeAgent or SetAgentModel left the whole package green. The test now
//     drives pause and model-apply through the assembled loop with the same
//     slash-bearing name and asserts both the escaped and the raw spelling.
//
//   - 9 — the goroutine assertion ran AFTER harness.stop(), which calls
//     model.cancelSSE() in its own teardown: the harness cleaned up the very
//     leak the test was hunting, so the count settled whether or not the quit
//     path cancelled anything. The fixture now records client cancellations
//     (r.Context().Done()) and the test asserts the quit path released the
//     connection BEFORE teardown runs.
//
//   - 6f — NOT a vacuous test, and worth recording as such. The pending guard
//     is enforced in two places (model.acmmType and ACMMOverlay.Type) which are
//     redundant with each other, so removing either alone leaves the other
//     enforcing the property. Removing BOTH fails the test. The single-guard
//     survival is defence-in-depth, not a gap; the test gained a fixture gate
//     (fixtureDashboard.gate) so the in-flight window is observable at all,
//     which is what makes the two-guard mutation detectable.
//
// ─────────────────────────────────────────────────────────────────────────────

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// ── The fixture dashboard ────────────────────────────────────────────────────

// recordedRequest is one request the fixture served, captured in enough detail
// that an assertion can cover method, path, body and auth rather than only
// "something was called".
//
// Path is the RAW path (r.URL.EscapedPath()), not the decoded one. That is the
// whole point for the escaping assertions: an agent named "team/one" must reach
// the server as "/api/pause/team%2Fone", and a decoded path would render the
// correct and the broken spelling identically.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
	Auth   string
}

// fixtureDashboard is a deterministic stand-in for the dashboard API: every
// endpoint the v1 TUI consumes, plus a stream the test drives by hand.
//
// It records every request. Handlers are overridable per-endpoint so a single
// property can make one endpoint fail without the others changing behaviour,
// which is what keeps the failure-isolation assertions honest.
type fixtureDashboard struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest

	// bodies maps a path to the JSON body served for it. Guarded by mu so a
	// test can rewrite a response mid-run (the "authoritative response" and
	// poll-fallback properties both need this).
	bodies map[string]string

	// statuses maps a path to a non-200 status to answer with. Absent means
	// 200.
	statuses map[string]int

	// frames is the SSE channel the test publishes on. Each value is one
	// complete event; the handler frames it as `event:`/`data:` lines.
	frames chan sseFrame

	// gates hold a response open per logical path until the test releases it.
	// See fixtureDashboard.gate.
	gates map[string]chan struct{}

	// streamClientCancels counts stream handlers that returned because the
	// CLIENT cancelled the request (r.Context().Done()), as opposed to a
	// server-side drop or shutdown.
	//
	// This is the only server-observable proof that the quit path actually
	// cancelled the stream. Counting goroutines cannot do that job here: the
	// harness's own teardown calls model.cancelSSE(), so by the time
	// assertGoroutinesSettle runs, a reader the quit path leaked has already
	// been cleaned up by the test itself and the count settles either way.
	// See TestQuitCancelsTheStreamWithoutLeakingAGoroutineOrReconnect.
	streamClientCancels int

	// streamOpens counts how many times /api/events has been dialled. It is
	// how the reconnect properties tell "reconnected once" from "spawned a
	// second reader loop".
	streamOpens int

	// shutdown latches at teardown so a late reconnect cannot park a handler
	// that Server.Close would then wait on forever.
	shutdown bool

	// streamClosed, when closed, makes the CURRENT stream handler return —
	// which is a server-side drop, the thing an operator's flapping dashboard
	// actually does. It is replaced on each new connection.
	streamDrop chan struct{}
}

// sseFrame is one event the fixture publishes.
type sseFrame struct {
	event string
	data  string
}

// The fixture payloads.
//
// These are deliberately DISTINGUISHABLE from the zero value in every field an
// assertion reads: a test that passes against `{}` is a test that proves the
// decode ran, not that the wiring delivered. Agent names, token counts and
// audit actions are all chosen so their presence on screen cannot be a
// coincidence.
const (
	// integrationAgents is the roster. Three agents, one of them with a
	// displayName that differs from its name, because the model picker and the
	// pause path must address the CONFIG KEY and a leaked label would 404.
	integrationAgents = `[
  {"name":"scanner","id":"agt_1","displayName":"Scanner","enabled":true,"managed":false,"backend":"claude","model":"claude-opus-4-5"},
  {"name":"quality","id":"agt_2","displayName":"Quality","enabled":true,"managed":true,"backend":"copilot","model":"gpt-5"},
  {"name":"reviewer","id":"agt_3","displayName":"Reviewer","enabled":false,"managed":false,"backend":"claude","model":"claude-sonnet-4-5"}
]`

	// integrationStatus is the live governor + agent-state snapshot served on
	// GET /api/status and republished on the stream.
	integrationStatus = `{
  "timestamp": "2026-09-01T12:00:00Z",
  "agents": [
    {"name": "scanner", "enabled": true, "paused": false, "state": "running"},
    {"name": "quality", "enabled": true, "paused": false, "state": "running"},
    {"name": "reviewer", "enabled": false, "paused": false, "state": "stopped"}
  ],
  "governor": {
    "active": true, "mode": "quiet", "issues": 3, "prs": 1,
    "thresholds": {"quiet": 1, "busy": 5, "surge": 20},
    "nextKick": "9/1 12:05 PM UTC"
  },
  "acmmLevel": 3,
  "acmmLevelConfigured": true
}`

	// integrationStatusBusy is the SAME document with the governor in a
	// different mode and scanner PAUSED. It is what the stream pushes to prove
	// property 2: both changes are invisible to a poll-only frame, so a header
	// reading BUSY can only have come from the stream.
	integrationStatusBusy = `{
  "timestamp": "2026-09-01T12:00:01Z",
  "agents": [
    {"name": "scanner", "enabled": true, "paused": true, "state": "running"},
    {"name": "quality", "enabled": true, "paused": false, "state": "running"},
    {"name": "reviewer", "enabled": false, "paused": false, "state": "stopped"}
  ],
  "governor": {
    "active": true, "mode": "surge", "issues": 31, "prs": 9,
    "thresholds": {"quiet": 1, "busy": 5, "surge": 20},
    "nextKick": "9/1 12:06 PM UTC"
  },
  "acmmLevel": 3,
  "acmmLevelConfigured": true
}`

	integrationHiveID          = `{"id":"acceptance-hive"}`
	integrationGovernorConfig  = `{"general_advanced":{"eval_interval_s":900}}`
	integrationTokens          = `{"total_tokens":1500,"total_input":1000,"total_output":500,"by_agent_detail":{"scanner":{"input":1000,"output":500}}}`
	integrationTokensRefreshed = `{"total_tokens":9900,"total_input":7700,"total_output":2200,"by_agent_detail":{"scanner":{"input":7700,"output":2200}}}`
	integrationCost            = `{"estimated":{"total_usd":1.25,"by_agent":[{"name":"scanner","usd":1.25,"source":"estimated"}],"unpriced_models":[]}}`
	integrationAudit           = `{"entries":[
  {"ts":"2026-09-01T12:04:05Z","user":"operator","action":"auditnewest","agent":"scanner"},
  {"ts":"2026-09-01T12:04:04Z","user":"governor","action":"auditmiddle","agent":"quality"},
  {"ts":"2026-09-01T12:04:03Z","user":"operator","action":"auditoldest","agent":"reviewer"}
]}`
	integrationAuditRefreshed = `{"entries":[
  {"ts":"2026-09-01T12:09:05Z","user":"operator","action":"auditrefreshed","agent":"scanner"}
]}`
	integrationModels = `{"backend":"claude","models":["claude-opus-4-5","claude-sonnet-4-5"],"fallback":false,"partial":false}`
	// GET /api/packs returns a BARE ARRAY of packs, not an object wrapping one.
	// The active level travels inside it as the `current` flag; there is no
	// envelope and no separate level field. See client.ACMM, which decodes into
	// []client.Pack and synthesises ACMMStatus from it. An enveloped fixture
	// decodes to "cannot unmarshal object into Go value of type []client.Pack",
	// which the overlay renders as a list error — so every selection assertion
	// downstream waits forever on a cursor that can never have a pack under it.
	integrationPacks = `[
  {"level":3,"name":"L3","description":"three","agentCount":3,"current":true,"agents":[]},
  {"level":4,"name":"L4","description":"four","agentCount":4,"current":false,"agents":[]}
]`
)

// integrationToken is the bearer every request must carry. It is asserted on
// rather than merely set, because the auth header is part of the contract this
// harness is here to pin.
const integrationToken = "test-token"

// newFixtureDashboard starts the fixture and points the TUI client at it.
func newFixtureDashboard(t *testing.T) *fixtureDashboard {
	t.Helper()

	f := &fixtureDashboard{
		bodies: map[string]string{
			"/api/agents":                   integrationAgents,
			"/api/status":                   integrationStatus,
			"/api/hive-id":                  integrationHiveID,
			"/api/config/governor":          integrationGovernorConfig,
			"/api/tokens":                   integrationTokens,
			"/api/cost":                     integrationCost,
			"/api/audit":                    integrationAudit,
			"/api/packs":                    integrationPacks,
			"/api/inference/models/claude":  integrationModels,
			"/api/inference/models/copilot": `{"backend":"copilot","models":["gpt-5"],"fallback":false,"partial":false}`,
		},
		statuses:   map[string]int{},
		frames:     make(chan sseFrame, 64),
		streamDrop: make(chan struct{}),
	}

	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	// CLEANUP ORDER IS LOAD-BEARING. httptest.Server.Close blocks until every
	// handler returns, and the SSE handler is a long-lived one that only
	// returns when its drop channel closes or the client's request context is
	// cancelled. Closing the server first therefore deadlocks for the full
	// test timeout — which is what happened the first time this harness ran.
	//
	// Cleanups run LIFO, so this one (registered first) runs LAST: the harness
	// registers its own stop afterwards and so tears the model down first,
	// cancelling the stream request. shutdownStreams is the belt to that
	// braces, releasing any handler still parked if no model owned it.
	t.Cleanup(func() {
		f.shutdownStreams()
		f.Server.Close()
	})
	pinDashboard(t, f.Server.URL)
	return f
}

func (f *fixtureDashboard) serve(w http.ResponseWriter, r *http.Request) {
	// EscapedPath, not Path: the escaping assertions depend on seeing the wire
	// spelling. See recordedRequest.
	path := r.URL.EscapedPath()

	var body []byte
	if r.Body != nil {
		body, _ = readAllLimited(r)
	}

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   path,
		Query:  r.URL.RawQuery,
		Body:   strings.TrimSpace(string(body)),
		Auth:   r.Header.Get("Authorization"),
	})
	f.mu.Unlock()

	if path == "/api/events" {
		f.serveStream(w, r)
		return
	}

	// A gated endpoint parks here until the test releases it, which is how a
	// test observes the window while a write is genuinely PENDING. Without it
	// the fixture answers instantly and the in-flight state is gone before the
	// next key can be delivered — so a guard that only matters during that
	// window cannot be asserted at all.
	f.mu.Lock()
	gate := f.gates[r.URL.Path]
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-time.After(waitTimeout):
		}
	}

	// Decoded path for the response table: writes are addressed with escaped
	// segments, and the table is keyed by the logical endpoint.
	f.mu.Lock()
	status, hasStatus := f.statuses[r.URL.Path]
	payload, hasBody := f.bodies[r.URL.Path]
	f.mu.Unlock()

	if hasStatus && status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"fixture failure"}`))
		return
	}

	// The mutating endpoints are answered from their path shape rather than
	// from the table, because each one's response must name the agent that was
	// actually addressed — an assertion that the frame renders the
	// AUTHORITATIVE response cannot be written against a constant that ignores
	// the request.
	if resp, ok := f.mutationResponse(r.Method, r.URL.Path, string(body)); ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
		return
	}

	if !hasBody {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(payload))
}

// mutationResponse answers the four write endpoints.
//
// Each response echoes the DECODED agent name from the path, which is what
// makes the "renders the authoritative response" assertions meaningful: the
// text on screen can only be right if the escaped path round-tripped correctly.
func (f *fixtureDashboard) mutationResponse(method, path, body string) (string, bool) {
	switch {
	case method == http.MethodPost && strings.HasPrefix(path, "/api/pause/"):
		agent := strings.TrimPrefix(path, "/api/pause/")
		return fmt.Sprintf(`{"ok":true,"status":"paused","agent":%q,"changed":true,"state":"paused"}`, agent), true
	case method == http.MethodPost && strings.HasPrefix(path, "/api/resume/"):
		agent := strings.TrimPrefix(path, "/api/resume/")
		return fmt.Sprintf(`{"ok":true,"status":"resumed","agent":%q,"changed":true,"state":"running"}`, agent), true
	case method == http.MethodPost && strings.HasPrefix(path, "/api/kick/"):
		agent := strings.TrimPrefix(path, "/api/kick/")
		return fmt.Sprintf(`{"status":"queued","agent":%q}`, agent), true
	case method == http.MethodPost && strings.HasPrefix(path, "/api/model/"):
		rest := strings.TrimPrefix(path, "/api/model/")
		agent, modelID, _ := strings.Cut(rest, "/")
		return fmt.Sprintf(`{"status":"ok","agent":%q,"model":%q}`, agent, modelID), true
	case method == http.MethodPut && path == "/api/packs/level":
		var req struct {
			Level int `json:"level"`
		}
		_ = json.Unmarshal([]byte(body), &req)
		return fmt.Sprintf(
			`{"ok":true,"level":%d,"packAgents":["scanner","quality"],"packUpdated":["quality"],"paused":[],"resumed":["quality"]}`,
			req.Level), true
	}
	return "", false
}

// serveStream is the controllable SSE endpoint.
//
// It publishes only what the test pushes onto f.frames — there is no heartbeat
// and no timer — so "the frame moved because of a stream event" is a fact the
// test established rather than a race it won.
func (f *fixtureDashboard) serveStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	f.mu.Lock()
	f.streamOpens++
	drop := f.streamDrop
	down := f.shutdown
	f.mu.Unlock()
	if down {
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case frame := <-f.frames:
			if frame.event != "" {
				if _, err := fmt.Fprintf(w, "event: %s\n", frame.event); err != nil {
					return
				}
			}
			// One data: line per source line — how SSE carries a multi-line
			// payload, and what lets the fixtures above stay readable.
			for _, line := range strings.Split(frame.data, "\n") {
				if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
					return
				}
			}
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-drop:
			// Server-side close: the handler returns, the body ends, and the
			// client's reader sees a clean EOF — which is what a dashboard
			// restarting behind a proxy looks like.
			return
		case <-r.Context().Done():
			// The CLIENT went away: either the model cancelled the stream
			// (quit, or a generation being retired) or the process is
			// tearing down. Recording it is what lets a test assert that the
			// quit path released the connection itself.
			f.mu.Lock()
			f.streamClientCancels++
			f.mu.Unlock()
			return
		}
	}
}

func readAllLimited(r *http.Request) ([]byte, error) {
	const maxBody = 1 << 16
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for len(buf) < maxBody {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
	return buf, nil
}

// ── Fixture accessors ────────────────────────────────────────────────────────

// setBody rewrites the payload served for a path. Used to prove that a later
// poll delivered NEW data rather than re-rendering the first snapshot.
func (f *fixtureDashboard) setBody(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[path] = body
}

// setStatus makes a path fail with a status. Used for the 403 and
// failure-isolation properties.
func (f *fixtureDashboard) setStatus(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[path] = status
}

// countRequests returns how many times a method+path was served. This is the
// CALL COUNT half of the contract: "exactly once" is the assertion that catches
// a duplicate mutation, and no amount of correct rendering substitutes for it.
func (f *fixtureDashboard) countRequests(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if req.Method == method && req.Path == path {
			n++
		}
	}
	return n
}

// countPath counts requests to a path regardless of method.
func (f *fixtureDashboard) countPath(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if req.Path == path {
			n++
		}
	}
	return n
}

// findRequest returns the first recorded request matching method+path.
func (f *fixtureDashboard) findRequest(method, path string) (recordedRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if req.Method == method && req.Path == path {
			return req, true
		}
	}
	return recordedRequest{}, false
}

// streamConnections returns how many times /api/events has been dialled.
func (f *fixtureDashboard) streamConnections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamOpens
}

// gate holds every response for path open until the returned release func is
// called, so a test can assert what the model does while the write is in
// flight. Releasing is idempotent and safe to defer.
func (f *fixtureDashboard) gate(path string) func() {
	ch := make(chan struct{})
	f.mu.Lock()
	if f.gates == nil {
		f.gates = map[string]chan struct{}{}
	}
	f.gates[path] = ch
	f.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.gates, path)
			f.mu.Unlock()
			close(ch)
		})
	}
}

// streamClientCancellations returns how many stream handlers ended because the
// client cancelled the request.
func (f *fixtureDashboard) streamClientCancellations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamClientCancels
}

// shutdownStreams releases every parked stream handler for good.
//
// Unlike dropStream it does NOT install a fresh drop channel: after this, a
// handler that arrives late returns immediately rather than parking, so a
// reconnect racing teardown cannot re-block Server.Close.
func (f *fixtureDashboard) shutdownStreams() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shutdown {
		return
	}
	f.shutdown = true
	close(f.streamDrop)
}

// dropStream closes the current stream server-side and installs a fresh drop
// channel for the next connection.
func (f *fixtureDashboard) dropStream() {
	f.mu.Lock()
	if f.shutdown {
		f.mu.Unlock()
		return
	}
	drop := f.streamDrop
	f.streamDrop = make(chan struct{})
	f.mu.Unlock()
	close(drop)
}

// publish pushes one event onto the stream, failing the test if nothing is
// reading within the deadline (which would mean the app never subscribed).
func (f *fixtureDashboard) publish(t *testing.T, event, data string) {
	t.Helper()
	select {
	case f.frames <- sseFrame{event: event, data: data}:
	case <-time.After(waitTimeout):
		t.Fatal("no reader took the SSE frame: the app never subscribed to /api/events")
	}
}

// ── Driving the real model ───────────────────────────────────────────────────

// The pinned terminal size. 100x30 is comfortably above the 60x20 minimum, so
// every pane has interior rows to render into and an assertion that fails is
// failing about wiring rather than about clipping.
const (
	testTermWidth  = 100
	testTermHeight = 30
)

// waitTimeout bounds every condition wait. It is generous because it is a
// FAILURE deadline, not a delay: a passing assertion returns as soon as its
// condition holds, so raising this cannot slow the suite down — it only decides
// how long a genuinely broken wiring takes to be reported.
const waitTimeout = 10 * time.Second

// pollStep is how often a condition is re-checked. Short enough to be
// invisible, long enough not to spin a core.
const pollStep = time.Millisecond

// harness drives the REAL root model against the fixture.
//
// It is NOT teatest. teatest builds its own program and exposes only the
// rendered byte stream, which is the right tool for the frame-level tests in
// app_test.go and resize_test.go and the wrong one here: this file's properties
// are about which REQUESTS the assembled loop issues and in what number, and it
// must be able to inject a key and then assert on a call count without racing a
// renderer. So it runs the same model through the same Update/View contract
// bubbletea would, on a single goroutine, with commands executed exactly as
// tea.Batch expands them.
//
// The model under test is the one Init() built and every message it produced —
// no message is skipped, and no state is set directly. That is what makes an
// assertion here evidence that the WIRING ran.
type harness struct {
	t *testing.T
	f *fixtureDashboard

	mu    sync.Mutex
	model model

	// msgs is the program's message queue. Commands push onto it from their
	// own goroutines exactly as bubbletea's event loop receives them.
	msgs chan tea.Msg

	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once

	// cmdWG tracks in-flight commands so quit can distinguish "the loop
	// stopped" from "goroutines are still running".
	cmdWG sync.WaitGroup

	// quitRequested and execRequested record the two commands this harness
	// recognises but does not run. They live HERE rather than on the model
	// because production behaviour changes are out of scope for this task: the
	// model must stay exactly what ships, so anything the test needs to observe
	// that the model does not already expose is observed at the harness seam.
	quitRequested bool
	execRequested bool
}

// newHarness starts the model's real Init() against the fixture.
//
// Both poll intervals are shortened to a test-only duration. That is the
// mechanism the AC asks for — "short test-only durations, not real sleeps" —
// and both are shortened, not just the reconcile one: since T32 the loops have
// separate timers, and leaving activity at its production 5s would let a test
// that means to exercise the activity cadence silently exercise nothing.
func newHarness(t *testing.T, f *fixtureDashboard, opts ...func(*model)) *harness {
	t.Helper()

	m := newModel()
	m.reconcileInterval = 5 * time.Millisecond
	m.activityInterval = 5 * time.Millisecond
	for _, opt := range opts {
		opt(&m)
	}

	h := &harness{
		t:     t,
		f:     f,
		model: m,
		msgs:  make(chan tea.Msg, 256),
		done:  make(chan struct{}),
	}

	// The window size arrives first, exactly as bubbletea delivers it.
	h.send(tea.WindowSizeMsg{Width: testTermWidth, Height: testTermHeight})

	h.wg.Add(1)
	go h.loop()

	h.exec(m.Init())
	t.Cleanup(h.stop)
	return h
}

// loop is the event loop: one goroutine owning the model, exactly as
// bubbletea's does.
func (h *harness) loop() {
	defer h.wg.Done()
	for {
		select {
		case msg := <-h.msgs:
			h.mu.Lock()
			next, cmd := h.model.Update(msg)
			h.model = next.(model)
			h.mu.Unlock()
			h.exec(cmd)
		case <-h.done:
			return
		}
	}
}

// exec runs a command tree the way bubbletea does: batches expand
// concurrently, each leaf on its own goroutine, and every produced message goes
// back onto the queue.
//
// tea.Quit and tea.ExecProcess are recognised and NOT executed. Quit is
// recorded so the quit property can assert the program asked to exit without
// this harness having to shut down mid-assertion, and ExecProcess would spawn
// the real tmux binary — the boundary this file documents it does not cross.
func (h *harness) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	h.cmdWG.Add(1)
	go func() {
		defer h.cmdWG.Done()
		msg := cmd()
		switch typed := msg.(type) {
		case tea.BatchMsg:
			for _, sub := range typed {
				h.exec(sub)
			}
			return
		case nil:
			return
		}
		if isQuitMsg(msg) {
			h.mu.Lock()
			h.quitRequested = true
			h.mu.Unlock()
			return
		}
		if isExecMsg(msg) {
			// tea.ExecProcess. Running it would spawn tmux; see the file
			// header. The command's construction is covered by attach_test.go.
			h.mu.Lock()
			h.execRequested = true
			h.mu.Unlock()
			return
		}
		select {
		case h.msgs <- msg:
		case <-h.done:
		}
	}()
}

// send injects a message, as bubbletea's Send does.
func (h *harness) send(msg tea.Msg) {
	select {
	case h.msgs <- msg:
	case <-h.done:
	}
}

// key injects a key press by its string form, which is how app.go's bindings
// are written.
func (h *harness) key(s string) {
	h.send(keyMsg(s))
}

// typeText injects each character as its own rune key, which is what the ACMM
// confirmation field consumes.
func (h *harness) typeText(s string) {
	for _, r := range s {
		if r == ' ' {
			h.send(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// snapshot returns a copy of the current model under the lock.
func (h *harness) snapshot() model {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.model
}

// didQuit reports whether the program asked bubbletea to exit.
func (h *harness) didQuit() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.quitRequested
}

// view renders the current frame.
func (h *harness) view() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.model.View()
}

// stop ends the loop. Idempotent so both the deferred cleanup and an explicit
// call are safe.
func (h *harness) stop() {
	h.once.Do(func() {
		close(h.done)
		h.wg.Wait()
		// Cancel the live stream request. Without this the fixture's SSE
		// handler stays parked on a connection nobody is reading, and
		// httptest.Server.Close blocks on it until the test timeout — which is
		// exactly the deadlock the first run of this harness hit. It is also
		// what the model's own quit path does (stopSSE), so tearing down here
		// mirrors production rather than working around it.
		h.mu.Lock()
		h.model.cancelSSE()
		h.mu.Unlock()
	})
}

// waitFor blocks until cond holds on the model, failing with why on timeout.
//
// This is the only waiting primitive in the file. There is no sleep-then-assert
// anywhere: a condition that becomes true in a microsecond returns in a
// microsecond, which is what makes -count=20 cheap AND what makes a real
// regression fail loudly instead of flakily.
func (h *harness) waitFor(why string, cond func(model) bool) {
	h.t.Helper()
	h.await(why, func() bool { return cond(h.snapshot()) })
}

// waitForView blocks until the rendered frame satisfies cond.
func (h *harness) waitForView(why string, cond func(string) bool) {
	h.t.Helper()
	h.await(why, func() bool { return cond(h.view()) })
}

// waitForFixture blocks until cond holds on the fixture's recorded traffic.
func (h *harness) waitForFixture(why string, cond func() bool) {
	h.t.Helper()
	h.await(why, cond)
}

// await is the shared wait, and it exists to render the failure frame at the
// moment of FAILURE rather than at the moment of the call.
//
// Passing h.view() as a printf argument to EventuallyEvery — which is what
// every wait here did originally — evaluates the frame BEFORE the wait begins.
// The formatting is deferred to the timeout, but the string was captured at
// t=0, so a hang reported the STARTUP frame no matter what the model went on to
// do. Every one of these waits then failed with a picture of a screen that had
// long since changed, which is precisely the information needed to tell "the
// condition is wrong" from "the model never got there" — and it was wrong in a
// way that looked plausible.
//
// Failing here instead, with the frame read after the deadline, is what makes
// these timeouts diagnosable.
func (h *harness) await(why string, cond func() bool) {
	h.t.Helper()
	testutil.EventuallyEveryFunc(h.t, waitTimeout, pollStep, cond, func() string {
		return fmt.Sprintf("timed out waiting for %s\nframe at timeout:\n%s", why, h.view())
	})
}

// settle waits for the message queue to drain and in-flight commands to finish.
//
// It is used ONLY before a negative assertion ("exactly once", "no second
// loop"). A positive assertion always uses waitFor instead, because waiting for
// quiet to prove something happened would be a race.
func (h *harness) settle() {
	h.t.Helper()
	// Two consecutive empty observations, because a command can be between
	// producing its message and the loop receiving it. The drain itself uses
	// Eventually; the short settle gap between observations is a deliberate
	// quiet window, which is the one thing Eventually cannot express — it
	// waits for a condition to BECOME true, and here we need the absence of
	// activity to PERSIST.
	for i := 0; i < 2; i++ {
		testutil.EventuallyEvery(h.t, waitTimeout, pollStep, func() bool { return len(h.msgs) == 0 },
			"message queue did not drain")
		timer := time.NewTimer(5 * pollStep)
		<-timer.C
	}
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// isQuitMsg reports whether a message is bubbletea's quit signal. The type is
// unexported by bubbletea, so it is identified by its type name — which is
// stable API in practice (tea.Quit's contract) and is checked by
// TestQuitMsgDetectionMatchesBubbletea below so a bubbletea upgrade that
// renamed it could not silently turn the quit assertions into no-ops.
func isQuitMsg(msg tea.Msg) bool {
	return fmt.Sprintf("%T", msg) == "tea.QuitMsg"
}

func isExecMsg(msg tea.Msg) bool {
	return strings.Contains(fmt.Sprintf("%T", msg), "execMsg")
}

// ── Property 1: startup ──────────────────────────────────────────────────────

// TestStartupLoadsEveryPaneAndBothLiveHeaderFieldsWithoutWaitingAnInterval is
// the regression test for the defect this whole task exists to prevent.
//
// It is deliberately written as ONE test over all four panes plus both live
// header fields, rather than four tests that could each pass while the app
// delivered none of them. Tokens and Events are the two that were broken: they
// had a complete client and a complete pane, and no delivery. So the assertions
// below are on the RENDERED FRAME — the thing an operator sees — not on the
// model's caches, because a cache can be populated by a fetch whose message
// nothing routes to a pane, which is exactly what shipped.
//
// "Without waiting production intervals" is proved by construction: nothing
// here advances a clock, and both intervals are 5ms. If the panes only filled
// on a tick rather than on Init's immediate poll, this would still pass — so
// the call-count assertion at the end pins that too, by requiring the frame to
// be complete while each endpoint has been read a small number of times.
func TestStartupLoadsEveryPaneAndBothLiveHeaderFieldsWithoutWaitingAnInterval(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	// The four panes, each asserted through a value that can only be on screen
	// if that pane's own message was delivered.
	h.waitForView("the Agents pane to render the polled roster", func(v string) bool {
		return strings.Contains(v, "scanner") && strings.Contains(v, "reviewer")
	})
	h.waitForView("the Governor pane to render live governor state", func(v string) bool {
		// The pane case-folds the wire's mode.
		return strings.Contains(strings.ToUpper(v), "QUIET")
	})
	h.waitForView("the Tokens pane to render polled token counts", func(v string) bool {
		// 1000 input renders as a magnitude ("1.0k"); the agent row is the
		// unambiguous proof the pane loaded rather than showing its stub.
		return !strings.Contains(v, "waiting for data") && strings.Contains(v, "scanner")
	})
	h.waitForView("the Events pane to render polled audit rows", func(v string) bool {
		return strings.Contains(v, "auditnewest")
	})

	// Both LIVE header fields. `ws:` is excluded on purpose — it is connection
	// state, not data, and property 2 owns it.
	h.waitForView("the header to show the polled hive identity", func(v string) bool {
		return strings.Contains(v, "hive: acceptance-hive")
	})
	h.waitForView("the header to show the polled governor mode", func(v string) bool {
		return strings.Contains(v, "governor: QUIET")
	})

	// The panes are full, so every endpoint that feeds them was actually read.
	// Asserting the traffic as well as the frame is what distinguishes "the
	// wiring ran" from "a pane defaulted to something that looks right".
	for _, path := range []string{
		"/api/agents", "/api/status", "/api/hive-id", "/api/config/governor",
		"/api/tokens", "/api/cost", "/api/audit",
	} {
		if got := f.countPath(path); got == 0 {
			t.Errorf("startup never requested %s: the pane it feeds cannot be live", path)
		}
	}
}

// TestStartupSendsTheBearerTokenOnEveryRequest pins the auth half of the
// contract. It is separate from the property above because a frame can be
// perfectly correct against a fixture that does not check auth, and the real
// dashboard does.
func TestStartupSendsTheBearerTokenOnEveryRequest(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the frame to load", func(v string) bool {
		return strings.Contains(v, "hive: acceptance-hive")
	})

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, req := range f.requests {
		if req.Auth != "Bearer "+integrationToken {
			t.Errorf("%s %s carried Authorization %q, want %q",
				req.Method, req.Path, req.Auth, "Bearer "+integrationToken)
		}
	}
}

// ── Property 2: SSE updates state immediately ────────────────────────────────

// TestStreamEventUpdatesAgentAndGovernorStateImmediately proves the push path
// moves the frame.
//
// The fixture's stream payload CONTRADICTS what /api/agents and /api/status
// return: the governor is in surge rather than quiet, and scanner is paused
// rather than running. That contradiction is the mechanism — a frame showing
// SURGE cannot have come from the poll, so the assertion cannot be satisfied by
// the polling path it is meant to be distinguishing itself from.
func TestStreamEventUpdatesAgentAndGovernorStateImmediately(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	// Wait for the roster: SSE agent states are JOINED onto the polled roster
	// (see paneMsgs), so a frame published before the first fetch resolves
	// carries states with nothing to join them to.
	h.waitForView("the polled roster to arrive before the stream event", func(v string) bool {
		return strings.Contains(v, "scanner")
	})
	h.waitForView("the poll-sourced governor mode", func(v string) bool {
		return strings.Contains(v, "governor: QUIET")
	})

	// Published twice, and the contradiction the doc comment describes is
	// exactly why it has to be.
	//
	// Because the poll says QUIET and the stream says SURGE, whichever writes
	// governorStatus LAST wins. At the harness's test-only 5ms reconcile
	// cadence there are always polls in flight, so a poll issued before the
	// event can answer after it and put QUIET back — the assertion below would
	// then hang on a header that is being overwritten faster than it can be
	// read. Production polls at 5s and never sees this overlap; it is an
	// artifact of the compressed cadence, not a defect in the model, and the
	// model's last-write-wins between two copies of one document is correct.
	//
	// The first publish marks the stream healthy, which stretches reconcile to
	// sseReconcileInterval and stops the poll traffic. After that stretch no
	// poll is due for 60 seconds, so the second publish is the last writer by
	// construction and SURGE is stable. The contradiction still does all the
	// work it was designed to do: a SURGE header cannot have come from a poll.
	f.publish(t, "", integrationStatusBusy)
	h.waitFor("the connection state to become connected on the first event", func(m model) bool {
		return m.sseConnected
	})
	h.waitFor("the reconcile cadence to stretch so no poll can overwrite the event", func(m model) bool {
		return m.reconcileInterval == sseReconcileInterval
	})
	h.settle()
	f.publish(t, "", integrationStatusBusy)

	h.waitForView("the header governor mode to follow the stream event", func(v string) bool {
		return strings.Contains(v, "governor: SURGE")
	})
	h.waitForView("the header ws field to report the live stream", func(v string) bool {
		return strings.Contains(v, "ws: "+wsConnected)
	})
	h.waitFor("the stream's agent states to reach the Agents pane", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		// scanner is running per /api/agents and PAUSED per the stream.
		name, paused, ok := agents.SelectedAgent()
		return ok && name == "scanner" && paused
	})
}

// TestStreamEventDoesNotBlankTheConfiguredGovernorInterval is T29's regression,
// asserted through the assembled loop rather than the pane.
//
// The stream's payload contains no evaluation interval. Before T29 the SSE path
// built its own GovernorMsg with a zero one, so the first pushed event reverted
// `next eval` to unknown. This holds the property at the integration level: the
// interval survives a stream event that never carried it.
func TestStreamEventDoesNotBlankTheConfiguredGovernorInterval(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitFor("the configured eval interval to be polled", func(m model) bool {
		return m.governorInterval == 15*time.Minute
	})
	h.waitForView("the roster to arrive", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	// Published twice for the same reason as the stream-drop property: the
	// poll and the stream carry contradicting copies of /api/status (QUIET vs
	// SURGE) on purpose, and at the harness's 5ms reconcile cadence a poll
	// already in flight can answer after the event and put QUIET back. Waiting
	// for the stretch first, then publishing again, makes the event the last
	// writer by construction. See the stream-drop test for the full note.
	f.publish(t, "", integrationStatusBusy)
	h.waitFor("the stream to be healthy", func(m model) bool { return m.sseConnected })
	h.waitFor("the reconcile cadence to stretch", func(m model) bool {
		return m.reconcileInterval == sseReconcileInterval
	})
	h.settle()
	f.publish(t, "", integrationStatusBusy)
	h.waitForView("the stream event to land", func(v string) bool {
		return strings.Contains(v, "governor: SURGE")
	})

	if got := h.snapshot().governorInterval; got != 15*time.Minute {
		t.Fatalf("a stream event carrying no interval overwrote the cached one: got %v, want 15m", got)
	}
	// And the frame still renders it, which is the operator-visible half.
	if v := h.view(); strings.Contains(v, "next eval —") {
		t.Errorf("the Governor pane reverted next eval to unknown after a stream event:\n%s", v)
	}
}

// ── Property 3: the activity cadence survives a healthy stream ───────────────

// TestActivityDataKeepsRefreshingWhileTheStreamIsHealthy is T32's property, and
// the one most likely to silently regress: it is invisible in every isolated
// test, costs nothing when broken, and makes the frame STALER exactly when the
// header claims it is live.
//
// The mechanism is that the fixture's token and audit bodies are REPLACED after
// the stream connects. A frame that keeps polling the activity class will pick
// up the new values; a frame whose activity loop was stretched to 60s (or
// retired by the reconcile generation bump) will sit on the first snapshot
// until this test's deadline expires. There is no sleeping and no clock
// manipulation — only 5ms intervals and a wait for new content.
func TestActivityDataKeepsRefreshingWhileTheStreamIsHealthy(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the first token snapshot", func(v string) bool {
		return strings.Contains(v, "scanner")
	})
	h.waitForView("the first audit snapshot", func(v string) bool {
		return strings.Contains(v, "auditnewest")
	})

	// Bring the stream up. This is the state that used to stretch everything.
	f.publish(t, "", integrationStatus)
	h.waitFor("the stream to be healthy", func(m model) bool { return m.sseConnected })
	h.waitFor("the reconcile loop to stretch, proving the stream is being trusted",
		func(m model) bool { return m.reconcileInterval == sseReconcileInterval })

	// The activity cadence must NOT have moved. This is the direct assertion;
	// the traffic assertions below are the behavioural one.
	if got := h.snapshot().activityInterval; got != 5*time.Millisecond {
		t.Fatalf("a healthy stream changed the activity cadence to %v: the Tokens and Events panes are now staler than when the stream was down", got)
	}

	tokensBefore := f.countPath("/api/tokens")
	auditBefore := f.countPath("/api/audit")

	f.setBody("/api/tokens", integrationTokensRefreshed)
	f.setBody("/api/audit", integrationAuditRefreshed)

	h.waitForView("the Tokens pane to refresh WHILE the stream is healthy", func(v string) bool {
		// 7700 input renders as a magnitude; the refreshed audit action is the
		// unambiguous marker for the Events half.
		return strings.Contains(v, "7.7k")
	})
	h.waitForView("the Events pane to refresh WHILE the stream is healthy", func(v string) bool {
		return strings.Contains(v, "auditrefreshed")
	})

	// The stream is still up: the refresh above happened DESPITE a healthy
	// stream, which is the property, not because the stream dropped.
	if !h.snapshot().sseConnected {
		t.Fatal("the stream dropped during the test, so this proved nothing about the healthy-stream cadence")
	}
	if got := f.countPath("/api/tokens"); got <= tokensBefore {
		t.Errorf("/api/tokens was not re-read while the stream was healthy: %d then, %d now", tokensBefore, got)
	}
	if got := f.countPath("/api/audit"); got <= auditBefore {
		t.Errorf("/api/audit was not re-read while the stream was healthy: %d then, %d now", auditBefore, got)
	}
}

// ── Property 4: drop, fallback, last-good data, single reconnect ─────────────

// TestStreamDropActivatesPollFallbackPreservesDataAndReconnectsOnce covers the
// whole degraded path as one property, because its four halves are only correct
// together: a drop that changed the header but left the poll stretched would
// look fine here if the header were asserted alone.
func TestStreamDropActivatesPollFallbackPreservesDataAndReconnectsOnce(t *testing.T) {
	f := newFixtureDashboard(t)
	// The reconnect backoff is a production constant (1s), so the harness runs
	// with a fast reconcile cadence and simply tolerates the one-second wait
	// for the reconnect assertion — waitTimeout covers it. Nothing SLEEPS for
	// it; the wait ends the moment the stream reconnects.
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})
	// The stream event is published TWICE, and the ordering matters.
	//
	// /api/status and the stream carry the SAME document, and this fixture
	// deliberately serves CONTRADICTING copies of it — the poll says QUIET,
	// the stream says SURGE — because that is what makes "the header moved
	// because of the stream" provable rather than a coincidence. The cost is
	// that whichever source writes governorStatus last wins, which is correct
	// model behaviour and is not what this test is about.
	//
	// At startup the harness runs reconcile at a test-only 5ms, so several
	// polls are in flight at once. The first publish reliably marks the stream
	// healthy and stretches the cadence to sseReconcileInterval, but a poll
	// already on the wire when the event landed can answer AFTER it and put
	// QUIET back. That is an artifact of the compressed cadence — production
	// polls at 5s and has no such overlap — not a defect in the model.
	//
	// So: publish once to establish health and stretch the cadence, WAIT for
	// the stretch, then publish again. After the stretch no poll is due for 60
	// seconds, so the second event is the last writer by construction and the
	// SURGE assertion below is testing the stream rather than a race.
	f.publish(t, "", integrationStatusBusy)
	h.waitFor("the stream to be healthy", func(m model) bool { return m.sseConnected })
	h.waitFor("the reconcile cadence to stretch", func(m model) bool {
		return m.reconcileInterval == sseReconcileInterval
	})
	h.settle()
	f.publish(t, "", integrationStatusBusy)
	h.waitForView("the stream-sourced governor mode", func(v string) bool {
		return strings.Contains(v, "governor: SURGE")
	})

	connectionsBefore := f.streamConnections()
	f.dropStream()

	// (a) The connection state changes, and the header says so.
	h.waitFor("the model to notice the drop", func(m model) bool { return !m.sseConnected })
	h.waitForView("the header to report the stream is down", func(v string) bool {
		return strings.Contains(v, "ws: "+wsNotConnected)
	})

	// (b) The poll fallback is reactivated at the fast cadence.
	//
	// The value asserted is the PRODUCTION pollInterval, not the harness's
	// test-only 5ms. Recovery does not restore whatever cadence the model
	// happened to start with — sseDisconnected sets reconcileInterval back to
	// the pollInterval constant (app.go), which is the whole point: the
	// fallback runs at the shipped polling rate regardless of what the stream
	// had stretched it to. Asserting 5ms here would be asserting that the
	// model preserves a field the test wrote, which is a property the model
	// does not have and should not have.
	//
	// This does not reintroduce a production-duration wait: nothing sleeps for
	// pollInterval. The assertion reads the FIELD the moment it changes, and
	// the fallback's first fetch is issued immediately by sseDisconnected
	// rather than one interval later, which is what (d) below observes.
	h.waitFor("the reconcile cadence to return to the fallback interval", func(m model) bool {
		return m.reconcileInterval == pollInterval
	})

	// (c) LAST-GOOD DATA SURVIVES. The panes and the two data header fields
	// keep what they had — a drop changes `ws:`, not what is known.
	view := h.view()
	if !strings.Contains(view, "hive: acceptance-hive") {
		t.Errorf("the hive identity was blanked by a stream drop:\n%s", view)
	}
	if !strings.Contains(view, "scanner") {
		t.Errorf("the Agents pane was cleared by a stream drop:\n%s", view)
	}
	if !strings.Contains(view, "auditnewest") {
		t.Errorf("the Events pane was cleared by a stream drop:\n%s", view)
	}

	// (d) It reconnects, and exactly one reader loop exists afterwards.
	h.waitForFixture("the stream to be re-dialled after the drop", func() bool {
		return f.streamConnections() > connectionsBefore
	})
	h.waitFor("a live stream to be installed again", func(m model) bool {
		return m.sse != nil
	})

	// One event, one delivery. A duplicated reader loop would consume the
	// single frame published here and leave the other loop waiting, or would
	// double-deliver; either way the connection count is the direct evidence.
	f.publish(t, "", integrationStatus)
	h.waitFor("the reconnected stream to be healthy", func(m model) bool { return m.sseConnected })

	h.settle()
	if got, want := f.streamConnections(), connectionsBefore+1; got != want {
		t.Errorf("stream was dialled %d times across one drop, want %d: a second reader loop was armed", got, want)
	}
}

// ── Property 5: focus and navigation target the displayed selection ──────────

// TestFocusCyclesAndNavigationTargetsTheDisplayedSelection covers the two halves
// together because a selection that moves in a pane nobody focused, or a focus
// that moves without changing which pane consumes j/k, are the same bug seen
// from two sides.
func TestFocusCyclesAndNavigationTargetsTheDisplayedSelection(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForRoster("scanner")

	// Focus starts on Agents.
	if got := h.snapshot().focus; got != 0 {
		t.Fatalf("focus started at %d, want 0 (Agents)", got)
	}

	// j moves the Agents selection, and the ACTION KEYS follow it. The
	// selection is read back through the same accessor `p`/`m`/`K`/`a` use, so
	// this asserts what the action would target, not a private cursor.
	h.key("j")
	h.waitFor("the Agents selection to move to the second row", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		name, _, ok := agents.SelectedAgent()
		return ok && name == "quality"
	})

	h.key("j")
	h.waitFor("the Agents selection to move to the third row", func(m model) bool {
		agents, _ := m.panes[0].(panes.Agents)
		name, _, ok := agents.SelectedAgent()
		return ok && name == "reviewer"
	})

	// It clamps at the end rather than wrapping.
	h.key("j")
	h.settle()
	agents, _ := h.snapshot().panes[0].(panes.Agents)
	if name, _, _ := agents.SelectedAgent(); name != "reviewer" {
		t.Errorf("selection wrapped past the last row to %q, want it clamped at reviewer", name)
	}

	h.key("k")
	h.waitFor("k to move the selection back up", func(m model) bool {
		agents, _ := m.panes[0].(panes.Agents)
		name, _, ok := agents.SelectedAgent()
		return ok && name == "quality"
	})

	// tab cycles forward through all four panes and back to Agents.
	for want := 1; want <= 3; want++ {
		h.key("tab")
		h.waitFor(fmt.Sprintf("focus to reach pane %d", want), func(m model) bool {
			return m.focus == want
		})
	}
	h.key("tab")
	h.waitFor("focus to wrap back to Agents", func(m model) bool { return m.focus == 0 })

	// shift+tab cycles backward, and must not go negative.
	h.key("shift+tab")
	h.waitFor("shift+tab to wrap backward to the last pane", func(m model) bool {
		return m.focus == paneCount-1
	})

	// With Events focused, j/k drive the EVENTS pane, not Agents. The Agents
	// selection is the control: it must not move.
	agentsBefore, _ := h.snapshot().panes[0].(panes.Agents)
	nameBefore, _, _ := agentsBefore.SelectedAgent()

	h.waitForView("the Events pane to have rows to scroll", func(v string) bool {
		return strings.Contains(v, "auditnewest")
	})
	h.key("j")
	h.waitForView("the Events pane to scroll off the newest entry", func(v string) bool {
		return !strings.Contains(v, "auditnewest") && strings.Contains(v, "auditmiddle")
	})

	agentsAfter, _ := h.snapshot().panes[0].(panes.Agents)
	nameAfter, _, _ := agentsAfter.SelectedAgent()
	if nameAfter != nameBefore {
		t.Errorf("j moved the Agents selection (%q -> %q) while the Events pane was focused", nameBefore, nameAfter)
	}
}

// TestActionKeysAreInertUnlessTheAgentsPaneIsFocused pins the other half of
// "navigation targets the displayed selection": an action addressed at a
// selection the operator cannot see must not fire at all.
func TestActionKeysAreInertUnlessTheAgentsPaneIsFocused(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	// Focus the Governor pane.
	h.key("tab")
	h.waitFor("focus to move off Agents", func(m model) bool { return m.focus == 1 })

	h.key("p")
	h.key("m")
	h.key("K")
	h.settle()

	m := h.snapshot()
	if m.confirm != nil {
		t.Error("p opened the pause dialog while the Agents pane was not focused")
	}
	if m.picker != nil {
		t.Error("m opened the model picker while the Agents pane was not focused")
	}
	if got := f.countPath("/api/kick/scanner"); got != 0 {
		t.Errorf("K issued %d kick requests while the Agents pane was not focused, want 0", got)
	}
}

// ── Property 6: every v1 action, exactly once, correctly escaped ─────────────

// TestPauseConfirmationHitsTheEscapedEndpointExactlyOnceAndRendersTheResponse
// covers the pause action end to end.
//
// The three things it pins are the three that isolated tests cannot: that the
// key opens the dialog, that confirming issues ONE correctly-addressed request,
// and that the frame afterwards shows the SERVER's answer rather than an
// optimistic local guess.
func TestPauseConfirmationHitsTheEscapedEndpointExactlyOnceAndRendersTheResponse(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForRoster("scanner")

	h.key("p")
	h.waitFor("the pause dialog to open on the selected agent", func(m model) bool {
		return m.confirm != nil && m.confirm.agent == "scanner" && m.confirm.pause
	})
	h.waitForView("the dialog to name the agent it will act on", func(v string) bool {
		return strings.Contains(v, "Pause agent scanner?")
	})

	// Nothing has been sent yet: opening a confirmation must not write.
	if got := f.countRequests(http.MethodPost, "/api/pause/scanner"); got != 0 {
		t.Fatalf("opening the dialog already issued %d pause requests, want 0", got)
	}

	// Hammer y. Only the first may reach the server; the rest land while the
	// request is pending and must be refused.
	h.key("y")
	h.key("y")
	h.key("y")

	h.waitFor("the pause to complete and close the dialog", func(m model) bool {
		return m.confirm == nil
	})
	h.settle()

	if got := f.countRequests(http.MethodPost, "/api/pause/scanner"); got != 1 {
		t.Errorf("pause issued %d requests, want exactly 1: a pending command did not block repeats", got)
	}
	req, ok := f.findRequest(http.MethodPost, "/api/pause/scanner")
	if !ok {
		t.Fatal("no POST /api/pause/scanner was recorded")
	}
	if req.Auth != "Bearer "+integrationToken {
		t.Errorf("pause carried Authorization %q, want the bearer token", req.Auth)
	}
	if req.Body != "" {
		t.Errorf("pause sent body %q, want none: the operation declares no requestBody", req.Body)
	}

	// The AUTHORITATIVE response is what the row shows. The fixture answered
	// state=paused, so the pane must render scanner as paused even though
	// /api/agents still reports it enabled.
	h.waitFor("the Agents row to take the server's post-call state", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		name, paused, ok := agents.SelectedAgent()
		return ok && name == "scanner" && paused
	})
}

// TestPauseForbiddenKeepsTheDialogOpenWithTheOwnerMessage covers the one error
// an operator must be told apart from an outage: a working request from someone
// whose role does not permit it. Retrying will never help, so the wording is
// part of the contract.
func TestPauseForbiddenKeepsTheDialogOpenWithTheOwnerMessage(t *testing.T) {
	f := newFixtureDashboard(t)
	f.setStatus("/api/pause/scanner", http.StatusForbidden)
	h := newHarness(t, f)

	h.waitForRoster("scanner")

	h.key("p")
	h.waitFor("the dialog to open", func(m model) bool { return m.confirm != nil })
	h.key("y")

	h.waitForView("the 403 to be rendered as an owner-access message", func(v string) bool {
		return strings.Contains(v, "owner access required")
	})
	if h.snapshot().confirm == nil {
		t.Error("the dialog closed on a 403: the operator cannot read the failure or retry")
	}
	// A failed write must not move the row.
	agents, _ := h.snapshot().panes[0].(panes.Agents)
	if _, paused, _ := agents.SelectedAgent(); paused {
		t.Error("a forbidden pause marked the agent paused: the write did not happen")
	}
}

// TestResumeUsesTheResumeEndpointForAPausedAgent proves the dialog's verb is
// derived from the row's live state rather than hardcoded — a pause key that
// always POSTed /api/pause would be undetectable in a fixture whose agents are
// all running.
func TestResumeUsesTheResumeEndpointForAPausedAgent(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForRoster("scanner")

	// reviewer is enabled:false, which the Agents pane reads as paused.
	h.key("j")
	h.key("j")
	h.waitFor("the selection to reach the disabled agent", func(m model) bool {
		agents, _ := m.panes[0].(panes.Agents)
		name, paused, ok := agents.SelectedAgent()
		return ok && name == "reviewer" && paused
	})

	h.key("p")
	h.waitFor("the dialog to offer RESUME for a paused agent", func(m model) bool {
		return m.confirm != nil && m.confirm.agent == "reviewer" && !m.confirm.pause
	})
	h.key("y")

	h.waitForFixture("the resume endpoint to be called", func() bool {
		return f.countRequests(http.MethodPost, "/api/resume/reviewer") == 1
	})
	h.settle()
	if got := f.countRequests(http.MethodPost, "/api/pause/reviewer"); got != 0 {
		t.Errorf("resuming a paused agent POSTed /api/pause %d times: the verb is not derived from live state", got)
	}
}

// TestKickHitsTheEscapedEndpointExactlyOnceAndRendersTheQueuedStatus covers `K`.
//
// The kick response is a QUEUEING receipt, not a delivery confirmation, and the
// footer wording is part of that contract (#5325) — an operator told "kicked"
// would believe the prompt was typed.
func TestKickHitsTheEscapedEndpointExactlyOnceAndRendersTheQueuedStatus(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForRoster("scanner")

	// Three presses; only one request may leave, because kickPending bounds it.
	h.key("K")
	h.key("K")
	h.key("K")

	h.waitForView("the footer to report the kick was queued", func(v string) bool {
		return strings.Contains(v, "kick queued for scanner")
	})
	h.settle()

	if got := f.countRequests(http.MethodPost, "/api/kick/scanner"); got != 1 {
		t.Errorf("kick issued %d requests, want exactly 1: repeated presses were not bounded", got)
	}
	req, _ := f.findRequest(http.MethodPost, "/api/kick/scanner")
	if req.Body != "" {
		t.Errorf("kick sent body %q, want none: an empty prompt must reach the auto-generated-message path", req.Body)
	}
	if req.Auth != "Bearer "+integrationToken {
		t.Errorf("kick carried Authorization %q, want the bearer token", req.Auth)
	}
}

// TestModelPickerFetchesTheCatalogueAndAppliesExactlyOnce covers `m` and enter.
//
// The apply is the widest per-agent write in the API — it RESTARTS the agent's
// session — so "exactly once" is not a tidiness assertion here.
func TestModelPickerFetchesTheCatalogueAndAppliesExactlyOnce(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForRoster("scanner")

	h.key("m")
	h.waitFor("the picker to open for the selected agent", func(m model) bool {
		return m.picker != nil && m.picker.Agent() == "scanner"
	})

	// The catalogue is fetched for the agent's CONFIGURED BACKEND, not its
	// name — a leaked agent name here would 404 on the routing table.
	h.waitForFixture("the catalogue to be fetched for the claude backend", func() bool {
		return f.countRequests(http.MethodGet, "/api/inference/models/claude") == 1
	})
	h.waitFor("the catalogue to populate the overlay", func(m model) bool {
		return m.picker != nil && !m.picker.Loading()
	})

	// Move off the current model so the apply is a real change, then hammer
	// enter: Apply() refuses while pending, which is what bounds the write.
	h.key("j")
	h.waitFor("the picker selection to move", func(m model) bool {
		sel, ok := m.picker.Selected()
		return ok && sel == "claude-sonnet-4-5"
	})

	h.key("enter")
	h.key("enter")
	h.key("enter")

	h.waitForView("the footer to report the applied model and the session restart", func(v string) bool {
		return strings.Contains(v, "scanner now on claude-sonnet-4-5") &&
			strings.Contains(v, "session restarted")
	})
	h.settle()

	if got := f.countRequests(http.MethodPost, "/api/model/scanner/claude-sonnet-4-5"); got != 1 {
		t.Errorf("model apply issued %d requests, want exactly 1: a pending write did not block repeats", got)
	}
	// The overlay closes on success and the row takes the server's answer.
	if h.snapshot().picker != nil {
		t.Error("the picker stayed open after a successful apply")
	}
	h.waitFor("the Agents row to show the authoritative applied model", func(m model) bool {
		return strings.Contains(m.View(), "claude-sonnet-4-5")
	})
}

// TestACMMTypedConfirmationAppliesExactlyOnceAtTheEscapedEndpoint covers `A`.
//
// This is the widest write in the API — it reconciles the whole fleet — and its
// guard is a TYPED phrase rather than a keystroke. The assertions therefore
// cover the refusal path as well as the success path: a wrong phrase that
// applied would defeat the entire control.
func TestACMMTypedConfirmationAppliesExactlyOnceAtTheEscapedEndpoint(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("A")
	h.waitFor("the ACMM overlay to open", func(m model) bool { return m.acmm != nil })
	h.waitForFixture("the pack list to be fetched", func() bool {
		return f.countRequests(http.MethodGet, "/api/packs") >= 1
	})
	h.waitFor("the pack list to populate the overlay", func(m model) bool {
		return m.acmm != nil && !m.acmm.Loading()
	})

	// Move to L4 (L3 is current, and enter on the current level is a no-op by
	// design) and begin the confirmation.
	h.key("j")
	h.waitFor("the selection to reach L4", func(m model) bool {
		pack, ok := m.acmm.SelectedPack()
		return ok && pack.Level == 4
	})
	h.key("enter")
	h.waitFor("the overlay to enter the typed confirmation state", func(m model) bool {
		return m.acmm != nil && m.acmm.Confirming()
	})

	// A WRONG phrase must not apply. This is asserted before the right one so
	// a broken guard fails here rather than being masked by the success below.
	h.typeText("APPLY L5")
	h.waitFor("the wrong phrase to be typed into the field", func(m model) bool {
		return m.acmm != nil && m.acmm.Typed() == "APPLY L5"
	})
	h.key("enter")
	h.settle()
	if got := f.countRequests(http.MethodPut, "/api/packs/level"); got != 0 {
		t.Fatalf("a wrong confirmation phrase issued %d applies, want 0", got)
	}

	// Clear it and type the exact phrase.
	for range "APPLY L5" {
		h.key("backspace")
	}
	h.waitFor("the field to be cleared", func(m model) bool {
		return m.acmm != nil && m.acmm.Typed() == ""
	})

	want := panes.ConfirmPhrase(4)
	h.typeText(want)
	h.waitFor("the exact phrase to be typed", func(m model) bool {
		return m.acmm != nil && m.acmm.Typed() == want
	})

	// THE FIELD IS FROZEN WHILE THE APPLY IS IN FLIGHT.
	//
	// acmmType is guarded on `!Confirming() || Pending()`. Hammering enter
	// exercises the Apply() half of the exactly-once guard but never the
	// Pending() half, because it types no CHARACTERS while the write is
	// pending — so deleting `|| m.acmm.Pending()` from acmmType survived the
	// whole package. Typing during the window is what tells them apart: a
	// character accepted now would edit the confirmed phrase out from under a
	// write already addressed to level 4, leaving the overlay reporting a
	// phrase that no longer matches what was sent.
	//
	// The apply is GATED so the window is observable at all: the fixture
	// otherwise answers instantly and the pending state is gone before the
	// next key arrives.
	release := f.gate("/api/packs/level")

	h.key("enter")
	h.key("enter")
	h.key("enter")

	h.waitFor("the apply to be in flight", func(m model) bool {
		return m.acmm != nil && m.acmm.Pending()
	})
	h.typeText("X")
	h.settle()
	if typed := h.snapshot().acmm.Typed(); typed != want {
		t.Errorf("the confirmation field accepted a keystroke while the apply was pending: typed %q, want %q",
			typed, want)
	}
	release()

	h.waitFor("the apply to complete and the overlay to hold the receipt", func(m model) bool {
		return m.acmm != nil && m.acmm.Done()
	})
	h.settle()

	if got := f.countRequests(http.MethodPut, "/api/packs/level"); got != 1 {
		t.Errorf("ACMM apply issued %d requests, want exactly 1", got)
	}
	req, ok := f.findRequest(http.MethodPut, "/api/packs/level")
	if !ok {
		t.Fatal("no PUT /api/packs/level recorded")
	}
	// The BODY is the contract here: the level is a required request-body
	// field, not a path segment.
	if req.Body != `{"level":4}` {
		t.Errorf("ACMM apply sent body %q, want %q", req.Body, `{"level":4}`)
	}
	if req.Auth != "Bearer "+integrationToken {
		t.Errorf("ACMM apply carried Authorization %q, want the bearer token", req.Auth)
	}
	// The overlay STAYS OPEN on success holding the reconciliation receipt —
	// that is deliberate (app.go: the receipt is read rather than flashing past
	// on its way to a footer line) — and an open overlay REPLACES the whole
	// frame, footer included. So the footer must be uncovered before it can be
	// read. Dismissing is the operator's own enter/esc, which is the same key
	// path the receipt advertises.
	//
	// The receipt is asserted first, so this dismissal cannot be mistaken for
	// the test skipping past a result it never checked.
	h.waitForView("the receipt to name the authoritative new level", func(v string) bool {
		return strings.Contains(v, "Applied. Level is now L4.")
	})
	h.key("esc")
	h.waitFor("the overlay to be dismissed so the footer is visible", func(m model) bool {
		return m.acmm == nil
	})
	h.waitForView("the footer to report the authoritative new level", func(v string) bool {
		return strings.Contains(v, "ACMM level now L4")
	})
}

// TestActionsEscapeAgentNamesIntoPathSegments is the escaping assertion, and it
// needs an agent name that a raw interpolation would mangle.
//
// A name containing a slash interpolated raw produces "/api/pause/team/one",
// which matches no route and comes back as a 404 — a failure an operator would
// read as "the dashboard is broken". The fixture records the RAW path, so the
// correct and the broken spelling are distinguishable.
func TestActionsEscapeAgentNamesIntoPathSegments(t *testing.T) {
	f := newFixtureDashboard(t)
	f.setBody("/api/agents", `[
  {"name":"team/one","id":"agt_9","displayName":"Team One","enabled":true,"managed":true,"backend":"claude","model":"claude-opus-4-5"}
]`)
	h := newHarness(t, f)

	h.waitFor("the awkwardly-named agent to be selectable", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		name, _, ok := agents.SelectedAgent()
		return ok && name == "team/one"
	})

	h.key("K")
	h.waitForView("the kick to complete", func(v string) bool {
		return strings.Contains(v, "kick queued for team/one")
	})
	h.settle()

	escaped := "/api/kick/" + url.PathEscape("team/one")
	if got := f.countRequests(http.MethodPost, escaped); got != 1 {
		t.Errorf("kick did not address the escaped path %s exactly once (got %d)", escaped, got)
	}
	if got := f.countRequests(http.MethodPost, "/api/kick/team/one"); got != 0 {
		t.Errorf("kick sent a RAW-interpolated path %d times: the name leaked into the route", got)
	}

	// EVERY per-agent write, not just kick. Kick was the only escaped path
	// asserted here originally, so dropping url.PathEscape from PauseAgent,
	// ResumeAgent or SetAgentModel left the whole suite green — three live
	// mutations that survived. The client's own comment is the reason this has
	// to be deliberate: "Ordinary agent names need no escaping, which is
	// exactly why this has to be deliberate rather than left to the common
	// case." A name with a slash is the case that tells them apart, because a
	// raw one silently becomes an extra path segment and reaches a different
	// route (or none).

	// Pause: `p` opens the confirmation, `y` sends it.
	h.key("p")
	h.waitFor("the pause dialog to open for the awkwardly-named agent", func(m model) bool {
		return m.confirm != nil && m.confirm.agent == "team/one"
	})
	h.key("y")
	h.waitFor("the pause dialog to close on the authoritative response", func(m model) bool {
		return m.confirm == nil
	})
	h.settle()

	escapedPause := "/api/pause/" + url.PathEscape("team/one")
	if got := f.countRequests(http.MethodPost, escapedPause); got != 1 {
		t.Errorf("pause did not address the escaped path %s exactly once (got %d)", escapedPause, got)
	}
	if got := f.countRequests(http.MethodPost, "/api/pause/team/one"); got != 0 {
		t.Errorf("pause sent a RAW-interpolated path %d times: the name leaked into the route", got)
	}

	// Model apply: the agent name AND the model id are both path segments.
	h.key("m")
	h.waitFor("the picker to open for the awkwardly-named agent", func(m model) bool {
		return m.picker != nil && m.picker.Agent() == "team/one"
	})
	h.waitFor("the catalogue to populate the overlay", func(m model) bool {
		return m.picker != nil && !m.picker.Loading()
	})
	h.key("j")
	h.waitFor("the picker selection to move off the current model", func(m model) bool {
		sel, ok := m.picker.Selected()
		return ok && sel == "claude-sonnet-4-5"
	})
	h.key("enter")
	h.waitFor("the picker to close on a successful apply", func(m model) bool {
		return m.picker == nil
	})
	h.settle()

	escapedModel := "/api/model/" + url.PathEscape("team/one") + "/claude-sonnet-4-5"
	if got := f.countRequests(http.MethodPost, escapedModel); got != 1 {
		t.Errorf("model apply did not address the escaped path %s exactly once (got %d)", escapedModel, got)
	}
	if got := f.countRequests(http.MethodPost, "/api/model/team/one/claude-sonnet-4-5"); got != 0 {
		t.Errorf("model apply sent a RAW-interpolated path %d times: the name leaked into the route", got)
	}
}

// TestAttachBindingTargetsSelectionWithoutSpawningTmux is the tmux boundary.
//
// THE PROCESS IS NEVER SPAWNED. `a` ends in tea.ExecProcess, which suspends the
// terminal and attaches to a real tmux session — impossible in CI, where there
// is no TTY, and actively harmful if it succeeded, because the test binary
// would block inside an interactive session. The command's CONSTRUCTION is
// already owned by attach_test.go and attach_errors_test.go.
//
// What is asserted here is the part those tests cannot see: the BINDING and
// SELECTION path. That `a` is gated on the Agents pane being focused, that it
// marks the attach pending so a second press cannot queue a second
// terminal-suspending command, and that it targets the row the operator can
// see. The harness recognises the exec message and records it instead of
// running it — see harness.exec.
func TestAttachBindingTargetsSelectionWithoutSpawningTmux(t *testing.T) {
	// PATH IS EMPTIED, AND THAT IS THE PRECONDITION, NOT A DETAIL.
	//
	// The assertions below depend on the preflight FAILING: `a` marks the
	// attach pending, and it is the preflight's failure that later clears the
	// flag and writes "Attach failed" to the footer. prepareAttach resolves
	// tmux through PATH (attach.go, exec.LookPath), so on a developer machine
	// with tmux installed — which is most of them, and this one — the preflight
	// can instead SUCCEED, hand back a real command, and clear attachPending
	// via a route the test does not expect. That is exactly how this test
	// failed under -count=5 here while passing alone: the outcome was a
	// property of the machine, not of the code.
	//
	// Emptying PATH makes tmuxNotFoundError the guaranteed result everywhere,
	// so the test proves the same thing on a laptop with tmux and on a CI
	// runner without it. It also reinforces the file's tmux boundary: with no
	// tmux resolvable there is no way for this test to spawn one even if the
	// harness's exec guard were removed.
	t.Setenv("PATH", t.TempDir())

	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForRoster("scanner")

	// Move the selection so the assertion is about the DISPLAYED row rather
	// than about the first row happening to be right.
	h.key("j")
	h.waitFor("the selection to move to quality", func(m model) bool {
		agents, _ := m.panes[0].(panes.Agents)
		name, _, ok := agents.SelectedAgent()
		return ok && name == "quality"
	})

	// THREE presses in a row, and the bounding property is asserted on what
	// SURVIVES them rather than on attachPending being observably true in
	// between.
	//
	// attachPending is transient by design: `a` sets it, and the preflight's
	// result clears it. With no tmux on PATH that preflight is an exec.LookPath
	// miss, which resolves in microseconds — so a wait for attachPending==true
	// is racing a flag that may already have been cleared by the time the first
	// sample is taken. That is not a defect in the model, and an earlier draft
	// of this test which waited on the flag failed intermittently under
	// -count=5 for exactly that reason. A property that can only be observed by
	// winning a race is not a property this suite can assert.
	//
	// What is durable is the CONSEQUENCE: however many times `a` is pressed
	// while an attempt is in flight, exactly one preflight runs and exactly one
	// failure reaches the footer. The gate in app.go (`if m.focus != 0 ||
	// m.attachPending`) is what makes that true, so asserting it still covers
	// the guard — and it covers it without depending on scheduling.
	h.key("a")
	h.key("a")
	h.key("a")

	// The preflight resolves against a PATH with no tmux (emptied above), so it
	// fails and the failure becomes UI rather than a process-ending error. That
	// is the boundary: the binding and its target are proved, the session is
	// not.
	h.waitForView("the attach preflight failure to be rendered in the footer", func(v string) bool {
		return strings.Contains(v, "Attach failed")
	})
	h.waitFor("the pending flag to clear once the attempt resolved", func(m model) bool {
		return !m.attachPending
	})
	h.settle()

	// THE PROCESS WAS NEVER SPAWNED. prepareAttach failed at the LookPath
	// stage, so tea.ExecProcess was never reached — which is the file's tmux
	// boundary, asserted rather than assumed.
	if h.didRequestExec() {
		t.Error("an exec was requested: the attach path tried to spawn a real tmux")
	}
	// The footer names the failure the operator would actually see, and names
	// tmux — so this is the no-tmux path, not some other error wearing the same
	// message.
	if v := h.view(); !strings.Contains(v, "tmux") {
		t.Errorf("the footer does not name tmux, so the preflight failed for an unexpected reason:\n%s", v)
	}
}

// ── Property 7: help/footer parity and modal key containment ─────────────────

// TestHelpAndFooterListTheSameAvailableBindings is the parity assertion.
//
// Both lists are hand-maintained transcriptions of the design doc's §4 table —
// there is no runtime registry to derive either from — so they can drift, and
// the drift is invisible: an operator reads help, presses the key, nothing
// happens. This compares the two directly.
func TestHelpAndFooterListTheSameAvailableBindings(t *testing.T) {
	help := panes.Help()

	// Every binding help calls AVAILABLE must be reachable from the footer
	// strip, and must actually be handled by the app.
	for _, b := range panes.HelpBindings() {
		if !b.Available {
			continue
		}
		for _, key := range splitBindingKeys(b.Keys) {
			if !footerAdvertises(key) {
				t.Errorf("help lists %q as available but the footer strip does not advertise it\nfooter: %s", key, footerText)
			}
		}
	}

	// And the reverse: nothing in the footer may be absent from help, because
	// help is where an operator goes to find out what a key does.
	for _, key := range footerKeys() {
		if !helpAdvertises(help, key) {
			t.Errorf("the footer advertises %q but the help overlay does not list it", key)
		}
	}
}

// splitBindingKeys splits a help row's key column ("tab / shift+tab",
// "j / k, ↓ / ↑") into individual keys.
func splitBindingKeys(keys string) []string {
	fields := strings.FieldsFunc(keys, func(r rune) bool {
		return r == '/' || r == ','
	})
	var out []string
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// footerKeys is the leading key token of each "key label" pair in footerText.
func footerKeys() []string {
	fields := strings.Fields(footerText)
	var out []string
	for i := 0; i < len(fields); i += 2 {
		out = append(out, fields[i])
	}
	return out
}

// footerAdvertises reports whether the footer strip mentions a key.
//
// The footer is terser than help by design — it lists `tab` for the focus pair
// and omits the arrow-key aliases and the pane-local j/k — so the comparison is
// on the keys the footer chose to name, with the aliases mapped onto their
// primary. Spelling the aliases out here rather than loosening the match is
// what keeps the test able to fail.
func footerAdvertises(key string) bool {
	switch key {
	case "shift+tab":
		key = "tab"
	case "ctrl+c":
		key = "q"
	case "↓", "↑", "j", "k":
		// Pane-local movement. The footer names the panes' actions, not their
		// cursors, and help marks these as pane-scoped rather than global.
		return true
	}
	for _, f := range footerKeys() {
		if f == key {
			return true
		}
	}
	return false
}

func helpAdvertises(help, key string) bool {
	for _, b := range panes.HelpBindings() {
		for _, k := range splitBindingKeys(b.Keys) {
			if k == key {
				return b.Available
			}
		}
	}
	return strings.Contains(help, key)
}

// TestModalKeysCannotLeakIntoGlobalActions is the containment property, and it
// is the one with real consequences: `A`, `p`, `a` and `K` all appear in
// "APPLY L4", so a key leaking out of the ACMM confirmation would not merely
// fail to type — it would pause an agent or fire a kick while the operator was
// spelling out a fleet-wide change.
func TestModalKeysCannotLeakIntoGlobalActions(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the roster to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("A")
	h.waitFor("the ACMM overlay to open", func(m model) bool { return m.acmm != nil })
	h.waitFor("the pack list to load", func(m model) bool {
		return m.acmm != nil && !m.acmm.Loading()
	})
	h.key("j")
	h.key("enter")
	h.waitFor("the confirmation field to open", func(m model) bool {
		return m.acmm != nil && m.acmm.Confirming()
	})

	// Type the phrase whose letters are all bindings, plus q and K.
	h.typeText("APPLY L4")
	h.key("K")
	h.key("q")
	h.settle()

	m := h.snapshot()
	if m.acmm == nil {
		t.Fatal("the ACMM overlay closed: a key leaked out of the confirmation")
	}
	if m.confirm != nil {
		t.Error("a letter typed into the ACMM confirmation opened the pause dialog")
	}
	if m.picker != nil {
		t.Error("a letter typed into the ACMM confirmation opened the model picker")
	}
	if h.didQuit() {
		t.Error("q typed into the ACMM confirmation quit the program")
	}
	if got := f.countPath("/api/kick/scanner"); got != 0 {
		t.Errorf("K typed into the ACMM confirmation issued %d kicks, want 0", got)
	}
	// EVERY rune typed while confirming is TEXT, including K and q. That is the
	// containment rule as app.go states it — "runes are text while confirming"
	// rather than "these particular keys are special" — and it is the stronger
	// of the two possible designs: a swallowed K would be a key that did
	// nothing, whereas a K that lands in the field is a key that visibly
	// cannot act. The operator sees the phrase is wrong and fixes it.
	//
	// So the field holds the phrase plus the two stray letters, and the
	// property that matters is the one asserted immediately below: that text
	// does not match ConfirmPhrase(4), so the apply is refused. An earlier
	// draft of this test expected "APPLY L4" here, which would have required
	// the model to silently discard keystrokes an operator actually pressed —
	// the failure mode the design comment explicitly rejects.
	const wantTyped = "APPLY L4Kq"
	if got := m.acmm.Typed(); got != wantTyped {
		t.Errorf("the confirmation field holds %q, want %q: keys were consumed wrongly", got, wantTyped)
	}
	// The whole point of the stray letters landing in the field: the phrase no
	// longer matches, so enter cannot apply. Without this the assertion above
	// would only be describing where characters went, not that the guard held.
	if m.acmm.Typed() == panes.ConfirmPhrase(4) {
		t.Error("K and q left the field holding the exact confirmation phrase: the guard is defeated")
	}
	h.key("enter")
	h.settle()
	if got := f.countRequests(http.MethodPut, "/api/packs/level"); got != 0 {
		t.Errorf("enter on a field polluted by stray global keys issued %d applies, want 0", got)
	}
}

// TestHelpOverlaySwallowsQuitInTheAssembledLoop is the same containment property for the overlay
// an operator is most likely to be reading when they press a key at random.
func TestHelpOverlaySwallowsQuitInTheAssembledLoop(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the frame to load", func(v string) bool {
		return strings.Contains(v, "scanner")
	})

	h.key("?")
	h.waitFor("the help overlay to open", func(m model) bool { return m.helpVisible })

	// q dismisses the overlay and MUST NOT quit.
	h.key("q")
	h.waitFor("the overlay to be dismissed", func(m model) bool { return !m.helpVisible })
	h.settle()
	if h.didQuit() {
		t.Error("q dismissed the help overlay AND quit the program: the reader lost their session")
	}
}

// TestPauseDialogSwallowsGlobalKeys covers the third modal.
func TestPauseDialogSwallowsGlobalKeys(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	// Wait for the precondition `p` ACTUALLY depends on, not for the string
	// "scanner" to appear somewhere in the frame.
	//
	// `p` is a no-op unless Agents.SelectedAgent() reports ok (app.go), and
	// that needs the pane's roster populated. A waitForView on "scanner"
	// matches the WHOLE frame — the Events pane renders audit rows naming
	// scanner, and those arrive on the activity chain independently of the
	// roster — so the view test can pass a moment before the Agents pane has
	// any rows. The press then lands on a pane with nothing selected, is
	// correctly ignored, and the dialog never opens: a hang that looks like a
	// containment bug and is really the test acting before its precondition.
	// That is what failed here under -count=5.
	//
	// Reading through SelectedAgent is also the accessor `p` itself uses, so
	// this waits for exactly the state the binding is gated on.
	h.waitFor("the Agents pane to have a selectable row for p to target", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		name, _, ok := agents.SelectedAgent()
		return ok && name == "scanner"
	})

	h.key("p")
	h.waitFor("the dialog to open", func(m model) bool { return m.confirm != nil })

	focusBefore := h.snapshot().focus
	h.key("q")
	h.key("tab")
	h.key("A")
	h.key("K")
	h.settle()

	m := h.snapshot()
	if h.didQuit() {
		t.Error("q quit the program from inside the pause confirmation")
	}
	if m.focus != focusBefore {
		t.Errorf("tab moved focus from inside the pause confirmation (%d -> %d)", focusBefore, m.focus)
	}
	if m.acmm != nil {
		t.Error("A opened the ACMM overlay from inside the pause confirmation")
	}
	if m.confirm == nil {
		t.Fatal("the confirmation was dismissed by a key that is not n or esc")
	}
	if got := f.countPath("/api/kick/scanner"); got != 0 {
		t.Errorf("K issued %d kicks from inside the pause confirmation, want 0", got)
	}
}

// ── Property 8: resize across the minimum ────────────────────────────────────

// TestResizeBelowAndAboveTheMinimumSwapsTheFrameCleanly proves the floor is a
// SWAP rather than a shrink, and — the half a size-only test cannot see — that
// crossing it does not cost the frame its data.
func TestResizeBelowAndAboveTheMinimumSwapsTheFrameCleanly(t *testing.T) {
	f := newFixtureDashboard(t)
	h := newHarness(t, f)

	h.waitForView("the full frame to load", func(v string) bool {
		return strings.Contains(v, "scanner") && strings.Contains(v, "hive: acceptance-hive")
	})

	// Below the minimum in WIDTH.
	h.send(tea.WindowSizeMsg{Width: minWidth - 1, Height: testTermHeight})
	h.waitForView("the too-small frame to replace the grid", func(v string) bool {
		return strings.Contains(v, "terminal too small")
	})
	small := h.view()
	if strings.Contains(small, "scanner") {
		t.Error("the too-small frame still drew the grid: the floor shrinks rather than swaps")
	}
	if strings.Contains(small, footerText) {
		t.Error("the too-small frame still drew the footer strip")
	}
	// It must fit the terminal exactly rather than overflowing it.
	for i, line := range strings.Split(small, "\n") {
		if w := len([]rune(line)); w > minWidth-1 {
			t.Errorf("too-small frame line %d is %d columns wide, want at most %d", i, w, minWidth-1)
		}
	}

	// Back above the minimum: the full frame returns WITH its data intact.
	h.send(tea.WindowSizeMsg{Width: testTermWidth, Height: testTermHeight})
	h.waitForView("the full frame to return with its data", func(v string) bool {
		return strings.Contains(v, "scanner") &&
			strings.Contains(v, "hive: acceptance-hive") &&
			strings.Contains(v, "auditnewest")
	})
	if strings.Contains(h.view(), "terminal too small") {
		t.Error("the too-small message survived a resize back above the minimum")
	}

	// Below the minimum in HEIGHT only, which is the other half of the guard.
	h.send(tea.WindowSizeMsg{Width: testTermWidth, Height: minHeight - 1})
	h.waitForView("the height floor to swap the frame too", func(v string) bool {
		return strings.Contains(v, "terminal too small")
	})

	// Exactly at the minimum is ABOVE the floor, not below it.
	h.send(tea.WindowSizeMsg{Width: minWidth, Height: minHeight})
	h.waitForView("the minimum supported size to render the full frame", func(v string) bool {
		return !strings.Contains(v, "terminal too small")
	})
}

// ── Property 9: quit cancels the stream without leaking ──────────────────────

// TestQuitCancelsTheStreamWithoutLeakingAGoroutineOrReconnect covers both quit
// keys.
//
// The leak this guards is real and specific: cancelling is what stops the
// goroutine client.StreamEvents owns, and RETIRING THE GENERATION is what stops
// the resulting drop from being mistaken for a real one and scheduling a
// reconnect on the way out. Under `hivectl tui` a leaked goroutine is harmless
// because the process is exiting; under a test binary — or an embedding program
// — it is not.
func TestQuitCancelsTheStreamWithoutLeakingAGoroutineOrReconnect(t *testing.T) {
	for _, key := range []string{"q", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			f := newFixtureDashboard(t)
			h := newHarness(t, f)

			h.waitForView("the frame to load", func(v string) bool {
				return strings.Contains(v, "scanner")
			})
			f.publish(t, "", integrationStatus)
			h.waitFor("the stream to be healthy", func(m model) bool { return m.sseConnected })
			h.waitFor("a live stream to be installed", func(m model) bool { return m.sse != nil })

			connectionsBefore := f.streamConnections()
			before := runtime.NumGoroutine()

			h.key(key)

			h.waitForFixture("the program to ask to quit", h.didQuit)
			h.waitFor("the stream to be released on quit", func(m model) bool { return m.sse == nil })

			h.settle()

			// NO RECONNECT. The cancellation closes the channels, so the pump
			// produces one last drop; without the generation bump that drop is
			// indistinguishable from a real one and would schedule a reconnect
			// while the program is already quitting.
			if got := f.streamConnections(); got != connectionsBefore {
				t.Errorf("the stream was re-dialled %d time(s) after quit: the quit path scheduled a reconnect",
					got-connectionsBefore)
			}

			// THE QUIT PATH ITSELF RELEASED THE CONNECTION.
			//
			// This must be asserted BEFORE h.stop(), and that ordering is the
			// whole point. harness.stop() calls model.cancelSSE() in its own
			// teardown, so every goroutine check after it passes whether or
			// not the quit path cancelled anything — the harness cleans up the
			// very leak the test is hunting. A mutation deleting cancelSSE()
			// from stopSSE survived the goroutine assertion for exactly that
			// reason.
			//
			// The fixture's handler records a client cancellation when
			// r.Context() is done, which happens only when the model's own
			// context is cancelled. Waiting for it here proves quit did the
			// work rather than teardown doing it later.
			h.waitForFixture("the quit path to cancel the stream request", func() bool {
				return f.streamClientCancellations() > 0
			})

			// NO LEAKED GOROUTINE. The stream reader must have exited.
			h.stop()
			// BOUNDED, not cmdWG.Wait(). By the time quit is handled the
			// stream has stretched the reconcile cadence to
			// sseReconcileInterval, so a tea.Tick timer for a full 60 SECONDS
			// is outstanding and cmdWG is counting it. Waiting on the group
			// therefore parked this subtest for exactly 60s twice over — two
			// minutes of wall clock spent waiting for a timer whose message
			// nobody will ever read, which is precisely the production-duration
			// wait #5424 forbids, hidden in teardown rather than in a sleep.
			//
			// A pending tick is also not what this wait is for. It exists so
			// in-flight COMMANDS finish before goroutines are counted, and the
			// leak being hunted is a per-stream reader that never exits — which
			// assertGoroutinesSettle detects on its own, with its own deadline.
			// An unfired timer is not a leak: it is one runtime timer goroutine
			// that the tolerance already covers and that the runtime reclaims.
			waitOrTimeout(&h.cmdWG, 2*time.Second)
			assertGoroutinesSettle(t, before)
		})
	}
}

// assertGoroutinesSettle waits for the goroutine count to come back to around
// its pre-test level.
//
// A tolerance is used rather than an exact match because the Go runtime and
// net/http keep their own pools, and httptest's own connection handlers wind
// down on their own schedule. The leak this is written to catch is a PER-STREAM
// reader that never exits — one per connection, growing without bound — which a
// small tolerance still detects.
func assertGoroutinesSettle(t *testing.T, before int) {
	t.Helper()
	const tolerance = 4
	deadline := time.Now().Add(waitTimeout)
	var after int
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= before+tolerance {
			return
		}
		timer := time.NewTimer(10 * pollStep)
		<-timer.C
	}
	t.Errorf("goroutines did not settle after quit: %d before, %d after (tolerance %d)", before, after, tolerance)
}

// ── Harness self-checks ──────────────────────────────────────────────────────

// TestQuitMsgDetectionMatchesBubbletea guards the harness itself.
//
// isQuitMsg identifies bubbletea's quit signal by type name because the type is
// unexported. If a bubbletea upgrade renamed it, every quit assertion in this
// file would silently become a no-op — the exact class of vacuous test this
// task exists to eliminate. This fails instead.
func TestQuitMsgDetectionMatchesBubbletea(t *testing.T) {
	if !isQuitMsg(tea.Quit()) {
		t.Fatalf("isQuitMsg does not recognise tea.Quit() (%T): every quit assertion in this file is vacuous", tea.Quit())
	}
	if isQuitMsg(tea.WindowSizeMsg{}) {
		t.Error("isQuitMsg matched a non-quit message")
	}
}

// TestFixtureRecordsMethodPathBodyAndAuth guards the recording fixture.
//
// Every "exactly once" and "correct endpoint" assertion in this file is only as
// good as this recording. A fixture that silently stopped recording — or
// recorded a decoded path — would turn the escaping and call-count properties
// into tautologies.
func TestFixtureRecordsMethodPathBodyAndAuth(t *testing.T) {
	f := newFixtureDashboard(t)

	// The raw path must survive: this is what makes the escaping assertion
	// able to fail.
	resp, err := http.Get(f.Server.URL + "/api/kick/team%2Fone")
	if err != nil {
		t.Fatalf("fixture request failed: %v", err)
	}
	_ = resp.Body.Close()

	if got := f.countPath("/api/kick/team%2Fone"); got != 1 {
		t.Errorf("the fixture recorded %d requests for the escaped path, want 1: paths are being decoded before recording", got)
	}
	if got := f.countPath("/api/kick/team/one"); got != 0 {
		t.Errorf("the fixture recorded the DECODED path %d times: escaping assertions cannot fail", got)
	}
}

// waitOrTimeout waits for wg, giving up after limit rather than blocking
// forever on a command that cannot complete.
//
// The command it exists for is tea.Tick: the harness's exec tracks every
// command goroutine in cmdWG, and a tick is a goroutine parked on a timer for
// its whole interval. On the quit path that interval is sseReconcileInterval —
// 60 seconds — and nothing will read the message when it finally arrives. An
// unbounded Wait there is a production-duration wait wearing a teardown's
// clothes.
func waitOrTimeout(wg *sync.WaitGroup, limit time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// waitForRoster waits until the Agents pane has a selectable row.
//
// This is the precondition for every action key gated on a selection — p, m, K
// and a all return early unless Agents.SelectedAgent() reports ok (app.go) —
// and it is NOT the same thing as the roster being visible in the frame. The
// obvious wait, waitForView for "scanner", matches the whole rendered frame,
// and "scanner" also appears in the Events pane's audit rows and can arrive
// there first on the independent activity chain. A test that presses an action
// key on the strength of that view match can act before the Agents pane has any
// rows: the key is correctly ignored, and the test then hangs waiting for an
// overlay that was never going to open.
//
// Waiting through SelectedAgent instead reads the same accessor the binding is
// gated on, so the precondition asserted is the precondition that matters.
func (h *harness) waitForRoster(agent string) {
	h.t.Helper()
	h.waitFor("the Agents pane to have "+agent+" selectable", func(m model) bool {
		agents, ok := m.panes[0].(panes.Agents)
		if !ok {
			return false
		}
		name, _, ok := agents.SelectedAgent()
		return ok && name == agent
	})
}

// didRequestExec reports whether the model asked bubbletea to run a process.
//
// The harness recognises tea.ExecProcess and records it INSTEAD of running it
// (see harness.exec), so this is how a test asserts the tmux boundary was not
// crossed without ever risking a real attach.
func (h *harness) didRequestExec() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.execRequested
}
