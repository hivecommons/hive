package github

import (
	"context"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// A dependency bot names its branch after the version it bumps, so the
// trailing number is never an issue. projectbluefin/utah#1494 (mergeraptor,
// `renovate/registry.fedoraproject.org-fedora-43`, body without `#43`)
// claimed utah#43 by branch name; the supersession sweep then called the
// digest bump superseded by utah#144, the unrelated PR that had closed utah#43.
const fedoraDigestBranch = "renovate/registry.fedoraproject.org-fedora-43"

func TestClaimsFromPR_DependencyBotBranchIsNotAClaim(t *testing.T) {
	now := time.Now()
	identity := HiveIdentity{AIAuthor: "hive-bot", AppLogin: "hive-app[bot]"}
	for _, tc := range []struct {
		name        string
		author      string
		branch      string
		body        string
		wantIssue   int // 0: no claim at all
		wantClosing bool
	}{
		{name: "renovate branch by a renovate-style app", author: "mergeraptor[bot]", branch: fedoraDigestBranch, body: "This PR updates the fedora image digest."},
		{name: "renovate branch by renovate", author: "renovate[bot]", branch: fedoraDigestBranch, body: "This PR updates the fedora image digest."},
		{name: "renovate branch by a self-hosted renovate account", author: "deps-runner", branch: fedoraDigestBranch, body: ""},
		{name: "dependabot branch by dependabot", author: "dependabot[bot]", branch: "dependabot/npm_and_yarn/node-22", body: "Bumps node from 20 to 22."},
		{name: "hive agent fix-issue branch", author: "hive-bot", branch: "fix-issue-423", body: "test", wantIssue: 423},
		{name: "hive app slug branch", author: "hive-app[bot]", branch: "423-add-retry", body: "", wantIssue: 423},
		{name: "human issue branch with empty body", author: "octocat", branch: "issue-77", body: "", wantIssue: 77},
		{name: "bot body closing keyword still claims", author: "mergeraptor[bot]", branch: fedoraDigestBranch, body: "Fixes #43", wantIssue: 43, wantClosing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := &gh.PullRequest{
				Number:  gh.Ptr(1494),
				Title:   gh.Ptr("chore(deps): update fedora docker digest"),
				Body:    gh.Ptr(tc.body),
				State:   gh.Ptr("open"),
				HTMLURL: gh.Ptr("https://github.com/o/r/pull/1494"),
				User:    &gh.User{Login: gh.Ptr(tc.author)},
				Head:    &gh.PullRequestBranch{Ref: gh.Ptr(tc.branch), SHA: gh.Ptr("abc1234")},
			}
			claims := claimsFromPR(pr, "o/r", identity, now)
			if tc.wantIssue == 0 {
				if len(claims) != 0 {
					t.Fatalf("claims = %+v, want none for a dependency bot's version branch", claims)
				}
				return
			}
			if len(claims) != 1 {
				t.Fatalf("claims = %+v, want exactly one on #%d", claims, tc.wantIssue)
			}
			c := claims[0]
			if c.Issue != tc.wantIssue || c.Closing != tc.wantClosing || c.Reference {
				t.Fatalf("claim = %+v, want issue %d closing=%v and not a reference", c, tc.wantIssue, tc.wantClosing)
			}
		})
	}
}

