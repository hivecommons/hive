package agent

import (
	"testing"
	"time"
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
