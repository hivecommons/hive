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
	deferring.openBlockedBy = 1
	got := decideIssueOwnershipGated(deferring, env)
	if got.YourTurn || !got.WaitingElsewhere || !strings.Contains(got.Reason, "blocked by") {
		t.Fatalf("a decision park blocked by an open issue = %+v, want waiting elsewhere", got)
	}
	// Only a decision defers: a park for a person's own work stays theirs.
	human := nonActionable11565("filtered", config.DefaultNeedsHumanLabel, config.DefaultNeedsHumanLabel)
	human.openBlockedBy = 1
	if got := decideIssueOwnershipGated(human, env); !got.YourTurn {
		t.Fatalf("a needs-human park with a blocker = %+v, want still your turn", got)
	}
}

// End to end from the enumeration rows the dashboard stores.
func TestRepoOwnershipCountsDeferringParkOnce(t *testing.T) {
	now := ownershipNow11565
	row := func(n, blockedBy int) github.RepoWorkIssue {
		return github.RepoWorkIssue{Repo: "Danathar/tududi", Number: n, Labels: []string{config.DefaultNeedsDecisionLabel},
			Bucket: "filtered", Reason: config.DefaultNeedsDecisionLabel, OpenBlockedBy: blockedBy,
			CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	}
	repos := []FrontendRepo{{Name: "tududi", Full: "Danathar/tududi", NonActionableIssues: []any{row(511, 0), row(512, 1)}}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
	if d := ownershipFor(repos[0].Ownership, ownershipKindIssue, 511); d == nil || !d.YourTurn {
		t.Fatalf("#511 = %+v, want your turn", d)
	}
	if d := ownershipFor(repos[0].Ownership, ownershipKindIssue, 512); d == nil || d.YourTurn || !d.WaitingElsewhere {
		t.Fatalf("#512 = %+v, want waiting on #511", d)
	}
}
