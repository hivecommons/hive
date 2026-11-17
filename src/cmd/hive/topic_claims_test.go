package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
)

func topicTestStores(t *testing.T, names ...string) map[string]*beads.Store {
	t.Helper()
	out := make(map[string]*beads.Store, len(names))
	for _, name := range names {
		s, err := beads.NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		out[name] = s
	}
	return out
}

func quietTopicLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestTopicClaimLookupProjectsEveryStore(t *testing.T) {
	stores := topicTestStores(t, "deps", "release")
	stores["empty"] = nil
	_, _ = stores["deps"].ClaimTopic("deps", beads.TopicClaim{Key: "deps-bump", Repo: "acme/widget", PR: 3, Branch: "b"})
	_, _ = stores["release"].ClaimTopic("release", beads.TopicClaim{Key: "rel", Repo: "acme/widget", PR: 4})
	got := topicClaimLookup(stores)()
	if len(got) != 2 || got[0].Agent != "deps" || got[0].Key != "deps-bump" || got[0].Branch != "b" || got[1].Agent != "release" || got[1].PR != 4 {
		t.Fatalf("lookup = %+v", got)
	}
}

func TestReleaseTopicClaimsForPR(t *testing.T) {
	stores := topicTestStores(t, "deps")
	stores["nil"] = nil
	_, _ = stores["deps"].ClaimTopic("deps", beads.TopicClaim{Key: "k", Repo: "acme/widget", PR: 3})
	if n := releaseTopicClaimsForPR(stores, "", 3, beads.TopicStateMerged, quietTopicLogger()); n != 0 {
		t.Fatalf("empty repo released %d", n)
	}
	if n := releaseTopicClaimsForPR(stores, "widget", 3, beads.TopicStateMerged, quietTopicLogger()); n != 1 {
		t.Fatalf("released %d, want 1", n)
	}
	if _, ok := stores["deps"].Topic("k"); ok {
		t.Fatal("claim still open after merge")
	}
}

func TestTopicStateMappings(t *testing.T) {
	if topicStateForPR(github.PullRequest{MergedAt: time.Now()}) != beads.TopicStateMerged ||
		topicStateForPR(github.PullRequest{State: "merged"}) != beads.TopicStateMerged ||
		topicStateForPR(github.PullRequest{State: "closed"}) != beads.TopicStateClosed ||
		topicStateForPR(github.PullRequest{State: "open"}) != "" {
		t.Fatal("topicStateForPR mismatch")
	}
	if topicStateForTerminal(github.PRStateMerged) != beads.TopicStateMerged ||
		topicStateForTerminal(github.PRStateClosed) != beads.TopicStateClosed ||
		topicStateForTerminal("open") != "" {
		t.Fatal("topicStateForTerminal mismatch")
	}
	if topicStateForAudit(github.AuditActionPRMerged) != beads.TopicStateMerged ||
		topicStateForAudit(github.AuditActionPRClosed) != beads.TopicStateClosed ||
		topicStateForAudit("other") != "" {
		t.Fatal("topicStateForAudit mismatch")
	}
}

func TestSyncTopicClaimsFollowsRenameAndReleases(t *testing.T) {
	stores := topicTestStores(t, "deps")
	stores["nil"] = nil
	s := stores["deps"]
	_, _ = s.ClaimTopic("deps", beads.TopicClaim{Key: "renamed", Repo: "acme/widget", PR: 1, Branch: "old"})
	_, _ = s.ClaimTopic("deps", beads.TopicClaim{Key: "merged", Repo: "acme/widget", PR: 2})
	_, _ = s.ClaimTopic("deps", beads.TopicClaim{Key: "closed", Repo: "acme/widget", PR: 3})
	_, _ = s.ClaimTopic("deps", beads.TopicClaim{Key: "unseen", Repo: "acme/widget", PR: 4, Branch: "keep"})
	_, _ = s.ClaimTopic("deps", beads.TopicClaim{Key: "nohead", Repo: "acme/widget", PR: 5, Branch: "keep"})

	actionable := &github.ActionableResult{PRs: github.PRResult{
		Items: []github.PullRequest{
			{Repo: "widget", Number: 1, HeadRef: "new", State: "open"},
			{Repo: "widget", Number: 2, HeadRef: "x", State: "open"},
			{Repo: "widget", Number: 5, State: "open"},
		},
		Held:       []github.PullRequest{{Repo: "acme/widget", Number: 3, State: "closed"}},
		Attributed: []github.PullRequest{{Repo: "acme/widget", Number: 2, MergedAt: time.Now(), State: "closed"}},
	}}
	syncTopicClaims(stores, actionable, quietTopicLogger())

	open := map[string]beads.TopicClaim{}
	for _, c := range s.OpenTopics() {
		open[c.Key] = c
	}
	if len(open) != 3 {
		t.Fatalf("open claims = %+v", open)
	}
	if open["renamed"].Branch != "new" {
		t.Errorf("branch did not follow rename: %+v", open["renamed"])
	}
	if open["unseen"].Branch != "keep" || open["nohead"].Branch != "keep" {
		t.Errorf("unrelated claims changed: %+v", open)
	}
	for _, b := range s.List(beads.ListFilter{}) {
		switch b.Meta(beads.MetaTopicKey) {
		case "merged":
			if b.Meta(beads.MetaTopicState) != beads.TopicStateMerged {
				t.Errorf("merged claim state = %q", b.Meta(beads.MetaTopicState))
			}
		case "closed":
			if b.Meta(beads.MetaTopicState) != beads.TopicStateClosed {
				t.Errorf("closed claim state = %q", b.Meta(beads.MetaTopicState))
			}
		}
	}

	syncTopicClaims(stores, nil, quietTopicLogger())
	syncTopicClaims(nil, actionable, quietTopicLogger())
}