func TestIssueFromPRBranch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pr     *gh.PullRequest
		want   int
		wantOK bool
	}{
		{name: "nil PR", pr: nil},
		{name: "renovate branch", pr: mergedTestPR(1, "mergeraptor[bot]", fedoraDigestBranch, "")},
		{name: "dependabot author on a plain branch", pr: mergedTestPR(2, "dependabot[bot]", "bump-node-22", "")},
		{name: "hive branch", pr: mergedTestPR(3, "hive[bot]", "fix-issue-423", ""), want: 423, wantOK: true},
		{name: "human branch", pr: mergedTestPR(4, "octocat", "issue-77", ""), want: 77, wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := issueFromPRBranch(tc.pr)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("issueFromPRBranch = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// Merging a bump must never close the issue its version number happens to
// match; an explicit closing keyword still does.
func TestCloseOnMergePRClaimsIssue_DependencyBotBranch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pr    *gh.PullRequest
		issue int
		want  bool
	}{
		{name: "renovate branch by a renovate-style app", pr: mergedTestPR(1494, "mergeraptor[bot]", fedoraDigestBranch, "Updates the fedora digest."), issue: 43},
		{name: "renovate branch by renovate", pr: mergedTestPR(1494, "renovate[bot]", fedoraDigestBranch, ""), issue: 43},
		{name: "dependabot branch", pr: mergedTestPR(9, "dependabot[bot]", "dependabot/npm_and_yarn/node-22", "Bumps node."), issue: 22},
		{name: "bot body closing keyword", pr: mergedTestPR(1494, "mergeraptor[bot]", fedoraDigestBranch, "Fixes #43"), issue: 43, want: true},
		{name: "hive agent branch", pr: mergedTestPR(10, "hive[bot]", "fix-issue-423", "test"), issue: 423, want: true},
		{name: "hive slug branch", pr: mergedTestPR(11, "hive[bot]", "423-add-retry", ""), issue: 423, want: true},
		{name: "human issue branch with empty body", pr: mergedTestPR(12, "octocat", "issue-77", ""), issue: 77, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := closeOnMergePRClaimsIssue(tc.pr, "o/r", "o/r", tc.issue); got != tc.want {
				t.Fatalf("closeOnMergePRClaimsIssue(#%d) = %v, want %v", tc.issue, got, tc.want)
			}
		})
	}
}

// The supersession sweep reads claims through claimsFromPR, so a dependency
// bot's version branch gets no "superseded by" notice for the PR that closed
// the matching issue. A closing keyword in the bot's body, and a person's
// numbered branch, still do.
func TestSweepSupersededOpenPRs_DependencyBotBranchGetsNoNotice(t *testing.T) {
	org, repo := "hivecommons", "hive"
	for _, tc := range []struct {
		name       string
		author     string
		branch     string
		body       string
		wantNotice bool
	}{
		{name: "renovate branch by a renovate-style app", author: "mergeraptor[bot]", branch: fedoraDigestBranch, body: "Updates the fedora digest."},
		{name: "renovate branch by renovate", author: "renovate[bot]", branch: fedoraDigestBranch, body: "Updates the fedora digest."},
		{name: "bot body closing keyword", author: "mergeraptor[bot]", branch: fedoraDigestBranch, body: "Fixes #43", wantNotice: true},
		{name: "human issue branch with empty body", author: "octocat", branch: "issue-43", body: "", wantNotice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, obs := supersessionSweepServer(t, org, repo,
				[]supersessionPRFixture{
					{Number: 1494, Title: "chore(deps): update fedora docker digest", Body: tc.body, Author: tc.author, HeadRef: tc.branch, Files: []string{"Containerfile"}},
					{Number: 144, Title: "flavors", Body: "Fixes #43", Author: "hive-app[bot]", Merged: true, Files: []string{"flavors.py"}},
				},
				[]supersessionIssueFixture{{Number: 43, State: "closed", GraphQLCloserPR: 144}},
			)
			defer server.Close()

			c, _ := newSupersessionTestClient(t, server, org, repo)
			res, err := c.SweepSupersededOpenPRs(context.Background(), SupersessionSweepOptions{ACMMLevelForRepo: func(string) int { return acmmLevelAutonomous }})
			if err != nil {
				t.Fatalf("SweepSupersededOpenPRs: %v", err)
			}
			noticed := len(res.Commented)+len(res.Closed)+len(res.Resolved) > 0 || len(obs.comments[1494]) > 0
			if noticed != tc.wantNotice {
				t.Fatalf("noticed = %v, want %v: result=%+v comments=%v", noticed, tc.wantNotice, res, obs.comments)
			}
			if !tc.wantNotice && (len(obs.closed) != 0 || len(obs.labels[1494]) != 0) {
				t.Fatalf("dependency bot PR was written to: closed=%v labels=%v", obs.closed, obs.labels)
			}
		})
	}
}
