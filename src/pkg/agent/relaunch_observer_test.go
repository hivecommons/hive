package agent

import (
	"testing"
	"time"
)

func TestRelaunchObserver_NotifyAndRemove(t *testing.T) {
	m := &Manager{}
	m.notifyRelaunchObserver("scanner") // nil-safe

	got := make(chan string, 2)
	m.SetRelaunchObserver(func(agentName string) { got <- agentName })
	m.notifyRelaunchObserver("scanner")
	select {
	case name := <-got:
		if name != "scanner" {
			t.Fatalf("agent = %q", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("observer never invoked")
	}

	m.SetRelaunchObserver(nil)
	m.notifyRelaunchObserver("scanner")
	select {
	case name := <-got:
		t.Fatalf("removed observer invoked: %v", name)
	case <-time.After(50 * time.Millisecond):
	}
}
