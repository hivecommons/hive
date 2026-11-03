package github

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// ReviewDecision is GitHub's own aggregate verdict on a PR's reviews, as
// reported by the GraphQL PullRequest.reviewDecision field. It is the only
// signal that distinguishes "a review is required and nobody has given one"
// from "a reviewer asked for changes" — REST exposes neither, and
// mergeable_state folds both into the single word "blocked".
type ReviewDecision string

const (
	// ReviewDecisionNone is the zero value: GitHub said nothing, either
	// because the base branch requires no review or because the field was
	// never fetched. It must never be read as "approved".
	ReviewDecisionNone ReviewDecision = ""
	// ReviewDecisionApproved means the review requirement is satisfied.
	ReviewDecisionApproved ReviewDecision = "APPROVED"
	// ReviewDecisionChangesRequested means a reviewer asked for changes and
	// has not dismissed that review.
	ReviewDecisionChangesRequested ReviewDecision = "CHANGES_REQUESTED"
	// ReviewDecisionReviewRequired means branch protection requires an
	// approving review that has not been given.
	ReviewDecisionReviewRequired ReviewDecision = "REVIEW_REQUIRED"
)

// ProtectionFacts are the branch-protection observations gathered for one PR
// during the sweep's normal enumeration — no extra per-PR API call, and
// certainly no call per dashboard hover. Every field is optional: an empty
// one means "not determined", never "not required".
type ProtectionFacts struct {
	// ReviewDecision is GitHub's aggregate review verdict, or "" when it was
	// not fetched.
	ReviewDecision ReviewDecision `json:"review_decision,omitempty"`
	// ChangesRequestedBy are the logins whose latest review on this PR asked
	// for changes.
	ChangesRequestedBy []string `json:"changes_requested_by,omitempty"`
	// LatestHumanReviewState is the newest trusted human opinionated review
	// (APPROVED or CHANGES_REQUESTED) observed on the PR. GitHub's aggregate
	// ReviewDecision can stay CHANGES_REQUESTED after an agent pushed or
	// replied; these fields let routing and dashboards agree on the specific
	// review that needs to be addressed.
	LatestHumanReviewState       ReviewDecision `json:"latest_human_review_state,omitempty"`
	LatestHumanReviewBy          string         `json:"latest_human_review_by,omitempty"`
	LatestHumanReviewSubmittedAt time.Time      `json:"latest_human_review_submitted_at,omitempty"`
	LatestHumanReviewAddressed   bool           `json:"latest_human_review_addressed,omitempty"`
	// ApprovalsGiven counts distinct reviewers whose latest opinionated
	// review is an approval.
	ApprovalsGiven int `json:"approvals_given,omitempty"`
	// RequiredChecksKnown records that the required status-check set for the
	// PR's base branch was determined (from auto_merge.required_checks or
	// from the branch-protection API). Without it, FailingRequiredChecks and
	// MissingRequiredChecks say nothing.
	RequiredChecksKnown bool `json:"required_checks_known,omitempty"`
	// FailingRequiredChecks are the required checks observed red on the head
	// commit.
	FailingRequiredChecks []string `json:"failing_required_checks,omitempty"`
	// RequiredChecksAllReported records that the known required set is
	// non-empty and every check in it has a check run on the head commit
	// (failing or not). Only then does an empty FailingRequiredChecks prove
	// that nothing required is red: a known-empty set enforces nothing, and a
	// required context reported as a commit status is invisible to the
	// check-run listing. PullRequest.OnlyNonRequiredChecksFailing reads it.
	RequiredChecksAllReported bool `json:"required_checks_all_reported,omitempty"`
	// MissingRequiredChecks are required checks for which NO check run exists
	// on the head commit. It is only ever populated when at least one OTHER
	// required check WAS observed as a check run: on a repository that
	// reports its required contexts as commit statuses rather than check
	// runs, every required context looks absent from a check-run listing, and
	// "required check X has not reported" would be confidently wrong for all
	// of them. See protectionCollector.attach.
	MissingRequiredChecks []string `json:"missing_required_checks,omitempty"`
	// CodeOwnerReviewRequired records that the base branch requires a code
	// owner's review — a ruleset pull_request rule's require_code_owner_review
	// or classic protection's require_code_owner_reviews (hivecommons/hive#12953).
	// It is only looked up for a "blocked" PR whose ReviewDecision is none:
	// with required_approving_review_count 0 GitHub reports a null
	// reviewDecision even while it waits on a code owner.
	CodeOwnerReviewRequired bool `json:"code_owner_review_required,omitempty"`
	// CodeOwnerRuleSource names the rule behind CodeOwnerReviewRequired, e.g.
	// `ruleset "protect main"` or "classic branch protection".
	CodeOwnerRuleSource string `json:"code_owner_rule_source,omitempty"`
}