// A pending claim from a sandboxed kick (hivecommons/hive#18572) binds to the
// PR opened from its branch, preferring an open PR, and stays pending until
// one appears.
func TestSyncTopicClaimsBindsPendingClaims(t *testing.T) {
	stores := topicTestStores(t, "deps")
	s := stores["deps"]
	_, _ = s.ClaimTopicForBranch("deps", "bump", "acme/widget", "hive/deps-1")
	_, _ = s.ClaimTopicForBranch("deps", "waiting", "acme/widget", "hive/deps-2")

	actionable := &github.ActionableResult{PRs: github.PRResult{
		Items:      []github.PullRequest{{Repo: "widget", Number: 21, HeadRef: "hive/deps-1", State: "open"}},
		Attributed: []github.PullRequest{{Repo: "acme/widget", Number: 20, HeadRef: "hive/deps-1", State: "closed"}},
	}}
	syncTopicClaims(stores, actionable, quietTopicLogger())

	open := map[string]beads.TopicClaim{}
	for _, c := range s.OpenTopics() {
		open[c.Key] = c
	}
	if open["bump"].PR != 21 {
		t.Errorf("pending claim bound to %d, want the open PR 21", open["bump"].PR)
	}
	if open["waiting"].PR != 0 {
		t.Errorf("claim with no PR on its branch was bound: %+v", open["waiting"])
	}
	bindPendingTopicClaim(s, "deps", nil, beads.TopicClaim{Key: "x"}, nil) // no branch: no-op
}

func TestSandboxTopicHandler(t *testing.T) {
	stores := topicTestStores(t, "deps")
	h := sandboxTopicHandler(func() map[string]*beads.Store { return stores })

	if r := h(github.SandboxTopicCall{Agent: "ghost", Op: github.SandboxTopicOpList}); r.OK || r.Error == "" {
		t.Errorf("unknown agent = %+v, want an error", r)
	}
	if r := sandboxTopicHandler(nil)(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpList}); r.OK {
		t.Errorf("no stores = %+v, want an error", r)
	}

	r := h(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpClaim, Key: "bump", Repo: "acme/widget", Branch: "hive/deps-1", Pending: true})
	if !r.OK || !r.Pending || r.Claim == nil || r.Claim.Actor != "deps" {
		t.Fatalf("pending claim = %+v", r)
	}
	r = h(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpClaim, Key: "rel", Repo: "acme/widget", PR: 4})
	if !r.OK || r.Pending || r.Claim.PR != 4 {
		t.Fatalf("claim = %+v", r)
	}
	r = h(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpClaim, Key: "rel", Repo: "acme/widget", PR: 5})
	if r.OK || !r.Refused || r.Claim == nil || r.Claim.PR != 4 {
		t.Fatalf("second claim = %+v, want refused naming #4", r)
	}
	if r := h(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpClaim, Key: "x"}); r.OK || r.Refused {
		t.Errorf("invalid claim = %+v, want a plain error", r)
	}
	if r := h(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpGet, Key: "bump"}); !r.OK || !r.Pending {
		t.Errorf("get = %+v", r)
	}
	if r := h(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpGet, Key: "none"}); r.OK || !r.Refused {
		t.Errorf("get missing = %+v, want refused", r)
	}
	if r := h(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpList}); !r.OK || len(r.Claims) != 2 {
		t.Errorf("list = %+v", r)
	}
	if r := h(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpRelease, Key: "bump"}); !r.OK || !r.Released {
		t.Errorf("release = %+v", r)
	}
	if r := h(github.SandboxTopicCall{Agent: "deps", Op: github.SandboxTopicOpRelease, Key: ""}); r.OK {
		t.Errorf("release with no key = %+v", r)
	}
	if r := h(github.SandboxTopicCall{Agent: "deps", Op: "steal"}); r.OK {
		t.Errorf("unknown op = %+v", r)
	}
}
