package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

func TestUpgradeDrain_ProceedsImmediatelyWithNoKicksInFlight(t *testing.T) {
	var d upgradeDrain
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	dec := d.decide("feedbeef", nil, 45*time.Minute, now)
	if !dec.Proceed || dec.TimedOut || dec.Waited != 0 {
		t.Fatalf("decision = %+v, want immediate proceed", dec)
	}
	// Once released, the process is about to exit: new kicks stay withheld.
	if target, active := d.Active(now.Add(time.Hour)); !active || target != "feedbeef" {
		t.Fatalf("Active after release = (%q, %v), want (feedbeef, true)", target, active)
	}
}

func TestUpgradeDrain_DefersWhileKicksInFlightThenProceedsWhenTheyEnd(t *testing.T) {
	var d upgradeDrain
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	timeout := 45 * time.Minute

	dec := d.decide("feedbeef", []string{"architect"}, timeout, start)
	if dec.Proceed {
		t.Fatalf("decision = %+v, want deferral while architect is mid-turn", dec)
	}
	if dec.Remaining != timeout || !reflect.DeepEqual(dec.InFlight, []string{"architect"}) {
		t.Fatalf("decision = %+v, want full window remaining and architect in flight", dec)
	}
	if _, active := d.Active(start); !active {
		t.Fatal("drain not active: new kicks would still be issued while draining")
	}

	beat := start.Add(heartbeatSendInterval)
	dec = d.decide("feedbeef", []string{"architect"}, timeout, beat)
	if dec.Proceed || dec.Waited != heartbeatSendInterval || dec.Remaining != timeout-heartbeatSendInterval {
		t.Fatalf("second beat decision = %+v, want deferral keeping the original deadline", dec)
	}

	done := beat.Add(heartbeatSendInterval)
	dec = d.decide("feedbeef", nil, timeout, done)
	if !dec.Proceed || dec.TimedOut || dec.Waited != 2*heartbeatSendInterval {
		t.Fatalf("drained decision = %+v, want proceed after %v", dec, 2*heartbeatSendInterval)
	}
}

func TestUpgradeDrain_TimesOutAndNamesCutKicks(t *testing.T) {
	var d upgradeDrain
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	timeout := 10 * time.Minute
	inFlight := []string{"ci-maintainer", "quality"}

	for at := start; at.Sub(start) < timeout; at = at.Add(heartbeatSendInterval) {
		if dec := d.decide("feedbeef", inFlight, timeout, at); dec.Proceed {
			t.Fatalf("proceeded at %v inside the drain window: %+v", at.Sub(start), dec)
		}
	}
	dec := d.decide("feedbeef", inFlight, timeout, start.Add(timeout))
	if !dec.Proceed || !dec.TimedOut || !reflect.DeepEqual(dec.InFlight, inFlight) {
		t.Fatalf("decision at deadline = %+v, want timed-out proceed naming the cut kicks", dec)
	}
}

func TestUpgradeDrain_NewerTargetKeepsOriginalDeadline(t *testing.T) {
	var d upgradeDrain
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	timeout := 45 * time.Minute

	d.decide("aaaa111", []string{"scanner"}, timeout, start)
	dec := d.decide("bbbb222", []string{"scanner"}, timeout, start.Add(heartbeatSendInterval))
	if dec.Proceed || dec.Waited != heartbeatSendInterval {
		t.Fatalf("decision = %+v, want the drain window carried across a newer target", dec)
	}
	if target, _ := d.Active(start.Add(heartbeatSendInterval)); target != "bbbb222" {
		t.Fatalf("drain target = %q, want the newest target", target)
	}
}

