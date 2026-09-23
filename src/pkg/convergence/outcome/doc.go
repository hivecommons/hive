// Package outcome stages the convergence outcome ledger: the durable record of
// what a convergence decision predicted and what actually happened, plus the
// observation helpers that close the loop.
// In the provisional long-running-run vocabulary, Outcome records are inputs to
// a Gate decision, not a peer archetype.
//
// It is deliberately not wired into any hive binary (#7281). Tracked for
// activation by the convergence rollout work (#4246/#4263); until then nothing
// here runs in a real hive. Note that the shadow-mode soak telemetry an
// operator sees today is a separate, live mechanism
// (dashboard.ConvergenceSoakHistory) -- this ledger is the staged replacement,
// not the thing currently recording those rows.
//
// The staged marker is enforced, not decorative:
// TestStagedConvergencePackagesAreMarkedAndUnwired in pkg/convergence fails if
// this package becomes reachable from a binary while still claiming to be
// unwired, so wiring it up forces this doc comment to be corrected in the same
// change.
package outcome
