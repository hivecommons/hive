// Package convergence is the runtime-independent core of Hive's
// convergence-driven admission judgment (kubestellar/hive#3845).
//
// It answers ONE question and deliberately nothing more: given the currently
// observed state of a candidate unit of work, is a transition on that candidate
// ADMISSIBLE right now? It is not a planner, not a scheduler, and not a
// router — Hive keeps decomposition, replanning, governor policy,
// prioritisation, role/lane routing, and contributor lifecycle. Admission only
// gates the candidate set those layers then choose from.
//
// The package holds no GitHub types, no bead types, no HTTP, and no clock. An
// observer in a caller package (see pkg/dashboard/contribute_admission_deps.go
// for the first one, which observes bead DependsOn) normalises whatever it can
// see into an Observation; Evaluate turns that into a Decision. Keeping the
// judgment pure is what makes it level-triggered by construction: the same
// observation always yields the same decision, so a duplicate, out-of-order, or
// entirely missed event cannot leave admission wedged — the next evaluation
// recomputes from current source state, and a process restart reconstructs the
// same answer from the same durable source.
//
// The vocabulary below is intentionally the small Kubernetes-style
// condition set the design contract asks for (True / False / Unknown plus a
// reason), NOT a mutually-exclusive phase enum, so later increments —
// generation-aware repository/outcome status, exact-subject proofs,
// non-monotonic invalidation, resource claims, authority policy — can extend it
// without a migration dead end.
package convergence

import (
	"fmt"
	"sort"
	"strings"
)

// ConditionStatus is the tri-state a condition may hold. Unknown is a
// first-class answer, not an error: "we could not establish this" must be
// distinguishable from both "established true" and "established false", because
// the three authorise different actions.
type ConditionStatus string

