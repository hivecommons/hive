// Package publish is the audit campaign's authorized issue publisher
// (hivecommons/hive#8353): the single trusted publication effect that turns
// a validated finding into exactly one issue, or one private disclosure,
// through the mutation boundary.
//
// It is the publication gate of the runs three-gate model. Verification and
// permission to publish are different gates: a technically confirmed finding
// never implies a public issue, because hive's policy requires private
// security disclosure. The publisher therefore
//
//   - is DEFAULT OFF (publication.enabled: false) and, even when on, files
//     only at ACMM L3 and above and only in the enforce convergence mode --
//     shadow and off never perform a forge write;
//   - executes every publication through mutation.Executor under a logical ID
//     derived from the finding identity, so a retried or replayed publication
//     returns the recorded issue instead of filing a second one;
//   - routes findings a named classifier marks security-sensitive to the
//     configured private channel and never to a public issue, refusing (typed
//     error plus audit entry) when no private channel is configured;
//   - books the campaign's predicted repository end state as an
//     outcome.Record so "issue filed" (mutation journal) and "repository
//     outcome satisfied" (outcome ledger) stay different statuses.
package publish
