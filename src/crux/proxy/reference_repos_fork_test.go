package proxy

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

// hivecommons/hive#18198: the fork-and-PR path for a prs-tier reference repo.
// The owner cannot push to a repo they do not own, so the gate allows forking
// the reference repo into the owner's account, pushing topic branches to that
// fork and opening the PR against the reference repo; nothing else on the
// fork.

func forkPolicy() ReferenceRepoPolicy {
	return ReferenceRepoPolicy{Repo: "hivecommons/docs", Issues: true, PRs: true, Login: "nate-double-u", Token: func() (string, bool) { return "owner-tok", true }}
}

func TestReferenceRepoDecision_ForkCreationAndForkWrites(t *testing.T) {
	prs := forkPolicy()
	issuesOnly := ReferenceRepoPolicy{Repo: "hivecommons/docs", Issues: true, Login: "nate-double-u"}
	fork := prs
	fork.Fork = "nate-double-u/docs"
	forkIssuesOnly := issuesOnly
	forkIssuesOnly.Fork = "nate-double-u/docs"

	cases := []struct {
		name   string
		pol    ReferenceRepoPolicy
		method string
		path   string
		query  string
		want   referenceDecision
		reason string
	}{
		// Forking the reference repo needs the prs tier.
		{"prs fork", prs, "POST", "/repos/hivecommons/docs/forks", "", referenceAllow, ""},
		{"issues fork", issuesOnly, "POST", "/repos/hivecommons/docs/forks", "", referenceDeny, "forking needs write: [prs]"},
		// On the fork: pushes pass to the default-branch guard, reads are not gated.
		{"fork push", fork, "POST", "/nate-double-u/docs.git/git-receive-pack", "", referencePush, ""},
		{"fork push adv", fork, "GET", "/nate-double-u/docs.git/info/refs", "service=git-receive-pack", referencePush, ""},
		{"fork fetch", fork, "GET", "/nate-double-u/docs.git/info/refs", "service=git-upload-pack", referenceNotReference, ""},
		{"fork GET", fork, "GET", "/repos/nate-double-u/docs", "", referenceNotReference, ""},
		// Everything else on the fork is refused, with the PR flow in the reason.
		{"fork pr on fork", fork, "POST", "/repos/nate-double-u/docs/pulls", "", referenceDeny, "--head nate-double-u:<branch>"},
		{"fork issue on fork", fork, "POST", "/repos/nate-double-u/docs/issues", "", referenceDeny, "open the PR against hivecommons/docs"},
		{"fork api ref", fork, "POST", "/repos/nate-double-u/docs/git/refs", "", referenceDeny, "only pushing a topic branch"},
		{"fork delete", fork, "DELETE", "/repos/nate-double-u/docs", "", referenceDeny, "only pushing a topic branch"},
		{"fork contents", fork, "PUT", "/repos/nate-double-u/docs/contents/x.md", "", referenceDeny, "only pushing a topic branch"},
		// A fork of an issues-only entry is never pushable.
		{"issues-only fork push", forkIssuesOnly, "POST", "/nate-double-u/docs.git/git-receive-pack", "", referenceDeny, "needs write: [prs]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, reason := ReferenceRepoDecision(tc.pol, tc.method, tc.path, tc.query)
			if got != tc.want {
				t.Fatalf("decision=%v want %v (reason %q)", got, tc.want, reason)
			}
			if tc.reason != "" && !strings.Contains(reason, tc.reason) {
				t.Fatalf("reason %q lacks %q", reason, tc.reason)
			}
			if got == referenceDeny && tc.pol.Fork != "" && !strings.Contains(reason, "fork of REFERENCE repo hivecommons/docs") {
				t.Fatalf("a fork refusal must name the fork relationship: %q", reason)
			}
		})
	}
	// The catch-all refusal on the reference repo itself names the fork path.
	_, _, reason := ReferenceRepoDecision(prs, "PUT", "/repos/hivecommons/docs/pulls/9/merge", "")
	if !strings.Contains(reason, "pushing to the owner's fork") {
		t.Fatalf("catch-all refusal must mention the fork path: %q", reason)
	}
}

