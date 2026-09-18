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

// ModeUnscheduledSource is the subset of *governor.Governor
// ApplyModeUnscheduled reads.
type ModeUnscheduledSource interface {
	ModeUnscheduledAgents() []governor.ModeUnscheduledAgent
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
	// ModeUnscheduledAlertID is the configured-but-not-scheduled banner
	// (#7474): enabled agents with a cadence in some mode but none the
	// governor can resolve in the mode the fleet is in.
	ModeUnscheduledAlertID = "agent-mode-unscheduled"
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

// ApplyModeUnscheduled keeps the configured-but-not-scheduled banner (#7474)
// in sync with the governor's view: raised (warning — the hive is not broken,
// a cadence ladder has a hole in it) while any enabled, governor-kickable
// agent has a cadence in some mode but none the governor resolves in the
// mode the fleet is in; cleared the moment the mode changes to one that
// schedules it or the operator fills the gap. This is the weaker sibling of
// ApplyNoCadence: that one says "no mode ever kicks this agent", this one
// says "the current mode does not, and here are the modes that do".
//
// The live shape is an agent whose only cadence is in surge — kicked while
// the backlog holds the fleet in surge, silent the moment its own work drives
// the backlog below the threshold. Every other signal calls that agent
// healthy: enabled, running, has a cadence, not on-demand.
func ApplyModeUnscheduled(gov ModeUnscheduledSource, alerts AlertSink) {
	agents := gov.ModeUnscheduledAgents()
	if len(agents) == 0 {
		alerts.ClearSystemAlert(ModeUnscheduledAlertID)
		return
	}
	alerts.AddSystemAlert(ModeUnscheduledAlertID, "warning", ModeUnscheduledMessage(agents))
}

// ModeUnscheduledMessage renders the banner line: which agents, which mode the
// fleet is in, which modes DO schedule each one, and the fix — an entry for
// the current mode, or an idle entry, which every mode inherits.
func ModeUnscheduledMessage(agents []governor.ModeUnscheduledAgent) string {
	if len(agents) == 0 {
		return ""
	}
	parts := make([]string, 0, len(agents))
	for _, a := range agents {
		parts = append(parts, fmt.Sprintf("%s (cadence only in %s)", a.Agent, joinModes(a.CadenceModes)))
	}
	return fmt.Sprintf("agent(s) %s not scheduled in the current %s mode — the governor will not kick them until the mode changes; add a %s cadence on the agent card (an idle cadence is inherited by every mode)",
		strings.Join(parts, ", "), agents[0].Mode, agents[0].Mode)
}

func joinModes(modes []string) string {
	switch len(modes) {
	case 0:
		return "other modes"
	case 1:
		return modes[0]
	case 2:
		return modes[0] + " and " + modes[1]
	default:
		return strings.Join(modes[:len(modes)-1], ", ") + " and " + modes[len(modes)-1]
	}
}
