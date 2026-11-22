package automerge

// Human-merge paths and the workflow-path fence's owner-approval release.
//
// With auto_merge.contributor_lane.owner_approval_releases_workflow_fence on,
// the 085 note on a workflow-path PR says a trusted merger's Approve review
// releases the fence and the hive merges it. That is false for a head that
// also touches auto_merge.human_merge_paths: once the approval releases the
// fence the lane reaches its human-merge-path gate, which adds
// needs-owner-merge and never merges. Under workflow_fence_scope "human_merge_paths" every fenced
// workflow edit is such a head. The person was asked for an approval that
// could never lead to a merge, and the PR then sat at 085 with no step left.
//
// The human routing therefore treats a head that touches a human-merge path
// (and is not one the pin-only exception might admit) as a person's merge
// from the start: no approval ask, the human-merge-path cause, and 090 once
// the reviewer approved it, CI is green and the soak is over. The label the
// hive set for the human-merge-path refusal reads the same way (lanes stop
// on it as auto_merge.human_merge_labels, a breaking-change reason, before
// their gate re-checks the files). The rule itself
// (humanMergePathGate), the pin-only exception and Sentinel are unchanged:
// this only changes what the stage says.
//
// The evidence costs no extra read in the steady state. The contributor lane
// already lists every head's complete changed-file list for its diff fence
// and records the match here (noteHumanMergePathFenceHead); the stage pass's
// own diff-fence read (headDiffFence) records it the same way when the lane
// stopped earlier (a label, a sentinel alert). The hive-set label's
// provenance (which the lift pass keeps current with the head) and the
// lane's own human-merge-path reason need no files at all. Every match uses
// intent.HumanMergePathMatches, the matcher the gate uses.

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// humanMergePathFenceEntry is what one PR head's complete changed-file list
// showed about auto_merge.human_merge_paths, and under which configuration.
type humanMergePathFenceEntry struct {
	head string
	// config is the repository's human_merge_paths and pin-only exception
	// the entry was judged under: a reload re-judges the head.
	config string
	// personMerged: a changed file matches human_merge_paths and the pin-only
	// exception cannot admit the PR, so only a person merges this head.
	personMerged bool
}

// humanMergePathFenceConfig keys an entry to the repository's
// human_merge_paths and pin-only exception.
func (c *Engine) humanMergePathFenceConfig(repo string, patterns []string) string {
	policy, ok := c.currentHumanMergePinOnly(repo)
	return strings.Join(patterns, "\x00") + "\x01" + fmt.Sprintf("%t%+v", ok, policy)
}

// noteHumanMergePathFenceHead records whether pr's head, whose complete
// changed-file list is files, is a person's merge under the repository's
// human_merge_paths. Nothing is recorded for a repository with no paths.
// It reads nothing from GitHub: a PR the pin-only exception might admit
// (humanMergePinOnlyCandidate) is recorded as not person-merged, so its
// route stays as it was and the gate decides.
func (c *Engine) noteHumanMergePathFenceHead(tracker *StageTracker, key, owner, repo, head string, pr *gh.PullRequest, files []intent.ChangedFile) {
	head = strings.TrimSpace(head)
	if c == nil || tracker == nil || head == "" || pr == nil {
		return
	}
	full := owner + "/" + repo
	patterns := c.currentHumanMergePaths(full)
	if len(patterns) == 0 {
		return
	}
	matches := intent.HumanMergePathMatches(files, patterns)
	personMerged := len(matches) > 0
	if personMerged {
		if _, _, candidate, _ := c.humanMergePinOnlyCandidate(owner, repo, pr, files, matches); candidate {
			personMerged = false
		}
	}
	entry := humanMergePathFenceEntry{head: head, config: c.humanMergePathFenceConfig(full, patterns), personMerged: personMerged}
	tracker.mu.Lock()
	if tracker.humanMergePathFence == nil {
		tracker.humanMergePathFence = map[string]humanMergePathFenceEntry{}
	}
	tracker.humanMergePathFence[key] = entry
	tracker.mu.Unlock()
}

// humanMergePathFenceHead reports what the recorded file list of obs's head
// showed. known is false when nothing is recorded for this head under the
// current configuration; a repository with no human_merge_paths is known and
// never person-merged.
func (c *Engine) humanMergePathFenceHead(tracker *StageTracker, key string, obs stageObservation) (personMerged, known bool) {
	if c == nil || tracker == nil {
		return false, false
	}
	full := obs.Owner + "/" + obs.Repo
	patterns := c.currentHumanMergePaths(full)
	if len(patterns) == 0 {
		return false, true
	}
	head := strings.TrimSpace(obs.HeadSHA)
	if head == "" {
		return false, false
	}
	tracker.mu.Lock()
	entry, ok := tracker.humanMergePathFence[key]
	tracker.mu.Unlock()
	if !ok || !strings.EqualFold(entry.head, head) || entry.config != c.humanMergePathFenceConfig(full, patterns) {
		return false, false
	}
	return entry.personMerged, true
}

