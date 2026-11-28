package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
)

type fakeReadAPI struct {
	pull     forge.PullView
	issue    forge.IssueView
	state    string
	statuses []forge.CommitStatus
	err      error
	calls    int
}

func (f *fakeReadAPI) PullView(context.Context, string, int) (forge.PullView, error) {
	f.calls++
	return f.pull, f.err
}

func (f *fakeReadAPI) IssueView(context.Context, string, int) (forge.IssueView, error) {
	f.calls++
	return f.issue, f.err
}

func (f *fakeReadAPI) CommitStatuses(context.Context, string, string) (string, []forge.CommitStatus, error) {
	f.calls++
	return f.state, f.statuses, f.err
}

type readHarness struct {
	*relayHarness
	api     *fakeReadAPI
	chowned []int
}

func newReadHarness(t *testing.T) *readHarness {
	t.Helper()
	h := &readHarness{relayHarness: newRelayHarness(t), api: &fakeReadAPI{
		pull: forge.PullView{Number: 50, Title: "Add x", Body: "Fixes #1", State: "open", Mergeable: true, Author: "hive-bot",
			Labels: []string{"lgtm"}, HeadRef: "feat", HeadSHA: "abc", Base: "main", URL: "https://fj.example/acme/widgets/pulls/50"},
		issue: forge.IssueView{Number: 3, Title: "Bug", Body: "broken", State: "open", Author: "reporter", Labels: []string{"bug"}},
		state: "failure",
		statuses: []forge.CommitStatus{
			{Context: "ci/build", State: "success", TargetURL: "u1"},
			{Context: "ci/test", State: "failure"},
		},
	}}
	uid := os.Getuid()
	h.relay.withCloneRelay(func(agent string, fileUID int) (agentmode.TokenTier, error) {
		if agent != "worker" || fileUID != uid {
			return "", errors.New("not this agent's request")
		}
		return agentmode.TokenTierContributor, nil
	})
	prevAPI, prevChown := forgeReadAPIForRepo, forgeCloneChown
	t.Cleanup(func() { forgeReadAPIForRepo, forgeCloneChown = prevAPI, prevChown })
	forgeReadAPIForRepo = func(*config.Config, string) (forge.ReadAPI, error) { return h.api, nil }
	forgeCloneChown = func(_ *os.File, uid int) error {
		h.chowned = append(h.chowned, uid)
		return nil
	}
	return h
}

// readView serves one request and returns the delivered view, decoded.
func (h *readHarness) readView(t *testing.T, req forgeReadRequest, into any) forgeReadResponse {
	t.Helper()
	path := writeRelayRequest(t, h.relay.readDir, "worker-1.json", req)
	h.relay.scanOnce(context.Background())
	var resp forgeReadResponse
	readRelayResult(t, path, &resp)
	if !resp.OK {
		t.Fatalf("resp = %+v", resp)
	}
	assertExists(t, path, false)
	if filepath.Dir(resp.View) != h.relay.readDir || !strings.HasPrefix(filepath.Base(resp.View), "worker-1.") || !strings.HasSuffix(resp.View, forgeReadViewSuffix) {
		t.Fatalf("view path %q", resp.View)
	}
	fi, err := os.Lstat(resp.View)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("view mode = %v, want a regular 0600 file", fi.Mode())
	}
	if len(h.chowned) != 1 || h.chowned[0] != os.Getuid() {
		t.Fatalf("view given to %v, want the request file's owner %d", h.chowned, os.Getuid())
	}
	b, err := os.ReadFile(resp.View)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("view %q: %v", b, err)
	}
	return resp
}

// Forgejo reports mergeable=false on a merged or closed pull request, and on
// an open one both for a conflict and while its merge check is still pending,
// so every false reads as gh's UNKNOWN, never CONFLICTING.
func TestForgeReadViewMergeable(t *testing.T) {
	for _, tc := range []struct {
		state            string
		merged, mergable bool
		want             string
	}{
		{"open", false, true, "MERGEABLE"},
		{"open", false, false, "UNKNOWN"},
		{"closed", true, false, "UNKNOWN"},
		{"closed", false, false, "UNKNOWN"},
	} {
		api := &fakeReadAPI{pull: forge.PullView{Number: 7, State: tc.state, Merged: tc.merged, Mergeable: tc.mergable}}
		b, err := forgeReadView(context.Background(), api, "acme/widgets", "pr", 7)
		if err != nil {
			t.Fatal(err)
		}
		var view map[string]any
		if err := json.Unmarshal(b, &view); err != nil {
			t.Fatal(err)
		}
		if view["mergeable"] != tc.want {
			t.Errorf("state=%s merged=%v mergeable=%v: got %v, want %s", tc.state, tc.merged, tc.mergable, view["mergeable"], tc.want)
		}
	}
}

func TestForgeReadRelayPullView(t *testing.T) {
	h := newReadHarness(t)
	var view map[string]any
	resp := h.readView(t, forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "pr", Number: 50}, &view)
	if resp.Repo != "acme/widgets" || resp.Kind != "pr" || resp.Number != 50 {
		t.Errorf("resp = %+v", resp)
	}
	for key, want := range map[string]any{
		"title": "Add x", "body": "Fixes #1", "state": "OPEN", "author": "hive-bot", "headRefOid": "abc",
		"headRefName": "feat", "baseRefName": "main", "mergeable": "MERGEABLE", "statusCheckState": "FAILURE", "forge": "forgejo",
	} {
		if view[key] != want {
			t.Errorf("view[%q] = %v, want %v", key, view[key], want)
		}
	}
	if got := fmt.Sprint(view["labels"]); got != "[lgtm]" {
		t.Errorf("labels = %s", got)
	}
	rollup, _ := view["statusCheckRollup"].([]any)
	if len(rollup) != 2 || fmt.Sprint(rollup[1]) != "map[conclusion:FAILURE name:ci/test status:FAILURE]" {
		t.Errorf("statusCheckRollup = %v", view["statusCheckRollup"])
	}
}

