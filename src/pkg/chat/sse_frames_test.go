package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

// sseFullFrame renders an unnamed (full status) SSE frame as the dashboard
// broadcasts it every eval cycle.
func sseFullFrame(t *testing.T, snap statusSnapshot) string {
	t.Helper()
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal full frame: %v", err)
	}
	return "data: " + string(data) + "\n\n"
}

// sseAgentStatusFrame renders an `event: agent-status` frame as the dashboard
// broadcasts it every agent poll: agents + govMode only, no governor /
// inception / runs fields.
func sseAgentStatusFrame(t *testing.T, agents []agentSnapshot, govMode string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"agents": agents, "govMode": govMode})
	if err != nil {
		t.Fatalf("marshal agent-status frame: %v", err)
	}
	return "event: agent-status\ndata: " + string(data) + "\n\n"
}

func countContaining(msgs []string, needle string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m, needle) {
			n++
		}
	}
	return n
}

// Regression for #9122: an `event: agent-status` frame between two full
// frames must not be treated as a full status. Before the fix it was
// unmarshalled into an empty statusSnapshot, which blanked Governor (mode
// change never announced), Inception (pending interviews cleared, questions
// re-posted every eval cycle) and flapped the channel topic.
func TestConsumeSSE_AgentStatusFrameDoesNotWipeFullState(t *testing.T) {
	capture := inceptionSnapshot{Active: true, Phase: "capture"}
	clarify := inceptionSnapshot{
		Active:    true,
		Phase:     "clarify",
		Questions: []inceptionQuestion{{ID: "q1", Text: "First?"}},
		Answers:   map[string]string{},
	}
	runs := runSnapshotList{{Key: "repo/a#1", Stage: "spec", WaitingOn: "agent"}}
	idle := []agentSnapshot{{Name: "scanner", Busy: "idle"}}
	working := []agentSnapshot{{Name: "scanner", Busy: "working", Doing: "triage"}}

	// full(idle,capture) → agent-status → full(busy,clarify) → agent-status → full(busy,clarify)
	stream := sseFullFrame(t, statusSnapshot{Agents: idle, Governor: governorSnapshot{Mode: "idle"}, Inception: capture, Runs: runs}) +
		sseAgentStatusFrame(t, working, "busy") +
		sseFullFrame(t, statusSnapshot{Agents: working, Governor: governorSnapshot{Mode: "busy", Issues: 1}, Inception: clarify, Runs: runs}) +
		sseAgentStatusFrame(t, working, "busy") +
		sseFullFrame(t, statusSnapshot{Agents: working, Governor: governorSnapshot{Mode: "busy", Issues: 1}, Inception: clarify, Runs: runs})

	var answerPosts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(stream))
		case "/api/inception/answer":
			answerPosts.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	backend := &recordingBackend{}
	s := NewService(backend, Config{DashboardURL: ts.URL, AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.client = ts.Client()
	s.topicDebounce = 20 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go s.topicLoop(ctx)

	// consumeSSE returns once the server closes the stream.
	if connected, _ := s.consumeSSE(ctx); !connected {
		t.Fatal("consumeSSE did not report connected")
	}

	var sent []string
	drainQueue(s, &sent)
	if got := countContaining(sent, "Governor mode change"); got != 1 {
		t.Fatalf("governor announcements = %d, want 1: %#v", got, sent)
	}
	if got := countContaining(sent, "Inception clarification questions"); got != 1 {
		t.Fatalf("question posts = %d, want 1: %#v", got, sent)
	}
	// The agent-status frame announced scanner idle→working; the full frame
	// that followed carried the same state and must not announce it again.
	if got := countContaining(sent, "Working"); got != 1 {
		t.Fatalf("agent working announcements = %d, want 1: %#v", got, sent)
	}

	s.mu.RLock()
	state := s.lastState
	s.mu.RUnlock()
	if state == nil || state.Governor.Mode != "busy" || state.Inception.Phase != "clarify" || len(state.Runs) != 1 {
		t.Fatalf("lastState after agent-status frame lost full-frame fields: %+v", state)
	}

	if s.pendingInterviews[s.pendingKey("uid")] == nil {
		t.Fatal("pending interview was cleared by the agent-status frame")
	}
	s.routeMessage(ctx, makeMsg("1", "my answer", false))
	if got := answerPosts.Load(); got != 1 {
		t.Fatalf("owner plain reply posted %d answers, want 1", got)
	}

	testutil.Eventually(t, 2*time.Second, func() bool {
		backend.mu.Lock()
		topics := append([]string(nil), backend.topics...)
		backend.mu.Unlock()
		if len(topics) > 1 {
			t.Fatalf("SetTopic calls = %#v, want exactly one for the busy state", topics)
		}
		return len(topics) == 1 && strings.Contains(topics[0], "busy · 1i 0pr")
	}, "topic never settled to the busy state")

	// Deliberate negative wait: confirm the debounce doesn't fire a second
	// SetTopic once its window has closed.
	<-time.After(4 * s.topicDebounce)
	backend.mu.Lock()
	final := len(backend.topics)
	backend.mu.Unlock()
	if final != 1 {
		t.Fatalf("SetTopic calls after settling = %d, want 1", final)
	}
}

// An agent-status frame arriving before any full frame has seeded lastState is
// ignored: the dashboard replays a full frame on connect, and diffing against
// nothing would announce every agent as a transition.
func TestOnAgentStatusEvent_IgnoredBeforeFirstFullFrame(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{}, discardLogger())
	s.onAgentStatusEvent(&agentStatusSnapshot{Agents: []agentSnapshot{{Name: "scanner", Busy: "working"}}})

	s.mu.RLock()
	state := s.lastState
	s.mu.RUnlock()
	if state != nil {
		t.Fatalf("agent-status frame seeded lastState: %+v", state)
	}
	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 0 {
		t.Fatalf("agent-status frame before first full frame queued %#v", sent)
	}
}

