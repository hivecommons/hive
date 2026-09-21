package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// ── #6902: explain every admission refusal, not only the convergence ones ─────
//
// An operator looking at an open `help wanted` issue on GitHub can reasonably
// ask "why isn't this in the Contributor Queue?". Before this file the answer
// existed and was thrown away: admissionQueueSnapshot's exclusion ladder is a
// column of bare `continue`s, so the only thing the operator saw was an absent
// row.
//
// #4246 already established the pattern — retain the decision the queue's OWN
// sweep computed, bounded by the same limit, never cached, never enforced — but
// it retained only convergence refusals (dependency_blocked / dependency_unknown
// / degraded observation) and only under the convergence rollout toggle. This
// file generalises the vocabulary to every gate on the ladder and decouples it
// from that toggle, because the gates it now explains are enforced with the
// toggle off.
//
// The hard rule this file exists to keep: a withheld row is an EXPLANATION of a
// refusal the live path already made, produced by the same pass that made it.
// Nothing here re-evaluates admission, and nothing here can admit anything —
// the withheld list is write-only from the ladder's point of view.

// withheldScope selects WHICH refusals a snapshot retains. One sweep always
// computes the same decisions; the scope decides how much of the explanation
// survives into the snapshot, so the two surfaces cannot drift apart by
// running different ladders.
//
//   - withheldNone: collect nothing. The default for every caller that only
//     wants the rendered queue, and the reason diagnostics cost nothing there.
//   - withheldConvergence: the #4246 shadow surface, unchanged — dependency
//     judgments only, open-PR claims deliberately excluded.
//   - withheldAll: the #6902 operator surface — every gate on the ladder.
type withheldScope int

const (
	withheldNone withheldScope = iota
	withheldConvergence
	withheldAll
)

// collects reports whether this scope retains anything at all.
func (s withheldScope) collects() bool { return s != withheldNone }

// Stable reason codes for the gates that had no reason type at all. The four
// structured reasons (open_pr_claim, workflow_blocked, dependency_blocked,
// dependency_unknown) keep their existing constants in contribute_admission.go
// and are reused verbatim, so one vocabulary covers the whole ladder.
const (
	// withheldReasonDisabledRepo: the repository is switched off for
	// contribution in hub config, so none of its issues are offered.
	withheldReasonDisabledRepo = "disabled_repo"
	// withheldReasonTracker: a tracker/umbrella issue is coordination-only; its
	// children carry the work and are queued independently.
	withheldReasonTracker = "tracker"
	// withheldReasonCooldown: the issue completed recently and is inside its
	// post-completion cooldown window.
	withheldReasonCooldown = "cooldown"
	// withheldReasonNoWorkNeeded: a live no_work_needed verdict says there is
	// nothing to do on this issue until it changes again.
	withheldReasonNoWorkNeeded = "no_work_needed"
	// withheldReasonFailureCooldown: the issue failed recently and is inside its
	// failure cooldown (or the longer quarantine window).
	withheldReasonFailureCooldown = "failure_cooldown"
	// withheldReasonInFlight: a contributor is working the issue right now.
	withheldReasonInFlight = "in_flight"
	// withheldReasonContributorFilter: the hub's title / author / label
	// contributor filters rejected the candidate.
	withheldReasonContributorFilter = "contributor_filter"
	// withheldReasonAssignedToOther: the issue is assigned to someone else and
	// the skip-assigned toggle is on.
	withheldReasonAssignedToOther = "assigned_to_other"
)

