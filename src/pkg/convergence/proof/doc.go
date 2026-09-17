// Package proof stages convergence proof objects: the assumptions a
// convergence decision rested on, their verification, and the invalidation
// rules that retire a proof when its assumptions stop holding.
//
// It is deliberately not wired into any hive binary (#7281). Tracked for
// activation by the convergence rollout work (#4246/#4263); until then nothing
// here runs in a real hive.
//
// The staged marker is enforced, not decorative:
// TestStagedConvergencePackagesAreMarkedAndUnwired in pkg/convergence fails if
// this package becomes reachable from a binary while still claiming to be
// unwired, so wiring it up forces this doc comment to be corrected in the same
// change.
package proof
