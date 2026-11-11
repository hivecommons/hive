package automerge

import (
	"testing"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestBlockedPRRefusedByEveryLane: a PR wearing `blocked` (the hive's hold
// on its Depends-on targets, or a person's) is refused by the shared label
// gate on every merge path — the self-authored and trusted-bot lane, the
// trusted-author lane, the contributor lane, the queued (merge-request)
// lane — is never seated on a merge train, and loses an App queue entry.
// Before, only the contributor lane's default exclude list named it.
func TestBlockedPRRefusedByEveryLane(t *testing.T) {
	c := newPrefilterEngine(t)
	ready := stagelabels.ReadyForHumanMerge.Label()
	withLabels := func(login string, labels ...string) *gh.PullRequest {
		return selfAuthoredListPR(func(pr *gh.PullRequest) {
			pr.User = &gh.User{Login: gh.Ptr(login)}
			pr.AuthorAssociation = gh.Ptr("CONTRIBUTOR")
			for _, l := range labels {
				pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(l)})
			}
		})
	}

	// Self-authored lane (the App's own PR; trusted bots share its prefilter).
	if got := c.prefilterSelfAuthoredPR(withLabels(testHiveAppBotLogin, ready, "blocked")); got != autoMergeReasonLabelBlocked {
		t.Fatalf("self-authored: prefilter = %q, want %q", got, autoMergeReasonLabelBlocked)
	}
	if got := c.prefilterSelfAuthoredPR(withLabels(testHiveAppBotLogin, "Blocked")); got != autoMergeReasonLabelBlocked {
		t.Fatalf("self-authored, label case: prefilter = %q, want %q", got, autoMergeReasonLabelBlocked)
	}
	// Trusted-author lane, with the default exclude list (which has no
	// `blocked`): the shared gate refuses it anyway.
	if got := c.prefilterTrustedAuthorPR("acme", "widget", withLabels("alice", ready, "blocked"), TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}); got != autoMergeReasonLabelBlocked {
		t.Fatalf("trusted author: prefilter = %q, want %q", got, autoMergeReasonLabelBlocked)
	}
	// Contributor lane, even with an exclude list that leaves `blocked` out.
	policy := defaultContributorLanePolicy()
	delete(policy.ExcludeLabels, "blocked")
	if got := c.prefilterContributorLanePR(withLabels("mallory", ready, "blocked"), policy); got != autoMergeReasonLabelBlocked {
		t.Fatalf("contributor: prefilter = %q, want %q", got, autoMergeReasonLabelBlocked)
	}
	// Queued (merge-request) lane listing.
	if got := c.prefilterQueuedIssue(queuedListIssue(hgithub.AutoMergeQueuedLabel, "blocked"), hgithub.AutoMergeQueuedLabel); got != autoMergeReasonLabelBlocked {
		t.Fatalf("queued: prefilter = %q, want %q", got, autoMergeReasonLabelBlocked)
	}
	// A hold outranks it: a PR wearing both still reads as held.
	if got := c.prefilterSelfAuthoredPR(withLabels(testHiveAppBotLogin, "hold", "blocked")); got != "label:hold" {
		t.Fatalf("hold and blocked: prefilter = %q, want label:hold", got)
	}
	// Merge train and native queue.
	if mergeTrainCandidate(autoMergeReasonLabelBlocked) {
		t.Fatal("a blocked PR must not be seated on a merge train")
	}
	if !nativeQueueWithdrawReason(autoMergeReasonLabelBlocked) {
		t.Fatal("a blocked PR must lose the App's native queue entry")
	}
	// Without the label nothing changes.
	if got := c.prefilterSelfAuthoredPR(withLabels(testHiveAppBotLogin, ready)); got != "" {
		t.Fatalf("plain hive PR: prefilter = %q, want it to pass", got)
	}
}

// TestBlockedGateClauseNamesTheTarget: the stage route's gate clause names
// the pull request the blocked sweep read from the PR's marker, and falls
// back to the bare label.
func TestBlockedGateClauseNamesTheTarget(t *testing.T) {
	if got := blockedGateClause("acme/widget", 9101); got != "the `blocked` label is set" {
		t.Fatalf("no marker read: %q", got)
	}
	t.Cleanup(hgithub.NoteBlockedOnSeenForTest("acme/widget", 9101, "acme/gadget", 5))
	if got := blockedGateClause("acme/widget", 9101); got != "blocked on acme/gadget#5" {
		t.Fatalf("marker read: %q", got)
	}
	e := stagelabels.Evidence{Blocked: blockedGateClause("acme/widget", 9101)}
	gates := stagelabels.GateReasons(e)
	if len(gates) != 1 || gates[0] != "blocked on acme/gadget#5" {
		t.Fatalf("gates = %v", gates)
	}
	if decisionCauseFor(autoMergeReasonLabelBlocked, false) != "" {
		t.Fatal("label:blocked is a gate, not a decision cause")
	}
}
