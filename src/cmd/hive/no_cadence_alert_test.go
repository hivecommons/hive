package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
)

// applyNoCadenceAlert (#5577) is the spoke-side parity for the hub's
// no-cadence amber: the dashboard's not-producing warnings name only the
// SYMPTOM (idle agent, zero tokens), and this banner adds the cause AND fix
// from the spoke's own governor config — no hub round-trip.

func noCadenceFixture(t *testing.T, agents map[string]config.AgentConfig) (*governor.Governor, *dashboard.Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gov := governor.New(config.GovernorConfig{}, agents, logger)
	srv := dashboard.NewServer(0, logger)
	return gov, srv
}

func TestApplyNoCadenceAlertRaisesCauseAndFixBanner(t *testing.T) {
	gov, srv := noCadenceFixture(t, map[string]config.AgentConfig{
		// The RFC's live shape: enabled agent, zero cadence config anywhere,
		// never kicked once.
		"telemetry": {Enabled: true},
		// On-demand and disabled agents are deliberate choices, not gaps.
		"helper":     {Enabled: true, OnDemand: true},
		"mothballed": {Enabled: false},
	})

	applyNoCadenceAlert(gov, srv)

	alerts := publishedAlerts(t, srv)
	a, ok := alertByID(alerts, noCadenceAlertID)
	if !ok {
		t.Fatalf("no %q alert published, alerts: %+v", noCadenceAlertID, alerts)
	}
	if a.Severity != "warning" {
		t.Errorf("severity = %q, want warning (unconfigured, not broken)", a.Severity)
	}
	// The banner must name the agent, the cause, and the fix.
	for _, want := range []string{"telemetry", "never kicked", "no cadence configured", "set cadences on the agent card"} {
		if !strings.Contains(a.Message, want) {
			t.Errorf("message %q missing %q", a.Message, want)
		}
	}
	// Deliberate configurations must not be named.
	for _, notWant := range []string{"helper", "mothballed"} {
		if strings.Contains(a.Message, notWant) {
			t.Errorf("message %q wrongly names %q", a.Message, notWant)
		}
	}
}

// The banner self-clears: any kick reaching the agent (or removing/disabling
// it) ends the never-kicked condition, and the next governor tick must take
// the stale instruction down.
func TestApplyNoCadenceAlertClearsAfterKick(t *testing.T) {
	gov, srv := noCadenceFixture(t, map[string]config.AgentConfig{
		"telemetry": {Enabled: true},
	})

	applyNoCadenceAlert(gov, srv)
	if _, ok := alertByID(publishedAlerts(t, srv), noCadenceAlertID); !ok {
		t.Fatal("precondition: alert not raised")
	}

	gov.RecordKick("telemetry")
	applyNoCadenceAlert(gov, srv)

	if a, ok := alertByID(publishedAlerts(t, srv), noCadenceAlertID); ok {
		t.Fatalf("alert not cleared after kick: %+v", a)
	}
}
