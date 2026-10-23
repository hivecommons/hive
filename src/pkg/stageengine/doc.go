// Package stageengine is the central stage engine of ADR-0025: one place
// that derives the `stage/*` label for issues and pull requests and the only
// code path that writes it.
//
// The package owns the shared fact model (Facts), the Deriver interface and
// its two kind-specific implementations (DeriveIssue, DerivePR), the
// Reconciler that turns a Decision into a label write, the FactSink that
// out-of-cycle writers (claim relay, ledger hooks, dashboard) submit facts
// through, and the expected-transition table (Expected) that turns a
// surprising from→to hop into a metric rather than a rejected write.
//
// Both kind-specific derivations are built in: derive_issue.go consumes
// IssueFacts and derive_pr.go consumes PRFacts, including the lane opinions
// gathered by automerge. Shared PR observations and reason vocabulary live
// in the prstage sub-package. Neither derivation imports github or automerge
// or performs forge I/O; the PR goldens pin the lane precedence unchanged.
//
// The engine is the only writer: ModeEnforce writes its Decision, ModeShadow derives and
// counts without writing (a pause); the legacy comparator that let shadow
// write the sweep's own derivation during the rollout is gone.
package stageengine