// referenceRepoFor reads login/name as the owner's fork of a prs-tier entry,
// never when the slug is itself a reference or authorized repo, never for
// another login or name, never without a resolved login.
func TestReferenceRepoFor_RecognisesOwnerFork(t *testing.T) {
	p := newReferenceTestProxy(t, forkPolicy(), nil)
	p.allowedRepos = map[string]bool{"cncf/techdocs": true}

	pol, ok := p.referenceRepoFor("/nate-double-u/docs.git/git-receive-pack")
	if !ok || pol.Fork != "nate-double-u/docs" || pol.Repo != "hivecommons/docs" || pol.Login != "nate-double-u" {
		t.Fatalf("fork push lookup = %+v ok=%v", pol, ok)
	}
	pol, ok = p.referenceRepoFor("/repos/Nate-Double-U/Docs/pulls")
	if !ok || pol.Fork != "Nate-Double-U/Docs" || pol.target() != "Nate-Double-U/Docs" {
		t.Fatalf("case-folded fork lookup = %+v ok=%v", pol, ok)
	}
	pol, ok = p.referenceRepoFor("/repos/hivecommons/docs/forks")
	if !ok || pol.Fork != "" || pol.target() != "hivecommons/docs" {
		t.Fatalf("the reference repo itself must never carry Fork: %+v", pol)
	}
	if installed, _ := p.ReferenceRepoPolicyFor("hivecommons/docs"); installed.Fork != "" {
		t.Fatal("the installed policy must not be mutated by a fork lookup")
	}
	for _, path := range []string{
		"/someone-else/docs.git/git-receive-pack",   // another login
		"/nate-double-u/other.git/git-receive-pack", // another name
		"/cncf/techdocs.git/git-receive-pack",       // authorized repo
	} {
		if _, ok := p.referenceRepoFor(path); ok {
			t.Errorf("%s must not be read as the owner's fork", path)
		}
	}

	// Authorized wins even when the owner's login matches the slug.
	p.allowedRepos["nate-double-u/docs"] = true
	if _, ok := p.referenceRepoFor("/nate-double-u/docs.git/git-receive-pack"); ok {
		t.Fatal("an authorized repo is never a fork")
	}
	delete(p.allowedRepos, "nate-double-u/docs")

	// A fork of an issues-only entry is not recognised: nothing to push for.
	p.SetReferenceRepos([]ReferenceRepoPolicy{{Repo: "hivecommons/docs", Issues: true, Login: "nate-double-u"}})
	if _, ok := p.referenceRepoFor("/nate-double-u/docs.git/git-receive-pack"); ok {
		t.Fatal("an issues-only entry has no fork path")
	}
	// No resolved login (read-only credential state): no fork either.
	p.SetReferenceRepos([]ReferenceRepoPolicy{{Repo: "hivecommons/docs", PRs: true}})
	if _, ok := p.referenceRepoFor("/nate-double-u/docs.git/git-receive-pack"); ok {
		t.Fatal("without a login there is no fork to recognise")
	}
	if (ReferenceRepoPolicy{Repo: "nodash", Login: "x"}).forkSlug() != "" {
		t.Fatal("malformed repo yields no fork slug")
	}
}

