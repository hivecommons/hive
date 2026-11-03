package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The Waiting breakdown names the cause from the facts the ownership decision
// used: a hiding hold (rule 1), then an open linked PR (rule 3), else the
// reporter (part D1 of hivecommons/hive#11507).
func TestOverviewWaitingCauseFollowsOwnershipFacts(t *testing.T) {
	openPR := []github.IssueLinkedPR{{Number: 90, State: "open"}}
	status := &StatusPayload{HiveID: "hive-test", Repos: []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		ActionableIssues: []any{
			github.Issue{Repo: "hivecommons/hive", Number: 1, Title: "covered", LinkedPRs: openPR, CreatedAt: ownershipNow11565, UpdatedAt: ownershipNow11565},
		},
		HeldIssues: []any{
			// No recorded hold source hides nothing, so the open linked PR decides.
			github.Issue{Repo: "hivecommons/hive", Number: 2, Title: "held, no hiding source", Labels: []string{"hold"}, LinkedPRs: openPR, CreatedAt: ownershipNow11565, UpdatedAt: ownershipNow11565},
			github.HoldItem{Repo: "hivecommons/hive", Number: 3, Type: "issue", Labels: []string{"hold"}, HoldSources: []string{github.HoldSourcePerson}, CreatedAt: ownershipNow11565},
			// A Hive-policy hold hides nothing; the reporter label decides (rule 6).
			github.HoldItem{Repo: "hivecommons/hive", Number: 4, Type: "issue", Labels: []string{"hold", "needs-reporter-confirmation"}, HoldSources: []string{github.HoldSourceSHAHold}, CreatedAt: ownershipNow11565},
		},
		WorkBreakdown: &github.RepoWorkBreakdown{Issues: github.RepoIssueBreakdown{Actionable: 1, Hold: 3}},
	}}}
	decideRepoOwnership(status.Repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))

	part := overviewPartitionOf(status, "")
	if part.issues.waiting != 4 {
		t.Fatalf("waiting = %d, want 4 (%+v)", part.issues.waiting, part.issues)
	}
	if got := part.issues.waitingByCause; got[waitingCauseCovered] != 2 || got[waitingCauseHold] != 1 || got[waitingCauseReporter] != 1 {
		t.Fatalf("waitingByCause = %+v, want covered 2, hold 1, reporter 1", got)
	}
	const reporterHref = "https://example.test/reporter-search"
	for _, row := range part.issues.waitingBreakdown(reporterHref) {
		switch row.Key {
		case "waiting-on-reporter":
			if row.Count != 1 || row.DocsHref != reporterHref {
				t.Fatalf("reporter row = %+v", row)
			}
		case "waiting-covered", "waiting-hold":
			if row.DocsHref == reporterHref {
				t.Fatalf("%s row carries the reporter search link: %+v", row.Key, row)
			}
		default:
			t.Fatalf("unexpected waiting row %+v", row)
		}
	}
	sum := 0
	for _, n := range part.issues.waitingByCause {
		sum += n
	}
	if sum != part.issues.waiting {
		t.Fatalf("cause counts sum to %d, waiting term is %d", sum, part.issues.waiting)
	}
}
