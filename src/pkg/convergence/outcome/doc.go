// Package outcome is the convergence outcome ledger: the durable record of
// what a convergence decision predicted and what actually happened, plus the
// observation helpers that close the loop.
// In the provisional long-running-run vocabulary, Outcome records are inputs to
// a Gate decision, not a peer archetype.
//
// It is wired into the hive binary through cmd/hive/publicationwire.go
// (#8353, resolving #7281): the audit campaign's authorized issue publisher
// books each campaign's predicted repository end state (one issue per
// validated finding, none for duplicates) as a Record, so "issue filed" (the
// mutation journal) and "repository outcome satisfied" (this ledger) remain
// different statuses. Note that the shadow-mode soak telemetry an operator
// sees today is a separate, live mechanism (dashboard.ConvergenceSoakHistory);
// this ledger records outcome generations, not soak rows.
package outcome