// withheldReasonLabels renders each code as the one-line English an operator
// reads in the Withheld section. Kept beside the constants so a new reason
// cannot ship without its prose, and exported through the API so the UI never
// has to carry a second copy of this vocabulary (the #6902 "one admission path"
// rule applies to the WORDS as much as to the decisions).
var withheldReasonLabels = map[string]string{
	contributorAdmissionReasonOpenPRClaim:       "An open pull request already claims this issue",
	contributorAdmissionReasonMergedClaimStale:  "Fixed by a merged pull request; the issue is still open — close it or say what remains",
	contributorAdmissionReasonIssueChurn:        "Too many pull requests on one issue — needs maintainer triage",
	contributorAdmissionReasonWorkflowBlocked:   "Workflow label: blocked",
	contributorAdmissionReasonDependencyBlocked: "A dependency is still open",
	contributorAdmissionReasonDependencyUnknown: "A dependency could not be resolved",
	withheldReasonDisabledRepo:                  "Repository is disabled for contribution",
	withheldReasonTracker:                       "Tracker / umbrella issue",
	withheldReasonCooldown:                      "Completion cooldown",
	withheldReasonNoWorkNeeded:                  "Previous no_work_needed verdict",
	withheldReasonFailureCooldown:               "Failure cooldown",
	withheldReasonInFlight:                      "Already in flight",
	withheldReasonContributorFilter:             "Rejected by a contributor filter",
	withheldReasonAssignedToOther:               "Assigned to another contributor",
}

// withheldReasonLabel returns the operator-facing prose for a reason code,
// falling back to the raw code so an unlabelled future reason still renders as
// something rather than as an empty line.
func withheldReasonLabel(reason string) string {
	if label, ok := withheldReasonLabels[reason]; ok {
		return label
	}
	return reason
}

// isConvergenceWithheldReason reports whether a reason belongs to the #4246
// convergence diagnostics surface. That surface predates this file and has a
// pinned contract — open-PR claims are deliberately NOT convergence diagnostics
// (TestAdmissionDiagnostics_OpenPRClaimIsNotAConvergenceDiagnostic) — so the
// shadow-mode payload is filtered back down to exactly its historical
// membership rather than inheriting the broader vocabulary above.
func isConvergenceWithheldReason(reason string) bool {
	switch reason {
	case contributorAdmissionReasonOpenPRClaim,
		contributorAdmissionReasonMergedClaimStale,
		contributorAdmissionReasonIssueChurn,
		withheldReasonDisabledRepo,
		withheldReasonTracker,
		withheldReasonCooldown,
		withheldReasonNoWorkNeeded,
		withheldReasonFailureCooldown,
		withheldReasonInFlight,
		withheldReasonContributorFilter,
		withheldReasonAssignedToOther:
		return false
	}
	// What remains is the convergence vocabulary: dependency_blocked,
	// dependency_unknown, and the degraded-observation reasons the evaluator
	// mints itself. Matching by exclusion rather than by list is deliberate —
	// convergence owns its own reason strings and may add to them, and a new
	// convergence reason belongs on the convergence surface by default.
	return true
}

// withheldCandidate is the issue-level context the ladder already has in hand
// at every gate, so recording a refusal costs one struct literal rather than a
// re-read of the issue map.
type withheldCandidate struct {
	repoFull string
	ref      worksource.Ref
	title    string
	url      string
}

// newWithheldItem builds the common shell of a withheld row. Reason-specific
// evidence is attached by the caller, which is the only place that holds it.
func newWithheldItem(c withheldCandidate, reason string) AdmissionWithheldItem {
	return AdmissionWithheldItem{
		Repo:   c.repoFull,
		Number: c.ref.Number,
		Key:    c.ref.Key(),
		Title:  c.title,
		URL:    c.url,
		Reason: reason,
		Detail: withheldReasonLabel(reason),
	}
}

// withheldCollector accumulates refusals during one sweep under the same bound
// the rendered queue uses, so a pathological population can never blow out the
// payload. It is deliberately a value-semantics helper on the snapshot rather
// than a second pass: every Add call sits inside the ladder's own `continue`.
type withheldCollector struct {
	scope withheldScope
	limit int
	items []AdmissionWithheldItem
}

func newWithheldCollector(scope withheldScope, limit int) *withheldCollector {
	c := &withheldCollector{scope: scope, limit: limit}
	if scope.collects() {
		c.items = []AdmissionWithheldItem{}
	}
	return c
}

