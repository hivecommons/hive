package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// Reset (RFC #11585): POST /api/nous/reset.
//
// An owner can throw away what the Experiment Lab has collected — the
// baseline samples, the ledger and principles, the gate protocol files —
// without touching what they configured (mode, scope, goals, …). The file
// operations live in pkg/nousstore behind the NousResetStore seam, like
// GateStore and KnowledgeStore, so this package stays free of storage
// concerns; cmd/hive wires the store.
//
// Scopes:
//
//	baseline   snapshots/*; the in-memory snapshot counter goes to zero,
//	           the phase back to collecting and the mode back to observe
//	           (there is nothing left to suggest against).
//	knowledge  governor/ledger.json and governor/principles.json become
//	           empty documents; the panel's ledger and principles empty.
//	all        both, plus gates/*. panel-config.json is never reset.
//
// A reset is refused with 409 while an experiment is active unless the
// scope is "all": abort it first (POST /api/nous/abort), or wipe everything.
const (
	NousResetScopeBaseline  = "baseline"
	NousResetScopeKnowledge = "knowledge"
	NousResetScopeAll       = "all"

	// NousResetConfirm is the literal the request body must carry; a
	// typo'd scope or a forgotten confirmation is a 400, never a reset.
	NousResetConfirm = "RESET"

	nousAuditReset       = "nous_reset"
	nousAuditResetFailed = "nous_reset_failed"

	// nousModeObserve is the trust-ladder rung a baseline reset returns to.
	nousModeObserve = "observe"

	nousStatusKeySnapshots     = "snapshots"
	nousStatusKeySnapshotCount = "snapshotCount"
	nousStatusKeyBaselinePct   = "baseline_pct"
	nousStatusKeyMode          = "mode"
)

// NousResetScope selects what a store reset clears.
type NousResetScope struct {
	// Baseline removes every baseline snapshot.
	Baseline bool
	// Knowledge empties the ledger and principles files.
	Knowledge bool
	// Gates removes the pending gate, decision and history.
	Gates bool
}

// NousResetCounts is what a store reset removed.
type NousResetCounts struct {
	Snapshots     int
	LedgerEntries int
	Principles    int
}

// NousResetStore performs the on-disk part of a reset in one call, so a
// multi-scope reset is one atomic move per directory under one trash
// stamp. cmd/hive wires pkg/nousstore.
type NousResetStore interface {
	Reset(scope NousResetScope) (NousResetCounts, error)
}

// nousResetScopeFor maps the API scope onto the store's selection.
func nousResetScopeFor(scope string) NousResetScope {
	return NousResetScope{
		Baseline:  scope == NousResetScopeBaseline || scope == NousResetScopeAll,
		Knowledge: scope == NousResetScopeKnowledge || scope == NousResetScopeAll,
		Gates:     scope == NousResetScopeAll,
	}
}

// ValidNousResetScope reports whether s is a known reset scope.
func ValidNousResetScope(s string) bool {
	switch s {
	case NousResetScopeBaseline, NousResetScopeKnowledge, NousResetScopeAll:
		return true
	}
	return false
}

// NousResetResult is what POST /api/nous/reset reports.
type NousResetResult struct {
	Scope                string `json:"scope"`
	RemovedSnapshots     int    `json:"removed_snapshots"`
	RemovedLedgerEntries int    `json:"removed_ledger_entries"`
	RemovedPrinciples    int    `json:"removed_principles"`
	Mode                 string `json:"mode"`
}

type nousResetRequest struct {
	Scope   string `json:"scope"`
	Confirm string `json:"confirm"`
}

// errNousExperimentActive is the 409 reason for a partial reset during a run.
var errNousExperimentActive = errors.New("an experiment is active: abort it first, or reset everything")

