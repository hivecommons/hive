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
)

func TestPRApproveReply(t *testing.T) {
	for _, tc := range []struct {
		name, author, actor, verdict, file, want string
		labels                                   []string
		bot, disabled, readError, trustedAuthor  bool
	}{
		{name: "says no needs-human blocks", author: testHiveAppBotLogin, actor: "owner", want: "No `needs-human` label blocks this PR."},
		{name: "says needs-human still blocks", author: testHiveAppBotLogin, actor: "owner", labels: []string{"needs-human"}, want: "`needs-human` still blocks this PR"},
		{name: "App lane", author: testHiveAppBotLogin, actor: "owner", want: "releases that hold on the current head"},
		{name: "trusted owner on own contributor PR", author: "owner", actor: "owner", want: "releases that hold on the current head"},
		{name: "trusted merger on contributor PR", author: "contributor", actor: "owner", want: "releases that hold on the current head"},
		{name: "trusted-author lane", author: "maintainer", actor: "owner", trustedAuthor: true, want: "releases that hold on the current head"},
		{name: "untrusted person on trusted-author lane", author: "maintainer", actor: "stranger", trustedAuthor: true, want: "not a trusted merger"},
		{name: "untrusted person on App lane", author: testHiveAppBotLogin, actor: "stranger", want: "not a trusted merger"},
		{name: "names the release lever", author: "contributor", actor: "owner", want: "releases that hold on the current head (`sha7`) on the lane's next pass, exactly as a GitHub **Approve** review"},
		{name: "untrusted self release", author: "contributor", actor: "contributor", want: "cannot release their own PR"},
		{name: "untrusted other release", author: "contributor", actor: "stranger", want: "not a trusted merger"},
		{name: "bot", author: "contributor", actor: "owner", bot: true, want: "a bot cannot release"},
		{name: "rejected contributor", author: "contributor", actor: "owner", verdict: "reject", want: "cannot override this contributor-lane verdict"},
		{name: "manual workflow", author: "contributor", actor: "owner", file: ".github/workflows/ci.yml", want: "requires a person to merge manually"},
		{name: "manual path", author: testHiveAppBotLogin, actor: "owner", file: "sensitive/config.yaml", want: "requires a person to merge manually"},
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
					var labels []*gh.Label
					for _, l := range tc.labels {
						labels = append(labels, &gh.Label{Name: gh.Ptr(l)})
					}
					json.NewEncoder(w).Encode(&gh.PullRequest{Number: gh.Ptr(7), State: gh.Ptr("open"), User: &gh.User{Login: gh.Ptr(tc.author)}, Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha7")}, Labels: labels})
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
			if strings.Contains(reply, "does not consume it") {
				t.Fatalf("reply still says the lane ignores /hive approve: %q", reply)
			}
		})
	}
}