// accepts reports whether this scope retains a row with the given reason. The
// membership test lives here, at the collector, so every gate on the ladder can
// call add unconditionally — a gate never has to know which surface is asking,
// which is what stops a future reason from being wired into one surface and
// forgotten on the other.
func (c *withheldCollector) accepts(reason string) bool {
	switch c.scope {
	case withheldAll:
		return true
	case withheldConvergence:
		return isConvergenceWithheldReason(reason)
	default:
		return false
	}
}

// add records one refusal. Out-of-scope and over-limit are both silent no-ops:
// the collector never affects the ladder's control flow, so a caller can always
// write `collector.add(...)` immediately before its existing `continue`.
//
// The cap is applied AFTER the scope test, so each surface is bounded by the
// rows it actually keeps — a convergence snapshot cannot have its limit spent
// on rows it was never going to emit.
func (c *withheldCollector) add(item AdmissionWithheldItem) {
	if c == nil || !c.accepts(item.Reason) || len(c.items) >= c.limit {
		return
	}
	c.items = append(c.items, item)
}

// addReason is the common case: a gate with no evidence beyond its own code.
func (c *withheldCollector) addReason(cand withheldCandidate, reason string) {
	if c == nil || !c.accepts(reason) {
		return
	}
	c.add(newWithheldItem(cand, reason))
}

// decodeActionableIssue normalises one raw ActionableIssues entry into the map
// form the ladder reads plus its canonical identity. Returns ok=false for an
// entry that cannot be decoded or has no identity at all — the same two cases
// the ladder itself skips, so an undecodable candidate is never explained as
// something it is not.
func decodeActionableIssue(repoFull string, raw any) (map[string]any, worksource.Ref, bool) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, worksource.Ref{}, false
	}
	var issue map[string]any
	if err := json.Unmarshal(b, &issue); err != nil {
		return nil, worksource.Ref{}, false
	}
	ref := refFromIssueMap(repoFull, issue)
	if ref.Key() == "" {
		return nil, worksource.Ref{}, false
	}
	return issue, ref, true
}

// withheldCandidateFrom lifts the public metadata a withheld row carries out of
// the decoded issue map. Title and URL only: nothing here reaches for prompts,
// tokens, or contributor execution data, and the ladder holds none of that
// anyway.
func withheldCandidateFrom(repoFull string, ref worksource.Ref, issue map[string]any) withheldCandidate {
	title, _ := issue["title"].(string)
	url, _ := issue["url"].(string)
	return withheldCandidate{repoFull: repoFull, ref: ref, title: title, url: url}
}

// withheldCooldownItem records a cooldown refusal with the moment it lapses, so
// the operator can tell "comes back in two minutes" from "comes back tomorrow"
// rather than only that it is gated.
func withheldCooldownItem(c withheldCandidate, reason string, until time.Time) AdmissionWithheldItem {
	item := newWithheldItem(c, reason)
	if !until.IsZero() {
		item.CooldownUntil = until.UTC().Format(time.RFC3339)
		item.Detail = fmt.Sprintf("%s until %s", withheldReasonLabel(reason), until.UTC().Format("15:04 MST"))
	}
	return item
}

// withheldFilterItem records which contributor filter rejected the candidate,
// because "rejected by a filter" with three configured is not an answer an
// operator can act on.
func withheldFilterItem(c withheldCandidate, which string) AdmissionWithheldItem {
	item := newWithheldItem(c, withheldReasonContributorFilter)
	item.Filter = which
	item.Detail = fmt.Sprintf("Rejected by the contributor %s filter", which)
	return item
}

// withheldAssignedItem records the skip-assigned refusal and names the logins
// the gate saw.
func withheldAssignedItem(c withheldCandidate, assignees []string) AdmissionWithheldItem {
	item := newWithheldItem(c, withheldReasonAssignedToOther)
	trimmed := make([]string, 0, len(assignees))
	for _, a := range assignees {
		if a = strings.TrimSpace(a); a != "" {
			trimmed = append(trimmed, a)
		}
	}
	if len(trimmed) > 0 {
		item.Assignees = trimmed
		item.Detail = fmt.Sprintf("Assigned to %s", strings.Join(trimmed, ", "))
	}
	return item
}

