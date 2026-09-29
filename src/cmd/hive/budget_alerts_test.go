package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/spokealerts"
)

// Tests for applyBudgetAlerts (cmd/hive/main.go), which was previously
// uncovered: it is the only bridge from governor budget threshold crossings
// to the dashboard system alerts and operator notifications, so a regression
// here silently drops the "budget warning" / "budget exhausted" banners.

func budgetAlertsFixture(t *testing.T) (*governor.Governor, *dashboard.Server, *notify.Notifier) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gov := governor.New(config.GovernorConfig{}, map[string]config.AgentConfig{}, logger)
	srv := dashboard.NewServer(0, logger)
	// Empty notifications config: Send is a no-op for every channel, so the
	// notifier path is exercised without any network traffic.
	notifier := notify.New(config.NotificationsConfig{}, logger)
	return gov, srv, notifier
}

// publishedAlerts publishes a status snapshot and returns the system alerts it
// carries — the same read path the dashboard frontend consumes.
func publishedAlerts(t *testing.T, srv *dashboard.Server) []dashboard.SystemAlert {
	t.Helper()
	payload := &dashboard.StatusPayload{}
	if !srv.UpdateStatusIfFresh(payload, srv.BeginStatusSnapshot()) {
		t.Fatal("status snapshot unexpectedly dropped as stale")
	}
	return payload.SystemAlerts
}

func alertByID(alerts []dashboard.SystemAlert, id string) (dashboard.SystemAlert, bool) {
	for _, a := range alerts {
		if a.ID == id {
			return a, true
		}
	}
	return dashboard.SystemAlert{}, false
}

func TestApplyBudgetAlertsWarnCrossingRaisesWarningAlert(t *testing.T) {
	gov, srv, notifier := budgetAlertsFixture(t)
	gov.SetBudgetLimit(1000)

	// First update opens the window (baseline 0), second crosses the 90% warn
	// threshold without exhausting the budget.
	gov.UpdateBudgetFromTotals(0, nil, nil)
	trans := gov.UpdateBudgetFromTotals(950, nil, nil)
	if !trans.WarnCrossed || trans.ExhaustedCrossed {
		t.Fatalf("fixture: WarnCrossed=%v ExhaustedCrossed=%v, want true/false", trans.WarnCrossed, trans.ExhaustedCrossed)
	}

	applyBudgetAlerts(gov, trans, srv, notifier)

	alerts := publishedAlerts(t, srv)
	warn, ok := alertByID(alerts, spokealerts.BudgetWarnAlertID)
	if !ok {
		t.Fatalf("no %q alert published, alerts: %+v", spokealerts.BudgetWarnAlertID, alerts)
	}
	if warn.Severity != "warning" {
		t.Errorf("warn alert severity = %q, want %q", warn.Severity, "warning")
	}
	if !strings.Contains(warn.Message, "950 of 1000 tokens used") {
		t.Errorf("warn alert message = %q, want spend/limit figures", warn.Message)
	}
	if _, ok := alertByID(alerts, spokealerts.BudgetExhaustedAlertID); ok {
		t.Error("exhausted alert raised on a warn-only crossing")
	}
}

func TestApplyBudgetAlertsExhaustedCrossingRaisesErrorAlert(t *testing.T) {
	gov, srv, notifier := budgetAlertsFixture(t)
	gov.SetBudgetLimit(1000)

	gov.UpdateBudgetFromTotals(0, nil, nil)
	trans := gov.UpdateBudgetFromTotals(1000, nil, nil)
	if !trans.ExhaustedCrossed {
		t.Fatalf("fixture: ExhaustedCrossed=%v, want true", trans.ExhaustedCrossed)
	}

	applyBudgetAlerts(gov, trans, srv, notifier)

	alerts := publishedAlerts(t, srv)
	exhausted, ok := alertByID(alerts, spokealerts.BudgetExhaustedAlertID)
	if !ok {
		t.Fatalf("no %q alert published, alerts: %+v", spokealerts.BudgetExhaustedAlertID, alerts)
	}
	if exhausted.Severity != "error" {
		t.Errorf("exhausted alert severity = %q, want %q", exhausted.Severity, "error")
	}
	if !strings.Contains(exhausted.Message, "agent kicks suspended") {
		t.Errorf("exhausted alert message = %q, want kick-suspension notice", exhausted.Message)
	}
}