// BranchProtectionBlockReason names, in one place, the branch-protection rule
// that GitHub's "blocked" mergeable_state is hiding (hivecommons/hive#7515).
//
// This is THE state → explanation mapping for the blocked case: the dashboard
// pill's tooltip renders whatever this returns, so the wording lives in Go,
// under test, and never in JS string concatenation.
//
// ok is false when the facts on hand do not identify a rule. The caller must
// then say so honestly rather than guess — a confidently wrong explanation
// sends the operator to fix something that is not broken, which is worse than
// admitting we do not know. Order is by what the operator acts on first: an
// explicit "changes requested" outranks a red check, which outranks a check
// that never ran, which outranks a merely missing approval.
func (p PullRequest) BranchProtectionBlockReason() (string, bool) {
	f := p.Protection
	if f == nil {
		return "", false
	}
	if f.ReviewDecision == ReviewDecisionChangesRequested {
		if who := mentionList(f.ChangesRequestedBy); who != "" {
			return "changes requested by " + who, true
		}
		return "a reviewer requested changes", true
	}
	if f.RequiredChecksKnown {
		if names := quotedList(f.FailingRequiredChecks); names != "" {
			return "required " + checkNoun(f.FailingRequiredChecks) + " " + names + " " + isAre(f.FailingRequiredChecks) + " failing", true
		}
		if names := quotedList(f.MissingRequiredChecks); names != "" {
			return "required " + checkNoun(f.MissingRequiredChecks) + " " + names + " " + hasHave(f.MissingRequiredChecks) + " not reported", true
		}
	}
	if f.ReviewDecision == ReviewDecisionReviewRequired {
		return "an approving review is required by branch protection (" +
			strconv.Itoa(f.ApprovalsGiven) + " given)", true
	}
	if reason, ok := p.codeOwnerReviewBlockReason(); ok {
		return reason, true
	}
	return "", false
}

// codeOwnerReviewBlockReason names an outstanding code-owner review
// (hivecommons/hive#12953). GitHub reports reviewDecision null for a PR that
// waits on a code owner when the rule asks for no other approvals, so the
// review decision alone reads such a PR as unblocked. Hive does not match
// CODEOWNERS itself: GitHub already did, and said "blocked". When the base
// branch requires code-owner review, no review decision was given and the
// required checks are green, that review is what GitHub is waiting on.
func (p PullRequest) codeOwnerReviewBlockReason() (string, bool) {
	f := p.Protection
	if f == nil || !f.CodeOwnerReviewRequired || f.ReviewDecision != ReviewDecisionNone {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(p.MergeableState), "blocked") {
		return "", false
	}
	if !p.requiredChecksGreen() {
		return "", false
	}
	source := strings.TrimSpace(f.CodeOwnerRuleSource)
	if source == "" {
		source = "branch protection"
	}
	// GitHub auto-requests the code owner, so a requested reviewer or team is
	// what makes "code-owner review" the honest name. Without one the block is
	// still a block, but its cause is some branch rule Hive does not read.
	if len(p.RequestedReviewers)+len(p.RequestedTeams) == 0 {
		return "blocked by a branch rule Hive does not read (" + source + " requires code-owner review)", true
	}
	return "code-owner review required by " + source, true
}

// requiredChecksGreen reports that no required check explains GitHub's
// "blocked": every check passed, or the required set is known and none of it
// is red, unreported or (as far as CIStatus tells) still running.
func (p PullRequest) requiredChecksGreen() bool {
	if p.CIStatus == "success" {
		return true
	}
	f := p.Protection
	if f == nil || !f.RequiredChecksKnown || p.CIStatus == "pending" {
		return false
	}
	return len(f.FailingRequiredChecks) == 0 && len(f.MissingRequiredChecks) == 0
}

// HumanReviewBlocksMerge reports whether a person's review decision keeps the
// PR from merging: GitHub's reviewDecision is CHANGES_REQUESTED or
// REVIEW_REQUIRED and the protection facts name the rule, or the base branch
// requires a code owner's review that GitHub is still waiting on
// (hivecommons/hive#12953). reason is that rule. The merge-eligibility classifier skips such a PR and the dashboard's
// your-turn decision hands it to a person; both read this one predicate so the
// merge verdict and the marker agree (hivecommons/hive#12378).
func (p PullRequest) HumanReviewBlocksMerge() (reason string, blocks bool) {
	if p.Protection == nil {
		return "", false
	}
	switch p.Protection.ReviewDecision {
	case ReviewDecisionChangesRequested, ReviewDecisionReviewRequired:
		return p.BranchProtectionBlockReason()
	case ReviewDecisionNone:
		return p.codeOwnerReviewBlockReason()
	}
	return "", false
}

// mentionList renders logins as "@a, @b", skipping blanks and duplicates.
func mentionList(logins []string) string {
	seen := make(map[string]bool, len(logins))
	out := make([]string, 0, len(logins))
	for _, l := range logins {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "@"))
		if l == "" || seen[strings.ToLower(l)] {
			continue
		}
		seen[strings.ToLower(l)] = true
		out = append(out, "@"+l)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// quotedList renders check names as `"build", "lint"` in a stable order.
func quotedList(names []string) string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, strconv.Quote(n))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func checkNoun(names []string) string {
	if countDistinct(names) == 1 {
		return "check"
	}
	return "checks"
}

func isAre(names []string) string {
	if countDistinct(names) == 1 {
		return "is"
	}
	return "are"
}

func hasHave(names []string) string {
	if countDistinct(names) == 1 {
		return "has"
	}
	return "have"
}

func countDistinct(names []string) int {
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			seen[n] = true
		}
	}
	return len(seen)
}
