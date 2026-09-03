package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// statusFixture is one full status snapshot as the dashboard publishes it on
// the stream's default (unnamed) event — a trimmed dashboard.StatusPayload
// carrying only the keys this frame reads.
//
// The three agents are the three in agentsFixture, and each is arranged to
// contradict what /api/agents alone would show, because that is the only way a
// test can tell a stream-fed frame from a polled one:
//
//   - scanner is enabled in config but PAUSED live (poll alone: running)
//   - quality is enabled and running (poll alone: running — the control)
//   - reviewer is disabled and carries a lastError (poll alone: paused)
const statusFixture = `{
  "timestamp": "2026-08-30T12:00:00Z",
  "agents": [
    {"name": "scanner", "enabled": true, "paused": true, "state": "running"},
    {"name": "quality", "enabled": true, "paused": false, "state": "running"},
    {"name": "reviewer", "enabled": false, "paused": false, "state": "stopped",
     "lastError": "backend auth failed"}
  ],
  "governor": {
    "active": true, "mode": "busy", "issues": 7, "prs": 2,
    "thresholds": {"quiet": 1, "busy": 5, "surge": 20},
    "nextKick": "8/30 12:05 PM UTC"
  },
  "acmmLevel": 4,
  "acmmLevelConfigured": true
}`

// agentStatusFixture is the lighter `agent-status` push: agents and the
// governor MODE, but no governor object.
const agentStatusFixture = `{
  "timestamp": "2026-08-30T12:00:01Z",
  "agents": [
    {"name": "scanner", "enabled": true, "paused": true, "state": "running"}
  ],
  "govMode": "busy"
}`

// sseEvent frames a fixture as the client would hand it to the app.
func sseEvent(eventType client.SSEEventType, data string) client.SSEEvent {
	return client.SSEEvent{Type: eventType, Data: json.RawMessage(data)}
}

// streamServer serves the poll's /api/agents alongside a live /api/events
// stream that republishes one frame on a heartbeat.
//
// It republishes rather than sending once because the startup poll and the
// stream race by construction: per-agent states are joined onto the polled
// roster, so a stream frame that arrives before the fetch resolves carries
// states with nothing to join them to. The real dashboard republishes every
// REFRESH_MS for the same reason a browser refreshing mid-connect still ends
// up correct; a single-shot test server would make this test depend on which
// of two concurrent startup commands happened to finish first.
type streamServer struct {
	*httptest.Server
}

func newStreamServer(t *testing.T, frame string) *streamServer {
	t.Helper()
	s := &streamServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agents":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(agentsFixture))
		case "/api/events":
			flusher, ok := w.(http.Flusher)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()

			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				// One data: line per source line, which is how SSE carries a
				// multi-line payload — and what lets the fixture above stay
				// readable instead of being one enormous line.
				for _, line := range strings.Split(frame, "\n") {
					if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
						return
					}
				}
				if _, err := fmt.Fprint(w, "\n"); err != nil {
					return
				}
				flusher.Flush()

				select {
				case <-ticker.C:
				case <-r.Context().Done():
					return
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// drainUntil runs cmd's leaves CONCURRENTLY — as bubbletea does — and returns
// the messages collected once want is satisfied, or everything that arrived
// within d.
//
// The sequential drain() in poll_test.go cannot be used on the fallback path:
// that batch contains the re-armed five-second tick, and running it in line
// would make the test sit out a real poll interval to learn something the
// model state already says. Concurrency is not a shortcut here — batched
// commands have no ordering guarantee under bubbletea, so nothing may depend
// on it.
func drainUntil(cmd tea.Cmd, d time.Duration, want func([]tea.Msg) bool) []tea.Msg {
	// Buffered so a command still running when this returns — the five-second
	// tick, every time — can finish into the channel rather than blocking on a
	// send nobody will receive.
	out := make(chan tea.Msg, 16)
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		go func() {
			// A batch is only discoverable by running it, and a batch of
			// timers must therefore be expanded on its own goroutine: doing it
			// in line would mean waiting out every timer just to learn what
			// the batch contained.
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, sub := range batch {
					run(sub)
				}
				return
			}
			out <- msg
		}()
	}
	run(cmd)

	deadline := time.After(d)
	var msgs []tea.Msg
	for {
		select {
		case msg := <-out:
			msgs = append(msgs, msg)
			if want != nil && want(msgs) {
				return msgs
			}
		case <-deadline:
			return msgs
		}
	}
}