// Crossings are one-shot per window: a second cycle at the same spend keeps
// the standing alert but must not re-raise (Crossed stays false while Active
// stays true, so the alert is neither duplicated nor cleared).
func TestApplyBudgetAlertsSteadyStateKeepsAlertWithoutReRaising(t *testing.T) {
	gov, srv, notifier := budgetAlertsFixture(t)
	gov.SetBudgetLimit(1000)

	gov.UpdateBudgetFromTotals(0, nil, nil)
	applyBudgetAlerts(gov, gov.UpdateBudgetFromTotals(950, nil, nil), srv, notifier)

	trans := gov.UpdateBudgetFromTotals(960, nil, nil)
	if trans.WarnCrossed {
		t.Fatal("fixture: WarnCrossed on second cycle, want one-shot semantics")
	}
	if !trans.WarnActive {
		t.Fatal("fixture: WarnActive false while still over threshold")
	}
	applyBudgetAlerts(gov, trans, srv, notifier)

	alerts := publishedAlerts(t, srv)
	count := 0
	for _, a := range alerts {
		if a.ID == spokealerts.BudgetWarnAlertID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("warn alert count = %d after steady-state cycle, want exactly 1", count)
	}
}

// When a threshold no longer applies (here: the operator raised the limit),
// the standing alert must be cleared — a stale "budget exhausted" banner
// after the limit was raised is exactly the misreporting this function's
// clear branches exist to prevent.
func TestApplyBudgetAlertsClearsAlertsWhenThresholdNoLongerApplies(t *testing.T) {
	gov, srv, notifier := budgetAlertsFixture(t)
	gov.SetBudgetLimit(1000)

	gov.UpdateBudgetFromTotals(0, nil, nil)
	applyBudgetAlerts(gov, gov.UpdateBudgetFromTotals(1000, nil, nil), srv, notifier)
	if _, ok := alertByID(publishedAlerts(t, srv), spokealerts.BudgetExhaustedAlertID); !ok {
		t.Fatal("fixture: exhausted alert not raised")
	}

	gov.SetBudgetLimit(10000)
	trans := gov.UpdateBudgetFromTotals(1000, nil, nil)
	if trans.WarnActive || trans.ExhaustedActive {
		t.Fatalf("fixture: thresholds still active after limit raise: %+v", trans)
	}
	applyBudgetAlerts(gov, trans, srv, notifier)

	alerts := publishedAlerts(t, srv)
	if _, ok := alertByID(alerts, spokealerts.BudgetExhaustedAlertID); ok {
		t.Error("exhausted alert not cleared after limit raise")
	}
	if _, ok := alertByID(alerts, spokealerts.BudgetWarnAlertID); ok {
		t.Error("warn alert not cleared after limit raise")
	}
}

// End-to-end restart scenario against the real dashboard.Server: a pod
// restart mid-exhaustion restores CurrentSpend via SeedBudget (what boot does
// through bootState), with no UpdateBudgetFromTotals call yet since the token
// collector has not produced a summary. BudgetLevel must read the standing
// exhaustion straight off the restored governor state, and applyBudgetAlerts
// must raise the banner from that alone (#9612 follow-up: previously this
// state produced a zero-value BudgetTransitions and no banner appeared).
func TestApplyBudgetAlertsRestartMidExhaustionShowsBanner(t *testing.T) {
	gov, srv, notifier := budgetAlertsFixture(t)
	gov.SetBudgetLimit(1000)
	gov.SeedBudget(1000, nil, nil, time.Now())

	applyBudgetAlerts(gov, gov.BudgetLevel(), srv, notifier)

	alerts := publishedAlerts(t, srv)
	if _, ok := alertByID(alerts, spokealerts.BudgetExhaustedAlertID); !ok {
		t.Fatalf("exhausted banner missing after restart-mid-exhaustion state, alerts: %+v", alerts)
	}
	// notify.Notifier has no exported call-recording hook (Send fans out to
	// real channels which are no-ops for this empty NotificationsConfig
	// fixture), so the no-notify-on-restart assertion for this path is
	// covered against a recording fake by
	// TestApplyBudgetRestartMidExhaustionRaisesBannerWithoutNotifying in
	// pkg/spokealerts.
}

