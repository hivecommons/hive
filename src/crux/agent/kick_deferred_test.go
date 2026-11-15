package agent

import (
	"fmt"
	"testing"
	"time"
)

// Deferred startup kicks and the kick-dropped notification (v6-only; moved
// out of kick_observer_test.go so that file stays identical to v5's).

func TestDeferredStartupMentionsQueueFIFOWithoutDrop(t *testing.T) {
	m := &Manager{agents: map[string]*AgentProcess{}, logger: discardLogger()}
	agent := &AgentProcess{Name: "scanner", startupLaunchQueued: true}
	m.agents[agent.Name] = agent
	got := make(chan [3]string, 4)
	m.SetKickObserver(func(agentName, event, detail string) {
		got <- [3]string{agentName, event, detail}
	})

	m.mu.Lock()
	for _, k := range []deferredKick{
		{message: "first", source: "mention:first"},
		{message: "second", source: "mention:second"},
		{message: "first again", source: "mention:first"},
	} {
		if !m.deferStartupKickLocked(agent, k.message, k.source) {
			t.Fatalf("deferred startup kick %q was not accepted", k.source)
		}
	}
	m.mu.Unlock()

	select {
	case ev := <-got:
		t.Fatalf("queued mentions must not be dropped, got %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	want := []deferredKick{
		{message: "first again", source: "mention:first"},
		{message: "second", source: "mention:second"},
	}
	if len(agent.deferredMentionKicks) != len(want) {
		t.Fatalf("deferredMentionKicks = %+v, want %+v", agent.deferredMentionKicks, want)
	}
	for i := range want {
		if agent.deferredMentionKicks[i] != want[i] {
			t.Fatalf("deferredMentionKicks[%d] = %+v, want %+v", i, agent.deferredMentionKicks[i], want[i])
		}
	}
	if agent.pendingStartupKick != "" {
		t.Fatalf("mention kick leaked into the coalesced startup slot: %q", agent.pendingStartupKick)
	}
}

func TestDeferredStartupCadenceKickDoesNotEvictMention(t *testing.T) {
	m := &Manager{agents: map[string]*AgentProcess{}, logger: discardLogger()}
	agent := &AgentProcess{Name: "reviewer", startupLaunchQueued: true}
	m.agents[agent.Name] = agent
	got := make(chan [3]string, 4)
	m.SetKickObserver(func(agentName, event, detail string) {
		got <- [3]string{agentName, event, detail}
	})

	m.mu.Lock()
	if !m.deferStartupKickLocked(agent, "please review", "mention:IC_1") {
		t.Fatal("mention kick was not deferred")
	}
	if !m.deferStartupKickLocked(agent, "cadence 1", GovernorCadenceKickSource) {
		t.Fatal("cadence kick was not deferred")
	}
	if !m.deferStartupKickLocked(agent, "cadence 2", GovernorCadenceKickSource) {
		t.Fatal("second cadence kick was not deferred")
	}
	m.mu.Unlock()

	select {
	case ev := <-got:
		t.Fatalf("cadence kick must not drop the deferred mention, got %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	if len(agent.deferredMentionKicks) != 1 || agent.deferredMentionKicks[0].source != "mention:IC_1" {
		t.Fatalf("deferredMentionKicks = %+v, want the mention kept", agent.deferredMentionKicks)
	}
	if agent.pendingStartupKick != "cadence 2" || agent.pendingStartupSource != GovernorCadenceKickSource {
		t.Fatalf("cadence kicks did not coalesce: (%q,%q)", agent.pendingStartupKick, agent.pendingStartupSource)
	}

	m.mu.Lock()
	msg, trigger, fromQueue := m.nextDeferredMentionKickLocked(agent, "one-shot prompt", "send-kick")
	m.mu.Unlock()
	if fromQueue || msg != "one-shot prompt" || trigger != "send-kick" {
		t.Fatalf("one-shot kick must be delivered as sent, got (%q,%q,%v)", msg, trigger, fromQueue)
	}
	if len(agent.deferredMentionKicks) != 1 {
		t.Fatalf("one-shot kick consumed the queued mention: %+v", agent.deferredMentionKicks)
	}

	m.mu.Lock()
	msg, trigger, fromQueue = m.nextDeferredMentionKickLocked(agent, "cadence 3", GovernorCadenceKickSource)
	m.mu.Unlock()
	if !fromQueue || msg != "please review" || trigger != "mention:IC_1" {
		t.Fatalf("next kick = (%q,%q,%v), want queued mention", msg, trigger, fromQueue)
	}
	removeDeferredMentionKickLocked(agent, trigger)
	if len(agent.deferredMentionKicks) != 0 {
		t.Fatalf("delivered mention not removed: %+v", agent.deferredMentionKicks)
	}

	m.mu.Lock()
	msg, trigger, fromQueue = m.nextDeferredMentionKickLocked(agent, "cadence 4", GovernorCadenceKickSource)
	m.mu.Unlock()
	if fromQueue || msg != "cadence 4" || trigger != GovernorCadenceKickSource {
		t.Fatalf("empty queue must pass the kick through, got (%q,%q,%v)", msg, trigger, fromQueue)
	}
}

func TestDeferredStartupMentionQueueOverflowNotifiesDropped(t *testing.T) {
	m := &Manager{agents: map[string]*AgentProcess{}, logger: discardLogger()}
	agent := &AgentProcess{Name: "scanner", startupLaunchQueued: true}
	m.agents[agent.Name] = agent
	got := make(chan [3]string, 4)
	m.SetKickObserver(func(agentName, event, detail string) {
		got <- [3]string{agentName, event, detail}
	})

	m.mu.Lock()
	for i := 0; i <= maxDeferredMentionKicks; i++ {
		src := fmt.Sprintf("mention:%d", i)
		if !m.deferStartupKickLocked(agent, "msg "+src, src) {
			t.Fatalf("deferred startup kick %q was not accepted", src)
		}
	}
	m.mu.Unlock()

	select {
	case ev := <-got:
		want := [3]string{"scanner", "kick-dropped", "mention:0"}
		if ev != want {
			t.Fatalf("event = %v, want %v", ev, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("overflow did not notify dropped mention source")
	}
	if len(agent.deferredMentionKicks) != maxDeferredMentionKicks {
		t.Fatalf("queue length = %d, want %d", len(agent.deferredMentionKicks), maxDeferredMentionKicks)
	}
	if agent.deferredMentionKicks[0].source != "mention:1" {
		t.Fatalf("oldest queued = %q, want mention:1", agent.deferredMentionKicks[0].source)
	}
}

func TestStopDropsDeferredMentionKicks(t *testing.T) {
	m := &Manager{agents: map[string]*AgentProcess{}, logger: discardLogger()}
	agent := &AgentProcess{Name: "scanner", State: StateStopped, startupLaunchQueued: true}
	m.agents[agent.Name] = agent
	got := make(chan [3]string, 4)
	m.SetKickObserver(func(agentName, event, detail string) {
		got <- [3]string{agentName, event, detail}
	})

	m.mu.Lock()
	for _, src := range []string{"mention:a", "action:b"} {
		if !m.deferStartupKickLocked(agent, "msg "+src, src) {
			t.Fatalf("deferred startup kick %q was not accepted", src)
		}
	}
	m.mu.Unlock()

	if err := m.Stop("scanner"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(agent.deferredMentionKicks) != 0 {
		t.Fatalf("Stop left queued mentions: %+v", agent.deferredMentionKicks)
	}
	dropped := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-got:
			if ev[0] != "scanner" || ev[1] != "kick-dropped" {
				t.Fatalf("event = %v, want kick-dropped", ev)
			}
			dropped[ev[2]] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("Stop did not report every dropped mention, got %v", dropped)
		}
	}
	if !dropped["mention:a"] || !dropped["action:b"] {
		t.Fatalf("dropped sources = %v, want mention:a and action:b", dropped)
	}
}
