package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

func TestKickObserver_NotifyAndRemove(t *testing.T) {
	m := &Manager{}

	// Nil-safe: no observer installed.
	m.notifyKickObserver("scanner", KickObserverEventDelivered, "governor")

	got := make(chan [3]string, 4)
	m.SetKickObserver(func(agentName, event, detail string) {
		got <- [3]string{agentName, event, detail}
	})
	m.notifyKickObserver("scanner", KickObserverEventDelivered, "governor")
	select {
	case ev := <-got:
		want := [3]string{"scanner", KickObserverEventDelivered, "governor"}
		if ev != want {
			t.Errorf("event = %v, want %v", ev, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("observer never invoked")
	}

	m.notifyKickObserver("scanner", KickObserverEventArchived, "kick")
	select {
	case ev := <-got:
		if ev[1] != KickObserverEventArchived {
			t.Errorf("event = %v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("observer never invoked for archive")
	}

	// Removing the observer makes notifications no-ops again.
	m.SetKickObserver(nil)
	m.notifyKickObserver("scanner", KickObserverEventDelivered, "x")
	select {
	case ev := <-got:
		t.Fatalf("removed observer invoked: %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

const kickObsTestWait = 2 * time.Second

func recvKickEvent(t *testing.T, ch <-chan crux.KickEvent) crux.KickEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(kickObsTestWait):
		t.Fatal("observer never received an event")
	}
	return crux.KickEvent{}
}

func TestAddKickObserver_FanOutInOrderAndRemove(t *testing.T) {
	m := &Manager{}
	a := make(chan crux.KickEvent, 8)
	b := make(chan crux.KickEvent, 8)
	removeA := m.AddKickObserver(func(ev crux.KickEvent) { a <- ev })
	removeB := m.AddKickObserver(func(ev crux.KickEvent) { b <- ev })
	defer removeB()

	m.notifyKickObserver("scanner", KickObserverEventDelivered, "governor")
	m.notifyKickObserver("scanner", KickObserverEventArchived, "kick")
	for _, ch := range []chan crux.KickEvent{a, b} {
		first, second := recvKickEvent(t, ch), recvKickEvent(t, ch)
		if first.Event != crux.KickEventDelivered || first.Detail != "governor" || first.Agent != "scanner" || first.At.IsZero() {
			t.Errorf("first event = %+v", first)
		}
		if second.Event != crux.KickEventArchived {
			t.Errorf("events out of order: second = %+v", second)
		}
		if first.KickID != "" {
			t.Errorf("kick outside the queue carried kick id %q", first.KickID)
		}
	}

	removeA()
	removeA() // idempotent
	m.notifyKickObserver("scanner", KickObserverEventDelivered, "x")
	recvKickEvent(t, b)
	select {
	case ev := <-a:
		t.Fatalf("removed observer received %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	if noop := m.AddKickObserver(nil); noop == nil {
		t.Fatal("nil observer returned a nil remover")
	} else {
		noop()
	}
}

func TestAddKickObserver_SlowObserverDropsInsteadOfBlocking(t *testing.T) {
	for _, m := range []*Manager{{}, {logger: discardLogger()}} {
		release := make(chan struct{})
		remove := m.AddKickObserver(func(crux.KickEvent) { <-release })
		done := make(chan struct{})
		go func() {
			for i := 0; i < kickObserverBuffer+2; i++ {
				m.emitKickEvent(crux.KickEvent{Agent: "a", Event: crux.KickEventQueued})
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(kickObsTestWait):
			t.Fatal("emit blocked on a slow observer")
		}
		close(release)
		remove()
	}
}

func TestKickObserver_QueueEventsCarryKickID(t *testing.T) {
	m := newKickQueueManager(t, func(string) bool { return true }, func(name, msg string) error {
		if msg == "broken" {
			return errors.New("no pane")
		}
		return nil
	})
	legacy := make(chan string, 8)
	m.SetKickObserver(func(_, event, _ string) { legacy <- event })
	events := make(chan crux.KickEvent, 16)
	defer m.AddKickObserver(func(ev crux.KickEvent) { events <- ev })()

	id, err := m.EnqueueKick(kqTestAgent, "r-1", "research")
	if err != nil {
		t.Fatal(err)
	}
	if ev := recvKickEvent(t, events); ev.Event != crux.KickEventQueued || ev.KickID != id || ev.Agent != kqTestAgent {
		t.Fatalf("queued event = %+v", ev)
	}
	waitKickPhase(t, m, id, crux.KickPhaseDelivered)

	// A delivery notification while the queued turn runs carries its id.
	m.notifyKickObserver(kqTestAgent, KickObserverEventDelivered, "send-kick")
	if ev := recvKickEvent(t, events); ev.Event != crux.KickEventDelivered || ev.KickID != id {
		t.Fatalf("delivered event = %+v, want kick id %s", ev, id)
	}
	if got := <-legacy; got != KickObserverEventDelivered {
		t.Fatalf("legacy observer got %q", got)
	}

	m.notifyKickOutcome(kqTestAgent, KickOutcome{Kind: KickOutcomeEnded})
	if ev := recvKickEvent(t, events); ev.Event != crux.KickEventCompleted || ev.KickID != id || ev.Detail != KickOutcomeEnded {
		t.Fatalf("completed event = %+v", ev)
	}

	broken, _ := m.EnqueueKick(kqTestAgent, "r-2", "broken")
	recvKickEvent(t, events) // queued
	if ev := recvKickEvent(t, events); ev.Event != crux.KickEventFailed || ev.KickID != broken || ev.Detail != "no pane" {
		t.Fatalf("failed event = %+v", ev)
	}

	// The single-slot observer saw the delivery and nothing from the queue.
	select {
	case ev := <-legacy:
		t.Fatalf("legacy observer received queue event %q", ev)
	case <-time.After(50 * time.Millisecond):
	}
}
