package dashboard

import (
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/bluefin-bling#128: the PR sat at 085 from 11-08, a
// maintainer reviewed it on 11-11, and "Let hive decide" on 11-12 read the
// review's updated_at bump as the author's clanker picking the rework up.
// The author had been offline for a day; the PR went back to them and
// nothing followed. Only a fix task the author's relay holds, or a new head,
// is a pickup.
func TestDelegatedReworkPickedUpIgnoresAnyonesActivity(t *testing.T) {
	entered := time.Date(2026, 11, 8, 20, 44, 0, 0, time.UTC)
	reviewed := time.Date(2026, 11, 11, 15, 3, 21, 0, time.UTC)
	decided := time.Date(2026, 11, 12, 19, 16, 34, 0, time.UTC)
	const repo, author = "projectbluefin/bluefin-bling", "mendezr"
	store := stagelabels.NewStore("", func() time.Time { return entered })
	store.ObserveHead(stagelabels.Key(repo, 128), "bbd5a8d", stagelabels.NeedsDecision, "adoption-stalled:hive-signoff-attribution")
	rec, _ := store.Get(stagelabels.Key(repo, 128))
	pr := &gh.PullRequest{Number: gh.Ptr(128), Head: &gh.PullRequestBranch{SHA: gh.Ptr("bbd5a8d")}, UpdatedAt: &gh.Timestamp{Time: reviewed}}

	// The tracker does read the bump as "acknowledged"…
	it := StageTrackerItem{Repo: repo, Kind: "pr", Number: 128, Head: "bbd5a8d", UpdatedAt: &reviewed}
	if rw := stageTrackerReworkFor(it, author, stagelabels.ReturnedToContributor, rec, true, nil, decided); rw == nil || rw.State != reworkStateAcknowledged {
		t.Fatalf("rework = %+v, want the activity-only acknowledged state this test is about", rw)
	}
	// …but the delegate does not take it for the author's pickup.
	s := &Server{stageStore: store}
	if s.delegatedReworkPickedUp(repo, pr, author, decided) {
		t.Fatal("a maintainer's review read as the offline author picking the rework up")
	}

	// The author's relay holding the fix task is a pickup.
	s.fixWorkLookup = func(login, r string, number int) (StageTrackerFixWork, bool) {
		return StageTrackerFixWork{InFlight: true, DispatchedAt: decided.Add(-time.Hour)}, login == author && r == repo && number == 128
	}
	if !s.delegatedReworkPickedUp(repo, pr, author, decided) {
		t.Fatal("an in-flight fix task held by the author's relay is not a pickup")
	}
	// A task only queued on the hub, never taken, is not.
	s.fixWorkLookup = func(string, string, int) (StageTrackerFixWork, bool) {
		return StageTrackerFixWork{Queued: true, QueuedAt: decided.Add(-time.Hour)}, true
	}
	if s.delegatedReworkPickedUp(repo, pr, author, decided) {
		t.Fatal("a queued, untaken fix task read as a pickup")
	}
}
