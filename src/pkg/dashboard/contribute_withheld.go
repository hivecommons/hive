package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

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
	// withheldReasonRepoUnmintable: a recorded repository-scoped token mint
	// failure excludes the repository until its cooldown expires.
	withheldReasonRepoUnmintable = taskUnavailableRepoUnmintable
	// withheldReasonTracker: a tracker/umbrella issue is coordination-only; its
	// children carry the work and are queued independently.
	withheldReasonTracker = "tracker"
	// withheldReasonUmbrella: an umbrella, epic or decomposed parent by the
	// hub.contribute_umbrella rules (label, marker, title prefix, task list,
	// sub-issues). Coordination work for the owner and the hive, never offered
	// to a contributor (hivecommons/hive#19078).
	withheldReasonUmbrella = "umbrella"
	// withheldReasonHumanOnly: waiting on something only a person can do
	// (`needs-human`, `human-only`, the human-only body marker).
	withheldReasonHumanOnly = "human_only"
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
	contributorAdmissionReasonOpenPRLinked:      "An open pull request already links this issue (claim skipped)",
	contributorAdmissionReasonIssueClaim:        "Someone has claimed this issue; the claim expires on its own",
	contributorAdmissionReasonMergedClaimStale:  "Fixed by a merged pull request; the issue is still open — close it or say what remains",
	contributorAdmissionReasonIssueChurn:        "Too many pull requests on one issue — needs maintainer triage",
	contributorAdmissionReasonWorkflowBlocked:   "Workflow label: blocked",
	contributorAdmissionReasonLabelSkipped:      "Contribute skip label",
	contributorAdmissionReasonDependencyBlocked: "A dependency is still open",
	contributorAdmissionReasonDependencyUnknown: "A dependency could not be resolved",
	contributorAdmissionReasonOwnerGrace:        "Filed by an owner or maintainer moments ago; they claim first",
	contributorAdmissionReasonCoveringPRStalled: "The open pull request covering this issue has stalled — close it or take it over; the owner decides",
	withheldReasonDisabledRepo:                  "Repository is disabled for contribution",
	withheldReasonRepoUnmintable:                "Repository excluded after a GitHub App token mint failure",
	withheldReasonTracker:                       "Tracker / umbrella issue",
	withheldReasonUmbrella:                      "Umbrella issue: the owner and the hive coordinate it, its children carry the work",
	withheldReasonHumanOnly:                     "Owner-only: needs a person, not a contributor",
	withheldReasonCooldown:                      "Completion cooldown",
	withheldReasonNoWorkNeeded:                  "Previous no_work_needed verdict",
	completionVerdictNeedsDecision:              "Waiting on a maintainer decision",
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

// withheldReasonTags is the short plain-words tag for each reason code, shown
// on the Held back rows of the Operations lifecycle card. withheldReasonLabels
// above is the full sentence (the row's hover text); a tag is the few words an
// operator can scan down a column. Every code in withheldReasonLabels has one
// (TestWithheldReasonTag_EveryReasonHasATag), plus the convergence reasons a
// dependency refusal carries verbatim in its Reason field.
//
// label_skipped and workflow_blocked are named by the label itself instead
// (withheldLabelTag); the entries here are their fallback for a row that did
// not record which label matched.
var withheldReasonTags = map[string]string{
	contributorAdmissionReasonOpenPRClaim:       "Open PR covers it",
	contributorAdmissionReasonOpenPRLinked:      "Open PR covers it",
	contributorAdmissionReasonIssueClaim:        "Claimed by someone",
	contributorAdmissionReasonMergedClaimStale:  "Fixed, still open",
	contributorAdmissionReasonIssueChurn:        "Several PRs merged",
	contributorAdmissionReasonWorkflowBlocked:   "Blocked",
	contributorAdmissionReasonLabelSkipped:      "Skip label",
	contributorAdmissionReasonDependencyBlocked: "Waiting on another issue",
	contributorAdmissionReasonDependencyUnknown: "Dependency unclear",
	contributorAdmissionReasonOwnerGrace:        "Owner's first pick",
	contributorAdmissionReasonCoveringPRStalled: "Stalled PR covers it",
	withheldReasonDisabledRepo:                  "Repo switched off",
	withheldReasonRepoUnmintable:                "Repo access failed",
	withheldReasonTracker:                       "Epic or tracker",
	withheldReasonCooldown:                      "Worked recently",
	withheldReasonNoWorkNeeded:                  "Nothing to do",
	completionVerdictNeedsDecision:              "Needs a decision",
	withheldReasonFailureCooldown:               "Failed recently",
	withheldReasonInFlight:                      "Agent working",
	withheldReasonContributorFilter:             "Filtered out",
	withheldReasonAssignedToOther:               "Assigned to someone",
	// A dependency refusal's row carries the convergence Decision's own reason
	// (withheldItemFromDecision), not dependency_blocked / dependency_unknown.
	convergence.ReasonWaitingForDependency: "Waiting on another issue",
	convergence.ReasonDependencyUnknown:    "Dependency unclear",
	convergence.ReasonObserverUnavailable:  "Dependency unclear",
}

