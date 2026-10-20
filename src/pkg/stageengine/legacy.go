package stageengine

import "sync"

// LegacyTable is the per-pass table a sweep running under ModeShadow fills
// with its own derivation before calling ReconcileBatch. Its Lookup is the
// Options.Legacy comparator: the reconciler writes the sweep's Decision
// verbatim and records where the engine disagreed, so a sweep routed
// through the reconciler keeps today's labels byte for byte. A Ref the
// sweep did not set is an Abstain — the sweep had nothing to write.
//
// One table serves a process: entries are keyed by Ref.Key, set before the
// batch and cleared after it, so concurrent sweeps over disjoint items
// share it safely.
type LegacyTable struct {
	m sync.Map // Ref.Key() → Decision
}

// Set records the sweep's Decision for ref.
func (t *LegacyTable) Set(ref Ref, d Decision) { t.m.Store(ref.Key(), d) }

// Delete clears ref once its batch is done.
func (t *LegacyTable) Delete(ref Ref) { t.m.Delete(ref.Key()) }

// Lookup is the Options.Legacy comparator.
func (t *LegacyTable) Lookup(f Facts) Decision {
	v, ok := t.m.Load(f.Ref.Key())
	if !ok {
		return abstain(reasonNoLegacy)
	}
	return v.(Decision)
}

const reasonNoLegacy = "stage engine: the sweep recorded no decision for the item"
