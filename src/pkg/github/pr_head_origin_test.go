package github

import (
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// hivecommons/hive#7386: the hive had no fork awareness at all, so a repair
// queue could hand an agent PRs whose head lives in a fork it cannot push to.
// prHeadOrigin is the one place the classification is computed.
func TestPRHeadOrigin(t *testing.T) {
	branch := func(repo, ref string) *gh.PullRequestBranch {
		b := &gh.PullRequestBranch{Ref: gh.Ptr(ref)}
		if repo != "" {
			b.Repo = &gh.Repository{FullName: gh.Ptr(repo)}
		}
		return b
	}
	cases := []struct {
		name         string
		pr           *gh.PullRequest
		wantRef      string
		wantRepo     string
		wantFromFork bool
	}{
		{"same repo", &gh.PullRequest{Head: branch("acme/app", "fix-1"), Base: branch("acme/app", "main")}, "fix-1", "acme/app", false},
		{"same repo, different case", &gh.PullRequest{Head: branch("Acme/App", "fix-1"), Base: branch("acme/app", "main")}, "fix-1", "Acme/App", false},
		{"fork", &gh.PullRequest{Head: branch("alice/app", "sec-check-dashboard"), Base: branch("projectbluefin/testsuite", "main")}, "sec-check-dashboard", "alice/app", true},
		{"fork deleted after opening", &gh.PullRequest{Head: branch("", "gone"), Base: branch("acme/app", "main")}, "gone", "", true},
		{"nil head", &gh.PullRequest{Base: branch("acme/app", "main")}, "", "", false},
		{"nil pr", nil, "", "", false},
		{"abbreviated: no base repo", &gh.PullRequest{Head: branch("acme/app", "x"), Base: &gh.PullRequestBranch{Ref: gh.Ptr("main")}}, "x", "acme/app", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, repo, fork := prHeadOrigin(tc.pr)
			if ref != tc.wantRef || repo != tc.wantRepo || fork != tc.wantFromFork {
				t.Errorf("prHeadOrigin = (%q, %q, %v), want (%q, %q, %v)", ref, repo, fork, tc.wantRef, tc.wantRepo, tc.wantFromFork)
			}
		})
	}
}

func TestReachableAction(t *testing.T) {
	if got := ReachableAction(PullRequest{FromFork: false}); got != ReachableActionPush {
		t.Errorf("same-repo PR = %q, want push", got)
	}
	if got := ReachableAction(PullRequest{FromFork: true, HeadRepo: "alice/app"}); got != ReachableActionCommentOnly {
		t.Errorf("fork PR = %q, want comment-only", got)
	}
}

