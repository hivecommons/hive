package github

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/beads"
)

// writeSignedSpoolKind writes request into spool as hive-open-issue or
// hive-topic does inside a sandbox: an envelope of kind, MACed for that kind.
func writeSignedSpoolKind(t *testing.T, spool, name, key, kind string, request any) string {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(crux.SandboxReviewEnvelope{Kind: kind, Request: string(raw), MAC: crux.SandboxSpoolMAC(key, kind, raw)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spool, name)
	if err := os.WriteFile(path, b, 0o660); err != nil {
		t.Fatal(err)
	}
	return path
}

// newKickSpool is newSandboxSpool for a kick that opens a PR in repo from
// branch, which a pending topic claim binds to.
func newKickSpool(t *testing.T, manifests, agent, repo, branch string) string {
	t.Helper()
	workspace := t.TempDir()
	spool := filepath.Join(workspace, filepath.FromSlash(crux.SandboxReviewSpoolRelPath))
	if err := os.MkdirAll(spool, 0o770); err != nil {
		t.Fatal(err)
	}
	writeSandboxManifest(t, manifests, agent, crux.SandboxReviewSpool{
		Agent: agent, Repo: repo, Branch: branch, Spool: spool, Key: testSpoolKey(agent), StartedAt: time.Now().UTC(),
	})
	return spool
}

func readTopicResultFile(t *testing.T, reqPath string) SandboxTopicResponse {
	t.Helper()
	b, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file missing: %v", err)
	}
	var r SandboxTopicResponse
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// A sandboxed agent's spooled issue is created through the issue relay's own
// handler, deduplicated by title, attributed and audited to the MANIFEST's
// agent (not the one the request names), and its result lands in the spool.
func TestSandboxSpoolIssue_CreatesAttributedToManifestAgent(t *testing.T) {
	created := 0
	srv := newIssueMockServer(t, "", &created, nil, nil)
	defer srv.Close()
	c := testClient(t, srv.URL)
	withReviewDir(t)
	withIssueDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "deps-bump", false)

	var authzAgent, authzKind string
	authzUID := -1
	c.SetSandboxSpoolRelays(func(agent string, uid int, kind string) error {
		authzAgent, authzUID, authzKind = agent, uid, kind
		return nil
	}, nil)
	var auditAgent string
	c.SetAttributionAudit(func(action, detail, agent string) {
		if action == AuditActionAgentIssueCreated {
			auditAgent = agent
		}
	})

	reqPath := writeSignedSpoolKind(t, spool, "i.json", testSpoolKey("deps-bump"), crux.SandboxSpoolKindIssue,
		IssueRequest{Repo: "o/r", Title: "dependency X breaks us", Body: "detail", Agent: "someone-else"})
	c.ProcessReviewRequestsOnce(context.Background())

	if created != 1 {
		t.Fatalf("issues created = %d, want 1", created)
	}
	if authzAgent != "deps-bump" || authzUID != 0 || authzKind != "issue" {
		t.Errorf("authorized (%q, uid %d, %q), want the manifest's agent deps-bump, uid 0, kind issue", authzAgent, authzUID, authzKind)
	}
	if auditAgent != "deps-bump" {
		t.Errorf("audited as %q, want deps-bump", auditAgent)
	}
	if _, err := os.Stat(reqPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("request still in the spool after delivery: %v", err)
	}
	if res := readIssueResultFile(t, reqPath); !res.OK || res.Number != 99 {
		t.Errorf("mirrored result = %+v, want ok #99", res)
	}
}

// The title dedupe is the relay's: an open issue with the same title is
// reused, not duplicated, for a spooled request too.
func TestSandboxSpoolIssue_DedupesByTitle(t *testing.T) {
	created := 0
	srv := newIssueMockServer(t, "dependency X breaks us", &created, nil, nil)
	defer srv.Close()
	c := testClient(t, srv.URL)
	withReviewDir(t)
	withIssueDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "deps-bump", false)
	c.SetSandboxSpoolRelays(func(string, int, string) error { return nil }, nil)

	reqPath := writeSignedSpoolKind(t, spool, "i.json", testSpoolKey("deps-bump"), crux.SandboxSpoolKindIssue,
		IssueRequest{Repo: "o/r", Title: "dependency X breaks us", Body: "detail"})
	c.ProcessReviewRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("issues created = %d, want 0 (dedupe)", created)
	}
	if res := readIssueResultFile(t, reqPath); !res.OK || !res.AlreadyExisted || res.Number != 7 {
		t.Errorf("result = %+v, want reuse of #7", res)
	}
}