// withheldOtherTagPrefix starts the tag for a reason code this vocabulary does
// not know ("Other: <code>"), so a future gate still renders a tag that can be
// looked up rather than a blank.
const withheldOtherTagPrefix = "Other: "

// withheldUnrecordedTag is the tag for a row with no reason code at all (for
// example an integration-orchestration refusal, whose withheld row has always
// carried an empty reason and detail).
const withheldUnrecordedTag = "Reason not recorded"

// withheldReasonTag returns the short tag for one withheld row. Label refusals
// name the label; a dependency reason the map does not list (a degraded
// observer's own code) falls back on the Ready condition the evaluator set; any
// other unknown code is shown as "Other: <code>", and a row with no code as
// withheldUnrecordedTag. Never empty.
func withheldReasonTag(item AdmissionWithheldItem, hub config.HubConfig) string {
	reason := item.Reason
	switch reason {
	case contributorAdmissionReasonLabelSkipped, contributorAdmissionReasonWorkflowBlocked:
		if tag := withheldLabelTag(item.SkippedLabel, hub); tag != "" {
			return tag
		}
	}
	if tag, ok := withheldReasonTags[reason]; ok {
		return tag
	}
	switch item.Ready {
	case string(convergence.ConditionFalse):
		return withheldReasonTags[convergence.ReasonWaitingForDependency]
	case string(convergence.ConditionUnknown):
		return withheldReasonTags[convergence.ReasonDependencyUnknown]
	}
	if reason == "" {
		return withheldUnrecordedTag
	}
	return withheldOtherTagPrefix + reason
}

// withheldLabelNote is extra hover text for a skip label whose effect is not
// obvious from its name, or "" for every other label. needs-triage holds back
// only an outside author's issue (MatchContributeSkipLabelFor passes over it
// for a trusted author), so the same label also appears on offered issues.
func withheldLabelNote(label string) string {
	if strings.EqualFold(strings.TrimSpace(label), config.DefaultReporterTrustAwaitingLabel) {
		return "This label holds back only issues from outside authors; issues from trusted authors are offered with it."
	}
	return ""
}

// withheldLabelTag names the skip label that withheld an issue in plain words:
// the default skip labels (and this hive's already-done and needs-decision
// labels, whatever they are configured to) get a phrase, any other label is
// shown as itself. "" when no label was recorded.
func withheldLabelTag(label string, hub config.HubConfig) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	switch lower := strings.ToLower(label); {
	case lower == config.DefaultReporterTrustAwaitingLabel:
		return "Needs triage"
	case lower == strings.ToLower(hub.ContributeAlreadyDoneLabelOrDefault()),
		lower == config.DefaultContributeAlreadyDoneLabel:
		return "Marked already done"
	case lower == strings.ToLower(hub.ContributeNeedsDecisionLabelOrDefault()),
		lower == config.DefaultNeedsDecisionLabel,
		lower == config.LegacyNeedsDecisionLabel:
		return "Needs a decision"
	case lower == blockedWorkflowLabel:
		return "Blocked"
	case lower == "epic", lower == "tracking", lower == "tracker":
		return "Epic or tracker"
	case lower == "discussion":
		return "Discussion"
	case lower == "question":
		return "Question"
	}
	return "Label: " + label
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
		contributorAdmissionReasonOpenPRLinked,
		contributorAdmissionReasonIssueClaim,
		contributorAdmissionReasonMergedClaimStale,
		contributorAdmissionReasonIssueChurn,
		contributorAdmissionReasonOwnerGrace,
		contributorAdmissionReasonCoveringPRStalled,
		withheldReasonDisabledRepo,
		withheldReasonRepoUnmintable,
		withheldReasonTracker,
		withheldReasonUmbrella,
		withheldReasonHumanOnly,
		withheldReasonCooldown,
		withheldReasonNoWorkNeeded,
		completionVerdictNeedsDecision,
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