func TestUpgradeDrain_StaleDrainReleasesKicks(t *testing.T) {
	var d upgradeDrain
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	d.decide("feedbeef", []string{"scanner"}, 45*time.Minute, start)
	// The hub stopped re-sending UpgradeTo (cancelled or superseded): kicks
	// must not stay withheld forever.
	if _, active := d.Active(start.Add(selfUpgradeDrainStaleAfter)); active {
		t.Fatal("drain still active after the hub stopped re-confirming the upgrade")
	}
	// A later instruction opens a fresh window rather than inheriting the old one.
	dec := d.decide("feedbeef", []string{"scanner"}, 45*time.Minute, start.Add(time.Hour))
	if dec.Proceed || dec.Waited != 0 {
		t.Fatalf("decision = %+v, want a fresh drain window", dec)
	}
}

func TestUpgradeDrain_DisabledByNonPositiveTimeout(t *testing.T) {
	var d upgradeDrain
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	dec := d.decide("feedbeef", []string{"architect"}, 0, now)
	if !dec.Proceed || dec.TimedOut {
		t.Fatalf("decision = %+v, want immediate proceed when draining is disabled", dec)
	}
	if _, active := d.Active(now); active {
		t.Fatal("disabled drain withheld kicks")
	}
}

func TestAgentKicksInFlight(t *testing.T) {
	processStart := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	kick := processStart.Add(10 * time.Minute)
	seeded := processStart.Add(-time.Hour)

	statuses := map[string]*agent.AgentProcess{
		"architect": {State: agent.StateRunning, LastKick: &kick},
		"reviewer": {State: agent.StateRunning, LastKick: &kick,
			KickOutcome: agent.KickOutcome{Kind: agent.KickOutcomeEnded, KickAt: kick}},
		"quality": {State: agent.StateRunning, LastKick: &kick,
			KickOutcome: agent.KickOutcome{Kind: agent.KickOutcomeEnded, KickAt: processStart.Add(time.Minute)}},
		"scanner":    {State: agent.StateRunning, LastKick: &seeded},
		"operations": {State: agent.StateRunning, Paused: true, LastKick: &kick},
		"telemetry":  {State: agent.StateIdle, LastKick: &kick},
		"sec-check":  {State: agent.StateRunning},
		"nil-proc":   nil,
	}

	got := agentKicksInFlight(statuses, processStart)
	want := []string{"architect", "quality"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agentKicksInFlight = %v, want %v", got, want)
	}
}

func TestHubConfigEffectiveUpgradeDrainTimeout(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want time.Duration
	}{
		{0, 45 * time.Minute},
		{600, 10 * time.Minute},
		{-1, 0},
	} {
		if got := (config.HubConfig{UpgradeDrainTimeoutS: tc.in}).EffectiveUpgradeDrainTimeout(); got != tc.want {
			t.Errorf("EffectiveUpgradeDrainTimeout(%d) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestUpgradeDrain_DeadlineReportedOnlyWhileWaiting(t *testing.T) {
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	timeout := 45 * time.Minute
	kicks := []string{"architect"}

	tests := []struct {
		name   string
		act    func(d *upgradeDrain)
		at     time.Time
		wantOK bool
	}{
		{"no drain opened", func(d *upgradeDrain) {}, start, false},
		{"draining reports the deadline", func(d *upgradeDrain) { d.decide("feedbeef", kicks, timeout, start) }, start.Add(time.Minute), true},
		{"released drain reports nothing", func(d *upgradeDrain) {
			d.decide("feedbeef", kicks, timeout, start)
			d.decide("feedbeef", nil, timeout, start.Add(time.Minute))
		}, start.Add(2 * time.Minute), false},
		{"drain the hub stopped re-confirming reports nothing", func(d *upgradeDrain) { d.decide("feedbeef", kicks, timeout, start) }, start.Add(selfUpgradeDrainStaleAfter), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d upgradeDrain
			tc.act(&d)
			deadline, ok := d.Deadline(tc.at)
			if ok != tc.wantOK {
				t.Fatalf("Deadline ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !deadline.Equal(start.Add(timeout)) {
				t.Fatalf("deadline = %v, want %v", deadline, start.Add(timeout))
			}
		})
	}
}
