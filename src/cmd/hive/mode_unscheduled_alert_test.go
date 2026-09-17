package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/spokealerts"
)

// applyModeUnscheduledAlert (#7474) is the weaker sibling of the never-kicked
// banner: an agent SOME mode schedules but the current one does not. The live
// shape is the projectbluefin reviewer — cadence only in surge — which reads
// as healthy on every other signal the moment the fleet leaves surge.

func modeUnscheduledFixture(t *testing.T) (*governor.Governor, *dashboard.Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "1h"}},
		"quiet": {Threshold: 28, Cadences: map[string]config.Cadence{"scanner": "30m"}},
		"busy":  {Threshold: 150, Cadences: map[string]config.Cadence{"scanner": "15m"}},
		"surge": {Threshold: 550, Cadences: map[string]config.Cadence{"scanner": "5m", "reviewer": "30m"}},
	}}
	gov := governor.New(cfg, map[string]config.AgentConfig{
		"scanner":  {Enabled: true},
		"reviewer": {Enabled: true},
	}, logger)
	srv := dashboard.NewServer(0, logger)
	return gov, srv
}

func TestApplyModeUnscheduledAlertRaisesBelowSurgeAndClearsInSurge(t *testing.T) {
	gov, srv := modeUnscheduledFixture(t)

	// In surge the reviewer is on its 30m cadence: nothing to say.
	gov.SetMode(governor.ModeSurge)
	applyModeUnscheduledAlert(gov, srv)
	if a, ok := alertByID(publishedAlerts(t, srv), spokealerts.ModeUnscheduledAlertID); ok {
		t.Fatalf("alert raised while the reviewer is scheduled in surge: %+v", a)
	}

	// The backlog drops, the fleet goes to busy, the reviewer has no cadence
	// there and none in idle to inherit.
	gov.SetMode(governor.ModeBusy)
	applyModeUnscheduledAlert(gov, srv)
	alerts := publishedAlerts(t, srv)
	a, ok := alertByID(alerts, spokealerts.ModeUnscheduledAlertID)
	if !ok {
		t.Fatalf("no %q alert published, alerts: %+v", spokealerts.ModeUnscheduledAlertID, alerts)
	}
	if a.Severity != "warning" {
		t.Errorf("severity = %q, want warning (a cadence gap, not a broken hive)", a.Severity)
	}
	for _, want := range []string{"reviewer", "only in surge", "busy", "will not kick", "add a busy cadence"} {
		if !strings.Contains(a.Message, want) {
			t.Errorf("message %q missing %q", a.Message, want)
		}
	}
	if strings.Contains(a.Message, "scanner") {
		t.Errorf("message %q wrongly names the scanner, which busy schedules", a.Message)
	}
	// It is not the never-kicked class, and must not be reported as one.
	if _, ok := alertByID(alerts, spokealerts.NoCadenceAlertID); ok {
		t.Errorf("the reviewer has a cadence; the never-kicked banner must stay down")
	}

	// Back in surge the banner comes down on the next tick.
	gov.SetMode(governor.ModeSurge)
	applyModeUnscheduledAlert(gov, srv)
	if a, ok := alertByID(publishedAlerts(t, srv), spokealerts.ModeUnscheduledAlertID); ok {
		t.Fatalf("alert not cleared once the mode schedules the reviewer again: %+v", a)
	}
}
