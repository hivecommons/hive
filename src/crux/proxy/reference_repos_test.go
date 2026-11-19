package proxy

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

func newReferenceTestProxy(t *testing.T, pol ReferenceRepoPolicy, lookup referenceLookupFunc) *GitHubProxy {
	t.Helper()
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.SetReferenceRepos([]ReferenceRepoPolicy{pol})
	p.referenceLookup = lookup
	return p
}

func refReq(method, path, rawQuery string, body []byte) *http.Request {
	req, _ := http.NewRequest(method, "https://api.github.com"+path, bytes.NewReader(body))
	req.URL.RawQuery = rawQuery
	req.Host = "api.github.com"
	return req
}

// pktLine encodes one git pkt-line.
func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

func TestReferenceRepoDecision_Matrix(t *testing.T) {
	ro := ReferenceRepoPolicy{Repo: "hivecommons/docs"}
	issues := ReferenceRepoPolicy{Repo: "hivecommons/docs", Issues: true}
	prs := ReferenceRepoPolicy{Repo: "hivecommons/docs", PRs: true}
	both := ReferenceRepoPolicy{Repo: "hivecommons/docs", Issues: true, PRs: true}

	cases := []struct {
		name   string
		pol    ReferenceRepoPolicy
		method string
		path   string
		query  string
		want   referenceDecision
	}{
		// Reads pass for every tier.
		{"ro GET", ro, "GET", "/repos/hivecommons/docs/contents/README.md", "", referenceNotReference},
		{"ro fetch", ro, "POST", "/hivecommons/docs.git/git-upload-pack", "", referenceNotReference},
		{"ro info/refs upload", ro, "GET", "/hivecommons/docs.git/info/refs", "service=git-upload-pack", referenceNotReference},
		// Read-only refuses every write, including the push advertisement.
		{"ro issue", ro, "POST", "/repos/hivecommons/docs/issues", "", referenceDeny},
		{"ro comment", ro, "POST", "/repos/hivecommons/docs/issues/3/comments", "", referenceDeny},
		{"ro push adv", ro, "GET", "/hivecommons/docs.git/info/refs", "service=git-receive-pack", referenceDeny},
		{"ro push", ro, "POST", "/hivecommons/docs.git/git-receive-pack", "", referenceDeny},
		{"ro fork", ro, "POST", "/repos/hivecommons/docs/forks", "", referenceDeny},
		// issues tier.
		{"issues open", issues, "POST", "/repos/hivecommons/docs/issues", "", referenceAllow},
		{"issues comment own-check", issues, "POST", "/repos/hivecommons/docs/issues/3/comments", "", referenceOwnItem},
		{"issues pr comment own-check", issues, "POST", "/repos/hivecommons/docs/pulls/9/comments", "", referenceOwnItem},
		{"issues edit", issues, "PATCH", "/repos/hivecommons/docs/issues/3", "", referenceDeny},
		{"issues label", issues, "POST", "/repos/hivecommons/docs/issues/3/labels", "", referenceDeny},
		{"issues assign", issues, "POST", "/repos/hivecommons/docs/issues/3/assignees", "", referenceDeny},
		{"issues open pr", issues, "POST", "/repos/hivecommons/docs/pulls", "", referenceDeny},
		{"issues push", issues, "POST", "/hivecommons/docs.git/git-receive-pack", "", referenceDeny},
		// prs tier.
		{"prs open pr", prs, "POST", "/repos/hivecommons/docs/pulls", "", referenceAllow},
		{"prs push adv", prs, "GET", "/hivecommons/docs.git/info/refs", "service=git-receive-pack", referencePush},
		{"prs push", prs, "POST", "/hivecommons/docs.git/git-receive-pack", "", referencePush},
		{"prs open issue", prs, "POST", "/repos/hivecommons/docs/issues", "", referenceDeny},
		{"prs merge", prs, "PUT", "/repos/hivecommons/docs/pulls/9/merge", "", referenceDeny},
		{"prs review", prs, "POST", "/repos/hivecommons/docs/pulls/9/reviews", "", referenceDeny},
		{"prs edit pr", prs, "PATCH", "/repos/hivecommons/docs/pulls/9", "", referenceDeny},
		{"prs api ref", prs, "POST", "/repos/hivecommons/docs/git/refs", "", referenceDeny},
		{"prs delete ref", prs, "DELETE", "/repos/hivecommons/docs/git/refs/heads/x", "", referenceDeny},
		{"prs contents", prs, "PUT", "/repos/hivecommons/docs/contents/x.md", "", referenceDeny},
		// both.
		{"both issue", both, "POST", "/repos/hivecommons/docs/issues", "", referenceAllow},
		{"both pr", both, "POST", "/repos/hivecommons/docs/pulls", "", referenceAllow},
		{"both merge", both, "PUT", "/repos/hivecommons/docs/pulls/9/merge", "", referenceDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, reason := ReferenceRepoDecision(tc.pol, tc.method, tc.path, tc.query)
			if got != tc.want {
				t.Fatalf("decision=%v want %v (reason %q)", got, tc.want, reason)
			}
			if got == referenceDeny && (!strings.Contains(reason, "reference_repos") || !strings.Contains(reason, tc.pol.tier())) {
				t.Fatalf("refusal must name reference_repos and the tier: %q", reason)
			}
		})
	}
}