func TestHandleSSEFrame_UnknownEventIgnored(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{}, discardLogger())
	s.onSSEEvent(&statusSnapshot{Agents: []agentSnapshot{{Name: "scanner", Busy: "idle"}}, Governor: governorSnapshot{Mode: "idle"}})

	s.handleSSEFrame("event: something-else\ndata: {\"agents\":[{\"name\":\"scanner\",\"busy\":\"working\"}]}")

	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 0 {
		t.Fatalf("unknown event frame queued %#v", sent)
	}
	s.mu.RLock()
	busy := s.lastState.Agents[0].Busy
	s.mu.RUnlock()
	if busy != "idle" {
		t.Fatalf("unknown event frame changed lastState agents: %q", busy)
	}
}

// The topic debounce is a single timer: the latest topic wins, coalesced
// changes produce one SetTopic, and cancelling the context drops a pending
// update instead of firing it after shutdown.
func TestTopicLoop_LatestWinsAndStopsOnCancel(t *testing.T) {
	backend := &recordingBackend{}
	s := NewService(backend, Config{}, discardLogger())
	s.topicDebounce = 30 * time.Millisecond
	// updateTopic skips the first full frame (no previous state); seed one.
	s.onSSEEvent(&statusSnapshot{Agents: []agentSnapshot{{Name: "scanner", Busy: "idle"}}})

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() {
		s.topicLoop(ctx)
		close(loopDone)
	}()

	for _, mode := range []string{"idle", "quiet", "busy"} {
		s.updateTopic(&statusSnapshot{Agents: []agentSnapshot{{Name: "scanner", Busy: "idle"}}, Governor: governorSnapshot{Mode: mode}})
	}

	testutil.Eventually(t, 2*time.Second, func() bool {
		backend.mu.Lock()
		topics := append([]string(nil), backend.topics...)
		backend.mu.Unlock()
		if len(topics) == 0 {
			return false
		}
		if len(topics) != 1 || !strings.Contains(topics[0], "busy") {
			t.Fatalf("SetTopic calls = %#v, want one call with the latest topic", topics)
		}
		return true
	}, "debounced SetTopic never fired")

	// A change queued and then cancelled before the debounce elapses must not
	// reach the backend.
	s.updateTopic(&statusSnapshot{Agents: []agentSnapshot{{Name: "scanner", Busy: "working"}}, Governor: governorSnapshot{Mode: "busy"}})
	cancel()
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("topicLoop did not exit after cancellation")
	}
	// Deliberate negative wait: cancellation must drop the queued update
	// instead of firing it after the debounce window closes.
	<-time.After(3 * s.topicDebounce)
	backend.mu.Lock()
	final := len(backend.topics)
	backend.mu.Unlock()
	if final != 1 {
		t.Fatalf("SetTopic calls after cancel = %d, want 1", final)
	}
}
