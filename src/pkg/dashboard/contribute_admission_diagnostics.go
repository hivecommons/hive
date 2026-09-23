package dashboard

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence"
	"github.com/hivecommons/hive/pkg/worksource"
)

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

	// ── #6902 evidence ────────────────────────────────────────────────────────
	//
	// Every field below is omitempty and populated only by the gate that owns
	// it, so the #4246 convergence payload above is byte-identical to what it
	// emitted before this vocabulary was generalised. None of it is new data:
	// each value is something the refusing gate already held at the moment it
	// said no.

	// Detail is the one-line English for this refusal, rendered server-side so
	// the operator UI never carries a second copy of the reason vocabulary —
	// the same "one admission path" rule that governs the decisions themselves.
	Detail string `json:"detail,omitempty"`
	// ClaimURL / ClaimAuthor are the open pull request that claims this issue
	// (reason open_pr_claim). Public metadata from the governor's claim ledger.
	ClaimURL    string `json:"claim_url,omitempty"`
	ClaimAuthor string `json:"claim_author,omitempty"`
	// ClaimedBy / ClaimExpiresAt carry the live ISSUE claim behind an
	// issue_claim refusal (hivecommons/hive#8380): who holds it and when it
	// lapses (RFC3339). Distinct from ClaimURL/ClaimAuthor, which describe a
	// pull request. omitempty: absent for every other reason.
	ClaimedBy      string `json:"claimed_by,omitempty"`
	ClaimExpiresAt string `json:"claim_expires_at,omitempty"`
	// CooldownUntil is when a completion or failure cooldown lapses, RFC3339 in
	// UTC, so a client can render a countdown without guessing the window.
	CooldownUntil string `json:"cooldown_until,omitempty"`
	// ChurnMerged / ChurnClosed are the pull-request counts behind an
	// issue_churn refusal (#7995), and ChurnPRs names them ("#1236",
	// "owner/repo#12") so a maintainer opening the row has the table rather
	// than a number. Public metadata from the governor's claim ledger.
	ChurnMerged int      `json:"churn_merged,omitempty"`
	ChurnClosed int      `json:"churn_closed,omitempty"`
	ChurnPRs    []string `json:"churn_prs,omitempty"`
	// Assignees are the logins the skip-assigned gate saw on the issue.
	Assignees []string `json:"assignees,omitempty"`
	// Filter names WHICH contributor filter rejected the candidate ("title",
	// "author" or "label"), because "a filter" is not actionable when three are
	// configured.
	Filter string `json:"filter,omitempty"`
	// FilterScope says whether the hive-wide filter or a repo-specific override
	// rejected the candidate. FilterMode/FilterMatch expose the effective rule.
	FilterScope string `json:"filter_scope,omitempty"`
	FilterMode  string `json:"filter_mode,omitempty"`
	FilterMatch string `json:"filter_match,omitempty"`
	// SkippedLabel is the issue label that matched the contribute skip-label set
	// for workflow_blocked / label_skipped refusals.
	SkippedLabel string `json:"skipped_label,omitempty"`
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
const admissionCoveragePolicy = "readable blocked records are gated; unmapped lookup misses are admitted (hivecommons/hive#3904)"

// queueAdmissionSnapshot is ONE read-only, ephemeral admission pass: the
// raw candidate/offerable/held totals, the bounded rendered queue, the withheld
// diagnostics for convergence-blocked/unknown candidates, and the sweep's ledger
// coverage — all from the SAME captured sweep, so status, queue and diagnostics
// cannot disagree about the state they judged. It lives for exactly one
// request/hydration and is never cached.
type queueAdmissionSnapshot struct {
	queue          []ReadyQueueItem
	candidateTotal int
	offerableTotal int
	heldTotal      int
	withheld       []AdmissionWithheldItem
	coverage       AdmissionCoverage
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

// convergenceWithheldScope is the scope the #4246 surfaces ask for: the
// convergence subset in shadow mode, nothing at all otherwise. It is what the
// SSE hello frame uses, so that frame's contract is unchanged by #6902.
func (s *Server) convergenceWithheldScope() withheldScope {
	if s.convergenceDiagnosticsEnabled() {
		return withheldConvergence
	}
	return withheldNone
}

// contributeQueueWithheldScope resolves what GET /api/contribute/queue should
// retain for this request (#6902).
//
// ?withheld=1 is an explicit opt-in for the FULL admission explanation and
// outranks the convergence toggle, because the gates it explains are enforced
// whether or not convergence is rolled out — an operator asking "why isn't this
// queued?" needs the answer in the default configuration. Without the
// parameter the behaviour is exactly what it was: the convergence subset in
// shadow mode, and an unchanged payload otherwise.
func (s *Server) contributeQueueWithheldScope(r *http.Request) withheldScope {
	if r != nil && queryFlagEnabled(r.URL.Query().Get("withheld")) {
		return withheldAll
	}
	return s.convergenceWithheldScope()
}

// queryFlagEnabled reads a boolean query parameter the forgiving way callers
// actually write them: 1/true/yes/on, case-insensitive. An absent or
// unrecognised value is false, so a typo never silently enables a surface.
func queryFlagEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
		Detail:             withheldReasonLabel(d.Reason),
		Blockers:           d.Blockers,
		ObservedRecord:     d.ObservedRecord,
		ObservedGeneration: d.ObservedGeneration,
	}
	// Name the blockers in the prose when the evaluator found them: "a dependency
	// is still open" is the rule, "#4321 is still open" is the answer.
	if len(d.Blockers) > 0 {
		item.Detail = fmt.Sprintf("%s: %s", item.Detail, strings.Join(d.Blockers, ", "))
	}
	if c, ok := d.Condition(convergence.ConditionObserved); ok {
		item.Observed = string(c.Status)
	}
	if c, ok := d.Condition(convergence.ConditionReady); ok {
		item.Ready = string(c.Status)
	}
	return item
}