func (s *Server) handleNousReset(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	var body nousResetRequest
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	body.Scope = sanitizeString(body.Scope)
	if !ValidNousResetScope(body.Scope) {
		jsonError(w, fmt.Sprintf("scope must be %s, %s or %s", NousResetScopeBaseline, NousResetScopeKnowledge, NousResetScopeAll), http.StatusBadRequest)
		return
	}
	if body.Confirm != NousResetConfirm {
		jsonError(w, fmt.Sprintf("confirm must be %q", NousResetConfirm), http.StatusBadRequest)
		return
	}
	if s.deps.Nous.ResetStore == nil {
		jsonError(w, "nous reset store not configured", http.StatusNotFound)
		return
	}

	res, err := s.resetNous(body.Scope)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errNousExperimentActive) {
			code = http.StatusConflict
		}
		s.auditFromRequest(r, nousAuditResetFailed, auditDetail("scope", body.Scope, "error", err.Error()), "")
		jsonError(w, err.Error(), code)
		return
	}
	s.auditFromRequest(r, nousAuditReset, auditDetail(
		"scope", res.Scope,
		"removed_snapshots", strconv.Itoa(res.RemovedSnapshots),
		"removed_ledger_entries", strconv.Itoa(res.RemovedLedgerEntries),
		"removed_principles", strconv.Itoa(res.RemovedPrinciples),
		"mode", res.Mode,
	), "")
	jsonResponse(w, map[string]interface{}{
		"ok":                     true,
		"scope":                  res.Scope,
		"removed_snapshots":      res.RemovedSnapshots,
		"removed_ledger_entries": res.RemovedLedgerEntries,
		"removed_principles":     res.RemovedPrinciples,
		"mode":                   res.Mode,
	})
}

// resetNous performs scope against the store and brings the in-memory
// state in step. Callers must NOT hold Nous.Mu.
func (s *Server) resetNous(scope string) (NousResetResult, error) {
	ns := s.deps.Nous
	res := NousResetResult{Scope: scope}

	s.syncNousKnowledge()
	if scope != NousResetScopeAll && ns.hasActiveExperiment() {
		return res, errNousExperimentActive
	}

	sel := nousResetScopeFor(scope)
	counts, err := ns.ResetStore.Reset(sel)
	if err != nil {
		return res, fmt.Errorf("reset %s: %w", scope, err)
	}
	res.RemovedSnapshots = counts.Snapshots
	res.RemovedLedgerEntries = counts.LedgerEntries
	res.RemovedPrinciples = counts.Principles

	ns.Mu.Lock()
	if ns.Status == nil {
		ns.Status = make(map[string]interface{})
	}
	if sel.Baseline {
		ns.Phase = nousPhaseCollecting
		ns.Mode = nousModeObserve
		ns.Status[nousStatusKeySnapshots] = 0
		ns.Status[nousStatusKeySnapshotCount] = 0
		ns.Status[nousStatusKeyBaselinePct] = NousBaselinePct(0)
		ns.Status[nousStatusKeyPhase] = ns.Phase
		ns.Status[nousStatusKeyMode] = ns.Mode
	}
	if sel.Knowledge {
		ns.Ledger = nil
		ns.Principles = nil
		ns.Status[nousStatusKeyIterations] = 0
		ns.Status[nousStatusKeyPrinciples] = 0
		ns.Status[nousStatusKeyPrincipleCount] = 0
	}
	if sel.Gates {
		ns.GatePending = nil
		ns.GateResponse = nil
	}
	res.Mode = ns.Mode
	ns.Mu.Unlock()

	if sel.Baseline {
		s.persistNousPanel()
	}
	if sel.Knowledge {
		// The live reader re-reads the emptied files; with no store wired
		// the nil slices above already are the truth.
		s.syncNousKnowledge()
	}
	return res, nil
}

// hasActiveExperiment reports whether any ledger row is in the active
// status. Callers must NOT hold Mu.
func (ns *NousState) hasActiveExperiment() bool {
	ns.Mu.Lock()
	defer ns.Mu.Unlock()
	for _, e := range ns.Ledger {
		if st, _ := e[NousKeyStatus].(string); st == NousLedgerStatusActive {
			return true
		}
	}
	return false
}
