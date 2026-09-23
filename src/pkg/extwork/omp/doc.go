// Package omp is the second host behind Hive's engine-neutral external-execution
// contract (pkg/extwork): an already-running OMP workbench that takes a lease
// over the existing contributor relay channel (#8361 step 9, answering #6899).
//
// Where the Flue adapter drives a batch engine through an HTTP surface, this
// adapter drives an interactive host through relay messages on the WebSocket
// the workbench already holds as a contributor peer. The channel is the
// existing one; the package adds message types on it, not a new API:
//
//   - the workbench registers as a contributor peer advertising the
//     ext-exec/omp capability; a peer without it is refused, never downgraded;
//   - it receives the assignment SUMMARY only (ext_offer) and must answer with
//     ext_accept or ext_decline before the bounded context bundle (ext_start)
//     is delivered; a declined or unanswered offer never sees the payload;
//   - it emits progress events (ext_progress: accepted, running, waiting,
//     terminal, unknown) which the binding records on the lease audit so the
//     workbench can render state without polling the receipt;
//   - it ends with a stage receipt in the existing stage-receipt/v1 schema
//     (ext_receipt), which the binding verifies by digest and size and binds
//     to the admission before Hive decides anything.
//
// Report-only: the host receives no repository credential and no dashboard
// token, and the receipt is evidence, not authority; publication stays on the
// Hive side. Because OMP is tier T3 (unconfined), only report-only stages may
// be bound to this host: CheckReportOnly refuses a write-capable stage or any
// non-report-only mode before an offer is made and again at Start.
//
// This package is linked into the hive binary only under the extwork_omp
// build tag (cmd/hive/extwork_omp.go); pkg/extwork's guard test enforces that.
package omp
