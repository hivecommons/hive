// Package spokealerts turns governor signals into the dashboard system alerts
// and notifications an operator actually sees: the weekly token-budget warning
// and exhaustion banners, and the never-kicked "no cadence configured" banner
// (#5577).
//
// Extracted from cmd/hive's package main (#7238 stage 4). This is policy, not
// wiring: it decides when a banner is raised and cleared, at what severity, and
// -- the part that matters most to an operator -- what the banner SAYS. The
// no-cadence copy exists specifically because the dashboard's other
// not-producing warnings name only the symptom, so the exact wording is a
// deliberate product decision worth testing directly.
//
// The dependencies are interfaces rather than *dashboard.Server and
// *notify.Notifier. That is not indirection for its own sake: it keeps
// pkg/dashboard out of this package's import graph entirely, and it means these
// decisions can be asserted against a recording fake instead of a booted
// dashboard server, which is what made them awkward to test inside package
// main.
package spokealerts

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/notify"
)

// AlertSink is the subset of *dashboard.Server these alerts need.
type AlertSink interface {
	AddSystemAlert(id, severity, message string)
	ClearSystemAlert(id string)
}

// Notifier is the subset of *notify.Notifier these alerts need.
type Notifier interface {
	Send(title, message string, priority notify.Priority)
}

// BudgetSource is the subset of *governor.Governor ApplyBudget reads.
type BudgetSource interface {
	GetBudget() governor.BudgetInfo
}

// NoCadenceSource is the subset of *governor.Governor ApplyNoCadence reads.
type NoCadenceSource interface {
	NoCadenceAgents() []string
}

// Dashboard system-alert IDs owned by this package. They are the stable
// identity a banner is raised and cleared under, so they are exported: a
// mismatch between the raise and the clear leaves a banner stuck on screen.
const (
	BudgetWarnAlertID      = "budget-warn"
	BudgetExhaustedAlertID = "budget-exhausted"
	// NoCadenceAlertID is the never-kicked cause+fix banner (#5577): enabled
	// agents with no cadence in any mode and no kick ever.
	NoCadenceAlertID = "agent-no-cadence"
)

// ApplyBudget turns budget threshold crossings into dashboard system
// alerts and notifications. Crossings fire once per window (governor tracks
// the one-shot flags); alerts are cleared when the threshold no longer
// applies (window rolled, limit raised, or budgeting disabled).
func ApplyBudget(gov BudgetSource, trans governor.BudgetTransitions, alerts AlertSink, notifier Notifier) {
	if !trans.WarnActive {
		alerts.ClearSystemAlert(BudgetWarnAlertID)
	}
	if !trans.ExhaustedActive {
		alerts.ClearSystemAlert(BudgetExhaustedAlertID)
	}

	budget := gov.GetBudget()
	if trans.WarnCrossed {
		msg := fmt.Sprintf("token budget at %d%%+ of weekly limit: %d of %d tokens used",
			governor.BudgetWarnPct, budget.CurrentSpend, budget.WeeklyLimit)
		alerts.AddSystemAlert(BudgetWarnAlertID, "warning", msg)
		notifier.Send("Budget warning", msg, notify.PriorityDefault)
	}
	if trans.ExhaustedCrossed {
		windowEnd := budget.ResetAt.Add(governor.BudgetWindowDuration)
		msg := fmt.Sprintf("token budget exhausted: %d of %d tokens used — agent kicks suspended until %s (exempt agents keep running)",
			budget.CurrentSpend, budget.WeeklyLimit, windowEnd.Format(time.RFC1123))
		alerts.AddSystemAlert(BudgetExhaustedAlertID, "error", msg)
		notifier.Send("Budget exhausted", msg, notify.PriorityHigh)
	}
}

// ApplyNoCadence keeps the never-kicked cause+fix banner (#5577) in sync
// with the governor's view: raised (warning, not error — the hive is not
// broken, it is unconfigured) while any enabled, governor-kickable agent has
// no cadence in any mode and has never been kicked; cleared the moment the
// operator sets a cadence or any kick path reaches the agent. This is the
// spoke-side parity for the hub verdict's no-cadence amber: the same
// governor-derived signal, rendered where the operator can act on it, with no
// hub round-trip.
func ApplyNoCadence(gov NoCadenceSource, alerts AlertSink) {
	agents := gov.NoCadenceAgents()
	if len(agents) == 0 {
		alerts.ClearSystemAlert(NoCadenceAlertID)
		return
	}
	alerts.AddSystemAlert(NoCadenceAlertID, "warning", NoCadenceMessage(agents))
}

// NoCadenceMessage renders the banner line: symptom, cause AND fix — the
// exact gap the RFC calls out in the dashboard's not-producing warnings,
// which name only the symptom.
func NoCadenceMessage(agents []string) string {
	return fmt.Sprintf("agent(s) %s enabled but never kicked — no cadence configured; set cadences on the agent card",
		strings.Join(agents, ", "))
}
