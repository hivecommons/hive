package dashboard

import (
	"fmt"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// covering_pr_stalled fixtures. The covering PR lives in another repository
// of its own so its stage record is keyed independently of the issue.
const coveringStalledRepo = "example/testrepo"

func coveringStalledURL(n int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", coveringStalledRepo, n)
}

// abandonedCoveringStore is a stage store holding repo#n parked at an
// adoption-stalled 085 for nine days, flagged abandoned when flagged is set
// (what the stage pass does once governor.labels.stage.abandon_after is
// past; unflagged is an active PR, one below the threshold, or a hive with
// the setting off).
func abandonedCoveringStore(t *testing.T, repo string, n int, flagged bool) *stagelabels.Store {
	t.Helper()
	at := time.Now().Add(-(9*24*time.Hour + time.Hour))
	st := stagelabels.NewStore("", func() time.Time { return at })
	key := stagelabels.Key(repo, n)
	st.ObserveHead(key, "aaa", stagelabels.NeedsDecision, stagelabels.LaneReasonAdoptionStalledPrefix+"check-failing:unit")
	st.NoteActivity(key, stagelabels.Activity{Head: "aaa", Comments: 1, Reviews: 0})
	if flagged && !st.MarkAbandoned(key, at) {
		t.Fatal("fixture: MarkAbandoned refused")
	}
	return st
}

func coveringClaim(issue int, prRepo string, pr int, merged bool) func(string, int) (ghpkg.IssueClaim, bool) {
	return func(_ string, number int) (ghpkg.IssueClaim, bool) {
		if number != issue {
			return ghpkg.IssueClaim{}, false
		}
		c := ghpkg.IssueClaim{
			PRNumber: pr, PRRepo: prRepo, PRURL: coveringStalledURL(pr), PRAuthor: "contributor",
			ExternalAuthor: true,
		}
		if merged {
			at := time.Now().Add(-(ghpkg.SettledClaimStaleAfter + 24*time.Hour))
			c.MergedPR, c.MergedAt, c.ObservedAt, c.FirstObservedAt = true, at, at, at
		}
		return c, true
	}
}

func coveringLink(issue int, prRepo string, pr int) func(string, int, []string) (ghpkg.OpenPRLink, bool) {
	return func(_ string, number int, _ []string) (ghpkg.OpenPRLink, bool) {
		if number != issue {
			return ghpkg.OpenPRLink{}, false
		}
		return ghpkg.OpenPRLink{Issue: issue, PRRepo: prRepo, PRNumber: pr, PRURL: coveringStalledURL(pr), PRAuthor: "contributor"}, true
	}
}

// withheldReasonFor601 runs the shared ladder and returns #601's reason,
// asserting the issue stays out of the queue and unassigned either way.
func withheldReasonFor601(t *testing.T, hub *ContributeWSHub) AdmissionWithheldItem {
	t.Helper()
	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)
	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	item, ok := findWithheld(snap.withheld, withheldKey601)
	if !ok {
		t.Fatalf("#601 not withheld; collected %v", snap.withheld)
	}
	return item
}

// An active covering PR, or one below the threshold (no flag on record),
// keeps the reasons the lock had before: open_pr_claim from the ledger,
// open_pr_linked from the live guard.
func TestCoveringPRStalled_ActiveCoveringPRKeepsOpenPRReasons(t *testing.T) {
	t.Run("ledger claim", func(t *testing.T) {
		hub, s := withheldHub(t, nil)
		hub.stageStore = abandonedCoveringStore(t, coveringStalledRepo, 4242, false)
		s.deps.IssueClaimed = coveringClaim(601, coveringStalledRepo, 4242, false)
		if got := withheldReasonFor601(t, hub).Reason; got != contributorAdmissionReasonOpenPRClaim {
			t.Fatalf("reason = %q, want %q", got, contributorAdmissionReasonOpenPRClaim)
		}
	})
	t.Run("live link", func(t *testing.T) {
		hub, s := withheldHub(t, nil)
		hub.stageStore = abandonedCoveringStore(t, coveringStalledRepo, 4243, false)
		s.deps.IssueClaimed = func(string, int) (ghpkg.IssueClaim, bool) { return ghpkg.IssueClaim{}, false }
		s.deps.OpenPRLinkingIssue = coveringLink(601, coveringStalledRepo, 4243)
		if got := withheldReasonFor601(t, hub).Reason; got != contributorAdmissionReasonOpenPRLinked {
			t.Fatalf("reason = %q, want %q", got, contributorAdmissionReasonOpenPRLinked)
		}
	})
	t.Run("no stage record", func(t *testing.T) {
		hub, s := withheldHub(t, nil)
		hub.stageStore = stagelabels.NewStore("", time.Now)
		s.deps.IssueClaimed = coveringClaim(601, coveringStalledRepo, 4242, false)
		if got := withheldReasonFor601(t, hub).Reason; got != contributorAdmissionReasonOpenPRClaim {
			t.Fatalf("reason = %q, want %q", got, contributorAdmissionReasonOpenPRClaim)
		}
	})
}

// A merged claim keeps the #8003 path, whatever the PR's stage record says.
func TestCoveringPRStalled_MergedClaimUnchanged(t *testing.T) {
	hub, s := withheldHub(t, nil)
	hub.stageStore = abandonedCoveringStore(t, coveringStalledRepo, 133, true)
	s.deps.IssueClaimed = coveringClaim(601, coveringStalledRepo, 133, true)
	if got := withheldReasonFor601(t, hub).Reason; got != contributorAdmissionReasonMergedClaimStale {
		t.Fatalf("reason = %q, want %q", got, contributorAdmissionReasonMergedClaimStale)
	}
}

func TestCoveringPRRepoFull(t *testing.T) {
	cases := []struct{ issueRepo, prRepo, want string }{
		{"example/issues", "", "example/issues"},
		{"example/issues", "testrepo", "example/testrepo"},
		{"example/issues", "other/testrepo", "other/testrepo"},
		{"bare", "testrepo", ""},
	}
	for _, c := range cases {
		if got := coveringPRRepoFull(c.issueRepo, c.prRepo); got != c.want {
			t.Errorf("coveringPRRepoFull(%q, %q) = %q, want %q", c.issueRepo, c.prRepo, got, c.want)
		}
	}
}
