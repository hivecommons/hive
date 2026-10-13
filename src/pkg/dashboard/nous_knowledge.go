package dashboard

// Live ledger and principles (RFC #11585 part 4, #11697).
//
// Before this seam the panel served the ledger and principles the hive had
// parsed once at boot, so a round the experimentation agent finished after
// start-up was invisible until the next restart. The dashboard now asks a
// NousKnowledgeStore for the current content on every /api/nous/status,
// /ledger and /principles request; cmd/hive wires pkg/nousstore's
// change-detecting reader through it, exactly like GateStore and
// PersistPanel, so this package stays free of storage concerns. With
// KnowledgeStore unset every handler keeps its boot-snapshot behaviour.

// Ledger entry and principle keys/values every entry served by the panel
// carries. Mirrors nousstore's schema constants without importing the
// package; cmd/hive's tests pin the two sets equal.
const (
	NousKeyType    = "type"
	NousKeyStatus  = "status"
	NousKeyOutcome = "outcome"

	NousLedgerTypeBaseline       = "baseline"
	NousLedgerTypeExperiment     = "experiment"
	NousLedgerTypeDryRun         = "dry_run"
	NousLedgerTypeAnalysis       = "analysis"
	NousLedgerTypeShadowAnalysis = "shadow_analysis"

	NousLedgerStatusPending   = "pending"
	NousLedgerStatusActive    = "active"
	NousLedgerStatusCompleted = "completed"
	NousLedgerStatusAborted   = "aborted"

	NousPrincipleTypeDomain = "domain"
	NousPrincipleTypeMeta   = "meta"

	NousPrincipleStatusActive  = "active"
	NousPrincipleStatusUpdated = "updated"
	NousPrincipleStatusPruned  = "pruned"
)

// Status map keys the knowledge sync keeps in step with the live content.
const (
	nousStatusKeyIterations     = "iterations"
	nousStatusKeyPrinciples     = "principles"
	nousStatusKeyPrincipleCount = "principleCount"
	nousStatusKeyPhase          = "phase"

	// Phases the panel moves between as data arrives; "collecting" until
	// the first ledger row, "observing" after.
	nousPhaseCollecting = "collecting"
	nousPhaseObserving  = "observing"
)

// NousKnowledge is the live ledger and principle content a store returns.
type NousKnowledge struct {
	Ledger     []map[string]interface{}
	Principles []NousPrinciple
}

// NousKnowledgeStore serves the ledger and principles live.
type NousKnowledgeStore interface {
	// LoadKnowledge returns the current content and whether it changed
	// since the previous call. An error means a file exists but could not
	// be read; the content returned alongside is the last good version
	// and is still applied.
	LoadKnowledge() (NousKnowledge, bool, error)
}

// SyncKnowledge refreshes Ledger, Principles and the derived Status counts
// from KnowledgeStore when the store reports a change. It is a no-op
// without a store. Callers must NOT hold Mu. The returned error is the
// store's read error, for logging; the state is still updated with
// whatever the store returned.
func (ns *NousState) SyncKnowledge() error {
	if ns == nil || ns.KnowledgeStore == nil {
		return nil
	}
	k, changed, err := ns.KnowledgeStore.LoadKnowledge()
	if !changed {
		return err
	}
	ns.Mu.Lock()
	defer ns.Mu.Unlock()
	ns.Ledger = k.Ledger
	ns.Principles = k.Principles
	if len(ns.Ledger) > 0 && ns.Phase == nousPhaseCollecting {
		ns.Phase = nousPhaseObserving
	}
	if ns.Status == nil {
		ns.Status = make(map[string]interface{})
	}
	ns.Status[nousStatusKeyIterations] = len(ns.Ledger)
	ns.Status[nousStatusKeyPrinciples] = len(ns.Principles)
	ns.Status[nousStatusKeyPrincipleCount] = len(ns.Principles)
	ns.Status[nousStatusKeyPhase] = ns.Phase
	return err
}

// syncNousKnowledge refreshes the live ledger/principles before a read
// handler serves them, logging (not failing on) an unreadable file.
func (s *Server) syncNousKnowledge() {
	if s.deps == nil || s.deps.Nous == nil {
		return
	}
	if err := s.deps.Nous.SyncKnowledge(); err != nil && s.logger != nil {
		s.logger.Warn("nous: ledger/principles unreadable, serving last good copy", "error", err)
	}
}