func withheldNoWorkVerdictItem(c withheldCandidate, rec noWorkVerdictRecord) AdmissionWithheldItem {
	item := newWithheldItem(c, withheldReasonNoWorkNeeded)
	if rec.NeedsDecision {
		item.Reason = completionVerdictNeedsDecision
		item.Detail = "Waiting on a maintainer decision"
		if rec.Reason != "" {
			item.Detail = fmt.Sprintf("Waiting on a maintainer decision: %s", rec.Reason)
		}
		return item
	}
	if rec.Blocked {
		item.Detail = "Previous blocked verdict"
		if rec.Reason != "" {
			item.Detail = fmt.Sprintf("Previous blocked verdict: %s", rec.Reason)
		}
	}
	return item
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
func withheldFilterItem(c withheldCandidate, decision config.ContributeFilterDecision) AdmissionWithheldItem {
	item := newWithheldItem(c, withheldReasonContributorFilter)
	item.Filter = decision.Filter
	item.FilterScope = decision.Scope
	item.FilterMode = decision.Mode
	item.FilterMatch = decision.Match
	if decision.Scope == "repo" {
		item.Detail = fmt.Sprintf("Repo filter %s %q %s for %s", decision.Filter, decision.Match, filterModeVerb(decision.Mode), decision.Repo)
		return item
	}
	item.Detail = fmt.Sprintf("Rejected by the contributor %s filter", decision.Filter)
	return item
}

func filterModeVerb(mode string) string {
	if config.NormalizeFilterMode(mode) == config.FilterModeAllow {
		return "required"
	}
	return "denied"
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
	case contributorAdmissionReasonOpenPRLinked:
		item := newWithheldItem(c, d.reason)
		item.ClaimURL = d.openPR.PRURL
		item.ClaimAuthor = d.openPR.PRAuthor
		if d.openPR.PRNumber > 0 {
			item.Detail = fmt.Sprintf("Claim skipped: open pull request #%d already links this issue", d.openPR.PRNumber)
		}
		return item
	case contributorAdmissionReasonMergedClaimStale:
		item := newWithheldItem(c, d.reason)
		item.ClaimURL = d.claim.PRURL
		item.ClaimAuthor = d.claim.PRAuthor
		item.Detail = mergedClaimStaleDetail(d.claim, time.Now())
		return item
	case contributorAdmissionReasonCoveringPRStalled:
		// The covering PR came from the ledger (claim) or the live link
		// guard (openPR); either way the row links it.
		item := newWithheldItem(c, d.reason)
		item.ClaimURL, item.ClaimAuthor = d.claim.PRURL, d.claim.PRAuthor
		if d.openPR.PRNumber > 0 {
			item.ClaimURL, item.ClaimAuthor = d.openPR.PRURL, d.openPR.PRAuthor
		}
		item.Detail = d.detail
		return item
	case contributorAdmissionReasonIssueClaim:
		// #8380: the evidence is the claimant and the expiry — the two facts
		// a reader needs to decide whether to wait or to coordinate.
		item := newWithheldItem(c, d.reason)
		item.ClaimedBy = d.issueClaim.Identity
		item.ClaimExpiresAt = formatRunTime(d.issueClaim.ExpiresAt)
		item.Detail = fmt.Sprintf("Claimed by %s until %s", d.issueClaim.Identity, item.ClaimExpiresAt)
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
	case contributorAdmissionReasonOwnerGrace:
		item := newWithheldItem(c, d.reason)
		item.Detail = d.detail
		return item
	case contributorAdmissionReasonWorkflowBlocked, contributorAdmissionReasonLabelSkipped:
		item := newWithheldItem(c, d.reason)
		if d.skippedLabel != "" {
			item.SkippedLabel = d.skippedLabel
			if d.reason == contributorAdmissionReasonWorkflowBlocked {
				item.Detail = fmt.Sprintf("Workflow label: %s", d.skippedLabel)
			} else {
				item.Detail = fmt.Sprintf("Skipped by contribute label: %s", d.skippedLabel)
			}
		}
		return item
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
func rejectingContributorFilter(hub config.HubConfig, repo, title, author string, labels []string, authorTrusted bool) config.ContributeFilterDecision {
	return hub.EvaluateContributeFiltersFor(repo, title, author, labels, authorTrusted)
}
