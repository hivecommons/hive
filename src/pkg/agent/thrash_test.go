package agent

import (
	"testing"
	"time"
)

func TestRecordBlockedAndCheck_TripsAtThresholdWithinWindow(t *testing.T) {
	st := &thrashState{}
	base := time.Now()
	window, threshold, cooldown := 60*time.Second, 5, 10*time.Minute

	// Four hits: below threshold, never trips.
	for i := 0; i < 4; i++ {
		if recordBlockedAndCheck(st, base.Add(time.Duration(i)*3*time.Second), window, threshold, cooldown) {
			t.Fatalf("tripped below threshold at hit %d", i+1)
		}
	}
	// Fifth hit inside the window: trips.
	if !recordBlockedAndCheck(st, base.Add(12*time.Second), window, threshold, cooldown) {
		t.Fatal("did not trip at threshold")
	}
	// Immediately after a trip (cooldown), even a fresh burst must not re-trip.
	for i := 0; i < 6; i++ {
		if recordBlockedAndCheck(st, base.Add(20*time.Second+time.Duration(i)*3*time.Second), window, threshold, cooldown) {
			t.Fatal("re-tripped inside cooldown")
		}
	}
	// After the cooldown, a new burst trips again.
	late := base.Add(11 * time.Minute)
	for i := 0; i < 4; i++ {
		recordBlockedAndCheck(st, late.Add(time.Duration(i)*3*time.Second), window, threshold, cooldown)
	}
	if !recordBlockedAndCheck(st, late.Add(12*time.Second), window, threshold, cooldown) {
		t.Fatal("did not trip after cooldown expired")
	}
}

func TestRecordBlockedAndCheck_SlowDripNeverTrips(t *testing.T) {
	st := &thrashState{}
	base := time.Now()
	// One blocked push every 2 minutes: legitimate occasional denials (an agent
	// probing once per task) must never pause the agent.
	for i := 0; i < 20; i++ {
		if recordBlockedAndCheck(st, base.Add(time.Duration(i)*2*time.Minute), 60*time.Second, 5, 10*time.Minute) {
			t.Fatalf("slow drip tripped at hit %d", i+1)
		}
	}
}