// humanMergePathFencePersonMerge reports whether obs's head is one only a
// person merges because it touches auto_merge.human_merge_paths: the hive
// set its needs-owner-merge label for that refusal (obs.HumanMergePathLabel,
// which the lift pass drops once the head no longer touches the path), a
// lane refused it for that reason, or the
// contributor lane's diff for this head matches a path the pin-only exception
// cannot admit. A contributor head whose files no pass has recorded yet is
// read once, through the cached diff-fence read. Anything it cannot establish
// answers false, which keeps the route as it was.
func (c *Engine) humanMergePathFencePersonMerge(ctx context.Context, tracker *StageTracker, key string, obs stageObservation) bool {
	if c == nil || strings.TrimSpace(obs.HeadSHA) == "" {
		return false
	}
	if obs.HumanMergePathLabel || (obs.Hold.SelfImposed() && obs.Hold.HumanMergePath()) {
		return true
	}
	if obs.Reason == autoMergeReasonHumanMergePath || obs.LaneReason == autoMergeReasonHumanMergePath {
		return true
	}
	if obs.Lane != stageLaneContributor || tracker == nil {
		return false
	}
	if personMerged, known := c.humanMergePathFenceHead(tracker, key, obs); known {
		return personMerged
	}
	if _, ok := c.headDiffFence(ctx, tracker, key, obs); !ok {
		return false
	}
	personMerged, _ := c.humanMergePathFenceHead(tracker, key, obs)
	return personMerged
}

// applyHumanMergePathFenceRoute rewrites the evidence of a head
// humanMergePathFencePersonMerge established as a person's merge. No
// approval releases anything, so the 085 actions never ask for one.
//
// A hold-class label (a person's `hold`, `do-not-merge/*`, a hold whose
// source is unknown) is that person's to lift: the route stays as it was,
// without the approval ask. The human-merge-path rule no longer holds a PR;
// a `hold` it applied before (with its legacy notice, not yet migrated)
// still reads as the rule.
//
// Otherwise the head is a manual merge. Where the hand-off is about the
// workflow fence, the human-merge path itself (the lane's reason, or the
// breaking-change reason the hive's own needs-owner-merge label stopped it
// with), or a gate riding the head's real progress (a sentinel alert), the
// cause is the human-merge path, a rule no approval overrides, and the PR is
// routed like a breaking-change PR: no decision is owed, so once the head is reviewed,
// green and soaked it is at 090 for a person's merge. A requires_human
// verdict nobody has answered still asks for its decision at 085. A
// hand-off for another decision (a sentinel alert with a requires_human
// verdict, needs-human, a reporter-trust hold, …) keeps its own route and
// wording.
//
// ownerApproval is true on a repository listed in
// auto_merge.human_merge_owner_approval (and the head not yet approved):
// there a trusted merger's approval of the head does release the refusal,
// so instead of the manual-merge rule the route asks for that approval at
// 085 (ApprovalReleasesPathHold).
func applyHumanMergePathFenceRoute(e *stagelabels.Evidence, obs stageObservation, ownerApproval bool) {
	e.ApprovalReleasesFence = false
	// Only a legacy `hold` the rule applied (before the boot migration
	// swaps it) reads as the rule; any other hold-class label is a person's.
	legacyHold := obs.Hold.SelfImposed() && obs.Hold.HumanMergePath()
	for _, h := range e.Holds {
		if h != "hold" || !legacyHold {
			return
		}
	}
	e.ManualMerge = true
	switch e.CauseReason {
	case "", prstage.CauseWorkflowPathManual, prstage.CauseWorkflowPathAwaitingApproval, prstage.CauseHumanMergePath:
	case prstage.CauseHumanMergeRequired:
		// The lane stopped on needs-owner-merge; the hive's own label
		// means the human-merge path, not a breaking change.
		if !obs.HumanMergePathLabel {
			return
		}
	default:
		return
	}
	owns := obs.Reason == contributorLaneReasonWorkflowPath || obs.Reason == autoMergeReasonHumanMergePath ||
		(obs.HumanMergePathLabel && obs.Reason == autoMergeReasonHumanMergeRequired)
	if e.Cause == stagelabels.CauseDecision && !owns {
		return
	}
	e.LaneReason = autoMergeReasonHumanMergePath
	if ownerApproval {
		e.CauseReason = prstage.CauseHumanMergePathAwaitingApproval
		e.ApprovalReleasesPathHold = true
		e.Cause = stagelabels.CauseDecision
		return
	}
	e.CauseReason = prstage.CauseHumanMergePath
	e.ManualMergeRule = true
	if e.Cause == stagelabels.CauseDecision && (e.Verdict != stagelabels.VerdictRequiresHuman || strings.TrimSpace(e.HumanApprover) != "") {
		e.Cause = stagelabels.CauseTrust
	}
}