// A comment goes through too, as the same agent.
func TestSandboxSpoolIssue_PostsComment(t *testing.T) {
	commented := 0
	srv := newIssueMockServer(t, "", nil, nil, &commented)
	defer srv.Close()
	c := testClient(t, srv.URL)
	withReviewDir(t)
	withIssueDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "deps-bump", false)
	c.SetSandboxSpoolRelays(func(string, int, string) error { return nil }, nil)

	writeSignedSpoolKind(t, spool, "c.json", testSpoolKey("deps-bump"), crux.SandboxSpoolKindIssue,
		IssueRequest{Kind: "comment", Repo: "o/r", Number: 41, Body: "still broken on the card"})
	c.ProcessReviewRequestsOnce(context.Background())

	if commented != 1 {
		t.Fatalf("comments posted = %d, want 1", commented)
	}
}

// Every gate of the issue relay applies: the mode gate's denial, a missing
// authorizer (fail closed), the repo gate and the repo scope each refuse,
// the refusal reaches the sandbox and nothing is created.
func TestSandboxSpoolIssue_GatesRefuse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(c *Client)
		repo  string
		want  string
	}{
		{"mode gate denies", func(c *Client) {
			c.SetSandboxSpoolRelays(func(string, int, string) error { return errors.New("advisory-only") }, nil)
		}, "o/r", "advisory-only"},
		{"no authorizer fails closed", func(c *Client) {
			// The pod-local issue authorizer is not borrowed by the spool.
			c.issueAuthz = func(string, int, string) error { return nil }
			c.SetSandboxSpoolRelays(nil, nil)
		}, "o/r", "fail closed"},
		{"placeholder repo refused", func(c *Client) {
			c.SetSandboxSpoolRelays(func(string, int, string) error { return nil }, nil)
		}, "owner/repo", "placeholder"},
		{"repo scope refused", func(c *Client) {
			c.SetSandboxSpoolRelays(func(string, int, string) error { return nil }, nil)
			c.SetAgentRepoScopeFunc(func(agent, repo string) bool { return agent != "deps-bump" })
		}, "o/r", "not scoped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := 0
			srv := newIssueMockServer(t, "", &created, nil, nil)
			defer srv.Close()
			c := testClient(t, srv.URL)
			withReviewDir(t)
			withIssueDir(t)
			manifests := withSandboxSpools(t)
			spool, _ := newSandboxSpool(t, manifests, "deps-bump", false)
			tc.setup(c)

			reqPath := writeSignedSpoolKind(t, spool, "i.json", testSpoolKey("deps-bump"), crux.SandboxSpoolKindIssue,
				IssueRequest{Repo: tc.repo, Title: "t", Body: "b"})
			c.ProcessReviewRequestsOnce(context.Background())

			if created != 0 {
				t.Fatalf("refused request created %d issues", created)
			}
			if res := readIssueResultFile(t, reqPath); res.OK || !strings.Contains(res.Error, tc.want) {
				t.Errorf("result = %+v, want an error containing %q", res, tc.want)
			}
		})
	}
}