// Pushes to the fork are guarded against ITS default branch, looked up on
// the fork under the owner token, and carry the owner token.
func TestReferenceRepoRefusal_ForkPushGuardAndAuth(t *testing.T) {
	var lookups []string
	p := newReferenceTestProxy(t, forkPolicy(), func(url, token string) ([]byte, error) {
		lookups = append(lookups, url+" "+token)
		return []byte(`{"default_branch":"main"}`), nil
	})
	p.injectGHAuth = true
	p.agentTokenSource = func(string) (string, bool) { return "hive-app-token", true }
	zero := strings.Repeat("0", 40)
	sha := strings.Repeat("a", 40)

	topic := pktLine(zero+" "+sha+" refs/heads/hive/assessment\x00caps") + "0000" + "PACK"
	req := refReq("POST", "/nate-double-u/docs.git/git-receive-pack", "", []byte(topic))
	if reason, refused := p.referenceRepoRefusal(req); refused {
		t.Fatalf("topic push to the fork refused: %s", reason)
	}
	if len(lookups) != 1 || lookups[0] != "https://api.github.com/repos/nate-double-u/docs owner-tok" {
		t.Fatalf("default branch must be read from the FORK under the owner token: %v", lookups)
	}
	def := pktLine(sha+" "+sha+" refs/heads/main") + "0000" + "PACK"
	reason, refused := p.referenceRepoRefusal(refReq("POST", "/nate-double-u/docs.git/git-receive-pack", "", []byte(def)))
	if !refused || !strings.Contains(reason, "default branch (main)") || !strings.Contains(reason, "nate-double-u/docs") {
		t.Fatalf("default-branch push to the fork must be refused naming the fork, got refused=%v %q", refused, reason)
	}
	if len(lookups) != 1 {
		t.Fatalf("fork default branch should be cached, got %d lookups", len(lookups))
	}

	// Auth: the push advertisement and the push carry the owner token as
	// git basic auth; a fork read goes out unauthenticated; forking the
	// reference repo carries the owner token.
	adv := refReq("GET", "/nate-double-u/docs.git/info/refs", "service=git-receive-pack", nil)
	adv.Header.Set("Authorization", "Basic agent-held")
	p.rewriteGitHubAuth(adv, "docs-agent")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(gitInjectBasicUser+":owner-tok"))
	if got := adv.Header.Get("Authorization"); got != want || !adv.Close {
		t.Fatalf("fork push advertisement auth = %q close=%v", got, adv.Close)
	}
	read := refReq("GET", "/repos/nate-double-u/docs", "", nil)
	read.Header.Set("Authorization", "token agent-held")
	p.rewriteGitHubAuth(read, "docs-agent")
	if got := read.Header.Get("Authorization"); got != "" {
		t.Fatalf("fork read must go out unauthenticated, got %q", got)
	}
	forkReq := refReq("POST", "/repos/hivecommons/docs/forks", "", nil)
	forkReq.Header.Set("Authorization", "token agent-held")
	p.rewriteGitHubAuth(forkReq, "docs-agent")
	if got := forkReq.Header.Get("Authorization"); got != "token owner-tok" {
		t.Fatalf("fork creation must carry the owner token, got %q", got)
	}
	if reason, refused := p.referenceRepoRefusal(forkReq); refused {
		t.Fatalf("fork creation refused: %s", reason)
	}
	// A write on the fork other than a push is refused before any lookup.
	if _, refused := p.referenceRepoRefusal(refReq("POST", "/repos/nate-double-u/docs/pulls", "", nil)); !refused {
		t.Fatal("opening a PR on the fork itself must be refused")
	}
	if !referenceWriteCredential(forkPolicyWithFork(), http.MethodPost, "/nate-double-u/docs.git/git-receive-pack", "") {
		t.Fatal("a fork push carries the write credential")
	}
	if referenceWriteCredential(forkPolicyWithFork(), http.MethodPost, "/repos/nate-double-u/docs/issues", "") {
		t.Fatal("a refused fork write carries no credential")
	}
}

func forkPolicyWithFork() ReferenceRepoPolicy {
	pol := forkPolicy()
	pol.Fork = "nate-double-u/docs"
	return pol
}

func TestReferenceWriteAllowedByMode_Fork(t *testing.T) {
	if referenceWriteAllowedByMode(agent.ModeIssuesOnly, "POST", "/repos/o/r/forks") {
		t.Error("ISSUES_ONLY may not fork")
	}
	if !referenceWriteAllowedByMode(agent.ModeIssuesAndPRs, "POST", "/repos/o/r/forks") {
		t.Error("ISSUES_AND_PRS may fork")
	}
	if referenceWriteAllowedByMode(agent.ModeIssuesOnly, "POST", "/nate/r.git/git-receive-pack") {
		t.Error("ISSUES_ONLY may not push to the fork")
	}
}

// The request-loop view: with the fork policy installed the owner's fork is
// gated by the reference gate, so RepoFilterAllowed's "repo not in config"
// never fires for the push.
func TestReferenceRepoFork_NotRepoNotInConfig(t *testing.T) {
	p := newReferenceTestProxy(t, forkPolicy(), func(string, string) ([]byte, error) { return []byte(`{"default_branch":"main"}`), nil })
	p.allowedRepos = map[string]bool{"cncf/techdocs": true}
	path := "/nate-double-u/docs.git/git-receive-pack"
	pol, isRef := p.referenceRepoFor(path)
	if !isRef {
		t.Fatal("fork must be seen by the reference gate")
	}
	if !referenceWriteCredential(pol, http.MethodPost, path, "") {
		t.Fatal("fork push must be an allowed reference write (so the repo filter is skipped)")
	}
	if RepoFilterAllowed(p.allowedRepos, http.MethodPost, path) {
		t.Fatal(fmt.Sprintf("precondition: without the reference gate the repo filter refuses %s", path))
	}
}
