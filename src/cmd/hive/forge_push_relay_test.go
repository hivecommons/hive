package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

const forgePushTestToken = "fj-push-token"

// gitCall is one git invocation the push relay made.
type gitCall struct {
	dir  string
	env  []string
	args []string
}

type pushHarness struct {
	*relayHarness
	calls    []gitCall
	pushErr  error
	defErr   error
	checkout string
}

func newPushHarness(t *testing.T) *pushHarness {
	t.Helper()
	t.Setenv(forgejoTestTokenEnv, forgePushTestToken)
	t.Cleanup(func() { proxy.SetForgeHosts(nil) })
	h := &pushHarness{relayHarness: newRelayHarness(t), checkout: t.TempDir()}
	uid := os.Getuid()
	h.relay.withPushRelay(func(agent string, fileUID int) error {
		if agent != "worker" || fileUID != uid {
			return errors.New("not this agent's request")
		}
		return nil
	})

	prevExec, prevDef := forgePushExec, forgeRepoDefaultBranch
	t.Cleanup(func() { forgePushExec, forgeRepoDefaultBranch = prevExec, prevDef })
	forgePushExec = func(_ context.Context, dir string, env []string, args ...string) (string, error) {
		h.calls = append(h.calls, gitCall{dir: dir, env: env, args: args})
		switch args[0] {
		case "rev-parse":
			return "0123456789abcdef0123456789abcdef01234567\n", nil
		case "push":
			if h.pushErr != nil {
				return "! [rejected] fix-1 -> fix-1 (non-fast-forward)\n", h.pushErr
			}
		}
		return "", nil
	}
	forgeRepoDefaultBranch = func(context.Context, *config.Config, string) (string, error) {
		return "main", h.defErr
	}
	return h
}

func (h *pushHarness) goodPush() github.PushBranchRequest {
	return github.PushBranchRequest{Repo: "widgets", Branch: "fix-1", Dir: h.checkout, Agent: "worker"}
}

func (h *pushHarness) pushCalls() []gitCall {
	var out []gitCall
	for _, c := range h.calls {
		if c.args[0] == "push" {
			out = append(out, c)
		}
	}
	return out
}

// A forge-only hive serves push-requests (#19916): the branch goes to the
// repo's Forgejo remote, with the forgejo token only as a header scoped to
// that remote, and the result names the pushed commit.
func TestForgeRelayPushesBranchToForgeRemote(t *testing.T) {
	h := newPushHarness(t)
	req := h.goodPush()
	req.ForceWithLease = true
	req.Branch = "refs/heads/fix-1"
	path := writeRelayRequest(t, h.relay.pushDir, "a.json", req)
	h.relay.scanOnce(context.Background())

	pushes := h.pushCalls()
	if len(pushes) != 1 {
		t.Fatalf("git calls = %+v, want one push", h.calls)
	}
	const remote = "https://fj.example/acme/widgets.git"
	want := []string{"push", "--force-with-lease", remote, "refs/heads/fix-1:refs/heads/fix-1"}
	if strings.Join(pushes[0].args, " ") != strings.Join(want, " ") {
		t.Errorf("push args = %v, want %v", pushes[0].args, want)
	}
	if pushes[0].dir != h.checkout {
		t.Errorf("push ran in %q, want the agent's checkout %q", pushes[0].dir, h.checkout)
	}
	env := strings.Join(pushes[0].env, "\n")
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+forgePushTestToken))
	if !strings.Contains(env, "http."+remote+".extraHeader") || !strings.Contains(env, header) {
		t.Errorf("push env does not scope the forge token to the remote:\n%s", env)
	}
	if !strings.Contains(env, "core.hooksPath") || !strings.Contains(env, "credential.helper") {
		t.Errorf("push env does not neutralise the checkout's hooks and helpers:\n%s", env)
	}
	for _, a := range pushes[0].args {
		if strings.Contains(a, forgePushTestToken) {
			t.Fatalf("the token reached git's argv: %v", pushes[0].args)
		}
	}

	var resp github.PushBranchResponse
	readRelayResult(t, path, &resp)
	if !resp.OK || resp.Branch != "fix-1" || resp.SHA != "0123456789abcdef0123456789abcdef01234567" || resp.Attempts != 1 {
		t.Errorf("resp = %+v", resp)
	}
	assertExists(t, path, false)
}

func TestForgeRelayPushRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*pushHarness, *github.PushBranchRequest)
		suffix string
		errHas string
	}{
		"wrong agent":    {func(_ *pushHarness, r *github.PushBranchRequest) { r.Agent = "intruder" }, ".denied", "authorization denied"},
		"nil authz":      {func(h *pushHarness, _ *github.PushBranchRequest) { h.relay.pushAuthz = nil }, ".denied", "fail closed"},
		"unmanaged repo": {func(_ *pushHarness, r *github.PushBranchRequest) { r.Repo = "other" }, ".rejected", "not managed"},
		"paused repo": {func(h *pushHarness, _ *github.PushBranchRequest) {
			h.cfg.Project.PausedRepos = []config.RepoPause{{Repo: "widgets"}}
		}, ".rejected", "push rejected"},
		"flag branch":    {func(_ *pushHarness, r *github.PushBranchRequest) { r.Branch = "-f" }, ".rejected", "not a valid branch name"},
		"refspec branch": {func(_ *pushHarness, r *github.PushBranchRequest) { r.Branch = "a:main" }, ".rejected", "not a valid branch name"},
		"relative dir":   {func(_ *pushHarness, r *github.PushBranchRequest) { r.Dir = "work/widgets" }, ".rejected", "absolute path"},
		"default branch": {func(_ *pushHarness, r *github.PushBranchRequest) { r.Branch = "refs/heads/Main" }, ".rejected", "default branch"},
		"no resolver":    {func(h *pushHarness, _ *github.PushBranchRequest) { h.relay.pushRemote = nil }, ".rejected", "fail closed"},
		"resolver refuses": {func(h *pushHarness, _ *github.PushBranchRequest) {
			h.relay.pushRemote = func(string) (pushbroker.ForgeRemote, bool, error) {
				return pushbroker.ForgeRemote{}, false, errors.New("cloning from and pushing to gitea is not implemented")
			}
		}, ".rejected", "not implemented"},
		"resolver says github": {func(h *pushHarness, _ *github.PushBranchRequest) {
			h.relay.pushRemote = func(string) (pushbroker.ForgeRemote, bool, error) { return pushbroker.ForgeRemote{}, false, nil }
		}, ".rejected", "no forge remote"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPushHarness(t)
			req := h.goodPush()
			tc.mutate(h, &req)
			path := writeRelayRequest(t, h.relay.pushDir, "a.json", req)
			h.relay.scanOnce(context.Background())

			if len(h.pushCalls()) != 0 {
				t.Fatalf("pushed on a refused request: %+v", h.calls)
			}
			var resp github.PushBranchResponse
			readRelayResult(t, path, &resp)
			if resp.OK || !strings.Contains(resp.Error, tc.errHas) {
				t.Errorf("resp = %+v, want an error containing %q", resp, tc.errHas)
			}
			assertExists(t, path+tc.suffix, true)
		})
	}
}

// A refused push is retried a bounded number of times, then quarantined, and
// each result counts the attempt.
func TestForgeRelayPushFailureRetriesThenQuarantines(t *testing.T) {
	h := newPushHarness(t)
	h.pushErr = errors.New("exit status 1")
	path := writeRelayRequest(t, h.relay.pushDir, "a.json", h.goodPush())
	for i := 1; i < forgeRelayMaxAttempts; i++ {
		h.relay.scanOnce(context.Background())
		assertExists(t, path, true)
		var resp github.PushBranchResponse
		readRelayResult(t, path, &resp)
		if resp.OK || resp.Attempts != i || !strings.Contains(resp.Error, "non-fast-forward") {
			t.Fatalf("attempt %d resp = %+v", i, resp)
		}
	}
	h.relay.scanOnce(context.Background())
	assertExists(t, path+".failed", true)
	if got := len(h.pushCalls()); got != forgeRelayMaxAttempts {
		t.Errorf("pushes = %d, want %d", got, forgeRelayMaxAttempts)
	}
}

// A failed default-branch lookup never lets the push through: it is retried.
func TestForgeRelayPushDefaultBranchLookupFailureRetries(t *testing.T) {
	h := newPushHarness(t)
	h.defErr = errors.New("forgejo API returned status 502")
	path := writeRelayRequest(t, h.relay.pushDir, "a.json", h.goodPush())
	h.relay.scanOnce(context.Background())
	if len(h.calls) != 0 {
		t.Fatalf("ran git without the default branch: %+v", h.calls)
	}
	assertExists(t, path, true)
	var resp github.PushBranchResponse
	readRelayResult(t, path, &resp)
	if resp.OK || !strings.Contains(resp.Error, "default branch") {
		t.Errorf("resp = %+v", resp)
	}
}

// An empty token is a mint failure, never an unauthenticated push.
func TestForgeRelayPushWithoutTokenRetries(t *testing.T) {
	h := newPushHarness(t)
	t.Setenv(forgejoTestTokenEnv, "")
	path := writeRelayRequest(t, h.relay.pushDir, "a.json", h.goodPush())
	h.relay.scanOnce(context.Background())
	if len(h.calls) != 0 {
		t.Fatalf("ran git without a token: %+v", h.calls)
	}
	var resp github.PushBranchResponse
	readRelayResult(t, path, &resp)
	if resp.OK || !strings.Contains(resp.Error, "minting push token") {
		t.Errorf("resp = %+v", resp)
	}
}