// Recovery: once the budget window rolls (or the limit is raised) and spend
// is no longer over any threshold, the banner set by the restart scenario
// above must clear on the next cycle.
func TestApplyBudgetAlertsRestartThenRecoveryClearsBanner(t *testing.T) {
	gov, srv, notifier := budgetAlertsFixture(t)
	gov.SetBudgetLimit(1000)
	gov.SeedBudget(1000, nil, nil, time.Now())
	applyBudgetAlerts(gov, gov.BudgetLevel(), srv, notifier)
	if _, ok := alertByID(publishedAlerts(t, srv), spokealerts.BudgetExhaustedAlertID); !ok {
		t.Fatal("fixture: exhausted banner not raised")
	}

	// Window rolls once its duration has elapsed: baseline re-anchors and
	// spend drops to 0.
	gov.SeedBudget(1000, nil, nil, time.Now().Add(-governor.BudgetWindowDuration-time.Hour))
	trans := gov.UpdateBudgetFromTotals(1000, nil, nil)
	applyBudgetAlerts(gov, trans, srv, notifier)

	alerts := publishedAlerts(t, srv)
	if _, ok := alertByID(alerts, spokealerts.BudgetExhaustedAlertID); ok {
		t.Errorf("exhausted banner still present after window roll: %+v", alerts)
	}
}

// Re-deriving the banner every cycle (the level-triggered raise) must not
// stack duplicate entries in the published status snapshot — AddSystemAlert
// upserts by ID.
func TestApplyBudgetAlertsNoDoubleBannerAcrossRepeatedCycles(t *testing.T) {
	gov, srv, notifier := budgetAlertsFixture(t)
	gov.SetBudgetLimit(1000)
	gov.SeedBudget(1000, nil, nil, time.Now())

	for i := 0; i < 5; i++ {
		applyBudgetAlerts(gov, gov.BudgetLevel(), srv, notifier)
	}

	alerts := publishedAlerts(t, srv)
	count := 0
	for _, a := range alerts {
		if a.ID == spokealerts.BudgetExhaustedAlertID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("exhausted banner count = %d after 5 re-derive cycles, want exactly 1", count)
	}
}

// WeeklyLimit == 0 disables budgeting: no transitions fire and no alerts may
// appear, while any stale alerts from a previously-enabled budget are cleared.
func TestApplyBudgetAlertsBudgetingDisabledClearsAndRaisesNothing(t *testing.T) {
	gov, srv, notifier := budgetAlertsFixture(t)
	gov.SetBudgetLimit(1000)

	gov.UpdateBudgetFromTotals(0, nil, nil)
	applyBudgetAlerts(gov, gov.UpdateBudgetFromTotals(950, nil, nil), srv, notifier)

	gov.SetBudgetLimit(0)
	trans := gov.UpdateBudgetFromTotals(2000, nil, nil)
	if trans.WarnActive || trans.ExhaustedActive || trans.WarnCrossed || trans.ExhaustedCrossed {
		t.Fatalf("fixture: transitions fired with budgeting disabled: %+v", trans)
	}
	applyBudgetAlerts(gov, trans, srv, notifier)

	if alerts := publishedAlerts(t, srv); len(alerts) != 0 {
		t.Errorf("alerts remain with budgeting disabled: %+v", alerts)
	}
}
