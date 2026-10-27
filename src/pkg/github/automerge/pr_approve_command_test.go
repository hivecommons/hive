package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestPRApproveReply(t *testing.T) {
	for _, tc := range []struct {
		name, author, actor, verdict, file, body, want string
		bot, disabled, readError, trustedAuthor        bool
	}{
		{name: "App lane", author: testHiveAppBotLogin, actor: "owner", want: "does not consume it on a pull request"},
		{name: "trusted owner on own contributor PR", author: "owner", actor: "owner", want: "does not consume it on a pull request"},
		{name: "trusted merger on contributor PR", author: "contributor", actor: "owner", want: "does not consume it on a pull request"},
		{name: "trusted-author lane", author: "maintainer", actor: "owner", trustedAuthor: true, want: "does not consume it on a pull request"},
		{name: "untrusted person on trusted-author lane", author: "maintainer", actor: "stranger", trustedAuthor: true, want: "not a trusted merger"},
		{name: "untrusted person on App lane", author: testHiveAppBotLogin, actor: "stranger", want: "not a trusted merger"},
		{name: "names the release lever", author: "contributor", actor: "owner", want: "GitHub **Approve** review for the current head (`sha7`)"},
		{name: "untrusted self release", author: "contributor", actor: "contributor", want: "cannot release their own PR"},
		{name: "untrusted other release", author: "contributor", actor: "stranger", want: "not a trusted merger"},
		{name: "bot", author: "contributor", actor: "owner", bot: true, want: "a bot cannot release"},
		{name: "rejected contributor", author: "contributor", actor: "owner", verdict: "reject", want: "cannot override this contributor-lane verdict"},
		{name: "manual workflow", author: "contributor", actor: "owner", file: ".github/workflows/ci.yml", want: "requires a person to merge manually"},
		{name: "manual path", author: testHiveAppBotLogin, actor: "owner", file: "sensitive/config.yaml", want: "requires a person to merge manually"},
		{name: "carried fence on App lane", author: testHiveAppBotLogin, actor: "owner", body: CarriedFenceMarker(contributorLaneReasonTestsRemoved, 9), want: "carries a diff fence from the contributor PR it supersedes"},
		{name: "disabled lane", author: "contributor", actor: "owner", disabled: true, want: "must merge it manually"},
		{name: "missing path evidence", author: "contributor", actor: "owner", readError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := tc.verdict
			if verdict == "" {
				verdict = "requires_human"
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("feedback must not mutate: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				switch r.URL.Path {
				case "/repos/acme/widget/pulls/7":
					json.NewEncoder(w).Encode(&gh.PullRequest{Number: gh.Ptr(7), State: gh.Ptr("open"), Body: gh.Ptr(tc.body), User: &gh.User{Login: gh.Ptr(tc.author)}, Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha7")}})
				case "/repos/acme/widget/pulls/7/files":
					if tc.readError {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					file := tc.file
					if file == "" {
						file = "src/main.go"
					}
					json.NewEncoder(w).Encode([]*gh.CommitFile{{Filename: gh.Ptr(file), Status: gh.Ptr("modified")}})
				default:
					t.Errorf("unexpected read: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			client.SetAppBotLogin(testHiveAppBotLogin)
			e := New(client, Options{
				MergerAuthorizer:      func(login string) bool { return login == "owner" },
				TrustedAuthorPolicy:   func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: tc.trustedAuthor} },
				TrustedAuthorizer:     func(string, string) TrustedAuthorDecision { return TrustedAuthorDecision{Allowed: tc.trustedAuthor} },
				ContributorLanePolicy: func() ContributorLanePolicy { return ContributorLanePolicy{Enabled: true} },
				ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
					return ContributorLaneVerdict{Configured: true, Verdict: verdict}
				},
				HumanMergePaths:      func(string) []string { return []string{"sensitive/**"} },
				RepoAutoMergeEnabled: func(string) bool { return !tc.disabled },
			})
			userType := "User"
			if tc.bot {
				userType = "Bot"
			}
			reply, err := e.PRApproveReply(context.Background(), "acme/widget", 7, &gh.IssueComment{Body: gh.Ptr("/hive approve"), User: &gh.User{Login: gh.Ptr(tc.actor), Type: gh.Ptr(userType)}})
			if tc.readError {
				if err == nil || reply != "" {
					t.Fatalf("failed evidence: reply=%q err=%v", reply, err)
				}
				return
			}
			if err != nil || !strings.Contains(reply, tc.want) {
				t.Fatalf("reply=%q err=%v, want %q", reply, err, tc.want)
			}
		})
	}
}

func TestDecisionCommentNamesLaneAction(t *testing.T) {
	for _, tc := range []struct {
		name, lane, reason, want string
		holds, labels            []string
		approver                 string
		manual                   bool
	}{
		{name: "self", lane: stageLaneSelf, want: "trusted merger can submit a GitHub **Approve** review"},
		{name: "contributor", lane: stageLaneContributor, want: "ordinary contributor cannot release their own PR"},
		{name: "trusted author", lane: stageLaneTrusted, want: "`/hive approve` replies on a PR are acknowledged but not consumed"},
		{name: "workflow", lane: stageLaneContributor, reason: contributorLaneReasonWorkflowPath, want: "must click **Merge** on GitHub", manual: true},
		{name: "tests removed", lane: stageLaneContributor, reason: contributorLaneReasonTestsRemoved, want: "must click **Merge** on GitHub", manual: true},
		{name: "human path", lane: stageLaneSelf, reason: autoMergeReasonHumanMergePath, want: "must click **Merge** on GitHub", manual: true},
		{name: "workflow, approver named", lane: stageLaneContributor, reason: contributorLaneReasonWorkflowPath, approver: "octocat", want: "@octocat has approved this head; the only step left is the merge", manual: true},
		{name: "label hold", lane: stageLaneSelf, holds: []string{"hold"}, want: "remove that label"},
		{name: "label needs-human reason", lane: stageLaneTrusted, reason: "label:needs-human", want: "Answer the `needs-human` ask"},
		{name: "label needs-human on the PR", lane: stageLaneTrusted, labels: []string{"Needs-Human"}, want: "Answer the `needs-human` ask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := stagelabels.Evidence{Verdict: stagelabels.VerdictRequiresHuman, Holds: tc.holds, HumanApprover: tc.approver, Sentinel: tc.approver != ""}
			route := stagelabels.RouteHuman(e)
			route.Actions, route.ManualMerge = decisionActions(stageObservation{Lane: tc.lane, Reason: tc.reason, Labels: tc.labels}, e)
			body := stagelabels.DecisionComment(route)
			if !strings.Contains(body, tc.want) || !strings.Contains(body, "**How to release it:**") || route.ManualMerge != tc.manual {
				t.Fatalf("manual=%v body=%q", route.ManualMerge, body)
			}
			if tc.manual && strings.Contains(body, "not a merge") {
				t.Fatalf("contradictory manual guidance: %q", body)
			}
		})
	}
}
