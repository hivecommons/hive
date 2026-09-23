// Package extwork is the engine-neutral external-execution binding selected by
// the #8201 Gate 0 decision (#8302) and piloted by #8361.
//
// It lets Hive admit one bounded, externally hosted workflow through the
// contributor protocol, observe it through the engine's native status surface,
// and accept or reject its stage receipt through Hive's own predicate. The
// package owns the vocabulary that any engine adapter must speak:
//
//   - Start is keyed by a logical execution key derived from the Hive-owned
//     admission (work key, assignment, generation, stage, contract revision,
//     engine and workflow version, input revision). The same key with the same
//     payload must yield one native run; the same key with a changed payload
//     must conflict, never silently adopt.
//   - Observe reports one of accepted, running, waiting, terminal, or unknown.
//     A transport failure is unknown, not failure.
//   - Cancel returns requested, acknowledged, and stopped as separate facts.
//     Nothing here ever invents a stopped state.
//   - Receipt fetch verifies the bytes against the declared digest and applies
//     size and path restrictions before parsing; an adapter opens artifacts by
//     execution key and relative path only, never by arbitrary URL.
//
// An offered assignment must be accepted or declined by the host before any
// context beyond the summary is delivered, and every transport-level state
// change is recorded as a progress event to the lease audit. Admission intent
// and authority binding are persisted before dispatch through an
// AdmissionStore that the caller backs with the existing task lease; the
// package introduces no store of its own.
//
// The binding defaults off and, in shadow mode, only observes: it never
// performs an external start or mutation. Report-only mode starts work but
// carries no publication credential of any kind.
package extwork
