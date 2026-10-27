package automerge

// Held workflow runs on registry-trusted contributor PRs (#14519).
//
// GitHub's fork-approval policy holds a first-time contributor's
// `pull_request` runs in `action_required`; nothing starts them until a person
// clicks "Approve and run". automerge_held_runs.go deliberately never nudges a
// human-authored lane. The one exception is here: an author the operator
// placed in this hive's contributor registry at a tier the lane accepts for
// MERGING (require_tiers) is trusted to land the change, so holding their CI
// is pure drag (hivecommons/docs#481 sat 17h). The lane approves those runs
// once per head with the App token when, and only when, the registry path
// admitted the author and the lane's own CI reason is
// `required-check-missing:<ctx>`. Claim-marker, auto-promotion and
// association admissions stay untouched.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

const (
	// contributorHeldRunsApprovedMarkerPrefix tags the one audit comment the
	// lane posts after approving a head's held runs.
	contributorHeldRunsApprovedMarkerPrefix = "<!-- hive:contributor-held-runs-approved"
	// contributorHeldRunsApprovalKeyPrefix namespaces the once-per-head
	// bookkeeping inside Engine.heldRunApprovals.
	contributorHeldRunsApprovalKeyPrefix = "registry-held-approve:"
)

// approveHeldRunsForRegistryTrusted approves the `action_required` workflow
// runs for a contributor-lane head once, when the author was admitted by the
// registry path and the lane stopped at reason (`required-check-missing:<ctx>`).
// It refuses for any other admission path, when the toggle is off, when the
// PR carries the sentinel or an exclude label, or when the diff touches CI
// definitions. It returns the run ids it approved (nil when it did nothing)
// and the lane reason to report: the input reason, or
// `unauthorized:approve-workflow-runs` while the App lacks `actions: write`
// for this head (hivecommons/hive#14589) — GitHub answers 403 "Resource not
// accessible by integration", the PR is marked via markUnauthorized, and the
// mark holds until the head's required checks exist or the head changes.
func (c *Engine) approveHeldRunsForRegistryTrusted(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA string, claim contributorClaimMatch, policy ContributorLanePolicy, files []intent.ChangedFile, reason string) ([]int64, string) {
	if c == nil || c.gh == nil || pr == nil || headSHA == "" {
		return nil, reason
	}
	if !policy.ApproveHeldRunsForRegistryTrusted || !claim.admittedByRegistry() {
		return nil, reason
	}
	if !strings.HasPrefix(reason, requiredCheckMissingPrefix) {
		return nil, reason
	}
	labels := labelNames(pr.Labels)
	if c.sentinelLabelPresent(labels) || policy.excludedLabel(labels) != "" || c.labelBlockReason(labels, false) != "" {
		return nil, reason
	}
	if contributorLaneDiffReason(files) == contributorLaneReasonWorkflowPath {
		return nil, reason
	}
	if c.contributorHeldRunsApprovalAttempted(owner, repo, number, headSHA) {
		// Already tried this head; keep reporting the permission gap while
		// it stands, without another API call or another log line.
		if step := c.unauthorizedStep(owner, repo, number, headSHA); step == unauthorizedStepApproveWorkflowRuns {
			return nil, unauthorizedReason(step)
		}
		return nil, reason
	}
	missing := strings.TrimPrefix(reason, requiredCheckMissingPrefix)
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(hgithub.WithRESTCaller(ctx, "hive:contributor_lane_automerge_sweep"), owner, repo, &gh.ListWorkflowRunsOptions{
		HeadSHA:     headSHA,
		Event:       "pull_request",
		ListOptions: gh.ListOptions{PerPage: workflowRunsPageSize},
	})
	if err != nil || runs == nil {
		c.warn("contributor-lane could not list workflow runs for held-run approval", "repo", displayRepo, "pr", number, "head", headSHA, "error", err)
		return nil, reason
	}
	held := heldWorkflowRunsForHead(runs.WorkflowRuns, contributorLaneHeadRepo(pr, owner, repo))
	if len(held) == 0 {
		return nil, reason
	}
	// Mark before calling the API: a partial failure must not be retried
	// every tick against the same head.
	if !c.markContributorHeldRunsApprovalAttempted(owner, repo, number, headSHA) {
		return nil, reason
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	var approved, forbidden []int64
	for _, run := range held {
		if err := c.approveWorkflowRun(ctx, owner, repo, run.GetID()); err != nil {
			if isResourceNotAccessible(err) {
				forbidden = append(forbidden, run.GetID())
				continue
			}
			c.warn("contributor-lane could not approve held workflow run", "repo", displayRepo, "pr", number, "head", headSHA, "author", author, "tier", claim.RegistryTier, "run_id", run.GetID(), "workflow", run.GetName(), "context", missing, "error", err)
			continue
		}
		approved = append(approved, run.GetID())
	}
	if len(forbidden) > 0 && len(approved) == 0 {
		sort.Slice(forbidden, func(i, j int) bool { return forbidden[i] < forbidden[j] })
		return nil, c.markUnauthorized(ctx, displayRepo, owner, repo, pr, number, headSHA, unauthorizedStepApproveWorkflowRuns, unauthorizedPermissionActionsWrite, approveWorkflowRunsHumanAction(owner, repo, missing, forbidden))
	}
	if len(approved) == 0 {
		return nil, reason
	}
	sort.Slice(approved, func(i, j int) bool { return approved[i] < approved[j] })
	c.info("contributor-lane approved held runs", "repo", displayRepo, "pr", number, "head", headSHA, "author", author, "tier", claim.RegistryTier, "run_ids", approved, "context", missing)
	if err := c.commentContributorHeldRunsApproved(ctx, owner, repo, number, headSHA, author, claim.RegistryTier, approved); err != nil {
		c.warn("contributor-lane held-run approval audit comment failed", "repo", displayRepo, "pr", number, "head", headSHA, "error", err)
	}
	return approved, reason
}

// approveWorkflowRun is POST /repos/{owner}/{repo}/actions/runs/{run_id}/approve.
// go-github v72 has no binding for it.
func (c *Engine) approveWorkflowRun(ctx context.Context, owner, repo string, runID int64) error {
	path := fmt.Sprintf("repos/%s/%s/actions/runs/%d/approve", owner, repo, runID)
	req, err := c.gh.NewRequest(http.MethodPost, path, nil)
	if err != nil {
		return err
	}
	_, err = c.gh.Do(hgithub.WithRESTCaller(ctx, "hive:contributor_lane_automerge_sweep"), req, nil)
	return err
}

// contributorLaneHeadRepo is the full name of the PR head's repository
// (the fork for a fork PR), falling back to the base repository.
func contributorLaneHeadRepo(pr *gh.PullRequest, owner, repo string) string {
	if pr.GetHead() != nil && pr.GetHead().GetRepo() != nil {
		if name := strings.TrimSpace(pr.GetHead().GetRepo().GetFullName()); name != "" {
			return name
		}
	}
	return owner + "/" + repo
}

// heldWorkflowRunsForHead returns the runs waiting on approval whose head
// repository is headRepo. Unlike heldSameRepoWorkflowRuns the expected head
// repository may be a fork: the PR author's registry tier, not the head's
// origin, is what admits the approval.
func heldWorkflowRunsForHead(runs []*gh.WorkflowRun, headRepo string) []*gh.WorkflowRun {
	var held []*gh.WorkflowRun
	for _, run := range runs {
		if run == nil || !strings.EqualFold(run.GetConclusion(), workflowRunConclusionActionRequired) {
			continue
		}
		if run.GetHeadRepository() == nil || !strings.EqualFold(run.GetHeadRepository().GetFullName(), headRepo) {
			continue
		}
		held = append(held, run)
	}
	return held
}

func contributorHeldRunsApprovalKey(prKey, headSHA string) string {
	return contributorHeldRunsApprovalKeyPrefix + prKey + "@" + strings.ToLower(headSHA)
}

// contributorHeldRunsApprovalAttempted reports whether this process already
// approved (or tried to approve) the held runs for this PR head.
func (c *Engine) contributorHeldRunsApprovalAttempted(owner, repo string, number int, headSHA string) bool {
	c.heldRunApprovalsMu.Lock()
	defer c.heldRunApprovalsMu.Unlock()
	return c.heldRunApprovals[contributorHeldRunsApprovalKey(c.evaluatedHeadKey(owner, repo, number), headSHA)]
}

// markContributorHeldRunsApprovalAttempted records the attempt for this PR
// head and reports whether it is the first.
func (c *Engine) markContributorHeldRunsApprovalAttempted(owner, repo string, number int, headSHA string) bool {
	key := contributorHeldRunsApprovalKey(c.evaluatedHeadKey(owner, repo, number), headSHA)
	c.heldRunApprovalsMu.Lock()
	defer c.heldRunApprovalsMu.Unlock()
	if c.heldRunApprovals == nil {
		c.heldRunApprovals = make(map[string]bool)
	}
	if c.heldRunApprovals[key] {
		return false
	}
	c.heldRunApprovals[key] = true
	return true
}

// contributorHeldRunsApprovedComment is the audit comment body; the marker
// line carries the head so a reader can tell which push it covered.
func contributorHeldRunsApprovedComment(headSHA, author, tier string, runIDs []int64) string {
	ids := make([]string, 0, len(runIDs))
	for _, id := range runIDs {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	author = strings.TrimPrefix(author, "@")
	return fmt.Sprintf("%s head=%s author=%s tier=%s runs=%s -->\nHive approved the workflow runs GitHub was holding for approval on this head (run %s) because @%s is registry-trusted at tier `%s`, which the contributor lane accepts for unattended merge. CI, reviewer verdict, diff fences, soak, holds and branch protection still apply before anything merges.",
		contributorHeldRunsApprovedMarkerPrefix, strings.ToLower(headSHA), author, tier, strings.Join(ids, ","), strings.Join(ids, ", "), author, tier)
}

// commentContributorHeldRunsApproved posts the audit comment once per head:
// an existing marker for this head on the PR, or a prior effects claim,
// suppresses it.
func (c *Engine) commentContributorHeldRunsApproved(ctx context.Context, owner, repo string, number int, headSHA, author, tier string, runIDs []int64) error {
	if c == nil || c.gh == nil || number <= 0 {
		return nil
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	marker := contributorHeldRunsApprovedMarkerPrefix + " head=" + strings.ToLower(headSHA)
	for _, comment := range comments {
		if comment != nil && strings.HasPrefix(comment.GetBody(), marker) {
			return nil
		}
	}
	body := contributorHeldRunsApprovedComment(headSHA, author, tier, runIDs)
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(contributorHeldRunsApprovedMarkerPrefix)},
	}, func(ctx context.Context) (effects.Result, error) {
		comment, _, apiErr := c.gh.Issues.CreateComment(hgithub.WithRESTCaller(ctx, "hive:contributor_lane_automerge_sweep"), owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		c.noteOwnStageComment(owner, repo, number)
		return effects.Result{Provenance: comment.GetHTMLURL()}, nil
	})
	return err
}
