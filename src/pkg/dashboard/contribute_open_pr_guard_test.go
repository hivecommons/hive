package dashboard

import (
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#14804: #14766 already had an open PR (#14775) when the
// contributor lane handed it out again, because the claim ledger had not yet
// scanned that PR. The live open-PR check behind the ledger withholds the
// issue from both selectTask and ReadyQueue on a ledger miss.
func TestClaimedIssue_LiveOpenPRCheckBehindLedger(t *testing.T) {
	for _, tc := range []struct {
		name       string
		live       bool
		wantOffers bool
	}{
		{name: "live open PR withholds the issue the ledger has not seen", live: true, wantOffers: false},
		{name: "no live hook keeps the ledger-only behaviour", live: false, wantOffers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, s := covK2Hub(t)
			claimedIssueStatus(s)
			s.deps.IssueClaimed = func(string, int) (ghpkg.IssueClaim, bool) {
				return ghpkg.IssueClaim{}, false
			}
			var liveRepos []string
			if tc.live {
				s.deps.IssueOpenPRClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
					liveRepos = append(liveRepos, repo)
					if number == 353 {
						return externalClaim(repo, number), true
					}
					return ghpkg.IssueClaim{}, false
				}
			}

			queue := hub.ReadyQueue(readyQueueDefaultLimit)
			offered := false
			for _, item := range queue {
				if item.Number == 353 {
					offered = true
				}
			}
			if offered != tc.wantOffers {
				t.Fatalf("ReadyQueue offers #353 = %v, want %v (queue %+v)", offered, tc.wantOffers, queue)
			}

			msg := hub.selectTask(claimedIssueConn())
			if msg == nil || msg.Type != "task_assign" {
				t.Fatalf("selectTask = %+v, want a task_assign", msg)
			}
			if !tc.wantOffers && msg.Number != 360 {
				t.Fatalf("selectTask assigned #%d, want #360 (the issue without an open PR)", msg.Number)
			}
			for _, repo := range liveRepos {
				if repo != "projectbluefin/dakota" {
					t.Fatalf("live check asked for %q, want the org-qualified repo", repo)
				}
			}
		})
	}
}
