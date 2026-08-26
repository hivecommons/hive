package linearagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingPoster is an in-memory ActivityPoster recording every activity.
type recordingPoster struct {
	mu         sync.Mutex
	activities []recordedActivity
	failWith   error
}

type recordedActivity struct {
	SessionID string
	Content   ActivityContent
	At        time.Time
}

func (p *recordingPoster) CreateActivity(_ context.Context, sessionID string, content ActivityContent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failWith != nil {
		return p.failWith
	}
	p.activities = append(p.activities, recordedActivity{SessionID: sessionID, Content: content, At: time.Now()})
	return nil
}

func (p *recordingPoster) snapshot() []recordedActivity {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedActivity(nil), p.activities...)
}

func createdEvent() SessionEvent {
	var ev SessionEvent
	ev.Type = agentSessionEventType
	ev.Action = actionCreated
	ev.AgentSession.ID = "sess-1"
	ev.AgentSession.Issue.ID = "iss-1"
	ev.AgentSession.Issue.Identifier = "ENG-42"
	ev.AgentSession.Issue.Title = "Fix it"
	ev.AgentSession.Issue.URL = "https://linear.app/acme/issue/ENG-42"
	ev.PromptContext = "<issue>context</issue>"
	return ev
}

func staticResolver(name string) AgentResolver {
	return func() (string, error) { return name, nil }
}

// TestResponder_AckDoesNotWaitOnKick is the architectural-crux test: the
// thought must be posted BEFORE the kick path runs at all, so a kick that
// blocks as long as a real tmux agent (or a 300s governor interval) cannot
// consume the 10-second budget. The kick blocks until the test releases it;
// the ack must be observable while it is still blocked.
func TestResponder_AckDoesNotWaitOnKick(t *testing.T) {
	poster := &recordingPoster{}
	kickEntered := make(chan struct{})
	kickRelease := make(chan struct{})
	kick := func(agent, msg string) error {
		close(kickEntered)
		<-kickRelease // the governor/tmux black hole
		return nil
	}
	r := NewResponder(poster, kick, staticResolver("scanner"), NewTracker(), quietLogger())

	done := make(chan struct{})
	go func() {
		r.HandleSessionEvent(createdEvent())
		close(done)
	}()

	select {
	case <-kickEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("kick never attempted")
	}
	// The kick is blocked right now. The ack must already have been posted.
	acts := poster.snapshot()
	if len(acts) == 0 {
		t.Fatal("no activity posted before the kick completed — the ack waited on the kick")
	}
	if acts[0].Content.Type != ActivityThought {
		t.Fatalf("first activity = %q, want thought", acts[0].Content.Type)
	}
	if acts[0].SessionID != "sess-1" {
		t.Errorf("session = %q", acts[0].SessionID)
	}

	close(kickRelease)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleSessionEvent never returned")
	}
}

func TestResponder_CreatedHappyPath(t *testing.T) {
	poster := &recordingPoster{}
	var gotAgent, gotMsg string
	kick := func(agent, msg string) error { gotAgent, gotMsg = agent, msg; return nil }
	tracker := NewTracker()
	r := NewResponder(poster, kick, staticResolver("scanner"), tracker, quietLogger())

	r.HandleSessionEvent(createdEvent())

	acts := poster.snapshot()
	if len(acts) != 2 {
		t.Fatalf("activities = %d, want thought + action", len(acts))
	}
	if acts[0].Content.Type != ActivityThought || !strings.Contains(acts[0].Content.Body, "ENG-42") || !strings.Contains(acts[0].Content.Body, "scanner") {
		t.Errorf("thought = %+v", acts[0].Content)
	}
	if acts[1].Content.Type != ActivityAction || acts[1].Content.Action != "Delegated" || !strings.Contains(acts[1].Content.Parameter, "scanner") {
		t.Errorf("action = %+v", acts[1].Content)
	}
	if gotAgent != "scanner" {
		t.Errorf("kicked %q", gotAgent)
	}
	for _, want := range []string{"sess-1", "ENG-42", "Fix it", "https://linear.app/acme/issue/ENG-42", "<issue>context</issue>"} {
		if !strings.Contains(gotMsg, want) {
			t.Errorf("kick message missing %q:\n%s", want, gotMsg)
		}
	}
	sessions := tracker.Snapshot()
	if len(sessions) != 1 || sessions[0].State != SessionStateWorking || sessions[0].Agent != "scanner" {
		t.Errorf("tracked = %+v", sessions)
	}
}

func TestResponder_PromptedForwardsMessage(t *testing.T) {
	poster := &recordingPoster{}
	var gotMsg string
	kick := func(agent, msg string) error { gotMsg = msg; return nil }
	r := NewResponder(poster, kick, staticResolver("scanner"), NewTracker(), quietLogger())

	ev := createdEvent()
	ev.Action = actionPrompted
	ev.AgentActivity.Content.Body = "please also add tests"
	r.HandleSessionEvent(ev)

	if !strings.Contains(gotMsg, "follow-up") || !strings.Contains(gotMsg, "please also add tests") {
		t.Errorf("kick message = %q", gotMsg)
	}
	acts := poster.snapshot()
	if !strings.Contains(acts[0].Content.Body, "follow-up") {
		t.Errorf("thought = %+v", acts[0].Content)
	}
}

