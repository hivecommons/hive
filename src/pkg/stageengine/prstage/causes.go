package prstage

import "github.com/hivecommons/hive/pkg/oktotest"

// WorkflowApprovalCommand is the comment the hive posts to have the
// repository's ok-to-test workflow approve held workflow runs.
const WorkflowApprovalCommand = "/ok-to-test"

// Decision causes: the prose the 085 comment gives for a non-trust
// human-only lane reason. The automerge sweep renders them and the stage
// goldens expect them, so the wording lives here once (hivecommons/hive#15384).
const (
	CauseTestsRemoved      = "the PR removes tests, which the lane never merges unattended"
	CauseReporterTrustHold = "the reporter-trust hold is in place"
	CauseHumanMergePath    = "the PR touches a human-merge path (auto_merge.human_merge_paths)"
	// CauseHumanMergeRequired: the breaking-change title or the
	// needs-owner-merge label; the words the 090 chip and gate reuse.
	CauseHumanMergeRequired   = "breaking change — owner merges"
	CauseIntentUnauthorized   = "the merge intent could not be authorized"
	CauseApprovalDeskWithheld = "the approval desk withheld this merge"
	// CauseWorkflowPathManual is the workflow-path fence read as a rule a
	// person merges around.
	CauseWorkflowPathManual = "the PR edits CI workflow / action files, which the lane never merges unattended"
	// CauseWorkflowPathAwaitingApproval is the same fence awaiting the
	// owner's explicit approval (hivecommons/hive#14664).
	CauseWorkflowPathAwaitingApproval = "the PR edits CI workflow / action files: awaiting owner approval — a hive owner / trusted merger's GitHub **Approve** review on the current head (or a dashboard `/hive decision approve`) releases the fence and the hive merges it; approval on an earlier head does not count, so re-approve after a push"
)

// CauseWorkflowApprovalUntrusted is the cause when the repository's
// ok-to-test command belongs to Prow, which does not trust the Hive App.
func CauseWorkflowApprovalUntrusted() string {
	return "GitHub Actions is holding the workflow runs for approval and Hive cannot start them: `" + WorkflowApprovalCommand + "` on this repository belongs to Prow, which does not trust the Hive App. A maintainer must comment `" + WorkflowApprovalCommand + "` (and `/lgtm` + `/approve` for tide), or install `" + oktotest.WorkflowPath + "` (`hivectl ok-to-test-template`) so the repository GITHUB_TOKEN approves trusted same-repo bot runs on Hive's command."
}

// CauseWorkflowApprovalWorkflowMissing is the cause when runs stay held after
// the hive posted the command and the repository has no ok-to-test workflow.
func CauseWorkflowApprovalWorkflowMissing() string {
	return "GitHub Actions is still waiting after Hive posted `" + WorkflowApprovalCommand + "`. Install the self-contained `" + oktotest.WorkflowPath + "` below (or render it with `hivectl ok-to-test-template`) so the repository GITHUB_TOKEN can approve trusted same-repo bot runs, or manually approve the runs from the PR checks banner."
}

// CauseWorkflowApprovalRequired is the cause when runs are held and the hive
// has just posted the command.
func CauseWorkflowApprovalRequired() string {
	return "GitHub Actions is waiting for workflow approval. Hive posted `" + WorkflowApprovalCommand + "`; install `" + oktotest.WorkflowPath + "` (`hivectl ok-to-test-template`) if the runs do not start, or manually approve them from the PR checks banner."
}

// CauseLaneReported is the fallback cause naming an unrecognised lane reason.
func CauseLaneReported(reason string) string {
	return "the lane reported `" + reason + "`"
}