func TestForgeReadRelayIssueAndChecks(t *testing.T) {
	h := newReadHarness(t)
	var issue map[string]any
	h.readView(t, forgeReadRequest{Agent: "worker", Repo: "acme/widgets", Kind: "issue", Number: 3}, &issue)
	if issue["title"] != "Bug" || issue["body"] != "broken" || issue["state"] != "OPEN" || fmt.Sprint(issue["labels"]) != "[bug]" {
		t.Errorf("issue view = %v", issue)
	}

	h = newReadHarness(t)
	var checks []map[string]any
	h.readView(t, forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "checks", Number: 50}, &checks)
	if len(checks) != 2 || checks[0]["bucket"] != "pass" || checks[1]["bucket"] != "fail" || checks[1]["state"] != "FAILURE" || checks[0]["link"] != "u1" {
		t.Errorf("checks view = %v", checks)
	}
}

func TestForgeReadRelayRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*readHarness, *forgeReadRequest)
		suffix string
		errHas string
	}{
		"wrong agent":    {func(_ *readHarness, r *forgeReadRequest) { r.Agent = "intruder" }, ".denied", "authorization denied"},
		"nil authz":      {func(h *readHarness, _ *forgeReadRequest) { h.relay.cloneAuthz = nil }, ".denied", "fail closed"},
		"bad kind":       {func(_ *readHarness, r *forgeReadRequest) { r.Kind = "diff" }, ".rejected", "not one of"},
		"bad number":     {func(_ *readHarness, r *forgeReadRequest) { r.Number = 0 }, ".rejected", "positive"},
		"unmanaged repo": {func(_ *readHarness, r *forgeReadRequest) { r.Repo = "other" }, ".rejected", "not managed"},
		"out of scope": {func(h *readHarness, _ *forgeReadRequest) {
			h.cfg.Project.Repos = []string{"widgets", "gadgets"}
			h.cfg.Agents = map[string]config.AgentConfig{"worker": {Repos: []string{"gadgets"}}}
		}, ".rejected", "read rejected"},
		"gitea repo": {func(h *readHarness, _ *forgeReadRequest) {
			h.cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "widgets", Forge: config.ForgeGitea}}
		}, ".rejected", "forgejo repos only"},
		"forge 404": {func(h *readHarness, _ *forgeReadRequest) {
			h.api.err = errors.New("forgejo: read pull request acme/widgets#50: GET /pulls/50 returned status 404")
		}, ".rejected", "404"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newReadHarness(t)
			req := forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "pr", Number: 50}
			tc.mutate(h, &req)
			path := writeRelayRequest(t, h.relay.readDir, "a.json", req)
			h.relay.scanOnce(context.Background())
			if name != "forge 404" && h.api.calls != 0 {
				t.Fatalf("read the forge on a refused request (%d calls)", h.api.calls)
			}
			if len(h.chowned) != 0 {
				t.Fatal("a view was handed out on a refused request")
			}
			var resp forgeReadResponse
			readRelayResult(t, path, &resp)
			if resp.OK || resp.View != "" || !strings.Contains(resp.Error, tc.errHas) {
				t.Errorf("resp = %+v, want an error containing %q", resp, tc.errHas)
			}
			assertExists(t, path+tc.suffix, true)
		})
	}
}

// A transient forge failure leaves the request for the next scan.
func TestForgeReadRelayRetriesTransientFailure(t *testing.T) {
	h := newReadHarness(t)
	h.api.err = errors.New("connection reset")
	path := writeRelayRequest(t, h.relay.readDir, "a.json", forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "issue", Number: 3})
	h.relay.scanOnce(context.Background())
	var resp forgeReadResponse
	readRelayResult(t, path, &resp)
	if resp.OK || resp.Attempts != 1 || !strings.Contains(resp.Error, "connection reset") {
		t.Fatalf("resp = %+v", resp)
	}
	assertExists(t, path, true)
}

// The relay idles unless every repo is off GitHub, so a GitHub hive's
// hive-gh is never answered from here.
func TestForgeReadRelayIdlesUnlessForgeOnly(t *testing.T) {
	h := newReadHarness(t)
	h.cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "gh-repo", Forge: config.ForgeGitHub}}
	path := writeRelayRequest(t, h.relay.readDir, "a.json", forgeReadRequest{Agent: "worker", Repo: "widgets", Kind: "pr", Number: 50})
	h.relay.scanOnce(context.Background())
	assertExists(t, path, true)
	assertExists(t, strings.TrimSuffix(path, ".json")+".result.json", false)
}

func TestForgeReadRelayExpiresOldViews(t *testing.T) {
	h := newReadHarness(t)
	old := filepath.Join(h.relay.readDir, "worker-1.abc"+forgeReadViewSuffix)
	fresh := filepath.Join(h.relay.readDir, "worker-2.abc"+forgeReadViewSuffix)
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * forgeReadViewTTL)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	h.relay.scanOnce(context.Background())
	assertExists(t, old, false)
	assertExists(t, fresh, true)
}
