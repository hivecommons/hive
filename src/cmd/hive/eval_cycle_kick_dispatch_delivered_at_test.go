package main

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/scheduler"
)

// TestDispatchAgentKicks_OnDeliveredCarriesLaneDeliveryTime pins the fix for
// the hosted scanner's "idle forever after a 3-minute turn": effects are
// applied in message order, so when the FIRST agent's lane spends minutes in
// prompt timeouts, a later agent's onDelivered runs long after its kick was
// typed — possibly after that agent's turn already ended. onDelivered must
// receive the lane's delivery time, not the time the effect was applied.
func TestDispatchAgentKicks_OnDeliveredCarriesLaneDeliveryTime(t *testing.T) {
	base := time.Date(2026, 10, 28, 4, 10, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := base
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(d)
	}
	// reviewer's send blocks until the scanner lane has read its delivery
	// time, so the clock cannot move before that read.
	scannerSent := make(chan struct{})
	var scannerAdvanced bool
	var releaseReviewer sync.Once
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := clock
		if scannerAdvanced {
			releaseReviewer.Do(func() { close(scannerSent) })
		}
		return t
	}

	// reviewer's send models a prompt timeout on the first lane.
	r := newKickDispatchRecorder()
	deps := r.deps()
	deps.now = now
	deps.sendKick = func(agent, message string) error {
		switch agent {
		case "reviewer":
			<-scannerSent
			advance(4 * time.Minute)
			return errors.New("agent reviewer CLI did not reach input prompt")
		case "scanner":
			mu.Lock()
			clock = clock.Add(30 * time.Second)
			scannerAdvanced = true
			mu.Unlock()
		}
		return nil
	}

	msgs := []scheduler.KickMessage{
		{Agent: "reviewer", Message: "review"},
		{Agent: "scanner", Message: "scan"},
	}
	delivered := dispatchAgentKicks(msgs, false, deps, slog.Default())
	if len(delivered) != 1 || delivered[0] != "scanner" {
		t.Fatalf("delivered = %v, want [scanner]", delivered)
	}
	if len(r.deliveredAt) != 1 {
		t.Fatalf("onDelivered calls = %d, want 1", len(r.deliveredAt))
	}
	want := base.Add(30 * time.Second)
	if !r.deliveredAt[0].Equal(want) {
		t.Fatalf("onDelivered deliveredAt = %v, want the lane's send time %v (effect applied at %v)",
			r.deliveredAt[0], want, now())
	}
}
