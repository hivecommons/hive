package dashboard

import (
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence"
	"github.com/hivecommons/hive/pkg/worksource"
)

// ── #4246: blocked / unknown / partial admission diagnostics ──────────────────
//
// Operators could see WHICH work was offerable (ReadyQueue) but not WHY a
// candidate was excluded: the rejected convergence.Decision was computed on the
// queue's own sweep and then discarded. This file retains it — the SAME
// Decision from the SAME captured sweep, never a re-evaluation — and surfaces
// it additively on GET /api/contribute/queue and the SSE hello frame.
//
// Everything here is gated by the convergence feature toggle
// (config.ConvergenceMode, kubestellar/hive#3845 rollout): with mode "off"
// (the default) no diagnostics are collected or emitted and both payloads are
// byte-for-byte what they were; with mode "shadow" the withheld collection and
// coverage snapshot appear as read-only diagnostics. Nothing is EVER enforced
// by this surface in either mode — a withheld row is a candidate the existing
// #3857/#3904 admission already excluded; the diagnostics only stop discarding
// the explanation.
//
// Deliberate non-facts: no condition the evaluator did not compute is emitted
// (no Converged, no HumanDecisionRequired, no quiescence, no generic
// Degraded), decisions are never cached across passes, and nothing is
// persisted — every request/hydration recomputes from current authoritative
// in-memory bead state through the queue's own sweep.

// AdmissionWithheldItem is one candidate the contributor-neutral convergence
// admission withheld from the offerable queue, with the exact facts the
// evaluator computed on the queue's captured sweep: the stable reason, the
// blocker IDs, the observed record/generation, and the Observed/Ready
// tri-state conditions. It exposes nothing the live path did not already
// compute.
//
// Open-PR-claim refusals are NOT represented here: they are a different,
// pre-convergence gate with their own surface, and #4246 owns only the
// blocked/unknown/partial convergence diagnostics.
type AdmissionWithheldItem struct {
	Repo   string `json:"repo"`
	Number int    `json:"number,omitempty"`
	// Key is the candidate's canonical, source-aware identity
	// (kubestellar/hive#4245): "owner/repo#42" for GitHub-backed work.
	Key   string `json:"key,omitempty"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
	// Reason is the stable machine-readable convergence reason
	// (convergence.ReasonWaitingForDependency / ReasonDependencyUnknown / a
	// degraded-observation reason).
	Reason string `json:"reason"`
	// Blockers are the dependency IDs that prevented admission, already sorted
	// by the evaluator.
	Blockers []string `json:"blockers,omitempty"`
	// ObservedRecord / ObservedGeneration echo which revision of desired state
	// the judgment was made against.
	ObservedRecord     string `json:"observed_record,omitempty"`
	ObservedGeneration string `json:"observed_generation,omitempty"`
	// Observed / Ready are the tri-state condition statuses ("True" | "False" |
	// "Unknown") the evaluator set, so an operator can distinguish a definite
	// blocker (Ready=False) from irreducible uncertainty (Ready=Unknown).
	Observed string `json:"observed,omitempty"`
	Ready    string `json:"ready,omitempty"`
}

// AdmissionCoverage reports, per snapshot, how much of the bead ledger the
// queue's sweep could actually read — the partial-ledger compromise that was
// previously visible only in a log line. Policy states the final #3904 rule in
// force so a partial view is never mistaken for authoritative proof of absence.
type AdmissionCoverage struct {
	// Partial is true when at least one configured bead store could not be
	// read, so a lookup miss is not fully trustworthy.
	Partial bool `json:"partial"`
	// StoresRead / StoresFailed count the configured stores the sweep read and
	// the ones that failed to load at startup.
	StoresRead   int `json:"stores_read"`
	StoresFailed int `json:"stores_failed,omitempty"`
	// Policy is the fixed statement of the final #3904 partial-ledger rule.
	Policy string `json:"policy"`
}

// admissionCoveragePolicy is the current (final #3904) partial-ledger policy,
// stated verbatim on every snapshot: candidates whose record WAS readable stay
// fully gated, while unmapped lookup misses remain admitted — a miss under
// reduced coverage is never presented as authoritative proof of absence.
const admissionCoveragePolicy = "readable blocked records are gated; unmapped lookup misses are admitted (kubestellar/hive#3904)"

// queueAdmissionSnapshot is ONE read-only, ephemeral admission pass: the
// offerable queue, the withheld diagnostics for convergence-blocked/unknown
// candidates, and the sweep's ledger coverage — all from the SAME captured
// sweep, so queue and diagnostics cannot disagree about the state they judged.
// It lives for exactly one request/hydration and is never cached.
type queueAdmissionSnapshot struct {
	queue    []ReadyQueueItem
	withheld []AdmissionWithheldItem
	coverage AdmissionCoverage
}

// convergenceDiagnosticsEnabled reports whether the #4246 diagnostics surface
// is on: convergence mode "shadow". With the default "off" the withheld
// collection is never built and every payload is unchanged. Nil-safe.
func (s *Server) convergenceDiagnosticsEnabled() bool {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return false
	}
	return s.deps.Config.ConvergenceMode() == config.ConvergenceModeShadow
}

// admissionCoverageFromSweep projects the sweep's ledger-coverage facts into
// the snapshot-level report.
func (h *ContributeWSHub) admissionCoverageFromSweep(sweep *contributorAdmissionSweep) AdmissionCoverage {
	cov := AdmissionCoverage{Policy: admissionCoveragePolicy}
	if sweep != nil && sweep.deps != nil {
		cov.Partial = sweep.deps.partial
		cov.StoresRead = sweep.deps.stores
	}
	if h != nil && h.server != nil && h.server.deps != nil {
		cov.StoresFailed = h.server.deps.BeadStoreLoadFailures
	}
	return cov
}

// withheldItemFromDecision renders one convergence-withheld candidate from the
// exact Decision the queue's sweep produced. It reads conditions out of the
// decision rather than recomputing anything — retaining, not re-evaluating.
func withheldItemFromDecision(repoFull string, ref worksource.Ref, title, url string, d convergence.Decision) AdmissionWithheldItem {
	item := AdmissionWithheldItem{
		Repo:               repoFull,
		Number:             ref.Number,
		Key:                ref.Key(),
		Title:              title,
		URL:                url,
		Reason:             d.Reason,
		Blockers:           d.Blockers,
		ObservedRecord:     d.ObservedRecord,
		ObservedGeneration: d.ObservedGeneration,
	}
	if c, ok := d.Condition(convergence.ConditionObserved); ok {
		item.Observed = string(c.Status)
	}
	if c, ok := d.Condition(convergence.ConditionReady); ok {
		item.Ready = string(c.Status)
	}
	return item
}
