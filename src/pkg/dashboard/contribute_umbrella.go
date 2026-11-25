package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Umbrella and owner-only exclusion for the contributor path
// (hivecommons/hive#19078).
//
// An umbrella, tracker or decomposed parent is coordination work and an
// owner-only issue waits on a person; neither is something a contributor relay
// can finish, so neither is handed to one (selectTask), listed as ready work
// (the queue projection) or counted as workable (/api/contribute/status). They
// stay actionable for the owner and the hive: this gate reads the snapshot and
// refuses at offer time only. It never edits an issue and is not the
// `hive/tracking` path, so closing an umbrella is unaffected.

// contributorExclusion classifies one decoded actionable issue for repoFull.
// It returns the withheld-reason code ("" when the issue is workable) and the
// rule that matched. The legacy enumerator flag `is_tracker` still excludes
// (reason withheldReasonTracker) so turning the classification off restores
// exactly the pre-#19078 behaviour.
//
// A parent with sub-issues is the tracker rule's case, not an umbrella:
// contributeTrackerHoldFor holds it while a child is open (reason
// withheldReasonTracker, with the open count) and offers it again once every
// child is closed. So the sub-issue rule here counts only open children and
// reports withheldReasonTracker, which the offer ladders hand to the tracker
// rule and the counts still tally as umbrella work.
func (h *ContributeWSHub) contributorExclusion(repoFull string, issue map[string]any) (reason, why string) {
	var pol gfi.UmbrellaPolicy
	if h != nil && h.server != nil && h.server.deps != nil && h.server.deps.Config != nil {
		pol = h.server.deps.Config.Hub.UmbrellaPolicyFor(repoFull)
	}
	title, _ := issue["title"].(string)
	body, _ := issue["body"].(string)
	labels := stringSliceFromAny(issue["labels"])
	if w, ok := pol.UmbrellaReason(title, labels, body, 0); ok {
		return withheldReasonUmbrella, w
	}
	if !pol.Disabled && childrenFromIssueMap(issue).Open() > 0 {
		return withheldReasonTracker, umbrellaWhySubIssues
	}
	if isTracker, _ := issue["is_tracker"].(bool); isTracker {
		return withheldReasonTracker, "tracker-detector"
	}
	if w, ok := pol.HumanOnlyReason(labels, body); ok {
		return withheldReasonHumanOnly, w
	}
	if governorSecurityFollowUp(labels, body) {
		return withheldReasonHumanOnly, humanOnlyWhySecurityFollowUp
	}
	return "", ""
}

// umbrellaWhySubIssues is the rule name for a parent held for its open
// sub-issues, the same name gfi.UmbrellaPolicy.UmbrellaReason gives it.
const umbrellaWhySubIssues = "sub-issues"

// humanOnlyWhySecurityFollowUp is the owner-only rule name for a governor
// follow-up that quotes a security review's hand-off to a person.
const humanOnlyWhySecurityFollowUp = "security-review-follow-up"

// governorSecurityFollowUp reports a follow-up issue the governor filed
// (origin/governor + follow-up, fileReviewQuestionFollowUp) whose quoted
// question is the security perspective's requires_human summary, in the
// exact form the review aggregate writes it (review.SecurityRequiresHumanSummary).
// Such an issue asks a person for a security read; no agent can act on it
// (hivecommons/hive#19229, claimed and dropped repeatedly at 010).
func governorSecurityFollowUp(labels []string, body string) bool {
	if !hasLabelFold(labels, reviewQuestionFollowUpLabel) || !hasLabelFold(labels, config.OriginLabelPrefix+config.OriginSourceGovernor) {
		return false
	}
	return review.SecurityRequiresHumanSummary(body)
}

// issueHumanOnlyAsk names why an issue's ask is a person's alone, for the
// owner-delegate decider (delegatedecision.Input.HumanOnly): the
// structured human-only marker or a configured human-only label, or a
// governor security follow-up. `needs-human` alone does not count: it is
// the ordinary park label a delegated decision answers and lifts.
func issueHumanOnlyAsk(cfg *config.Config, repo string, labels []string, body string) string {
	var pol gfi.UmbrellaPolicy
	if cfg != nil {
		pol = cfg.Hub.UmbrellaPolicyFor(repo)
	}
	kept := make([]string, 0, len(labels))
	for _, l := range labels {
		if !strings.EqualFold(strings.TrimSpace(l), stageengine.NeedsHumanLabel) {
			kept = append(kept, l)
		}
	}
	if why, ok := pol.HumanOnlyReason(kept, body); ok {
		return "human-only " + why
	}
	if governorSecurityFollowUp(labels, body) {
		return delegatedecision.HumanOnlySecurityReview
	}
	return ""
}

// excludedCounts counts the umbrella and owner-only issues among one repo's
// actionable snapshot. Umbrella includes the legacy tracker detector.
func (h *ContributeWSHub) excludedCounts(repoFull string, raw []any) (umbrella, humanOnly int) {
	forEachActionableIssue(nil, "", repoFull, raw, func(issue map[string]any, _ worksource.Ref) {
		switch reason, _ := h.contributorExclusion(repoFull, issue); reason {
		case withheldReasonUmbrella, withheldReasonTracker:
			umbrella++
		case withheldReasonHumanOnly:
			humanOnly++
		}
	})
	return umbrella, humanOnly
}

// ExcludedCounts is the hive-wide umbrella and owner-only totals over the
// current actionable snapshot, for surfaces that do not run the admission
// sweep (the fleet payload behind the contributor page).
func (h *ContributeWSHub) ExcludedCounts() (umbrella, humanOnly int) {
	if h == nil || h.server == nil {
		return 0, 0
	}
	h.server.statusMu.RLock()
	status := h.server.status
	h.server.statusMu.RUnlock()
	if status == nil {
		return 0, 0
	}
	for _, repo := range status.Repos {
		u, ho := h.excludedCounts(repo.Full, repo.ActionableIssues)
		umbrella += u
		humanOnly += ho
	}
	return umbrella, humanOnly
}

// stageTrackerOwnerFlags classifies one enumerated issue for the dashboard's
// "Umbrella · owner" and "Owner only" pills: the same rules the contributor
// path applies, resolved for the issue's repo. Display only; the issue's stage
// and actionable bucket are unchanged. umbrella includes the legacy tracker
// detector.
func stageTrackerOwnerFlags(cfg *config.Config, repo string, issue github.Issue) (umbrella, ownerOnly string) {
	var pol gfi.UmbrellaPolicy
	if cfg != nil {
		pol = cfg.Hub.UmbrellaPolicyFor(repo)
	}
	// Open children only, matching the contributor path: a parent whose
	// sub-issues are all closed is ordinary work again.
	children := 0
	if issue.Children != nil {
		children = issue.Children.Open()
	}
	if why, ok := pol.UmbrellaReason(issue.Title, issue.Labels, issue.Body, children); ok {
		umbrella = why
	} else if issue.IsTracker {
		umbrella = "tracker-detector"
	}
	if why, ok := pol.HumanOnlyReason(issue.Labels, issue.Body); ok {
		ownerOnly = why
	} else if governorSecurityFollowUp(issue.Labels, issue.Body) {
		ownerOnly = humanOnlyWhySecurityFollowUp
	}
	return umbrella, ownerOnly
}
