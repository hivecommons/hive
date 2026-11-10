package automerge

// Workflow fence scope (auto_merge.contributor_lane.workflow_fence_scope).
//
// The contributor lane's workflow-path fence keeps a PR that edits CI
// workflow / action definitions (contributorLaneWorkflowPaths) from merging
// without a hive owner's approval of its head (automerge_contributor_workflow_approval.go).
// With the default scope, "all", every such edit trips it. With
// "human_merge_paths" only an edit to a file that also matches the
// repository's auto_merge.human_merge_paths does, so a routine pin bump or
// permission change in a workflow the operator did not list goes through
// review, CI and soak like any other change. The operator already lists the
// release-critical workflows there for the human-merge hold; this reuses the
// list and its matcher (intent.HumanMergePathMatches) rather than adding a
// second one.
//
// Fail closed: a repository with no human_merge_paths entry keeps the full
// fence, as does a renamed workflow / action file (the files API reports the
// new name only, so a listed file renamed away would otherwise escape) and a
// head the App could not merge for want of `workflows: write`.
//
// Only the MERGE fence is scoped. Two guards stay on every workflow edit:
//   - automerge_contributor_held_runs.go never approves held fork Actions
//     runs for a diff that touches CI definitions (contributorLaneTouchesWorkflowFiles):
//     that decides what code runs in CI, not what merges.
//   - adoption never publishes a workflow edit as an upstream branch
//     (adoptionDiffRefusal, hivecommons/hive#14055): pull_request workflows
//     there run with repository context before any merge gate.
// The tests-removed fence, Sentinel, the human-merge hold and the pin-only
// exception are untouched.
//
// A repository can override the hive-wide scope with
// project.repo_policies[].workflow_fence_scope; the policy's
// WorkflowFenceScopeForRepo resolves it per pull request, so every reader
// below sees the PR's own repository's scope.
//
// Every merge-side reader (the lane, the stage engine's explanations via
// headDiffFence, the merge asks via headManualMergeRule, `/hive approve`)
// goes through contributorLaneMergeFence, so they cannot disagree.

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

// workflowFenceScopedToHumanMergePaths reports whether the policy limits the
// workflow-path fence to human_merge_paths for repo ("owner/repo"): the
// repository's project.repo_policies[].workflow_fence_scope when
// WorkflowFenceScopeForRepo resolves one, else WorkflowFenceScope. Anything
// but that exact value (any case) is the full fence.
func (p ContributorLanePolicy) workflowFenceScopedToHumanMergePaths(repo string) bool {
	scope := p.WorkflowFenceScope
	if p.WorkflowFenceScopeForRepo != nil {
		scope = p.WorkflowFenceScopeForRepo(repo)
	}
	return strings.EqualFold(strings.TrimSpace(scope), config.ContributorLaneWorkflowFenceScopeHumanMergePaths)
}

// contributorLaneWorkflowFiles returns the changed files that are CI
// workflow / action definitions (contributorLaneWorkflowPaths).
func contributorLaneWorkflowFiles(files []intent.ChangedFile) []intent.ChangedFile {
	var out []intent.ChangedFile
	for _, f := range files {
		if intent.PathMatchesAny(f.Filename, contributorLaneWorkflowPaths) {
			out = append(out, f)
		}
	}
	return out
}

// contributorLaneTouchesWorkflowFiles reports whether the diff edits any CI
// workflow / action definition, whatever the fence scope. Execution guards
// (held-run approval) use it; merge decisions use contributorLaneMergeFence.
func contributorLaneTouchesWorkflowFiles(files []intent.ChangedFile) bool {
	return len(contributorLaneWorkflowFiles(files)) > 0
}

