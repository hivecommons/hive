package dashboard

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// A PR that only says `Refs #N` and has merged landed part of the work and
// left the issue open for the rest. On projectbluefin the contribute queue
// kept such issues as "already claimed by a PR" (common#1156 after
// chairlift#633, utah#755 after common#1486, utah-packages#496 after
// utah-packages#631), so nobody was offered the remainder. These tests wire a
// real ClaimLedger in as Dependencies.IssueClaimed, as the hive does.

func mergedRefIssueHub(t *testing.T, live ...ghpkg.IssueClaim) (*ContributeWSHub, *Server) {
	t.Helper()
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)
	ledger := ghpkg.NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), s.logger)
	ledger.Reconcile(live, true)
	s.deps.IssueClaimed = ledger.Lookup
	return hub, s
}

func dakotaClaim(pr int, mutate func(*ghpkg.IssueClaim)) ghpkg.IssueClaim {
	c := ghpkg.IssueClaim{
		Repo: "projectbluefin/dakota", Issue: 353,
		PRNumber: pr, PRRepo: "projectbluefin/dakota",
		PRURL:      "https://github.com/projectbluefin/dakota/pull/" + strconv.Itoa(pr),
		PRAuthor:   "hivecommons-hive[bot]",
		ObservedAt: time.Now(),
	}
	mutate(&c)
	return c
}

// firstOffered reports the issue ReadyQueue lists first and the one selectTask
// assigns, which must agree.
func firstOffered(t *testing.T, hub *ContributeWSHub) int {
	t.Helper()
	queue := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) == 0 {
		t.Fatal("ReadyQueue offered nothing")
	}
	msg := hub.selectTask(claimedIssueConn())
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("selectTask = %+v, want a task_assign", msg)
	}
	if msg.Number != queue[0].Number {
		t.Fatalf("selectTask assigned #%d but ReadyQueue lists #%d first", msg.Number, queue[0].Number)
	}
	return msg.Number
}

func TestMergedReferenceClaim_ContributeAdmission(t *testing.T) {
	merged := time.Now().Add(-2 * time.Hour)
	cases := []struct {
		name  string
		claim ghpkg.IssueClaim
		want  int
	}{
		{
			// An open PR is someone on the issue now: withheld (#3768, #3980).
			name:  "open Refs PR holds the issue",
			claim: dakotaClaim(400, func(c *ghpkg.IssueClaim) { c.Reference = true }),
			want:  360,
		},
		{
			// A merged closing keyword closes the issue; until the enumeration
			// sees it closed (or for a cross-repo close GitHub could not act
			// on), it stays withheld exactly as before (#6867, #8003).
			name: "merged closing PR still holds the issue",
			claim: dakotaClaim(401, func(c *ghpkg.IssueClaim) {
				c.Closing, c.MergedPR, c.MergedAt = true, true, merged
			}),
			want: 360,
		},
		{
			name: "merged Refs PR returns the issue to the queue",
			claim: dakotaClaim(402, func(c *ghpkg.IssueClaim) {
				c.Reference, c.MergedPR, c.MergedAt = true, true, merged
			}),
			want: 353,
		},
		{
			// The common#1156 shape: a person's PR in another repo.
			name: "cross-repo merged Refs PR returns the issue to the queue",
			claim: dakotaClaim(633, func(c *ghpkg.IssueClaim) {
				c.PRRepo = "projectbluefin/chairlift"
				c.PRURL = "https://github.com/projectbluefin/chairlift/pull/633"
				c.PRAuthor, c.ExternalAuthor = "thatguywob", true
				c.Reference, c.MergedPR, c.MergedAt = true, true, merged
			}),
			want: 353,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, _ := mergedRefIssueHub(t, tc.claim)
			if got := firstOffered(t, hub); got != tc.want {
				t.Fatalf("offered #%d, want #%d", got, tc.want)
			}
		})
	}
}

// TestMergedReferenceClaim_DoesNotHideAnOpenPR: a person's open `Refs #N` PR
// on the same issue still holds it. The ledger keeps one claim per issue, and
// before this a merged hive-authored `Refs #N` outranked it, so releasing the
// merged claim would have offered an issue someone is working on.
func TestMergedReferenceClaim_DoesNotHideAnOpenPR(t *testing.T) {
	hiveMerged := dakotaClaim(631, func(c *ghpkg.IssueClaim) {
		c.Reference, c.MergedPR, c.MergedAt = true, true, time.Now().Add(-time.Hour)
	})
	personOpen := dakotaClaim(640, func(c *ghpkg.IssueClaim) {
		c.PRAuthor, c.ExternalAuthor, c.Reference = "arteeh", true, true
	})
	for _, live := range [][]ghpkg.IssueClaim{{hiveMerged, personOpen}, {personOpen, hiveMerged}} {
		hub, _ := mergedRefIssueHub(t, live...)
		if got := firstOffered(t, hub); got != 360 {
			t.Fatalf("scan order #%d,#%d: offered #%d, want #360 while PR #640 is open",
				live[0].PRNumber, live[1].PRNumber, got)
		}
	}
}

// TestAdmissionSkipLoggedOnce: selectTask runs for every relay every 20-40s,
// and the projectbluefin hive logged the same "already claimed" line for each
// withheld issue on every pass. A refusal is logged when it first appears and
// when it changes, not on repeat.
func TestAdmissionSkipLoggedOnce(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)
	var buf bytes.Buffer
	hub.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	claim, claimed := dakotaClaim(400, func(*ghpkg.IssueClaim) {}), true
	s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
		if number == 353 && claimed {
			return claim, true
		}
		return ghpkg.IssueClaim{}, false
	}
	skips := func() int { return strings.Count(buf.String(), "skip: issue already claimed by a PR") }
	pass := func() *WSMessage { return hub.selectTask(claimedIssueConn()) }

	for i := 0; i < 3; i++ {
		pass()
	}
	if got := skips(); got != 1 {
		t.Fatalf("three passes over the same claim logged %d skips, want 1", got)
	}

	claim = dakotaClaim(401, func(*ghpkg.IssueClaim) {})
	pass()
	pass()
	if got := skips(); got != 2 {
		t.Fatalf("a different claiming PR should log once more: got %d skips, want 2", got)
	}

	// Admitted, then withheld again: a new episode, logged again.
	claimed = false
	if msg := pass(); msg == nil || msg.Number != 353 {
		t.Fatalf("with no claim #353 should be offered, got %+v", msg)
	}
	claimed = true
	pass()
	if got := skips(); got != 3 {
		t.Fatalf("withholding again after admission should log: got %d skips, want 3", got)
	}
}
