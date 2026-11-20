package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// governor.labels.stage.abandon_after: a contributor PR the idle clock
// flagged abandoned is a CLOSE in its own "Stale: close or take over" group.
func TestAbandonedPRIsACloseInItsOwnGroup(t *testing.T) {
	env := ownershipEnv11565(6)
	env.abandonedPR = func(github.PullRequest) (string, bool) {
		return abandonedOwnershipText("abandoned 9d: check-failing:unit"), true
	}
	got := decidePROwnership(personPR11565("stage/085-needs-decision"), false, env)
	checkOwnership11565(t, got, want11565{OwnershipVerbClose, OwnershipReasonAbandonedRecommendedClose})
	if !strings.Contains(got.Reason, "Stale: close or take over") || !strings.Contains(got.Reason, "abandoned 9d: check-failing:unit") {
		t.Fatalf("reason = %q", got.Reason)
	}
	if ownershipReasonVerbs[OwnershipReasonAbandonedRecommendedClose] != OwnershipVerbClose {
		t.Fatal("abandoned_recommended_close must map to CLOSE")
	}

	// A held PR is decided by its hold: a person is already deciding.
	held := personPR11565("stage/085-needs-decision", "hold")
	held.HoldSources = []string{github.HoldSourcePerson}
	if got := decidePROwnership(held, true, env); got.ReasonCode == OwnershipReasonAbandonedRecommendedClose {
		t.Fatalf("a held PR read as abandoned: %+v", got)
	}
	// The hive's own PRs never take the reason.
	if got := decidePROwnership(hivePR11565("stage/085-needs-decision"), false, env); got.ReasonCode == OwnershipReasonAbandonedRecommendedClose {
		t.Fatalf("a hive PR read as abandoned: %+v", got)
	}
	// No flag (setting off, below threshold, or activity cleared it): the
	// row is decided exactly as before.
	env.abandonedPR = func(github.PullRequest) (string, bool) { return "", false }
	if got := decidePROwnership(personPR11565("stage/085-needs-decision"), false, env); got.ReasonCode == OwnershipReasonAbandonedRecommendedClose {
		t.Fatalf("an unflagged PR read as abandoned: %+v", got)
	}
}

// abandonedPRFor reads the stage store's flag for the listed PR, with the
// repository qualified by the org.
func TestAbandonedPRForReadsTheStageStore(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	parked := now.Add(-9*24*time.Hour - time.Hour)
	clock := parked
	st := stagelabels.NewStore("", func() time.Time { return clock })
	flagged := stagelabels.Key("example/testrepo", 7)
	quiet := stagelabels.Key("example/testrepo", 8)
	for _, key := range []string{flagged, quiet} {
		st.ObserveHead(key, "aaa", stagelabels.NeedsDecision, stagelabels.LaneReasonAdoptionStalledPrefix+"check-failing:unit")
		st.NoteActivity(key, stagelabels.Activity{Head: "aaa", Comments: 0, Reviews: 0})
	}
	clock = now
	st.MarkAbandoned(flagged, now)

	prev := adoptedPushStore
	adoptedPushStore = func() *stagelabels.Store { return st }
	t.Cleanup(func() { adoptedPushStore = prev })

	read := abandonedPRFor("example", now)
	text, ok := read(github.PullRequest{Repo: "testrepo", Number: 7})
	if !ok || text != "Stale: close or take over — abandoned 9d: check-failing:unit" {
		t.Fatalf("flagged PR: %q %v", text, ok)
	}
	if _, ok := read(github.PullRequest{Repo: "example/testrepo", Number: 8}); ok {
		t.Fatal("an unflagged PR read as abandoned")
	}
	if _, ok := read(github.PullRequest{Repo: "example/testrepo", Number: 99}); ok {
		t.Fatal("an unknown PR read as abandoned")
	}
}