func hasMsg[T tea.Msg](msgs []tea.Msg) bool {
	for _, msg := range msgs {
		if _, ok := msg.(T); ok {
			return true
		}
	}
	return false
}

// connectedModel is a model in the state a healthy stream leaves behind: an
// open subscription, the header reporting connected, and the poll stretched to
// the reconcile cadence.
func connectedModel(t *testing.T, url string) model {
	t.Helper()
	pinDashboard(t, url)
	m := newModel()
	m.sse = &sseStream{
		events: make(chan client.SSEEvent),
		errs:   make(chan error),
		cancel: func() {},
		gen:    m.sseGen,
	}
	m.sseConnected = true
	m.interval = sseReconcileInterval
	return m
}

// TestSSEEventUpdatesAPaneWithoutATick is the AC's first case, driven through
// the real program.
//
// The model's poll interval is set to an hour, so no tick can fire inside the
// test: everything on screen beyond the single startup fetch arrived because
// the stream pushed it. Each assertion is something polling cannot produce —
// /api/agents carries no live state at all, and nothing polled feeds the
// governor pane today.
func TestSSEEventUpdatesAPaneWithoutATick(t *testing.T) {
	server := newStreamServer(t, statusFixture)
	pinDashboard(t, server.URL)

	m := newModel()
	m.interval = time.Hour

	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(100, 30))
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		out := string(b)
		return strings.Contains(out, "ws: "+wsConnected) && // header went live
			strings.Contains(out, "BUSY") && // governor pane, SSE-only
			strings.Contains(out, "×") // reviewer's error glyph
	}, teatest.WithDuration(finalWait))

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(finalWait))

	final, ok := tm.FinalModel(t).(model)
	if !ok {
		t.Fatalf("final model has unexpected type %T", tm.FinalModel(t))
	}
	if !final.sseConnected {
		t.Error("a stream that delivered events left the header disconnected")
	}
	if final.interval != sseReconcileInterval {
		t.Errorf("interval = %v after a healthy stream, want the reconcile cadence %v",
			final.interval, sseReconcileInterval)
	}
}

// TestSSEDropFallsBackToPolling is the AC's second case: the stream ends and
// the frame goes back to being poll-driven, at the poll's own cadence, saying
// so in the header.
//
// The assertions are on the model rather than on a rendered frame for the
// cadence, because "polling every five seconds" is not observable in a test
// that must not take five seconds — but the header and the immediate refetch
// are, and both are checked.
func TestSSEDropFallsBackToPolling(t *testing.T) {
	server := newAgentsServer(t)
	m := connectedModel(t, server.URL)

	next, cmd := m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})
	got, ok := next.(model)
	if !ok {
		t.Fatalf("a dropped stream returned %T, want the root model", next)
	}

	if got.interval != pollInterval {
		t.Errorf("interval = %v after a drop, want the fallback cadence %v", got.interval, pollInterval)
	}
	if got.sseConnected {
		t.Error("a dropped stream still reports connected")
	}
	if got.sse != nil {
		t.Error("a dropped stream was not released")
	}
	if got.tickGen == m.tickGen {
		t.Error("the stretched tick chain was not retired; the 60s chain would keep ticking alongside the 5s one")
	}
	if got.sseGen == m.sseGen {
		t.Error("the stream generation did not advance; a straggler from the dead stream could still be believed")
	}
	if got.sseBackoff != sseBackoffMin {
		t.Errorf("sseBackoff = %v after the first drop, want %v", got.sseBackoff, sseBackoffMin)
	}

	sized, _ := got.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if view := sized.(model).View(); !strings.Contains(view, "ws: "+wsNotConnected) {
		t.Errorf("the header does not report the degraded connection:\n%s", view)
	}

	// The fallback must fetch NOW, not at the end of the interval it just
	// restored, and it must schedule the reconnect.
	msgs := drainUntil(cmd, finalWait, func(msgs []tea.Msg) bool {
		return findAgentsMsg(msgs) != nil && hasMsg[sseReconnectMsg](msgs)
	})
	if findAgentsMsg(msgs) == nil {
		t.Error("the drop issued no immediate poll; the frame would sit stale for a whole interval")
	}
	if !hasMsg[sseReconnectMsg](msgs) {
		t.Error("the drop scheduled no reconnect; the stream would never come back")
	}
	for _, msg := range msgs {
		if reconnect, isReconnect := msg.(sseReconnectMsg); isReconnect && reconnect.gen != got.sseGen {
			t.Errorf("reconnect carries generation %d, want the new %d", reconnect.gen, got.sseGen)
		}
	}
}