const (
	// ConditionTrue means the condition is established as satisfied by the
	// current observation.
	ConditionTrue ConditionStatus = "True"
	// ConditionFalse means the condition is established as NOT satisfied.
	ConditionFalse ConditionStatus = "False"
	// ConditionUnknown means the current observation is insufficient to decide.
	// It never authorises a mutating transition (no false satisfaction) but it
	// also never blocks anything beyond the affected candidate.
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition types. Only the two the first vertical genuinely computes are
// defined; the rest of the design's vocabulary (Progressing, Converged,
// Degraded, HumanDecisionRequired, ExternallyBlocked, ResourceConflict) is
// deliberately NOT declared here until something computes it, so no caller can
// start depending on a constant that is never set.
const (
	// ConditionObserved reports whether an authoritative record for the
	// candidate was found at all. False is a normal, common answer: most live
	// GitHub issues have no declared intent record, and that is not a fault.
	ConditionObserved = "Observed"
	// ConditionReady reports whether a transition on this candidate is
	// admissible now.
	ConditionReady = "Ready"
)

// Condition reasons. These are stable machine-readable strings; callers log and
// test against them, so treat them as API.
const (
	// ReasonNoIntentRecord: no dependency-bearing record exists for this
	// candidate. See Evaluate for why this admits rather than blocks.
	ReasonNoIntentRecord = "NoIntentRecord"
	// ReasonIntentObserved: a record was found and read.
	ReasonIntentObserved = "IntentObserved"
	// ReasonNoDeclaredDependencies: a record exists and declares no dependencies.
	ReasonNoDeclaredDependencies = "NoDeclaredDependencies"
	// ReasonDependenciesSatisfied: every declared dependency is satisfied.
	ReasonDependenciesSatisfied = "DependenciesSatisfied"
	// ReasonWaitingForDependency: at least one declared dependency is
	// established as NOT satisfied.
	ReasonWaitingForDependency = "WaitingForDependency"
	// ReasonDependencyUnknown: no dependency is established as unsatisfied, but
	// at least one could not be resolved, so satisfaction cannot be asserted.
	ReasonDependencyUnknown = "DependencyUnknown"
)

// Subject is the normalised identity of a unit of work. Repo is the canonical
// "owner/repo" spelling where the observer knows it; Number is the issue
// number. Identity is deliberately a value type with a stable string form so a
// later increment can widen it (project, repository intent, outcome) without
// every caller re-deriving keys by hand.
type Subject struct {
	Repo   string
	Number int
}

// Key is the canonical "owner/repo#number" form used across Hive's admission
// paths (cooldown, hold, active-work, and claim exclusions all key on it).
func (s Subject) Key() string {
	return fmt.Sprintf("%s#%d", s.Repo, s.Number)
}

// Dependency is one observed dependency edge of a candidate.
//
// Status carries the tri-state judgment the observer reached about the
// DEPENDENCY's satisfaction — True (satisfied), False (not satisfied), or
// Unknown (the observer could not resolve it, e.g. the referenced record is
// absent from every store it can see). Detail is a short human-readable note
// used in log lines and operator-facing messages.
type Dependency struct {
	ID     string
	Status ConditionStatus
	Detail string
}

// Observation is what an observer could establish about ONE candidate at one
// instant. It is a snapshot, never a subscription: the caller re-observes on
// every evaluation rather than caching a decision, which is what keeps
// admission reversible when a dependency later becomes unsatisfied again.
//
// Found distinguishes "there is no record for this candidate" from "there is a
// record and it declares nothing". Those look identical in a nil slice and must
// not: the first is the overwhelmingly common case for live GitHub work and
// must stay admissible, while the second is a real, if empty, declaration.
type Observation struct {
	Subject Subject
	// Found reports whether an authoritative record for Subject exists.
	Found bool
	// RecordID identifies the record that was observed (e.g. a bead ID). Empty
	// when Found is false.
	RecordID string
	// Generation identifies WHICH revision of the desired state was observed.
	// It is the observedGeneration seed the design contract asks admission to
	// convey; the first observer uses the record's last-update timestamp. It is
	// reported, never compared — comparing generations is a later increment.
	Generation string
	// Dependencies are the record's declared dependency edges, already resolved
	// to a tri-state by the observer.
	Dependencies []Dependency
	// Degraded marks an observation the observer could not complete
	// authoritatively (an unavailable source, a partial read). A degraded
	// observation must never be read as satisfaction; Evaluate refuses to admit
	// on one. DegradedReason is a short machine-readable note for logs.
	Degraded       bool
	DegradedReason string
}

// Condition is one tri-state judgment with its reason, in the Kubernetes
// condition shape the design contract prescribes.
type Condition struct {
	Type    string
	Status  ConditionStatus
	Reason  string
	Message string
}

// Decision is the admission judgment for one candidate. It conveys exactly what
// the first vertical is asked to convey: readiness, the observed desired
// generation, blockers, and the conditions behind them.
//
// Admitted is the ONLY field the live paths gate on. Everything else exists so
// an operator, a log line, or a later status projection can explain WHY without
// re-running the evaluation.
type Decision struct {
	// Admitted is true when a transition on this candidate is admissible now.
	Admitted bool
	// Reason is the reason of the Ready condition, lifted for convenience.
	Reason string
	// Blockers are the dependency IDs that prevented admission, sorted for
	// deterministic logs and tests. Empty when Admitted.
	Blockers []string
	// ObservedRecord and ObservedGeneration echo the observation that produced
	// this decision, so a log line can say which revision of desired state was
	// judged.
	ObservedRecord     string
	ObservedGeneration string
	// Conditions are the full tri-state judgments, in a stable order
	// (Observed, then Ready).
	Conditions []Condition
}

// Condition returns the condition of the given type and whether it was set.
func (d Decision) Condition(condType string) (Condition, bool) {
	for _, c := range d.Conditions {
		if c.Type == condType {
			return c, true
		}
	}
	return Condition{}, false
}

// Evaluate turns one Observation into an admission Decision. It is pure: no
// I/O, no clock, no package state.
//
// The rules, in the order they are applied:
//
//  1. A DEGRADED observation never admits. The observer told us it could not
//     read authoritatively, so asserting satisfaction would be a false
//     positive — exactly the failure mode the design contract forbids ("no
//     false satisfaction" when an observer is unavailable). Ready is Unknown,
//     not False: nothing is established about the candidate, and the caller
//     should retry, not conclude.
//
//  2. NOT FOUND admits. This is the explicit lookup-miss policy the contract
//     demands be stated rather than left accidental: a candidate with no
//     dependency-bearing record has DECLARED NO DEPENDENCIES, so there is
//     nothing to wait for. It is emphatically NOT read as "an unknown
//     dependency was satisfied" — Observed is recorded False with reason
//     NoIntentRecord, so the decision says plainly that the judgment rests on
//     an absent record. Failing closed here instead would block the entire
//     live contributor queue on day one, since the overwhelming majority of
//     actionable GitHub issues have no bead at all; that is a repository-wide
//     stall, which the contract also forbids.
//
//  3. An ESTABLISHED-UNSATISFIED dependency blocks. Ready=False,
//     reason WaitingForDependency. This is the core gate.
//
//  4. An UNRESOLVABLE dependency blocks, but as Unknown. Ready=Unknown,
//     reason DependencyUnknown. The candidate is not mutably dispatched (no
//     false satisfaction), while every unrelated candidate is judged
//     independently — one unknown never serialises the queue, because
//     Evaluate has no cross-candidate state through which it could.
//
//     False is checked before Unknown so a decision with both reports the
//     definite blocker, which is the more actionable reason.
//
//  5. Otherwise the candidate is admitted.
func Evaluate(obs Observation) Decision {
	d := Decision{
		ObservedRecord:     obs.RecordID,
		ObservedGeneration: obs.Generation,
	}

	// Rule 1: a degraded read authorises nothing.
	if obs.Degraded {
		reason := obs.DegradedReason
		if reason == "" {
			reason = "ObserverUnavailable"
		}
		d.Conditions = []Condition{
			{Type: ConditionObserved, Status: ConditionUnknown, Reason: reason,
				Message: "dependency state could not be observed authoritatively"},
			{Type: ConditionReady, Status: ConditionUnknown, Reason: reason,
				Message: "not admitted: dependency state is unknown while the observer is degraded"},
		}
		d.Reason = reason
		return d
	}

	// Rule 2: no record for this candidate means no declared dependency.
	if !obs.Found {
		d.Admitted = true
		d.Reason = ReasonNoDeclaredDependencies
		d.Conditions = []Condition{
			{Type: ConditionObserved, Status: ConditionFalse, Reason: ReasonNoIntentRecord,
				Message: "no dependency-bearing record exists for " + obs.Subject.Key()},
			{Type: ConditionReady, Status: ConditionTrue, Reason: ReasonNoDeclaredDependencies,
				Message: "admitted: candidate declares no dependencies"},
		}
		return d
	}

	observed := Condition{Type: ConditionObserved, Status: ConditionTrue, Reason: ReasonIntentObserved,
		Message: recordMessage(obs)}

	var unsatisfied, unresolved []string
	for _, dep := range obs.Dependencies {
		switch dep.Status {
		case ConditionFalse:
			unsatisfied = append(unsatisfied, dep.ID)
		case ConditionTrue:
			// satisfied; nothing to collect
		default:
			unresolved = append(unresolved, dep.ID)
		}
	}
	sort.Strings(unsatisfied)
	sort.Strings(unresolved)

	switch {
	// Rule 3: an established-unsatisfied dependency is a definite blocker.
	case len(unsatisfied) > 0:
		d.Blockers = unsatisfied
		d.Reason = ReasonWaitingForDependency
		d.Conditions = []Condition{observed, {
			Type: ConditionReady, Status: ConditionFalse, Reason: ReasonWaitingForDependency,
			Message: "not admitted: waiting for " + strings.Join(unsatisfied, ", "),
		}}

	// Rule 4: an unresolvable dependency cannot be asserted satisfied.
	case len(unresolved) > 0:
		d.Blockers = unresolved
		d.Reason = ReasonDependencyUnknown
		d.Conditions = []Condition{observed, {
			Type: ConditionReady, Status: ConditionUnknown, Reason: ReasonDependencyUnknown,
			Message: "not admitted: cannot resolve " + strings.Join(unresolved, ", "),
		}}

	// Rule 5: admitted.
	default:
		d.Admitted = true
		reason := ReasonDependenciesSatisfied
		if len(obs.Dependencies) == 0 {
			reason = ReasonNoDeclaredDependencies
		}
		d.Reason = reason
		d.Conditions = []Condition{observed, {
			Type: ConditionReady, Status: ConditionTrue, Reason: reason,
			Message: "admitted: every declared dependency is satisfied",
		}}
	}

	return d
}

// recordMessage renders the observed-record note, including the generation when
// the observer supplied one.
func recordMessage(obs Observation) string {
	msg := "observed record " + obs.RecordID
	if obs.Generation != "" {
		msg += " at generation " + obs.Generation
	}
	return msg
}
