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

func TestKickObserver_ChainsObservers(t *testing.T) {
	m := &Manager{}
	got := make(chan string, 2)
	m.SetKickObserver(func(agentName, event, detail string) {
		got <- "first:" + event
	})
	m.SetKickObserver(func(agentName, event, detail string) {
		got <- "second:" + event
	})
	m.notifyKickObserver("scanner", KickObserverEventArchived, "archive")
	want := map[string]bool{
		"first:" + KickObserverEventArchived:  false,
		"second:" + KickObserverEventArchived: false,
	}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-got:
			if _, ok := want[ev]; !ok {
				t.Fatalf("unexpected event %q", ev)
			}
			want[ev] = true
		case <-time.After(2 * time.Second):
			t.Fatal("observer chain did not fire")
		}
	}
	for ev, seen := range want {
		if !seen {
			t.Fatalf("observer %q did not fire", ev)
		}
	}
}

func TestKickObserverArchiveDetailCarriesSource(t *testing.T) {
	detail := kickObserverArchiveDetail("kick", "mention")
	if detail != "kick source=mention" {
		t.Fatalf("detail = %q", detail)
	}
	if got := KickObserverDetailReason(detail); got != "kick" {
		t.Fatalf("reason = %q", got)
	}
	if got := KickObserverDetailSource(detail); got != "mention" {
		t.Fatalf("source = %q", got)
	}
	if got := kickObserverArchiveDetail("kick", ""); got != "kick" {
		t.Fatalf("empty source detail = %q", got)
	}
}