// TestRepeatedDropsDoNotRestartTheFallback pins the other half of the fallback
// rule. Only the first drop has a stretched cadence to undo; doing the work
// again on every failed reconnect would arm a tick chain and issue a fetch per
// backoff step, so a dashboard that is DOWN would receive more requests than
// one that is up.
func TestRepeatedDropsDoNotRestartTheFallback(t *testing.T) {
	m := connectedModel(t, closedDashboard)

	first, _ := m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})
	degraded := first.(model)

	second, cmd := degraded.Update(sseDroppedMsg{gen: degraded.sseGen, err: errSSEClosed})
	again := second.(model)

	if again.tickGen != degraded.tickGen {
		t.Error("a repeated drop armed a second tick chain")
	}
	if again.interval != pollInterval {
		t.Errorf("interval = %v after a repeated drop, want %v", again.interval, pollInterval)
	}
	msgs := drainUntil(cmd, finalWait, func(msgs []tea.Msg) bool { return hasMsg[sseReconnectMsg](msgs) })
	if findAgentsMsg(msgs) != nil {
		t.Error("a repeated drop issued another poll; the 5s loop is already doing that")
	}
	if !hasMsg[sseReconnectMsg](msgs) {
		t.Error("a repeated drop stopped retrying the stream")
	}
}

// TestSSEEventStretchesThePollWithoutASecondChain pins both halves of the
// healthy-stream case: the poll slows to the reconcile cadence, and it does so
// WITHOUT arming a new chain — the pending tick re-arms itself from the field,
// and a second chain would double the fetch rate for the rest of the session.
func TestSSEEventStretchesThePollWithoutASecondChain(t *testing.T) {
	m := connectedModel(t, closedDashboard)
	m.sseConnected = false
	m.interval = pollInterval
	m.sseBackoff = sseBackoffMax

	next, cmd := m.Update(sseEventMsg{
		gen:   m.sseGen,
		event: sseEvent(client.SSEEventTypeMessage, statusFixture),
	})
	got := next.(model)

	if got.interval != sseReconcileInterval {
		t.Errorf("interval = %v while the stream is healthy, want %v", got.interval, sseReconcileInterval)
	}
	if got.tickGen != m.tickGen {
		t.Error("stretching the poll armed a second tick chain")
	}
	if !got.sseConnected {
		t.Error("a received event did not mark the stream connected")
	}
	if got.sseBackoff != 0 {
		t.Errorf("sseBackoff = %v after a received event, want it reset", got.sseBackoff)
	}
	if got.headerText() != fmt.Sprintf(headerFormat, wsConnected) {
		t.Errorf("header = %q, want it to report a live stream", got.headerText())
	}
	// The pump must re-arm, or the stream delivers exactly one event and then
	// looks healthy forever while nothing more arrives.
	if cmd == nil {
		t.Error("a received event did not re-arm the reader")
	}
}

// TestSSEBackoffDoublesAndCaps pins the reconnect schedule. Without the cap a
// long outage pushes the retry interval past any useful value; without the
// doubling a dashboard that is down is retried once a second forever.
func TestSSEBackoffDoublesAndCaps(t *testing.T) {
	m := connectedModel(t, closedDashboard)

	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, wantDelay := range want {
		next, _ := m.Update(sseDroppedMsg{gen: m.sseGen, err: errSSEClosed})
		m = next.(model)
		if m.sseBackoff != wantDelay {
			t.Fatalf("drop %d: sseBackoff = %v, want %v", i+1, m.sseBackoff, wantDelay)
		}
	}
}

