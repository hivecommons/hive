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
// It imports stagelabels, authorkind and claims only. The issue derivation
// (derive_issue.go) is built in over IssueFacts whose label families
// pkg/github resolves; pkg/github/automerge registers the lane-backed PR
// deriver through SetDerivers. Nothing here knows about lanes or the forge.
//
// DeriveIssue is the engine's own body; DerivePR delegates to the
// registered lane-backed deriver, which wraps automerge's resolveStage
// unchanged (the PR goldens pin it). The engine is the only writer: ModeEnforce writes its Decision, ModeShadow derives and
// counts without writing (a pause); the legacy comparator that let shadow
// write the sweep's own derivation during the rollout is gone.
package stageengine
