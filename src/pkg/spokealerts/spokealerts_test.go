package spokealerts

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/notify"
)

// The integration-style tests for this policy stayed in cmd/hive on purpose:
// they drive a real *dashboard.Server and read the alerts back through the
// status-snapshot publish path the frontend actually consumes, which no fake
// can stand in for. They also keep the thin wrappers covered.
//
// These tests cover what those cannot see cheaply: the exact severities, the
// exact alert IDs, and the operator-facing banner copy — the parts where a
// change is silent because a banner still appears, just wrong.

// recordingSink captures raises and clears in order.
type recordingSink struct {
	added   []addedAlert
	cleared []string
}

type addedAlert struct{ id, severity, message string }

func (r *recordingSink) AddSystemAlert(id, severity, message string) {
	r.added = append(r.added, addedAlert{id, severity, message})
}

func (r *recordingSink) ClearSystemAlert(id string) { r.cleared = append(r.cleared, id) }

func (r *recordingSink) find(id string) (addedAlert, bool) {
	for _, a := range r.added {
		if a.id == id {
			return a, true
		}
	}
	return addedAlert{}, false
}

type recordingNotifier struct {
	sent []sentNote
}

type sentNote struct {
	title, message string
	priority       notify.Priority
}

func (r *recordingNotifier) Send(title, message string, priority notify.Priority) {
	r.sent = append(r.sent, sentNote{title, message, priority})
}

type fakeBudget struct{ info governor.BudgetInfo }

func (f fakeBudget) GetBudget() governor.BudgetInfo { return f.info }

type fakeNoCadence struct{ agents []string }

func (f fakeNoCadence) NoCadenceAgents() []string { return f.agents }

// Severity is not cosmetic: the dashboard renders "error" as a red blocking
// banner and "warning" as amber. Exhaustion genuinely suspends agent kicks and
// must be red; the warning must not be, or every hive at 80% of its weekly
// budget looks broken. Nothing else in the suite pins the two apart.
func TestBudgetSeveritiesDistinguishWarningFromExhaustion(t *testing.T) {
	sink := &recordingSink{}
	note := &recordingNotifier{}
	gov := fakeBudget{governor.BudgetInfo{CurrentSpend: 900, WeeklyLimit: 1000}}

	ApplyBudget(gov, governor.BudgetTransitions{WarnCrossed: true, WarnActive: true}, sink, note)

	warn, ok := sink.find(BudgetWarnAlertID)
	if !ok {
		t.Fatalf("no alert raised under %q; got %+v", BudgetWarnAlertID, sink.added)
	}
	if warn.severity != "warning" {
		t.Errorf("warn severity = %q, want %q — a hive at the warn threshold is not broken",
			warn.severity, "warning")
	}
	if len(note.sent) != 1 || note.sent[0].priority != notify.PriorityDefault {
		t.Errorf("warn notification = %+v, want exactly one at default priority", note.sent)
	}

	sink2 := &recordingSink{}
	note2 := &recordingNotifier{}
	ApplyBudget(gov, governor.BudgetTransitions{ExhaustedCrossed: true, ExhaustedActive: true}, sink2, note2)

	ex, ok := sink2.find(BudgetExhaustedAlertID)
	if !ok {
		t.Fatalf("no alert raised under %q; got %+v", BudgetExhaustedAlertID, sink2.added)
	}
	if ex.severity != "error" {
		t.Errorf("exhausted severity = %q, want %q — kicks are suspended", ex.severity, "error")
	}
	if len(note2.sent) != 1 || note2.sent[0].priority != notify.PriorityHigh {
		t.Errorf("exhausted notification = %+v, want exactly one at high priority", note2.sent)
	}
	if ex.id == warn.id {
		t.Error("warn and exhausted must use distinct alert IDs or one clear removes both")
	}
}

// A crossing that is not reported must raise nothing, and an inactive threshold
// must clear. Getting this backwards leaves a banner on screen for a window
// that has already rolled — the failure operators actually report.
func TestBudgetClearsInactiveThresholdsAndRaisesNothingWithoutACrossing(t *testing.T) {
	sink := &recordingSink{}
	note := &recordingNotifier{}
	gov := fakeBudget{governor.BudgetInfo{CurrentSpend: 10, WeeklyLimit: 1000}}

	ApplyBudget(gov, governor.BudgetTransitions{}, sink, note)

	if len(sink.added) != 0 {
		t.Errorf("no crossing must raise no alert; got %+v", sink.added)
	}
	if len(note.sent) != 0 {
		t.Errorf("no crossing must notify nobody; got %+v", note.sent)
	}
	for _, want := range []string{BudgetWarnAlertID, BudgetExhaustedAlertID} {
		if !contains(sink.cleared, want) {
			t.Errorf("inactive threshold %q was not cleared; cleared=%v", want, sink.cleared)
		}
	}
}

