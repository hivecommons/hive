package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Danathar/openaudible-autoupdate#177 (architect) parked on options A/B/C
// while hive PR #176, "Closes #177" and green, carried the answer. It stayed
// in the owner's inbox until a maintainer posted a decision. A decision park
// an open hive PR closes waits on that PR.
func TestRepoOwnershipParkClosedByHivePRWaitsOnPR(t *testing.T) {
	now := ownershipNow11565
	issue := func(n int, park string) github.RepoWorkIssue {
		return github.RepoWorkIssue{Repo: "Danathar/openaudible-autoupdate", Number: n, Labels: []string{park},
			Bucket: "filtered", Reason: park, CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	}
	pr := func(n int, app bool, closes ...github.PRLinkedIssue) github.PullRequest {
		return github.PullRequest{Repo: "Danathar/openaudible-autoupdate", Number: n, AppAuthored: app, LinkedIssues: closes,
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	}
	repos := []FrontendRepo{{Name: "openaudible-autoupdate", Full: "Danathar/openaudible-autoupdate",
		NonActionableIssues: []any{
			issue(177, config.DefaultNeedsDecisionLabel), // closed by hive PR #176
			issue(173, config.DefaultNeedsDecisionLabel), // only an outside PR closes it
			issue(174, config.DefaultNeedsDecisionLabel), // the hive PR only refers to it
			issue(175, config.DefaultNeedsHumanLabel),    // a person's park, closed by #176 too
			issue(178, config.DefaultNeedsDecisionLabel), // a same-numbered issue in another repo
		},
		OpenPrs: []any{
			pr(176, true, github.PRLinkedIssue{Number: 177}, github.PRLinkedIssue{Number: 175}, github.PRLinkedIssue{Number: 174, Via: github.PRLinkViaRefs}),
			pr(180, false, github.PRLinkedIssue{Number: 173}),
		},
		HeldPrs: []any{pr(181, true, github.PRLinkedIssue{Number: 178, Repo: "Danathar/other"})},
	}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
	d := ownershipFor(repos[0].Ownership, ownershipKindIssue, 177)
	if d == nil || d.YourTurn || !d.WaitingElsewhere || !strings.Contains(d.Reason, "#176") {
		t.Fatalf("#177 = %+v, want waiting on hive PR #176", d)
	}
	for _, n := range []int{173, 174, 175, 178} {
		if d := ownershipFor(repos[0].Ownership, ownershipKindIssue, n); d == nil || !d.YourTurn {
			t.Errorf("#%d = %+v, want still your turn", n, d)
		}
	}
}

// A held hive PR counts too: it is still open and still closes the issue.
func TestHiveClosingPRForReadsHeldPRs(t *testing.T) {
	repo := FrontendRepo{HeldPrs: []any{github.PullRequest{Number: 9, AppAuthored: true, LinkedIssues: []github.PRLinkedIssue{{Number: 4}}}}}
	if got := hiveClosingPRFor(repo, "o/r", 4); got != 9 {
		t.Fatalf("hiveClosingPRFor = %d, want 9", got)
	}
	if got := hiveClosingPRFor(repo, "o/r", 5); got != 0 {
		t.Fatalf("hiveClosingPRFor(other issue) = %d, want 0", got)
	}
}