// Review and merge requests are answered with a reason on a forge-only hive,
// so the agent does not wait on a queue nothing reads (#19916).
func TestForgeRelayRejectsReviewAndMergeRequests(t *testing.T) {
	h := newPushHarness(t)
	review := writeRelayRequest(t, h.relay.reviewDir, "r.json", github.ReviewRequest{Repo: "widgets", Number: 7, Event: "approve", Agent: "worker"})
	merge := writeRelayRequest(t, h.relay.mergeDir, "m.json", github.MergeRequest{Repo: "widgets", Number: 8, Agent: "worker"})
	h.relay.scanOnce(context.Background())

	var rr github.ReviewResponse
	readRelayResult(t, review, &rr)
	if rr.OK || rr.Number != 7 || rr.Error != forgeRelayReviewUnsupported {
		t.Errorf("review resp = %+v", rr)
	}
	assertExists(t, review+".rejected", true)
	var mr github.MergeResponse
	readRelayResult(t, merge, &mr)
	if mr.OK || mr.Number != 8 || mr.Error != forgeRelayMergeUnsupported {
		t.Errorf("merge resp = %+v", mr)
	}
	assertExists(t, merge+".rejected", true)
	if len(h.writer.comments)+len(h.writer.specs) != 0 {
		t.Errorf("a rejected request wrote to the forge: %+v", h.writer)
	}
}

// On a hive with a GitHub repo the GitHub watchers own these queues, so the
// forge relay leaves them untouched and creates nothing.
func TestForgeRelayPushQueuesIdleUnlessForgeOnly(t *testing.T) {
	h := newPushHarness(t)
	h.cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "gh-repo", Forge: config.ForgeGitHub}}
	missing := filepath.Join(t.TempDir(), "merge-requests")
	h.relay.mergeDir = missing
	push := writeRelayRequest(t, h.relay.pushDir, "a.json", h.goodPush())
	review := writeRelayRequest(t, h.relay.reviewDir, "r.json", github.ReviewRequest{Repo: "widgets", Number: 7, Agent: "worker"})
	h.relay.scanOnce(context.Background())
	assertExists(t, push, true)
	assertExists(t, review, true)
	assertExists(t, missing, false)
	if len(h.calls) != 0 {
		t.Fatalf("ran git on a hive with a GitHub repo: %+v", h.calls)
	}

	// Forge-only: the missing queue is created, so requests can land.
	h.cfg.Project.RepoPolicies = nil
	h.relay.scanOnce(context.Background())
	if fi, err := os.Stat(missing); err != nil || !fi.IsDir() {
		t.Fatalf("merge queue not created on a forge-only hive: %v", err)
	}
	assertExists(t, push, false)
}

type fakeRepoForge struct {
	repo *forge.Repo
	err  error
}

func (f fakeRepoForge) ListOpenIssues(context.Context, string) ([]forge.Issue, error) {
	return nil, nil
}

func (f fakeRepoForge) ListOpenChangeRequests(context.Context, string) ([]forge.ChangeRequest, error) {
	return nil, nil
}

func (f fakeRepoForge) GetRepo(context.Context, string) (*forge.Repo, error) { return f.repo, f.err }

// listerOnly is an adapter that can list but not read a repo.
type listerOnly struct{}

func (listerOnly) ListOpenIssues(context.Context, string) ([]forge.Issue, error) { return nil, nil }

func (listerOnly) ListOpenChangeRequests(context.Context, string) ([]forge.ChangeRequest, error) {
	return nil, nil
}

func TestForgeRepoDefaultBranch(t *testing.T) {
	prev := forgeListerForRepo
	t.Cleanup(func() { forgeListerForRepo = prev })
	cfg := forgejoOnlyConfig("https://fj.example")
	ctx := context.Background()

	for name, tc := range map[string]struct {
		lister forgeLister
		err    error
		want   string
		errHas string
	}{
		"default":    {lister: fakeRepoForge{repo: &forge.Repo{DefaultBranch: "trunk"}}, want: "trunk"},
		"empty":      {lister: fakeRepoForge{repo: &forge.Repo{}}, errHas: "no default branch"},
		"nil repo":   {lister: fakeRepoForge{}, errHas: "no default branch"},
		"api error":  {lister: fakeRepoForge{err: errors.New("status 502")}, errHas: "status 502"},
		"no adapter": {err: errors.New("token host mismatch"), errHas: "token host mismatch"},
		"no getrepo": {lister: listerOnly{}, errHas: "cannot read repos"},
	} {
		t.Run(name, func(t *testing.T) {
			forgeListerForRepo = func(*config.Config, string) (forgeLister, error) { return tc.lister, tc.err }
			got, err := forgeRepoDefaultBranch(ctx, cfg, "widgets")
			if tc.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errHas) {
					t.Fatalf("err = %v, want %q", err, tc.errHas)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