func TestResponder_NoAgentConfigured(t *testing.T) {
	poster := &recordingPoster{}
	kicked := false
	kick := func(agent, msg string) error { kicked = true; return nil }
	tracker := NewTracker()
	resolve := func() (string, error) { return "", errors.New("set work_source.linear.session_agent") }
	r := NewResponder(poster, kick, resolve, tracker, quietLogger())

	r.HandleSessionEvent(createdEvent())

	if kicked {
		t.Error("kick attempted with no resolvable agent")
	}
	acts := poster.snapshot()
	if len(acts) != 2 || acts[0].Content.Type != ActivityThought || acts[1].Content.Type != ActivityError {
		t.Fatalf("activities = %+v", acts)
	}
	if !strings.Contains(acts[1].Content.Body, "session_agent") {
		t.Errorf("error body = %q", acts[1].Content.Body)
	}
	if s := tracker.Snapshot(); s[0].State != SessionStateFailed {
		t.Errorf("state = %q", s[0].State)
	}
}

func TestResponder_KickFailureReportsError(t *testing.T) {
	poster := &recordingPoster{}
	kick := func(agent, msg string) error { return fmt.Errorf("agent scanner cannot be kicked: paused") }
	tracker := NewTracker()
	r := NewResponder(poster, kick, staticResolver("scanner"), tracker, quietLogger())

	r.HandleSessionEvent(createdEvent())

	acts := poster.snapshot()
	if len(acts) != 2 || acts[1].Content.Type != ActivityError {
		t.Fatalf("activities = %+v", acts)
	}
	if !strings.Contains(acts[1].Content.Body, "paused") {
		t.Errorf("error body = %q", acts[1].Content.Body)
	}
	if s := tracker.Snapshot(); s[0].State != SessionStateFailed {
		t.Errorf("state = %q", s[0].State)
	}
}

// TestResponder_AckFailureStillKicks: a lost ack must not lose the work.
func TestResponder_AckFailureStillKicks(t *testing.T) {
	poster := &recordingPoster{failWith: errors.New("linear graphql status 502")}
	kicked := false
	kick := func(agent, msg string) error { kicked = true; return nil }
	r := NewResponder(poster, kick, staticResolver("scanner"), NewTracker(), quietLogger())
	r.HandleSessionEvent(createdEvent())
	if !kicked {
		t.Fatal("kick skipped after ack failure")
	}
}

func TestResponder_HandleAgentEvent(t *testing.T) {
	poster := &recordingPoster{}
	kick := func(agent, msg string) error { return nil }
	tracker := NewTracker()
	r := NewResponder(poster, kick, staticResolver("scanner"), tracker, quietLogger())
	r.HandleSessionEvent(createdEvent())

	// Unrelated events and unrelated agents are ignored.
	r.HandleAgentEvent("scanner", "kick-delivered", "governor")
	r.HandleAgentEvent("other-agent", "kick-log-archived", "kick")
	if n := len(poster.snapshot()); n != 2 {
		t.Fatalf("activities after ignored events = %d, want 2", n)
	}

	// The bound agent's archive finishes the session.
	r.HandleAgentEvent("scanner", "kick-log-archived", "kick")
	acts := poster.snapshot()
	if len(acts) != 3 || acts[2].Content.Type != ActivityResponse {
		t.Fatalf("activities = %+v", acts)
	}
	if !strings.Contains(acts[2].Content.Body, "scanner") {
		t.Errorf("response body = %q", acts[2].Content.Body)
	}
	if s := tracker.Snapshot(); s[0].State != SessionStateFinished {
		t.Errorf("state = %q", s[0].State)
	}

	// A second archive (the next unrelated run rotating) posts nothing —
	// the binding was released when the session finished.
	r.HandleAgentEvent("scanner", "kick-log-archived", "restart")
	if n := len(poster.snapshot()); n != 3 {
		t.Errorf("activities after released binding = %d, want 3", n)
	}
}

func TestBuildKickMessage_Truncates(t *testing.T) {
	ev := createdEvent()
	ev.PromptContext = strings.Repeat("y", responderKickLimit*2)
	msg := buildKickMessage(ev)
	if len([]rune(msg)) > responderKickLimit+1 {
		t.Errorf("kick message length = %d", len([]rune(msg)))
	}
}

func TestResponder_TrackerAccessorAndDefaults(t *testing.T) {
	r := NewResponder(&recordingPoster{}, nil, nil, nil, nil)
	if r.Tracker() == nil {
		t.Fatal("nil tracker not defaulted")
	}
	if r.ackTimeout != ackDeadline {
		t.Errorf("ackTimeout = %v", r.ackTimeout)
	}
	r.SetAckTimeout(time.Second)
	if r.ackTimeout != time.Second {
		t.Errorf("SetAckTimeout ignored")
	}
}