// TestStaleSSEMessagesAreIgnored pins the generation guard, one message type
// at a time. Each of these is a straggler from a stream that has already been
// replaced, and each would corrupt the successor's state in its own way.
func TestStaleSSEMessagesAreIgnored(t *testing.T) {
	pinDashboard(t, closedDashboard)

	t.Run("event", func(t *testing.T) {
		m := newModel()
		m.sseGen = 3

		next, cmd := m.Update(sseEventMsg{
			gen:   2,
			event: sseEvent(client.SSEEventTypeMessage, statusFixture),
		})
		got := next.(model)
		if cmd != nil {
			t.Error("a stale event produced a command")
		}
		if got.sseConnected {
			t.Error("a stale event reported the replacement stream healthy")
		}
		if got.interval != pollInterval {
			t.Errorf("interval = %v, want the fallback cadence untouched by a stale event", got.interval)
		}
	})

	t.Run("drop", func(t *testing.T) {
		m := connectedModel(t, closedDashboard)
		m.sseGen = 3

		next, cmd := m.Update(sseDroppedMsg{gen: 1, err: errSSEClosed})
		got := next.(model)
		if cmd != nil {
			t.Error("a stale drop produced a command")
		}
		if !got.sseConnected || got.interval != sseReconcileInterval {
			t.Error("a stale drop degraded a healthy stream")
		}
	})

	t.Run("reconnect", func(t *testing.T) {
		m := newModel()
		m.sseGen = 3

		if _, cmd := m.Update(sseReconnectMsg{gen: 1}); cmd != nil {
			t.Error("an abandoned backoff timer opened a second stream")
		}
	})

	t.Run("open", func(t *testing.T) {
		m := newModel()
		m.sseGen = 2

		ctx, cancel := context.WithCancel(context.Background())
		next, cmd := m.Update(sseOpenMsg{gen: 1, stream: &sseStream{cancel: cancel, gen: 1}})
		if cmd != nil {
			t.Error("a stale subscription was read from")
		}
		if next.(model).sse != nil {
			t.Error("a stale subscription was adopted")
		}
		select {
		case <-ctx.Done():
		default:
			t.Error("an abandoned subscription was left holding its request open")
		}
	})
}

// TestStaleTickDoesNotRearm pins the mechanism the fallback relies on: the
// retired chain ends at its next fire instead of running forever beside the
// new one.
func TestStaleTickDoesNotRearm(t *testing.T) {
	m := pollTestModel(t, closedDashboard)
	m.tickGen = 1

	next, cmd := m.Update(tickMsg{})
	if cmd != nil {
		t.Error("a tick from a retired chain re-armed itself")
	}
	if next.(model).tickGen != 1 {
		t.Error("a stale tick changed the tick generation")
	}
}

// TestScheduleTickStampsTheCurrentGeneration is the other half of that
// mechanism: a chain armed after a cadence change must carry the new
// generation, or the guard above would drop the live chain instead of the dead
// one and the poll would stop entirely.
func TestScheduleTickStampsTheCurrentGeneration(t *testing.T) {
	m := pollTestModel(t, closedDashboard)
	m.tickGen = 7

	msgs := drain(m.scheduleTick())
	if len(msgs) != 1 {
		t.Fatalf("scheduleTick produced %d messages, want 1", len(msgs))
	}
	tick, ok := msgs[0].(tickMsg)
	if !ok {
		t.Fatalf("scheduleTick produced %T, want a tickMsg", msgs[0])
	}
	if tick.gen != 7 {
		t.Errorf("tick carries generation %d, want the model's 7", tick.gen)
	}
}