// A pod restart mid-exhaustion has Active=true but Crossed=false: the
// governor's one-shot budgetWarned/budgetExhaustedAlerted flags are in-memory
// only and reset on restart, so the crossing that originally raised the
// banner can never be observed again — but CurrentSpend, restored from the
// persisted snapshot, still says the budget is over the threshold. The
// banner must come back on this state alone, level-triggered, with no
// duplicate notification (#9612 follow-up).
func TestApplyBudgetRestartMidExhaustionRaisesBannerWithoutNotifying(t *testing.T) {
	sink := &recordingSink{}
	note := &recordingNotifier{}
	gov := fakeBudget{governor.BudgetInfo{CurrentSpend: 1000, WeeklyLimit: 1000}}

	ApplyBudget(gov, governor.BudgetTransitions{ExhaustedActive: true}, sink, note)

	ex, ok := sink.find(BudgetExhaustedAlertID)
	if !ok {
		t.Fatalf("no %q alert raised on restart-mid-exhaustion state; got %+v", BudgetExhaustedAlertID, sink.added)
	}
	if ex.severity != "error" {
		t.Errorf("severity = %q, want %q", ex.severity, "error")
	}
	if !strings.Contains(ex.message, "1000 of 1000 tokens used") {
		t.Errorf("message = %q, want spend/limit figures", ex.message)
	}
	if len(note.sent) != 0 {
		t.Errorf("restart-mid-exhaustion must not notify (no fresh crossing); got %+v", note.sent)
	}
}

// The same restart scenario, but only warn-level: exercises the sibling
// branch so a copy/paste regression between Warn and Exhausted cannot hide.
func TestApplyBudgetRestartMidWarnRaisesBannerWithoutNotifying(t *testing.T) {
	sink := &recordingSink{}
	note := &recordingNotifier{}
	gov := fakeBudget{governor.BudgetInfo{CurrentSpend: 950, WeeklyLimit: 1000}}

	ApplyBudget(gov, governor.BudgetTransitions{WarnActive: true}, sink, note)

	if _, ok := sink.find(BudgetWarnAlertID); !ok {
		t.Fatalf("no %q alert raised on restart-mid-warn state; got %+v", BudgetWarnAlertID, sink.added)
	}
	if len(note.sent) != 0 {
		t.Errorf("restart-mid-warn must not notify (no fresh crossing); got %+v", note.sent)
	}
}

// Recovery (the window rolled, or the operator raised the limit) must clear
// the banner: Active flips false and the standing alert comes down.
func TestApplyBudgetRecoveryClearsBanner(t *testing.T) {
	sink := &recordingSink{}
	note := &recordingNotifier{}
	gov := fakeBudget{governor.BudgetInfo{CurrentSpend: 0, WeeklyLimit: 1000}}

	// Banner standing from a prior cycle.
	ApplyBudget(gov, governor.BudgetTransitions{ExhaustedActive: true}, sink, note)
	if _, ok := sink.find(BudgetExhaustedAlertID); !ok {
		t.Fatal("fixture: exhausted alert not raised")
	}

	// Window rolled: Active goes false.
	ApplyBudget(gov, governor.BudgetTransitions{Rolled: true}, sink, note)
	if !contains(sink.cleared, BudgetExhaustedAlertID) {
		t.Errorf("exhausted alert not cleared on recovery; cleared=%v", sink.cleared)
	}
}

// Raising the banner every cycle while the level stays active must not stack
// duplicate entries: AddSystemAlert upserts by ID, and this pins that
// ApplyBudget relies on that rather than accumulating raises itself.
func TestApplyBudgetLevelTriggeredRaiseNeverDuplicatesAcrossCycles(t *testing.T) {
	sink := &recordingSink{}
	note := &recordingNotifier{}
	gov := fakeBudget{governor.BudgetInfo{CurrentSpend: 1000, WeeklyLimit: 1000}}

	// First cycle crosses (notifies); the next several stay active without a
	// fresh crossing (steady state / re-derivation after a restart).
	ApplyBudget(gov, governor.BudgetTransitions{ExhaustedActive: true, ExhaustedCrossed: true}, sink, note)
	for i := 0; i < 3; i++ {
		ApplyBudget(gov, governor.BudgetTransitions{ExhaustedActive: true}, sink, note)
	}

	count := 0
	for _, a := range sink.added {
		if a.id == BudgetExhaustedAlertID {
			count++
		}
	}
	if count != 4 {
		t.Errorf("AddSystemAlert called %d times across 4 cycles, want 4 (recordingSink logs every call; the dashboard's real AddSystemAlert upserts by ID so the operator sees exactly one banner)", count)
	}
	if len(note.sent) != 1 {
		t.Errorf("notifications = %d across 4 cycles, want exactly 1 (only the first carried Crossed=true)", len(note.sent))
	}
}

// The no-cadence banner exists because the dashboard's other not-producing
// warnings name only the SYMPTOM. Its whole value is that it also carries the
// cause and the fix, so the copy is the feature (#5577).
func TestNoCadenceMessageCarriesSymptomCauseAndFix(t *testing.T) {
	msg := NoCadenceMessage([]string{"scanner", "architect"})

	for _, want := range []struct{ part, why string }{
		{"scanner", "names the affected agent"},
		{"architect", "names every affected agent, not just the first"},
		{"never kicked", "states the symptom"},
		{"no cadence configured", "states the cause"},
		{"set cadences on the agent card", "states the fix, and where to apply it"},
	} {
		if !strings.Contains(msg, want.part) {
			t.Errorf("banner copy missing %q — it must be the line that %s.\ngot: %s",
				want.part, want.why, msg)
		}
	}
}