func TestReferenceRepoRefusal_OwnItemCheck(t *testing.T) {
	pol := ReferenceRepoPolicy{Repo: "hivecommons/docs", Issues: true, Login: "hivecommons-hive[bot]", Token: func() (string, bool) { return "ref-tok", true }}
	var gotURL, gotToken string
	author := "hivecommons-hive[bot]"
	p := newReferenceTestProxy(t, pol, func(url, token string) ([]byte, error) {
		gotURL, gotToken = url, token
		return []byte(`{"user":{"login":"` + author + `"}}`), nil
	})

	if reason, refused := p.referenceRepoRefusal(refReq("POST", "/repos/hivecommons/docs/issues/12/comments", "", nil)); refused {
		t.Fatalf("comment on own issue refused: %s", reason)
	}
	if gotURL != "https://api.github.com/repos/hivecommons/docs/issues/12" || gotToken != "ref-tok" {
		t.Fatalf("lookup url=%q token=%q", gotURL, gotToken)
	}

	author = "someone-else"
	reason, refused := p.referenceRepoRefusal(refReq("POST", "/repos/hivecommons/docs/issues/12/comments", "", nil))
	if !refused || !strings.Contains(reason, "someone-else") {
		t.Fatalf("comment on another's issue must be refused naming the author, got refused=%v %q", refused, reason)
	}

	p.referenceLookup = func(string, string) ([]byte, error) { return nil, fmt.Errorf("boom") }
	if _, refused := p.referenceRepoRefusal(refReq("POST", "/repos/hivecommons/docs/issues/12/comments", "", nil)); !refused {
		t.Fatal("a failed author lookup must fail closed")
	}

	// A different repo is not a reference repo at all.
	if _, refused := p.referenceRepoRefusal(refReq("POST", "/repos/cncf/techdocs/issues/1/comments", "", nil)); refused {
		t.Fatal("non-reference repo must not be gated here")
	}
}

func TestReferenceRepoRefusal_PushGuardsDefaultBranch(t *testing.T) {
	pol := ReferenceRepoPolicy{Repo: "hivecommons/docs", PRs: true, Token: func() (string, bool) { return "ref-tok", true }}
	lookups := 0
	p := newReferenceTestProxy(t, pol, func(url, token string) ([]byte, error) {
		lookups++
		if url != "https://api.github.com/repos/hivecommons/docs" {
			t.Fatalf("unexpected lookup %s", url)
		}
		return []byte(`{"default_branch":"main"}`), nil
	})
	zero := strings.Repeat("0", 40)
	sha := strings.Repeat("a", 40)
	pack := "PACK-bytes-follow"

	topic := pktLine(zero+" "+sha+" refs/heads/hive/assessment\x00report-status side-band-64k") + "0000" + pack
	req := refReq("POST", "/hivecommons/docs.git/git-receive-pack", "", []byte(topic))
	if reason, refused := p.referenceRepoRefusal(req); refused {
		t.Fatalf("topic-branch push refused: %s", reason)
	}
	rest, _ := io.ReadAll(req.Body)
	if string(rest) != topic {
		t.Fatalf("push body not restored byte-for-byte:\n got %q\nwant %q", rest, topic)
	}

	def := pktLine(zero+" "+sha+" refs/heads/hive/x\x00caps") + pktLine(sha+" "+sha+" refs/heads/main") + "0000" + pack
	reason, refused := p.referenceRepoRefusal(refReq("POST", "/hivecommons/docs.git/git-receive-pack", "", []byte(def)))
	if !refused || !strings.Contains(reason, "default branch (main)") {
		t.Fatalf("default-branch push must be refused, got refused=%v %q", refused, reason)
	}
	if lookups != 1 {
		t.Fatalf("default branch should be cached after one lookup, got %d", lookups)
	}

	gz := refReq("POST", "/hivecommons/docs.git/git-receive-pack", "", []byte(topic))
	gz.Header.Set("Content-Encoding", "gzip")
	if _, refused := p.referenceRepoRefusal(gz); !refused {
		t.Fatal("an encoded push body the guard cannot read must fail closed")
	}

	if _, refused := p.referenceRepoRefusal(refReq("GET", "/hivecommons/docs.git/info/refs", "service=git-receive-pack", nil)); refused {
		t.Fatal("push ref advertisement must pass for a prs entry")
	}
}