// withheldFromAdmissionDecision renders a refusal from evaluateContributorNeutral‐
// Admission. The convergence reasons keep the exact #4246 projection of the
// Decision the sweep computed; the two non-convergence reasons it can also
// return (open-PR claim, `blocked` workflow label) get their own evidence,
// which the convergence projection has no field for.
func withheldFromAdmissionDecision(c withheldCandidate, d contributorAdmissionDecision) AdmissionWithheldItem {
	switch d.reason {
	case contributorAdmissionReasonOpenPRClaim:
		item := newWithheldItem(c, d.reason)
		item.ClaimURL = d.claim.PRURL
		item.ClaimAuthor = d.claim.PRAuthor
		if d.claim.PRNumber > 0 {
			item.Detail = fmt.Sprintf("Existing pull request #%d", d.claim.PRNumber)
		}
		return item
	case contributorAdmissionReasonMergedClaimStale:
		item := newWithheldItem(c, d.reason)
		item.ClaimURL = d.claim.PRURL
		item.ClaimAuthor = d.claim.PRAuthor
		item.Detail = mergedClaimStaleDetail(d.claim, time.Now())
		return item
	case contributorAdmissionReasonIssueChurn:
		item := newWithheldItem(c, d.reason)
		item.ChurnMerged = len(d.churn.Merged)
		item.ChurnClosed = len(d.churn.ClosedUnmerged)
		item.ChurnPRs = d.churn.PRNumbers()
		// The counts ARE the answer a maintainer is being asked for, so the
		// prose states them rather than restating the rule.
		item.Detail = d.churn.Reason()
		return item
	case contributorAdmissionReasonWorkflowBlocked:
		return newWithheldItem(c, d.reason)
	}
	return withheldItemFromDecision(c.repoFull, c.ref, c.title, c.url, d.convergence)
}

// mergedClaimStaleDetail is the #8003 question put to a maintainer: which PR
// fixed it, how long ago, and what the two possible answers are. The age is
// in whole days because the point is "days, not hours" — the hold has already
// outlasted every automatic bound.
func mergedClaimStaleDetail(claim ghpkg.IssueClaim, now time.Time) string {
	days := int(now.Sub(claim.SettledAt()).Hours() / 24)
	unit := "days"
	if days == 1 {
		unit = "day"
	}
	by := "a merged pull request"
	if claim.PRNumber > 0 {
		by = fmt.Sprintf("merged PR #%d", claim.PRNumber)
	}
	if claim.Source == ghpkg.ClaimSourceVerdict {
		by = "a verified no_work_needed verdict"
		if claim.PRNumber > 0 {
			by = fmt.Sprintf("a verified no_work_needed verdict citing merged PR #%d", claim.PRNumber)
		}
	}
	return fmt.Sprintf("Fixed by %s %d %s ago; the issue is still open — close it or say what remains", by, days, unit)
}

// rejectingContributorFilter reports WHICH of the three contributor filters
// rejects this candidate, or "" when all three pass. Short-circuit order and
// outcome are identical to the single boolean expression it replaces — the only
// thing it adds is the name of the filter that said no.
func rejectingContributorFilter(hub config.HubConfig, title, author string, labels []string) string {
	if !config.FilterPasses(title, hub.ContributeDenyTitles, hub.ContributeTitlesMode) {
		return "title"
	}
	if !config.FilterPasses(author, hub.ContributeDenyAuthors, hub.ContributeAuthorsMode) {
		return "author"
	}
	if !config.LabelsFilterPasses(labels, hub.ContributeDenyLabels, hub.ContributeLabelsMode) {
		return "label"
	}
	return ""
}
