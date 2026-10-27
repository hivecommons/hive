package agent

import (
	"testing"
	"time"
)

// The per-agent cooldown alone re-creates the burst GitHub limited: every
// agent that saw the banner comes due together 300s later. The fleet clock
// admits one relaunch per rateLimitRelaunchFleetSpacing so they go one by one.
func TestRateLimitRelaunchDue(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 15, 40, 0, 0, time.UTC)
	cooldown := time.Duration(rateLimitRestartCooldownSec) * time.Second
	cases := []struct {
		name                 string
		lastAgent, lastFleet time.Time
		now                  time.Time
		want                 bool
	}{
		{"first ever", time.Time{}, time.Time{}, t0, true},
		{"agent inside cooldown", t0, time.Time{}, t0.Add(cooldown - time.Second), false},
		{"agent past cooldown, fleet quiet", t0, t0, t0.Add(cooldown), true},
		{"agent past cooldown, fleet just relaunched another", t0, t0.Add(cooldown - time.Second), t0.Add(cooldown), false},
		{"agent past cooldown, fleet spacing elapsed", t0, t0.Add(cooldown - rateLimitRelaunchFleetSpacing), t0.Add(cooldown), true},
		{"never relaunched itself but fleet busy", time.Time{}, t0, t0.Add(rateLimitRelaunchFleetSpacing / 2), false},
	}
	for _, tc := range cases {
		if got := rateLimitRelaunchDue(tc.lastAgent, tc.lastFleet, tc.now); got != tc.want {
			t.Errorf("%s: due=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestClaimRateLimitRelaunch_SpacesTheFleet(t *testing.T) {
	m := &Manager{}
	now := time.Date(2026, 10, 20, 15, 40, 0, 0, time.UTC)
	a, b := &AgentProcess{Name: "scanner"}, &AgentProcess{Name: "quality"}
	if !m.claimRateLimitRelaunch(a, now) {
		t.Fatal("first relaunch must be admitted")
	}
	if m.claimRateLimitRelaunch(b, now.Add(time.Second)) {
		t.Fatal("a second agent inside the fleet spacing must wait")
	}
	if !m.claimRateLimitRelaunch(b, now.Add(rateLimitRelaunchFleetSpacing)) {
		t.Fatal("the second agent is admitted once the spacing elapsed")
	}
	if m.claimRateLimitRelaunch(a, now.Add(2*rateLimitRelaunchFleetSpacing)) {
		t.Fatal("the first agent is still inside its own cooldown")
	}
	if !a.lastRateLimitRestart.Equal(now) || !b.lastRateLimitRestart.Equal(now.Add(rateLimitRelaunchFleetSpacing)) {
		t.Fatalf("per-agent clocks: a=%v b=%v", a.lastRateLimitRestart, b.lastRateLimitRestart)
	}
	if !m.lastRateLimitRelaunch.Equal(now.Add(rateLimitRelaunchFleetSpacing)) {
		t.Fatalf("fleet clock = %v", m.lastRateLimitRelaunch)
	}
}