// The kind is bound into the MAC: an issue request signed as a review (the
// MAC hive-review computes) or under another run's key is rejected, and a
// copy of an accepted one is a replay whatever kind it now claims.
func TestSandboxSpoolIssue_MACBindsKindAndRefusesReplay(t *testing.T) {
	created := 0
	srv := newIssueMockServer(t, "", &created, nil, nil)
	defer srv.Close()
	c := testClient(t, srv.URL)
	withReviewDir(t)
	withIssueDir(t)
	manifests := withSandboxSpools(t)
	spool, _ := newSandboxSpool(t, manifests, "deps-bump", false)
	c.SetSandboxSpoolRelays(func(string, int, string) error { return nil }, nil)
	key := testSpoolKey("deps-bump")

	raw, _ := json.Marshal(IssueRequest{Repo: "o/r", Title: "t", Body: "b"})
	crossKind, _ := json.Marshal(crux.SandboxReviewEnvelope{Kind: crux.SandboxSpoolKindIssue, Request: string(raw), MAC: crux.SandboxReviewMAC(key, raw)})
	if err := os.WriteFile(filepath.Join(spool, "cross.json"), crossKind, 0o660); err != nil {
		t.Fatal(err)
	}
	otherRun := writeSignedSpoolKind(t, spool, "other.json", testSpoolKey("other-run"), crux.SandboxSpoolKindIssue, IssueRequest{Repo: "o/r", Title: "t2", Body: "b"})
	unknown := writeSignedSpoolKind(t, spool, "unknown.json", key, "merge", IssueRequest{Repo: "o/r", Title: "t3", Body: "b"})
	c.ProcessReviewRequestsOnce(context.Background())

	if created != 0 {
		t.Fatalf("mis-signed requests created %d issues", created)
	}
	for _, p := range []string{filepath.Join(spool, "cross.json"), otherRun, unknown} {
		if _, err := os.Lstat(p + ".rejected"); err != nil {
			t.Errorf("%s should be set aside as .rejected: %v", filepath.Base(p), err)
		}
	}

	first := writeSignedSpoolKind(t, spool, "first.json", key, crux.SandboxSpoolKindIssue, IssueRequest{Repo: "o/r", Title: "t", Body: "b"})
	copied, _ := os.ReadFile(first)
	c.ProcessReviewRequestsOnce(context.Background())
	if created != 1 {
		t.Fatalf("signed request: %d issues created, want 1", created)
	}
	replay := filepath.Join(spool, "replay.json")
	if err := os.WriteFile(replay, copied, 0o660); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if created != 1 {
		t.Fatalf("replayed request created an issue: %d, want 1", created)
	}
	if _, err := os.Lstat(replay + ".rejected"); err != nil {
		t.Errorf("replay should be set aside as .rejected: %v", err)
	}
}

// fakeTopicStore records the calls a topic handler receives and answers
// from a real bead store, as cmd/hive's handler does.
func fakeTopicHandler(t *testing.T, calls *[]SandboxTopicCall) SandboxTopicHandler {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return func(call SandboxTopicCall) SandboxTopicResponse {
		*calls = append(*calls, call)
		switch call.Op {
		case SandboxTopicOpClaim:
			var claim beads.TopicClaim
			var err error
			if call.Pending {
				claim, err = store.ClaimTopicForBranch(call.Agent, call.Key, call.Repo, call.Branch)
			} else {
				claim, err = store.ClaimTopic(call.Agent, beads.TopicClaim{Key: call.Key, Repo: call.Repo, PR: call.PR})
			}
			if err != nil {
				return SandboxTopicResponse{Error: err.Error(), Refused: errors.Is(err, beads.ErrTopicClaimed)}
			}
			return SandboxTopicResponse{OK: true, Claim: &claim, Pending: claim.PR == 0}
		case SandboxTopicOpRelease:
			released, err := store.ReleaseTopic(call.Key, beads.TopicStateReleased)
			if err != nil {
				return SandboxTopicResponse{Error: err.Error()}
			}
			return SandboxTopicResponse{OK: true, Released: released}
		case SandboxTopicOpList:
			return SandboxTopicResponse{OK: true, Claims: store.OpenTopics()}
		}
		claim, ok := store.Topic(call.Key)
		if !ok {
			return SandboxTopicResponse{Refused: true, Error: "no open claim"}
		}
		return SandboxTopicResponse{OK: true, Claim: &claim}
	}
}

