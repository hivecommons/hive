package standby

import (
	"strings"
	"time"
)

// This file is step S6 of the design's phase map
// (`src/docs/design/standby-contributors.md`, "Suspend rule"): the rule that
// takes a configuration out of standby after N consecutive closed-unmerged
// donated PRs, and the reading of a merged PR that decides whether a human had
// to fix it first.
//
// Like the rest of this package it is pure — no I/O, no ambient clock, no hub
// types. The persisted ledger and the surfaces that render a suspension are
// the caller's; what lives here is the decision, so that "when is a donor
// suspended?" has exactly one answer and that answer is a table test.
//
// Two properties are load-bearing and are asserted by tests that fail when the
// code stops stating them:
//
//  1. Only `closed_unmerged` counts toward suspension. A merge resets the
//     streak; a merge that a human had to rework counts as neither and leaves
//     the streak where it was; a row this build does not recognize is skipped.
//     Every ambiguity resolves toward NOT suspending, because the effect of
//     this rule is to stop accepting someone's donated work.
//  2. A threshold of zero or less is the default, not "suspend everybody". A
//     config field nobody set must not silently suspend every donor on the
//     first pass.

// DefaultSuspendThreshold is the N in "N consecutive closed-unmerged donated
// PRs suspends this configuration". It is hive-wide rather than per lane: a
// suspension is a statement about a donor, and a donor is not a lane.
const DefaultSuspendThreshold = 2

// OutcomeKind is the settled fate of one donated PR, as recorded in the
// outcome ledger. The zero value is OutcomeUnknown: a row nobody classified is
// not evidence of anything, and in particular is not evidence against the
// donor.
type OutcomeKind string

const (
	// OutcomeUnknown is the absence of a classification — the zero value, and
	// what an unrecognized ledger value normalizes to. It is skipped by the
	// rule, never counted.
	OutcomeUnknown OutcomeKind = ""
	// OutcomeOpen is a donated PR that has not settled yet. Not an outcome;
	// skipped.
	OutcomeOpen OutcomeKind = "open"
	// OutcomeMerged is a donated PR that merged with the donating contributor
	// as its sole author. It resets the streak.
	OutcomeMerged OutcomeKind = "merged"
	// OutcomeClosedUnmerged is a donated PR that was closed without merging.
	// This is the only kind that counts toward suspension.
	OutcomeClosedUnmerged OutcomeKind = "closed_unmerged"
	// OutcomeMergedAfterRework is a donated PR that merged only after somebody
	// other than the donor pushed to it. It is evidence in neither direction:
	// the work was not mergeable as donated, and it was not garbage either. It
	// is skipped and the streak is preserved across it.
	OutcomeMergedAfterRework OutcomeKind = "merged_after_rework"
	// OutcomeCleared is the owner's clear, written as a row so the ledger
	// stays an append-only audit record rather than being edited. It resets
	// the streak.
	OutcomeCleared OutcomeKind = "cleared"
)

// String renders the outcome for logs and wire fields. OutcomeUnknown renders
// as "unknown" so a reader never sees an empty cell and guesses.
func (k OutcomeKind) String() string {
	if k == OutcomeUnknown {
		return "unknown"
	}
	return string(k)
}

// Known reports whether k is one of the classifications this build recognizes.
func (k OutcomeKind) Known() bool {
	switch k {
	case OutcomeOpen, OutcomeMerged, OutcomeClosedUnmerged, OutcomeMergedAfterRework, OutcomeCleared:
		return true
	default:
		return false
	}
}

// NormalizeOutcome is the total, fail-open-toward-the-donor reading of an
// outcome from an untrusted string: a recognized kind in any casing becomes
// that kind, and everything else — "", a typo, a value written by a newer
// build — becomes OutcomeUnknown and is skipped by the rule.
//
// Skipping is the safe direction here and is the opposite of the tier
// vocabulary's fail-closed reading, deliberately: an unreadable tier must not
// clear a floor, and an unreadable outcome must not suspend a donor.
func NormalizeOutcome(s string) OutcomeKind {
	k := OutcomeKind(strings.ToLower(strings.TrimSpace(s)))
	if !k.Known() {
		return OutcomeUnknown
	}
	return k
}