// Raise while agents lack a cadence; clear the moment none do. A banner that
// never clears trains operators to ignore banners.
func TestNoCadenceRaisesAndClearsOnTheGovernorSignal(t *testing.T) {
	sink := &recordingSink{}
	ApplyNoCadence(fakeNoCadence{[]string{"scanner"}}, sink)
	got, ok := sink.find(NoCadenceAlertID)
	if !ok {
		t.Fatalf("no alert raised under %q; got %+v", NoCadenceAlertID, sink.added)
	}
	if got.severity != "warning" {
		t.Errorf("severity = %q, want %q — an unconfigured hive is not a broken one",
			got.severity, "warning")
	}
	if len(sink.cleared) != 0 {
		t.Errorf("must not clear while agents still have no cadence; cleared=%v", sink.cleared)
	}

	sink2 := &recordingSink{}
	ApplyNoCadence(fakeNoCadence{nil}, sink2)
	if len(sink2.added) != 0 {
		t.Errorf("must raise nothing once every agent has a cadence; got %+v", sink2.added)
	}
	if !contains(sink2.cleared, NoCadenceAlertID) {
		t.Errorf("must clear %q once every agent has a cadence; cleared=%v",
			NoCadenceAlertID, sink2.cleared)
	}
}

type fakeModeUnscheduled struct {
	agents []governor.ModeUnscheduledAgent
}

func (f fakeModeUnscheduled) ModeUnscheduledAgents() []governor.ModeUnscheduledAgent {
	return f.agents
}

// The mode-unscheduled banner (#7474) exists because an agent whose only
// cadence is in surge reads as healthy on every other signal once the fleet
// leaves surge. Its copy must say which agent, which mode the fleet is in,
// which modes DO schedule it, and how to close the gap.
func TestModeUnscheduledMessageNamesAgentModeAndFix(t *testing.T) {
	msg := ModeUnscheduledMessage([]governor.ModeUnscheduledAgent{
		{Agent: "reviewer", Mode: "busy", CadenceModes: []string{"surge"}},
		{Agent: "auditor", Mode: "busy", CadenceModes: []string{"quiet", "surge"}},
	})

	for _, want := range []struct{ part, why string }{
		{"reviewer", "names the affected agent"},
		{"auditor", "names every affected agent, not just the first"},
		{"only in surge", "says which modes DO schedule the reviewer"},
		{"quiet and surge", "lists every scheduling mode for the auditor"},
		{"current busy mode", "names the mode the fleet is in"},
		{"will not kick", "states the symptom"},
		{"add a busy cadence", "states the fix for the mode the fleet is in"},
		{"idle cadence is inherited", "offers the fix that covers every mode"},
	} {
		if !strings.Contains(msg, want.part) {
			t.Errorf("banner copy missing %q — it must be the line that %s.\ngot: %s",
				want.part, want.why, msg)
		}
	}
	if ModeUnscheduledMessage(nil) != "" {
		t.Errorf("an empty list must render no banner line")
	}
}

// Raise while an agent is configured but not scheduled in the current mode;
// clear as soon as none is. The banner is amber: a hole in the cadence ladder
// is not a broken hive.
func TestModeUnscheduledRaisesAndClearsOnTheGovernorSignal(t *testing.T) {
	sink := &recordingSink{}
	ApplyModeUnscheduled(fakeModeUnscheduled{[]governor.ModeUnscheduledAgent{
		{Agent: "reviewer", Mode: "busy", CadenceModes: []string{"surge"}},
	}}, sink)
	got, ok := sink.find(ModeUnscheduledAlertID)
	if !ok {
		t.Fatalf("no alert raised under %q; got %+v", ModeUnscheduledAlertID, sink.added)
	}
	if got.severity != "warning" {
		t.Errorf("severity = %q, want %q — a cadence gap is not a broken hive", got.severity, "warning")
	}
	if !strings.Contains(got.message, "reviewer") {
		t.Errorf("banner does not name the agent: %q", got.message)
	}
	if len(sink.cleared) != 0 {
		t.Errorf("must not clear while an agent is unscheduled; cleared=%v", sink.cleared)
	}
	// Distinct identity from the never-scheduled banner: the two describe
	// disjoint classes and must be able to coexist on screen.
	if ModeUnscheduledAlertID == NoCadenceAlertID {
		t.Fatal("ModeUnscheduledAlertID must not collide with NoCadenceAlertID")
	}

	sink2 := &recordingSink{}
	ApplyModeUnscheduled(fakeModeUnscheduled{nil}, sink2)
	if len(sink2.added) != 0 {
		t.Errorf("must raise nothing once every agent is scheduled; got %+v", sink2.added)
	}
	if !contains(sink2.cleared, ModeUnscheduledAlertID) {
		t.Errorf("must clear %q once every agent is scheduled; cleared=%v", ModeUnscheduledAlertID, sink2.cleared)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