// End to end: a sandboxed kick claims a topic for the PR hive will open from
// it (no PR number yet), reads it back, and releases it. Every call is the
// manifest's agent's, whatever the request says.
func TestSandboxSpoolTopic_ClaimGetReleaseAttributedToManifestAgent(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	withReviewDir(t)
	manifests := withSandboxSpools(t)
	spool := newKickSpool(t, manifests, "deps-bump", "o/r", "hive/deps-bump-1")
	var calls []SandboxTopicCall
	c.SetSandboxSpoolRelays(nil, fakeTopicHandler(t, &calls))
	key := testSpoolKey("deps-bump")

	claim := writeSignedSpoolKind(t, spool, "claim.json", key, crux.SandboxSpoolKindTopic, SandboxTopicRequest{Op: "claim", Key: "Deps-Bump", Agent: "someone-else"})
	c.ProcessReviewRequestsOnce(context.Background())
	res := readTopicResultFile(t, claim)
	if !res.OK || !res.Pending || res.Claim == nil || res.Claim.Repo != "o/r" || res.Claim.Branch != "hive/deps-bump-1" || res.Claim.PR != 0 || res.Claim.Actor != "deps-bump" {
		t.Fatalf("claim result = %+v (%+v), want a pending claim by deps-bump on the kick's branch", res, res.Claim)
	}
	if _, err := os.Stat(claim); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("claim request still in the spool: %v", err)
	}

	get := writeSignedSpoolKind(t, spool, "get.json", key, crux.SandboxSpoolKindTopic, SandboxTopicRequest{Op: "get", Key: "deps-bump"})
	list := writeSignedSpoolKind(t, spool, "list.json", key, crux.SandboxSpoolKindTopic, SandboxTopicRequest{Op: "list"})
	c.ProcessReviewRequestsOnce(context.Background())
	if r := readTopicResultFile(t, get); !r.OK || r.Claim == nil || r.Claim.Key != "deps-bump" {
		t.Errorf("get = %+v, want the claim", r)
	}
	if r := readTopicResultFile(t, list); !r.OK || len(r.Claims) != 1 {
		t.Errorf("list = %+v, want one claim", r)
	}

	release := writeSignedSpoolKind(t, spool, "release.json", key, crux.SandboxSpoolKindTopic, SandboxTopicRequest{Op: "release", Key: "deps-bump"})
	c.ProcessReviewRequestsOnce(context.Background())
	if r := readTopicResultFile(t, release); !r.OK || !r.Released {
		t.Errorf("release = %+v, want released", r)
	}
	for _, call := range calls {
		if call.Agent != "deps-bump" {
			t.Errorf("call %+v attributed to %q, want the manifest's agent", call, call.Agent)
		}
	}
}

// A claim with a PR number is recorded as given; a second claim on the same
// open topic for another PR is refused and the refusal reaches the sandbox.
func TestSandboxSpoolTopic_ExplicitPRAndRefusal(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	withReviewDir(t)
	manifests := withSandboxSpools(t)
	spool := newKickSpool(t, manifests, "deps-bump", "o/r", "hive/deps-bump-1")
	var calls []SandboxTopicCall
	c.SetSandboxSpoolRelays(nil, fakeTopicHandler(t, &calls))
	key := testSpoolKey("deps-bump")

	first := writeSignedSpoolKind(t, spool, "a.json", key, crux.SandboxSpoolKindTopic, SandboxTopicRequest{Op: "claim", Key: "k", Repo: "o/r", PR: 5})
	c.ProcessReviewRequestsOnce(context.Background())
	if r := readTopicResultFile(t, first); !r.OK || r.Claim == nil || r.Claim.PR != 5 {
		t.Fatalf("claim = %+v, want #5", r)
	}
	second := writeSignedSpoolKind(t, spool, "b.json", key, crux.SandboxSpoolKindTopic, SandboxTopicRequest{Op: "claim", Key: "k", Repo: "o/r", PR: 6})
	c.ProcessReviewRequestsOnce(context.Background())
	if r := readTopicResultFile(t, second); r.OK || !r.Refused {
		t.Errorf("second claim = %+v, want refused", r)
	}
	if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("refused request still in the spool: %v", err)
	}
}

// Topic gates: no handler fails closed, a repo the agent is not scoped to is
// refused, and malformed requests never reach the store.
func TestSandboxSpoolTopic_GatesRefuse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		branch  string
		handler bool
		scope   func(agent, repo string) bool
		req     SandboxTopicRequest
		want    string
	}{
		{"no handler fails closed", "b", false, nil, SandboxTopicRequest{Op: "claim", Key: "k", PR: 1}, "fail closed"},
		{"repo scope", "b", true, func(string, string) bool { return false }, SandboxTopicRequest{Op: "claim", Key: "k", PR: 1}, "not scoped"},
		{"unknown op", "b", true, nil, SandboxTopicRequest{Op: "steal", Key: "k"}, "unknown topic op"},
		{"bad key", "b", true, nil, SandboxTopicRequest{Op: "get", Key: "a b"}, "whitespace"},
		{"negative pr", "b", true, nil, SandboxTopicRequest{Op: "claim", Key: "k", PR: -1}, "positive"},
		{"no PR and the kick opens none", "", true, nil, SandboxTopicRequest{Op: "claim", Key: "k"}, "needs --pr"},
		{"no PR in another repo", "b", true, nil, SandboxTopicRequest{Op: "claim", Key: "k", Repo: "o/other"}, "binds to the pull request"},
		{"no PR on another branch", "b", true, nil, SandboxTopicRequest{Op: "claim", Key: "k", Branch: "elsewhere"}, "this kick's branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, "http://127.0.0.1:1")
			withReviewDir(t)
			manifests := withSandboxSpools(t)
			spool := newKickSpool(t, manifests, "deps-bump", "o/r", tc.branch)
			var calls []SandboxTopicCall
			if tc.handler {
				c.SetSandboxSpoolRelays(nil, fakeTopicHandler(t, &calls))
			}
			if tc.scope != nil {
				c.SetAgentRepoScopeFunc(tc.scope)
			}
			p := writeSignedSpoolKind(t, spool, "t.json", testSpoolKey("deps-bump"), crux.SandboxSpoolKindTopic, tc.req)
			c.ProcessReviewRequestsOnce(context.Background())

			if len(calls) != 0 {
				t.Fatalf("refused request reached the store: %+v", calls)
			}
			if r := readTopicResultFile(t, p); r.OK || !strings.Contains(r.Error, tc.want) {
				t.Errorf("result = %+v, want an error containing %q", r, tc.want)
			}
		})
	}
}

