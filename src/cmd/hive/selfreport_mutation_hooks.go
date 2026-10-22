package main

import (
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

// installMutationSelfReportHooks wires the mutation boundary's queue-health
// hooks to the self-report tracker: a journaled external effect that stays
// Unknown/Planned past mutation.DefaultStuckAfter while callers keep retrying
// it is reported as fleetreport.ClassEffectLedgerOpStuck (key = the op's
// logical ID), and the condition clears when the op is applied or reconciled.
func installMutationSelfReportHooks(b *mutation.Boundary) {
	if b == nil {
		return
	}
	b.OnStuck = observeEffectLedgerOpStuck
	b.OnResolved = func(logicalID string) {
		fleetreport.Clear(fleetreport.ClassEffectLedgerOpStuck, logicalID)
	}
}

func observeEffectLedgerOpStuck(op mutation.Operation, age time.Duration) {
	repo := op.Effect.Inputs["repo"]
	msg := "effect ledger op stuck: " + op.Effect.Kind + " has been " + op.Status + " for " + age.Round(time.Minute).String() + "; retries refused pending reconciliation"
	diag := "logical_id=" + op.LogicalID + " kind=" + op.Effect.Kind + " status=" + op.Status +
		" attempts=" + strconv.Itoa(len(op.Attempts)) + " updated_at=" + op.UpdatedAt.UTC().Format(time.RFC3339) +
		" target=" + op.Effect.Inputs["target"] + " age=" + age.Round(time.Second).String()
	// The claim target is a content digest, not an issue/PR ref, so it rides
	// in diagnostics rather than the evidence table's ref column.
	fleetreport.ObserveItem(fleetreport.ClassEffectLedgerOpStuck, op.LogicalID, msg, diag, repo, "")
}
