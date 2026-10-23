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
// It imports stagelabels, authorkind and claims only. pkg/github and
// pkg/github/automerge import it and register their derivers through
// SetDerivers; nothing here knows about lanes, buckets or the forge.
//
// Until the derivation bodies move in (plan PR-F part 2), DeriveIssue and
// DerivePR delegate to the adapters registered by those packages, which
// wrap today's IssueStageFor and resolveStage unchanged. The engine is the
// only writer: ModeEnforce writes its Decision, ModeShadow derives and
// counts without writing (a pause); the legacy comparator that let shadow
// write the sweep's own derivation during the rollout is gone.
package stageengine