// TestPaneMsgsTranslateStatusEvents pins the translation itself: which pane
// messages each event type produces, and what the wire's several state fields
// map onto.
func TestPaneMsgsTranslateStatusEvents(t *testing.T) {
	pinDashboard(t, closedDashboard)
	m := newModel()
	m.agents = []client.Agent{{Name: "scanner"}, {Name: "quality"}, {Name: "reviewer"}}

	t.Run("full status", func(t *testing.T) {
		msgs := m.paneMsgs(sseEvent(client.SSEEventTypeMessage, statusFixture))

		agents := findAgentsMsg(msgs)
		if agents == nil {
			t.Fatal("a full status event delivered no agents snapshot")
		}
		if len(agents.Agents) != len(m.agents) {
			t.Errorf("delivered %d agents, want the polled roster's %d", len(agents.Agents), len(m.agents))
		}
		want := map[string]panes.AgentStatus{
			"scanner":  panes.AgentStatusPaused,  // enabled but paused live
			"quality":  panes.AgentStatusRunning, // the control
			"reviewer": panes.AgentStatusError,   // lastError beats disabled
		}
		for name, wantStatus := range want {
			if got := agents.States[name].Status; got != wantStatus {
				t.Errorf("%s status = %q, want %q", name, got, wantStatus)
			}
		}
		if got, wantAt := agents.ObservedAt, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC); !got.Equal(wantAt) {
			t.Errorf("ObservedAt = %v, want the payload's own publish time %v", got, wantAt)
		}

		governor := findGovernorMsg(msgs)
		if governor == nil {
			t.Fatal("a full status event delivered no governor snapshot")
		}
		if governor.Status.Mode != "busy" || governor.Status.QueueDepth() != 9 {
			t.Errorf("governor = %+v, want mode busy and queue depth 9", governor.Status)
		}
		if governor.Status.ACMMLevel != 4 || !governor.Status.ACMMLevelConfigured {
			t.Errorf("governor ACMM = %d (configured %v), want a configured L4",
				governor.Status.ACMMLevel, governor.Status.ACMMLevelConfigured)
		}
	})

	t.Run("agent-status carries no governor", func(t *testing.T) {
		msgs := m.paneMsgs(sseEvent(client.SSEEventTypeAgentStatus, agentStatusFixture))

		if findAgentsMsg(msgs) == nil {
			t.Fatal("an agent-status event delivered no agents snapshot")
		}
		if governor := findGovernorMsg(msgs); governor != nil {
			t.Errorf("an agent-status event produced a governor snapshot (%+v); it carries no governor object "+
				"and would blank the pane", governor.Status)
		}
	})

	t.Run("no governor object", func(t *testing.T) {
		// A message event whose payload has no governor section. Active is the
		// payload's own marker for one (buildGovernor hardcodes it true), so
		// this must not overwrite a good frame with an all-dashes one.
		msgs := m.paneMsgs(sseEvent(client.SSEEventTypeMessage, `{"timestamp":"2026-08-30T12:00:00Z"}`))
		if findGovernorMsg(msgs) != nil {
			t.Error("a governor-less payload still produced a governor snapshot")
		}
	})

	t.Run("before the first poll", func(t *testing.T) {
		// No roster to join onto yet. Sending states alone would blank the
		// fleet pane, which is worse than the one fetch's wait.
		empty := newModel()
		msgs := empty.paneMsgs(sseEvent(client.SSEEventTypeMessage, statusFixture))
		if findAgentsMsg(msgs) != nil {
			t.Error("an event delivered an agents snapshot with no roster to join onto")
		}
		if findGovernorMsg(msgs) == nil {
			t.Error("the governor snapshot was withheld along with it; it needs no roster")
		}
	})

	t.Run("undecodable payload", func(t *testing.T) {
		// Valid JSON, wrong shape. Like a failed fetch, it must reach no pane:
		// the panes keep what they were last told rather than being handed a
		// zero value they cannot tell from an empty hive.
		if msgs := m.paneMsgs(sseEvent(client.SSEEventTypeMessage, `{"agents":"not a list"}`)); msgs != nil {
			t.Errorf("an undecodable event produced %d pane messages, want none", len(msgs))
		}
	})
}

// TestStreamErrorSurvivesTheRaceForIt pins terminalErr. The producer closes
// both channels after buffering its failure, so the pump can observe the
// closed events channel first — and reporting a bare close then would throw
// away the only description of what went wrong.
func TestStreamErrorSurvivesTheRaceForIt(t *testing.T) {
	events := make(chan client.SSEEvent)
	errs := make(chan error, 1)
	wantErr := fmt.Errorf("GET /api/events: connection refused")
	errs <- wantErr
	close(errs)
	close(events)

	stream := &sseStream{events: events, errs: errs, cancel: func() {}}
	msg, ok := waitSSE(stream)().(sseDroppedMsg)
	if !ok {
		t.Fatalf("a closed stream produced %T, want a drop", waitSSE(stream)())
	}
	if msg.err == nil {
		t.Fatal("a failed stream reported no error at all")
	}
	if msg.err != wantErr && msg.err != errSSEClosed {
		t.Fatalf("drop error = %v, want the stream's own error or the clean-close sentinel", msg.err)
	}
}

// TestCleanStreamCloseIsADrop pins that a server closing tidily is still the
// end of push: the frame must fall back rather than sit forever waiting for
// events on a stream nobody is sending on.
func TestCleanStreamCloseIsADrop(t *testing.T) {
	events := make(chan client.SSEEvent)
	errs := make(chan error, 1)
	close(errs)
	close(events)

	msg, ok := waitSSE(&sseStream{events: events, errs: errs, cancel: func() {}})().(sseDroppedMsg)
	if !ok {
		t.Fatal("a cleanly closed stream did not produce a drop")
	}
	if msg.err != errSSEClosed {
		t.Errorf("drop error = %v, want %v", msg.err, errSSEClosed)
	}
}

// findGovernorMsg returns the delivered governor snapshot, or nil if none was
// produced.
func findGovernorMsg(msgs []tea.Msg) *panes.GovernorMsg {
	for _, msg := range msgs {
		if g, ok := msg.(panes.GovernorMsg); ok {
			return &g
		}
	}
	return nil
}