// workflowFenceCovers reports whether the workflow-path fence applies to
// this head's workflow / action edits under policy's scope: always for the
// full fence; under human_merge_paths when the repository lists no paths,
// when a workflow / action file was renamed, when the App was refused this
// head's merge for want of `workflows: write`, or when a workflow / action
// file matches the repository's human_merge_paths.
func (c *Engine) workflowFenceCovers(policy ContributorLanePolicy, owner, repo string, number int, headSHA string, files []intent.ChangedFile) bool {
	if !policy.workflowFenceScopedToHumanMergePaths(owner + "/" + repo) {
		return true
	}
	patterns := c.currentHumanMergePaths(owner + "/" + repo)
	if len(patterns) == 0 {
		return true
	}
	if c.unauthorizedStep(owner, repo, number, headSHA) == unauthorizedStepMergeWorkflowPath {
		return true
	}
	workflow := contributorLaneWorkflowFiles(files)
	for _, f := range workflow {
		if strings.EqualFold(strings.TrimSpace(f.Status), "renamed") {
			return true
		}
	}
	return len(intent.HumanMergePathMatches(workflow, patterns)) > 0
}

// contributorLaneMergeFence is the contributor lane's merge diff fence for a
// head: contributorLaneDiffReason with the workflow-path verdict scoped by
// auto_merge.contributor_lane.workflow_fence_scope. A workflow edit outside
// the scope falls through to the tests-removed check, so test deletion
// still stops the merge. It returns "" when the diff may merge unattended.
func (c *Engine) contributorLaneMergeFence(policy ContributorLanePolicy, owner, repo string, number int, headSHA string, files []intent.ChangedFile) string {
	reason := contributorLaneDiffReason(files)
	if reason != contributorLaneReasonWorkflowPath || c.workflowFenceCovers(policy, owner, repo, number, headSHA, files) {
		return reason
	}
	return contributorLaneTestsRemovedReason(files)
}

// workflowFenceScopeKey names the inputs contributorLaneMergeFence reads
// besides the diff, so a cached verdict for a head (headDiffFence) is
// re-read when the scope, the repository's human_merge_paths or the
// head's workflows-permission refusal changes.
func (c *Engine) workflowFenceScopeKey(policy ContributorLanePolicy, owner, repo string, number int, headSHA string) string {
	if !policy.workflowFenceScopedToHumanMergePaths(owner + "/" + repo) {
		return config.ContributorLaneWorkflowFenceScopeAll
	}
	key := config.ContributorLaneWorkflowFenceScopeHumanMergePaths + "\x00" + strings.Join(c.currentHumanMergePaths(owner+"/"+repo), "\x00")
	if c.unauthorizedStep(owner, repo, number, headSHA) == unauthorizedStepMergeWorkflowPath {
		key += "\x00unauthorized"
	}
	return key
}

// workflowMergeRefusedOnRecord reports whether an earlier pass already
// learnt that the App cannot merge this head's workflow edits (the durable
// hive:unauthorized merge-workflow-path comment covers it), re-arming the
// in-memory mark so the head stays fenced without another merge attempt.
// The lane asks only for a workflow edit the scope waived; the fence's own
// release path re-arms the mark the same way (workflowFenceReleased). An
// unreadable comment list keeps the fence (fail closed) without marking.
func (c *Engine) workflowMergeRefusedOnRecord(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA string) bool {
	if c == nil || c.gh == nil {
		return true
	}
	if c.unauthorizedStep(owner, repo, number, headSHA) == unauthorizedStepMergeWorkflowPath {
		return true
	}
	comments, err := c.listIssueComments(hgithub.WithRESTCaller(ctx, workflowApprovalRESTCaller), owner, repo, number)
	if err != nil {
		c.warn("workflow fence scope: could not read comments; keeping the fence", "repo", displayRepo, "pr", number, "head", headSHA, "error", err)
		return true
	}
	if !unauthorizedCommentCoversHead(comments, unauthorizedStepMergeWorkflowPath, headSHA) {
		return false
	}
	c.markUnauthorized(ctx, displayRepo, owner, repo, pr, number, headSHA, unauthorizedStepMergeWorkflowPath, unauthorizedPermissionWorkflowsWrite, mergeWorkflowPathHumanAction())
	return true
}