// A topic file that is not JSON past the in-flight grace is quarantined with
// a result; one still inside the grace waits.
func TestSandboxSpoolTopic_BadJSON(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	dir := t.TempDir()
	p := filepath.Join(dir, "t.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := reviewRequestSource{agent: "deps-bump"}
	c.handleOneSandboxTopicRequest(p, src, time.Now)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("a fresh torn file must be left for the next pass: %v", err)
	}
	late := func() time.Time { return time.Now().Add(time.Hour) }
	c.handleOneSandboxTopicRequest(p, src, late)
	if _, err := os.Lstat(p + ".bad"); err != nil {
		t.Errorf("bad JSON should be quarantined: %v", err)
	}
	if r := readTopicResultFile(t, p); r.OK || !strings.Contains(r.Error, "invalid JSON") {
		t.Errorf("result = %+v", r)
	}
	// A vanished file is a no-op.
	c.handleOneSandboxTopicRequest(filepath.Join(dir, "gone.json"), src, time.Now)
}

func TestSandboxTopicCall_ResolvesAgainstManifest(t *testing.T) {
	src := reviewRequestSource{agent: "deps", repo: "o/r", branch: "hive/deps-1"}
	call, reason := sandboxTopicCall(SandboxTopicRequest{Op: " Claim ", Key: "K", Agent: "liar"}, src)
	if reason != "" || call.Agent != "deps" || call.Key != "k" || call.Repo != "o/r" || call.Branch != "hive/deps-1" || !call.Pending {
		t.Fatalf("call = %+v, %q", call, reason)
	}
	call, reason = sandboxTopicCall(SandboxTopicRequest{Op: "claim", Key: "k", Repo: "o/x", PR: 3, Branch: "b"}, src)
	if reason != "" || call.Pending || call.Repo != "o/x" || call.PR != 3 {
		t.Fatalf("explicit call = %+v, %q", call, reason)
	}
	if _, reason := sandboxTopicCall(SandboxTopicRequest{Op: "list"}, reviewRequestSource{}); !strings.Contains(reason, "manifest agent") {
		t.Errorf("no agent: %q", reason)
	}
	if _, reason := sandboxTopicCall(SandboxTopicRequest{Op: "claim", Key: "k", PR: 1}, reviewRequestSource{agent: "deps"}); !strings.Contains(reason, "--repo") {
		t.Errorf("no repo: %q", reason)
	}
}

func TestSetSandboxSpoolRelays_NilClient(t *testing.T) {
	var c *Client
	c.SetSandboxSpoolRelays(nil, nil) // must not panic
}

// Two kinds never share a staged name, so their results cannot collide in
// the spool: a name staged as one kind waits.
func TestSandboxSpool_StagedNameIsUniqueAcrossKinds(t *testing.T) {
	staging := t.TempDir()
	if stagedAnywhere(staging, "x.json") || stagingHasRequest(staging) {
		t.Fatal("empty staging reports a request")
	}
	dir := stagingFor(staging, crux.SandboxSpoolKindTopic)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !stagedAnywhere(staging, "x.json") || !stagingHasRequest(staging) {
		t.Error("a staged topic request is not seen")
	}
	if stagingFor(staging, crux.SandboxSpoolKindReview) != staging {
		t.Error("reviews must stay at the staging root")
	}
}