// Outcome is one appended row of the donated-PR outcome ledger, keyed on the
// contributor-plus-configuration tuple rather than on the person: the RFC
// suspends a configuration, and the same contributor on a different model is a
// different donor.
//
// The rule below reads only Kind. The remaining fields are what makes the
// ledger an audit record an operator can read, and what lets a caller filter
// the file down to one key.
type Outcome struct {
	// Key is LedgerKey(contributor, configuration).
	Key string `json:"key"`
	// Lane is the paused lane the donated task came from.
	Lane string `json:"lane,omitempty"`
	// Repo is "org/name" and Number is the donated PR's number. Both are empty
	// on a `cleared` row, which is an owner action rather than a PR.
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	// DispatchedAt is when the donated task was assigned.
	DispatchedAt time.Time `json:"dispatched_at,omitempty"`
	// Kind is the settled fate. OutcomeOpen until the PR closes or merges.
	Kind OutcomeKind `json:"outcome"`
	// OutcomeAt is when Kind was decided.
	OutcomeAt time.Time `json:"outcome_at,omitempty"`
	// HumanReworked records that somebody other than the donor pushed to the
	// PR, which is what makes a merge OutcomeMergedAfterRework. It is stored
	// alongside the kind so the ledger explains itself without the reader
	// having to know the classification rule.
	HumanReworked bool `json:"human_reworked,omitempty"`
	// ClearedBy is the owner login on a `cleared` row.
	ClearedBy string `json:"cleared_by,omitempty"`
}

// LedgerKey is the ledger's key for one donor: the contributor login joined to
// the whole configuration they are running, in the same canonical spelling
// Configuration.String() uses, so that a relay reporting "Claude" and an owner
// who wrote "claude" land on one row rather than two.
func LedgerKey(contributor string, c Configuration) string {
	return strings.ToLower(strings.TrimSpace(contributor)) + "|" + c.String()
}

// OutcomesFor narrows a whole ledger to the rows for one key, preserving the
// file's append order. SuspendState expects the rows of a single donor; handing
// it a mixed ledger would let one configuration's closures suspend another.
//
// It never returns the caller's slice, so a caller holding ledger state cannot
// be surprised by aliasing.
func OutcomesFor(rows []Outcome, key string) []Outcome {
	out := make([]Outcome, 0, len(rows))
	for _, r := range rows {
		if r.Key == key {
			out = append(out, r)
		}
	}
	return out
}

// SuspendState is the suspend rule as a pure function over one donor's rows.
//
// rows are in ledger (append) order, oldest first. The walk runs newest-first
// and stops at the first row that settles the question:
//
//   - closed_unmerged → increments the streak.
//   - merged → resets to zero and stops. A merge in between breaks the streak.
//   - cleared → resets to zero and stops. The owner's clear reinstates the
//     configuration without deleting the history that suspended it.
//   - merged_after_rework → counts as neither; skipped, streak preserved.
//   - open → skipped; not yet an outcome.
//   - anything else → skipped, for the reason NormalizeOutcome gives.
//
// The configuration is suspended when streak >= threshold. A threshold of zero
// or less means DefaultSuspendThreshold: a hive that never set the field gets
// the documented default, and not a rule that suspends every donor on an empty
// ledger.
//
// The function is total and takes no clock. An empty ledger is not suspended,
// which is the state every hive starts in.
func SuspendState(rows []Outcome, threshold int) (suspended bool, streak int) {
	if threshold <= 0 {
		threshold = DefaultSuspendThreshold
	}
	for i := len(rows) - 1; i >= 0; i-- {
		switch NormalizeOutcome(string(rows[i].Kind)) {
		case OutcomeClosedUnmerged:
			streak++
		case OutcomeMerged, OutcomeCleared:
			// Settled: everything older than this row is behind a reset.
			return streak >= threshold, streak
		default:
			// open, merged_after_rework, unknown: skipped, streak preserved.
		}
	}
	return streak >= threshold, streak
}

// Suspended is SuspendState at the default threshold, for the callers that
// only need the verdict: the matching path filling Candidate.Suspended, and
// the declare path that refuses a suspended configuration.
func Suspended(rows []Outcome) bool {
	s, _ := SuspendState(rows, DefaultSuspendThreshold)
	return s
}

// ClassifyMerge decides which of the two merge outcomes a merged donated PR
// gets, from the donating contributor and the authors that appeared on the PR
// after the hub snapshotted it at hold time — that is,
// holdguard.Drift.NewAuthors, whose job is already "who touched this after we
// recorded it".
//
// Any author on that list other than the donor means a human reworked the PR
// before it merged, so the merge is not evidence the configuration produces
// mergeable work. An empty list, or one naming only the donor pushing again,
// is a plain merge.
//
// Two limits the design records rather than papers over: a maintainer who
// rewrites history before merging can collapse the author set, and one who
// fixes the work in a separate follow-up PR is invisible here. Both bias
// toward OutcomeMerged — generous to the donor, which is the safer direction
// for a rule whose effect is suspension.
func ClassifyMerge(donor string, newAuthors []string) OutcomeKind {
	if ReworkedByOther(donor, newAuthors) {
		return OutcomeMergedAfterRework
	}
	return OutcomeMerged
}

// ReworkedByOther reports whether newAuthors names anybody but the donor.
// Empty entries are ignored: an unattributed commit is not evidence of a
// second author.
func ReworkedByOther(donor string, newAuthors []string) bool {
	d := strings.ToLower(strings.TrimSpace(donor))
	for _, a := range newAuthors {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || a == d {
			continue
		}
		return true
	}
	return false
}
