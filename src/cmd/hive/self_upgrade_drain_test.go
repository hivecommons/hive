package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub/spoke"
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

	statuses := map[string]*crux.Process{
		"architect": {State: crux.StateRunning, LastKick: &kick},
		"reviewer": {State: crux.StateRunning, LastKick: &kick,
			KickOutcome: crux.KickOutcome{Kind: crux.KickOutcomeEnded, KickAt: kick}},
		"quality": {State: crux.StateRunning, LastKick: &kick,
			KickOutcome: crux.KickOutcome{Kind: crux.KickOutcomeEnded, KickAt: processStart.Add(time.Minute)}},
		"scanner":    {State: crux.StateRunning, LastKick: &seeded},
		"operations": {State: crux.StateRunning, Paused: true, LastKick: &kick},
		"telemetry":  {State: crux.StateIdle, LastKick: &kick},
		"sec-check":  {State: crux.StateRunning},
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

// TestUpgradeDrain_ReportReflectsDeferralForHub pins the heartbeat view of the
// drain: nil until a deferral starts, the in-flight kicks and remaining budget
// while deferring, nil again once the drain releases or goes stale. The hub's
// orphan sweep relies on this to tell a deliberate hold from a dead attempt.
func TestUpgradeDrain_ReportReflectsDeferralForHub(t *testing.T) {
	d := &upgradeDrain{}
	start := time.Date(2026, 10, 23, 17, 16, 0, 0, time.UTC)
	timeout := 45 * time.Minute

	if r := d.Report(start); r != nil {
		t.Fatalf("no drain yet, Report = %+v, want nil", r)
	}

	// Re-confirmed every heartbeat, as the hub re-delivers the instruction.
	at := start
	for i := 0; i <= 5; i++ {
		at = start.Add(time.Duration(i) * 2 * time.Minute)
		d.decide("4f8be0b", []string{"brainstorm"}, timeout, at)
	}
	r := d.Report(at)
	if r == nil {
		t.Fatal("deferring drain must report")
	}
	want := &spoke.UpgradeDrainReport{Target: "4f8be0b", InFlight: []string{"brainstorm"}, WaitedS: 600, RemainingS: 2100}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("Report = %+v, want %+v", r, want)
	}

	if r := d.Report(at.Add(selfUpgradeDrainStaleAfter)); r != nil {
		t.Fatalf("stale drain must report nil, got %+v", r)
	}

	d.decide("4f8be0b", nil, timeout, at.Add(time.Minute))
	if r := d.Report(at.Add(time.Minute)); r != nil {
		t.Fatalf("released drain must report nil, got %+v", r)
	}
}
