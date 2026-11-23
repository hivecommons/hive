package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The owner's dashboard approval, bound to the current head by its marker,
// releases requires_human; a stale head, a non-owner, an edited comment or a
// comment not authored by the App does not (hivecommons/hive#19536).
func TestRequiresHumanReleasedByDashboardApproval(t *testing.T) {
	const (
		approvedAt = "2026-10-15T12:00:00Z"
		editedAt   = "2026-10-15T12:05:00Z"
	)
	edited := func(c map[string]any) map[string]any {
		c["created_at"], c["updated_at"] = approvedAt, editedAt
		return c
	}
	forged := func(c map[string]any) map[string]any {
		c["user"] = map[string]string{"login": "passerby"}
		return c
	}
	for _, tt := range []struct {
		name       string
		comment    map[string]any
		wantMerges int
	}{
		{name: "owner marker on current head releases", comment: dashboardApproveComment(5, "clubanderson", "sha7", approvedAt), wantMerges: 1},
		{name: "owner marker on a stale head does not release", comment: dashboardApproveComment(5, "clubanderson", "sha6", approvedAt)},
		{name: "non-owner marker does not release", comment: dashboardApproveComment(5, "passerby", "sha7", approvedAt)},
		{name: "edited marker does not release", comment: edited(dashboardApproveComment(5, "clubanderson", "sha7", ""))},
		{name: "marker not authored by the App does not release", comment: forged(dashboardApproveComment(5, "clubanderson", "sha7", approvedAt))},
		{name: "unmarked dashboard approve does not release", comment: dashboardApproveComment(5, "clubanderson", "", approvedAt)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.prComments = []map[string]any{tt.comment}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{
				ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
					return ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
				},
				ContributorSoak: soakedTracker(clock),
			})
			engine.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "clubanderson") })
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatal(err)
			}
			if fx.merges != tt.wantMerges {
				t.Fatalf("reason=%q merges=%d, want %d merges", reason, fx.merges, tt.wantMerges)
			}
			if tt.wantMerges == 0 && reason == "" {
				t.Fatal("an unreleased requires_human verdict must still hold the PR")
			}
		})
	}
}

// A swarm changes_requested verdict is not overridden by a dashboard
// approval: only requires_human is a question a person answers.
func TestDashboardApprovalDoesNotOverrideChangesRequested(t *testing.T) {
	fx := validContributorFixture()
	fx.prComments = []map[string]any{dashboardApproveComment(5, "clubanderson", "sha7", "2026-10-15T12:00:00Z")}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{
		ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
			return ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}
		},
		ContributorSoak: soakedTracker(clock),
	})
	engine.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "clubanderson") })
	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil {
		t.Fatal(err)
	}
	if reason != contributorLaneReasonChangesRequested || fx.merges != 0 {
		t.Fatalf("reason=%q merges=%d, want %q/0", reason, fx.merges, contributorLaneReasonChangesRequested)
	}
}

func TestDashboardApprovalActor(t *testing.T) {
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	body := func(actor, head string) string {
		b := "/approve\n\nPosted by @" + actor + " from the Hive dashboard; the merge gate decides from here."
		if head != "" {
			b += "\n<!-- hive:pr-approval head=" + head + " by=" + actor + " -->"
		}
		return b
	}
	edited := func(c *gh.IssueComment) *gh.IssueComment {
		c.UpdatedAt = &gh.Timestamp{Time: c.GetCreatedAt().Time.Add(time.Minute)}
		return c
	}
	for _, tc := range []struct {
		name      string
		comment   *gh.IssueComment
		head      string
		wantLogin string
		wantOK    bool
	}{
		{name: "marker on head", comment: newIssueComment(testHiveAppBotLogin, body("clubanderson", "sha7"), at), head: "sha7", wantLogin: "clubanderson", wantOK: true},
		{name: "marker head compared case-insensitively", comment: newIssueComment(testHiveAppBotLogin, body("clubanderson", "SHA7"), at), head: "sha7", wantLogin: "clubanderson", wantOK: true},
		{name: "marker on stale head", comment: newIssueComment(testHiveAppBotLogin, body("clubanderson", "sha6"), at), head: "sha7"},
		{name: "unmarked", comment: newIssueComment(testHiveAppBotLogin, body("clubanderson", ""), at), head: "sha7"},
		{name: "lgtm is not the approval", comment: newIssueComment(testHiveAppBotLogin, strings.Replace(body("clubanderson", "sha7"), "/approve", "/lgtm", 1), at), head: "sha7"},
		{name: "person-authored copy", comment: newIssueComment("passerby", body("clubanderson", "sha7"), at), head: "sha7"},
		{name: "edited App comment", comment: edited(newIssueComment(testHiveAppBotLogin, body("clubanderson", "sha7"), at)), head: "sha7"},
		{name: "empty head", comment: newIssueComment(testHiveAppBotLogin, body("clubanderson", "sha7"), at), head: ""},
		{name: "nil comment", head: "sha7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login, ok := dashboardApprovalActor(tc.comment, tc.head, testHiveAppBotLogin)
			if ok != tc.wantOK || login != tc.wantLogin {
				t.Fatalf("got (%q, %v), want (%q, %v)", login, ok, tc.wantLogin, tc.wantOK)
			}
		})
	}
	if _, ok := dashboardApprovalActor(newIssueComment(testHiveAppBotLogin, body("clubanderson", "sha7"), at), "sha7", ""); ok {
		t.Fatal("an unknown App login must release nothing")
	}
}
