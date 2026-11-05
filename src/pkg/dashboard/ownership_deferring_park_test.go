package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Danathar/tududi#512 parked "Answer on #511; the same choice applies here"
// and sat in the inbox as its own ask beside #511. Linked "blocked by #511",
// it waits on #511's answer: one owner ask for the pair.
func TestDeferringParkIsNotItsOwnAsk(t *testing.T) {
	env := ownershipEnv11565(5)
	parked := nonActionable11565("filtered", config.DefaultNeedsDecisionLabel, config.DefaultNeedsDecisionLabel)
	if got := decideIssueOwnershipGated(parked, env); !got.YourTurn || got.ReasonCode != OwnershipReasonParkedNeedsDecision {
		t.Fatalf("a plain decision park = %+v, want your turn to decide", got)
	}
	deferring := parked
	deferring.decisionBlocker = "#511"
	got := decideIssueOwnershipGated(deferring, env)
	if got.YourTurn || !got.WaitingElsewhere || !strings.Contains(got.Reason, "decision on #511") {
		t.Fatalf("a decision park blocked by another decision park = %+v, want waiting on #511", got)
	}
	// Only a decision defers: a park for a person's own work stays theirs.
	human := nonActionable11565("filtered", config.DefaultNeedsHumanLabel, config.DefaultNeedsHumanLabel)
	human.decisionBlocker = "#511"
	if got := decideIssueOwnershipGated(human, env); !got.YourTurn {
		t.Fatalf("a needs-human park with a blocker = %+v, want still your turn", got)
	}
}

// End to end from the enumeration rows the dashboard stores. Only a blocker
// that is itself a decision park takes the ask out of the inbox: a decision
// blocked by ordinary work, or by a link to an issue outside the snapshot,
// is still the owner's to make (hive reviewer on hivecommons/hive#16694).
func TestRepoOwnershipCountsDeferringParkOnce(t *testing.T) {
	now := ownershipNow11565
	row := func(repo string, n int, label string, blockers ...string) github.RepoWorkIssue {
		return github.RepoWorkIssue{Repo: repo, Number: n, Labels: []string{label},
			Bucket: "filtered", Reason: label, OpenBlockedBy: len(blockers), OpenBlockers: blockers,
			CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	}
	const tu, other = "Danathar/tududi", "Danathar/other"
	repos := []FrontendRepo{
		{Name: "tududi", Full: tu, NonActionableIssues: []any{
			row(tu, 511, config.DefaultNeedsDecisionLabel),
			row(tu, 512, config.DefaultNeedsDecisionLabel, "Danathar/tududi#511"),
			row(tu, 528, config.DefaultNeedsDecisionLabel, "danathar/TUDUDI#511"),
			row(tu, 530, config.DefaultNeedsDecisionLabel, "Danathar/tududi#600"), // ordinary open work
			row(tu, 531, config.DefaultNeedsDecisionLabel, "Danathar/tududi#540"), // blocked by a needs-human park
			row(tu, 540, config.DefaultNeedsHumanLabel),
			row(tu, 532, config.DefaultNeedsDecisionLabel, "Danathar/other#7"),
		}},
		{Name: "other", Full: other, NonActionableIssues: []any{row(other, 7, config.DefaultNeedsDecisionLabel)}},
	}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
	want := map[int]string{511: "", 512: "#511", 528: "#511", 530: "", 531: "", 532: "Danathar/other#7"}
	for n, blocker := range want {
		d := ownershipFor(repos[0].Ownership, ownershipKindIssue, n)
		if blocker == "" {
			if d == nil || !d.YourTurn {
				t.Errorf("#%d = %+v, want your turn", n, d)
			}
			continue
		}
		if d == nil || d.YourTurn || !d.WaitingElsewhere || !strings.HasSuffix(d.Reason, "decision on "+blocker) {
			t.Errorf("#%d = %+v, want waiting on the decision on %s", n, d, blocker)
		}
	}
}