func TestReferenceRepoAuth_StripsOnReadsInjectsOnWrites(t *testing.T) {
	pol := ReferenceRepoPolicy{Repo: "hivecommons/docs", Issues: true, PRs: true, Token: func() (string, bool) { return "ref-tok", true }}
	p := newReferenceTestProxy(t, pol, nil)
	p.injectGHAuth = true
	p.agentTokenSource = func(string) (string, bool) { return "hive-app-token", true }

	read := refReq("GET", "/repos/hivecommons/docs/contents/README.md", "", nil)
	read.Header.Set("Authorization", "token agent-held")
	p.rewriteGitHubAuth(read, "scanner")
	if got := read.Header.Get("Authorization"); got != "" {
		t.Fatalf("reference read must go out unauthenticated, got %q", got)
	}

	fetch := refReq("POST", "/hivecommons/docs.git/git-upload-pack", "", nil)
	fetch.Header.Set("Authorization", "Basic xyz")
	p.rewriteGitHubAuth(fetch, "scanner")
	if got := fetch.Header.Get("Authorization"); got != "" {
		t.Fatalf("reference fetch must go out unauthenticated, got %q", got)
	}

	write := refReq("POST", "/repos/hivecommons/docs/issues", "", nil)
	write.Header.Set("Authorization", "token agent-held")
	p.rewriteGitHubAuth(write, "scanner")
	if got := write.Header.Get("Authorization"); got != "token ref-tok" {
		t.Fatalf("reference write must carry the reference credential, got %q", got)
	}

	push := refReq("GET", "/hivecommons/docs.git/info/refs", "service=git-receive-pack", nil)
	p.rewriteGitHubAuth(push, "scanner")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(gitInjectBasicUser+":ref-tok"))
	if got := push.Header.Get("Authorization"); got != want || !push.Close {
		t.Fatalf("push advertisement must carry basic reference credential and close, got %q close=%v", got, push.Close)
	}

	// A managed repo still takes the ordinary injection path.
	managed := refReq("GET", "/repos/cncf/techdocs/issues", "", nil)
	p.rewriteGitHubAuth(managed, "scanner")
	if got := managed.Header.Get("Authorization"); got != "token hive-app-token" {
		t.Fatalf("managed repo should get the agent token, got %q", got)
	}

	// Read-only entries never carry anything, even on a (refused upstream) write.
	p.SetReferenceRepos([]ReferenceRepoPolicy{{Repo: "hivecommons/docs"}})
	ro := refReq("POST", "/repos/hivecommons/docs/issues", "", nil)
	ro.Header.Set("Authorization", "token agent-held")
	p.rewriteGitHubAuth(ro, "scanner")
	if got := ro.Header.Get("Authorization"); got != "" {
		t.Fatalf("read-only reference repo must never carry a credential, got %q", got)
	}
}

func TestReferenceWriteAllowedByMode(t *testing.T) {
	if referenceWriteAllowedByMode(agent.ModeAdvisory, "POST", "/repos/o/r/issues") {
		t.Error("ADVISORY may not open issues on a reference repo")
	}
	if !referenceWriteAllowedByMode(agent.ModeIssuesOnly, "POST", "/repos/o/r/issues") {
		t.Error("ISSUES_ONLY may open issues")
	}
	if referenceWriteAllowedByMode(agent.ModeIssuesOnly, "POST", "/repos/o/r/pulls") {
		t.Error("ISSUES_ONLY may not open PRs")
	}
	if !referenceWriteAllowedByMode(agent.ModeIssuesAndPRs, "POST", "/o/r.git/git-receive-pack") {
		t.Error("ISSUES_AND_PRS may push a topic branch")
	}
}
