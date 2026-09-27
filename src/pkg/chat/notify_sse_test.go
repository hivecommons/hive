package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

func fullFrame(t *testing.T, busy, mode, phase string) string {
	t.Helper()
	snap := statusSnapshot{
		Agents:   []agentSnapshot{{Name: "scanner", Busy: busy}},
		Governor: governorSnapshot{Mode: mode, Issues: 1},
		Inception: inceptionSnapshot{
			Active:    true,
			Phase:     phase,
			Questions: []inceptionQuestion{{ID: "q1", Text: "First?"}},
			Answers:   map[string]string{},
		},
		Runs: runSnapshotList{{Key: "repo/a#1", Stage: "spec", WaitingOn: "agent"}},
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(data)
}

func agentStatusFrameBlock(busy string) string {
	return fmt.Sprintf(`event: agent-status
data: {"agents":[{"name":"scanner","busy":%q}],"govMode":"busy"}`, busy)
}

func TestHandleSSEBlock_AgentStatusPreservesFullState(t *testing.T) {
	backend := &recordingBackend{}
	s := NewService(backend, Config{AllowedUsers: []string{"uid:owner"}}, discardLogger())
	s.topicDebounce = 20 * time.Millisecond

	s.handleSSEBlock(fullFrame(t, "idle", "idle", ""))
	s.handleSSEBlock(fullFrame(t, "idle", "idle", "clarify"))
	s.handleSSEBlock(agentStatusFrameBlock("idle"))
	s.handleSSEBlock(fullFrame(t, "idle", "busy", "clarify"))
	s.handleSSEBlock(agentStatusFrameBlock("idle"))
	if got := len(s.pendingInterviews); got != 1 {
		t.Fatalf("pendingInterviews after agent-status frame = %d, want 1", got)
	}
	s.handleSSEBlock(fullFrame(t, "idle", "busy", "clarify"))

	var sent []string
	drainQueue(s, &sent)
	var gov, questions int
	for _, msg := range sent {
		if strings.Contains(msg, "Governor mode change") {
			gov++
		}
		if strings.Contains(msg, "Inception clarification questions") {
			questions++
		}
	}
	if gov != 1 {
		t.Fatalf("governor announcements = %d, want 1: %v", gov, sent)
	}
	if questions != 1 {
		t.Fatalf("questions posted %d times, want 1: %v", questions, sent)
	}
	if st := s.lastState; st.Governor.Mode != "busy" || st.Inception.Phase != "clarify" || len(st.Runs) != 1 {
		t.Fatalf("full-frame state lost: %+v", st)
	}

	testutil.Eventually(t, 2*time.Second, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return len(backend.topics) == 1
	}, "debounced SetTopic never fired")
	backend.mu.Lock()
	topics := append([]string(nil), backend.topics...)
	backend.mu.Unlock()
	if len(topics) != 1 || !strings.Contains(topics[0], "busy") {
		t.Fatalf("SetTopic calls = %q, want one busy topic", topics)
	}
}

func TestHandleSSEBlock_AgentStatusDiffsAgents(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{}, discardLogger())
	s.handleSSEBlock(agentStatusFrameBlock("working")) // no baseline yet: ignored
	if s.lastState != nil {
		t.Fatal("agent-status frame without a baseline should not seed state")
	}
	s.handleSSEBlock(fullFrame(t, "idle", "idle", ""))
	s.handleSSEBlock(agentStatusFrameBlock("working"))
	s.handleSSEBlock(agentStatusFrameBlock("working"))
	s.handleSSEBlock("event: other\ndata: {}")
	s.handleSSEBlock("event: agent-status\ndata: not-json")
	s.handleSSEBlock(": keepalive")

	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "Working") {
		t.Fatalf("agent transitions = %v, want one Working", sent)
	}
}

func TestUpdateTopic_DebounceLatestWins(t *testing.T) {
	backend := &recordingBackend{}
	s := NewService(backend, Config{}, discardLogger())
	s.topicDebounce = 20 * time.Millisecond

	for _, mode := range []string{"a", "b", "c"} {
		s.updateTopic(&statusSnapshot{Governor: governorSnapshot{Mode: mode}})
	}
	testutil.Eventually(t, 2*time.Second, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return len(backend.topics) == 1
	}, "debounced SetTopic never fired")
	backend.mu.Lock()
	topics := append([]string(nil), backend.topics...)
	backend.mu.Unlock()
	if len(topics) != 1 || !strings.Contains(topics[0], " c ") {
		t.Fatalf("SetTopic calls = %q, want only the latest topic", topics)
	}

	s.updateTopic(&statusSnapshot{Governor: governorSnapshot{Mode: "d"}})
	s.stopTopicTimer()
	// Deliberate negative wait: the debounce is 20ms, so give a stopped timer
	// more than one debounce window to (wrongly) fire before asserting silence.
	<-time.After(50 * time.Millisecond)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.topics) != 1 {
		t.Fatalf("stopped debounce still applied topic: %q", backend.topics)
	}
}

func TestConsumeSSE_IdleWatchdogReconnects(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.sseIdleTimeout = 50 * time.Millisecond

	done := make(chan error, 1)
	var connected bool
	go func() {
		var err error
		connected, err = s.consumeSSE(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if !connected || !errors.Is(err, errSSEIdle) {
			t.Fatalf("consumeSSE = (%v, %v), want (true, idle error)", connected, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle watchdog did not tear down a silent SSE stream")
	}
}

func TestConsumeSSE_IdleWatchdogCoversHeaders(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(release)

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.sseIdleTimeout = 50 * time.Millisecond
	connected, err := s.consumeSSE(context.Background())
	if connected || !errors.Is(err, errSSEIdle) {
		t.Fatalf("consumeSSE = (%v, %v), want (false, idle error)", connected, err)
	}
}

func TestConsumeSSE_CapsUnterminatedFrame(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := strings.Repeat("x", 64<<10)
		for i := 0; i <= sseMaxPendingBytes/len(chunk)+1; i++ {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer ts.Close()

	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	_, err := s.consumeSSE(context.Background())
	if err == nil || !strings.Contains(err.Error(), "without terminator") {
		t.Fatalf("consumeSSE err = %v, want frame-size error", err)
	}
}
