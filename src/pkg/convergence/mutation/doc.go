// Package mutation stages the convergence mutation executor: the claim,
// journal, and ledger machinery that would apply a convergence decision as a
// durable, crash-safe repository mutation.
//
// It is deliberately not wired into any hive binary (#7281). Tracked for
// activation by the convergence rollout work (#4246/#4263); until then the
// executor has no production caller and nothing here runs in a real hive.
//
// The staged marker is enforced, not decorative:
// TestStagedConvergencePackagesAreMarkedAndUnwired in pkg/convergence fails if
// this package becomes reachable from a binary while still claiming to be
// unwired, so wiring it up forces this doc comment to be corrected in the same
// change.
package mutation