// End to end through fetchPRs: a fork PR and a same-repo PR come back with
// HeadRef/HeadRepo/FromFork populated from the REST payload, so every
// downstream consumer (ci-failing.json, the kick lists) can say "comment
// only" without probing.
func TestFetchPRs_PopulatesForkOrigin(t *testing.T) {
	base := &wireBranch{Ref: "main", SHA: "base", Repo: &wireRepo{FullName: "projectbluefin/testsuite"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/projectbluefin/testsuite/pulls", prsHandler(t, []wirePR{
		{Number: 839, Title: "fork: dashboard", User: wireUser{Login: "alice"}, CreatedAt: hoursAgo(1),
			Head: &wireBranch{Ref: "sec-check-dashboard", SHA: "f1", Repo: &wireRepo{FullName: "alice/testsuite"}}, Base: base},
		{Number: 840, Title: "same repo", User: wireUser{Login: "hive[bot]"}, CreatedAt: hoursAgo(1),
			Head: &wireBranch{Ref: "hive/fix-840", SHA: "f2", Repo: &wireRepo{FullName: "projectbluefin/testsuite"}}, Base: base},
		{Number: 841, Title: "fork deleted", User: wireUser{Login: "bob"}, CreatedAt: hoursAgo(1),
			Head: &wireBranch{Ref: "gone", SHA: "f3", Repo: nil}, Base: base},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "projectbluefin", []string{"testsuite"})
	actionable, _, _, _, _, _, _, err := c.fetchPRs(t.Context(), "testsuite")
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	byNum := map[int]PullRequest{}
	for _, pr := range actionable {
		byNum[pr.Number] = pr
	}
	if pr := byNum[839]; !pr.FromFork || pr.HeadRepo != "alice/testsuite" || pr.HeadRef != "sec-check-dashboard" || ReachableAction(pr) != ReachableActionCommentOnly {
		t.Errorf("fork PR not classified: %+v", pr)
	}
	if pr := byNum[840]; pr.FromFork || pr.HeadRepo != "projectbluefin/testsuite" || pr.HeadRef != "hive/fix-840" || ReachableAction(pr) != ReachableActionPush {
		t.Errorf("same-repo PR misclassified: %+v", pr)
	}
	if pr := byNum[841]; !pr.FromFork || pr.HeadRepo != "" {
		t.Errorf("deleted-fork PR must be unpushable: %+v", pr)
	}
	// The base branch rides along from the same payload so the merge
	// verdict can say "has merge conflicts with main" (hivecommons/hive#7515).
	for _, n := range []int{839, 840, 841} {
		if pr := byNum[n]; pr.BaseRef != "main" {
			t.Errorf("#%d: BaseRef = %q, want %q", n, pr.BaseRef, "main")
		}
	}
}

// End to end through fetchPRs (hivecommons/hive#7638): a PR a hive agent
// opened on a person's credentials — human author, `— hive:` trailer in the
// body — comes back HiveAttributed from the list payload alone, so the
// review-thread reconciler can recognise it without a second fetch. A
// hand-written PR by the same person, and a PR with no body at all, do not.
func TestFetchPRs_PopulatesHiveAttributed(t *testing.T) {
	head := &wireBranch{Ref: "x", SHA: "s", Repo: &wireRepo{FullName: "acme/app"}}
	base := &wireBranch{Ref: "main", SHA: "b", Repo: &wireRepo{FullName: "acme/app"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", prsHandler(t, []wirePR{
		{Number: 195, Title: "relay PR", User: wireUser{Login: "danathar"}, CreatedAt: hoursAgo(1), Head: head, Base: base,
			Body: "Refuses the new options.\n\n— hive: backend=claude model=claude-opus-5"},
		{Number: 196, Title: "hand-written", User: wireUser{Login: "danathar"}, CreatedAt: hoursAgo(1), Head: head, Base: base,
			Body: "I wrote this one myself."},
		{Number: 197, Title: "no body", User: wireUser{Login: "danathar"}, CreatedAt: hoursAgo(1), Head: head, Base: base},
		{Number: 198, Title: "held relay PR", User: wireUser{Login: "danathar"}, CreatedAt: hoursAgo(1), Head: head, Base: base,
			Labels: []wireLabel{{Name: "hold"}}, Body: "— hive: agent=quality backend=claude model=claude-opus-5"},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"app"})
	actionable, _, heldPRs, _, _, _, _, err := c.fetchPRs(t.Context(), "app")
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	byNum := map[int]PullRequest{}
	for _, pr := range actionable {
		byNum[pr.Number] = pr
	}
	if pr := byNum[195]; !pr.HiveAttributed || pr.Author != "danathar" {
		t.Errorf("relay PR must be HiveAttributed with its human author kept: %+v", pr)
	}
	if pr := byNum[196]; pr.HiveAttributed {
		t.Errorf("hand-written PR must not be HiveAttributed: %+v", pr)
	}
	if pr := byNum[197]; pr.HiveAttributed {
		t.Errorf("PR with no body must not be HiveAttributed: %+v", pr)
	}
	if len(heldPRs) != 1 || heldPRs[0].Number != 198 || !heldPRs[0].HiveAttributed {
		t.Errorf("held PR must carry the flag too: %+v", heldPRs)
	}
}

func TestPRBaseRef(t *testing.T) {
	if got := prBaseRef(&gh.PullRequest{Base: &gh.PullRequestBranch{Ref: gh.Ptr("v4")}}); got != "v4" {
		t.Errorf("prBaseRef = %q, want v4", got)
	}
	if got := prBaseRef(&gh.PullRequest{}); got != "" {
		t.Errorf("prBaseRef with no base = %q, want empty", got)
	}
}
