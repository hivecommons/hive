package automerge

import (
	"context"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"strings"
	"testing"
)

// Audit F3: the label-queued auto-merge sweep had NO trusted-merger check.
// trySweepQueuedPR proved only that the queuer was not the PR author
// (self-merge ban), so anything that could get the merger-queue label applied
// had its PR merged regardless of WHO asked, and a sockpuppet pair (author A,
// queuer B, neither trusted) defeated the ban outright. These tests pin the
// tier gate at the point the merge actually happens.
//
// The sweep is LIVE on v4: cmd/hive/main.go calls SweepQueuedAutoMerges from
// runAutoMergeSweepIfDue off the governor eval tick.

// greenQueuedPR is a PR that passes every gate EXCEPT whatever the test varies.
func greenQueuedPR(number int, author, queuedBy string) sweepPR {
	return sweepPR{
		number:          number,
		author:          author,
		queuedBy:        queuedBy,
		label:           true,
		mergeableState:  "clean",
		statusState:     "success",
		checkStatus:     "completed",
		checkConclusion: "success",
	}
}

// newUngatedSweepClient builds a sweep client with NO merger authorizer — the
// pre-fix shape, used to assert the fail-closed default.
func newUngatedSweepClient(apiURL string) *Engine {
	c := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	c.SetAppBotLogin(testHiveAppBotLogin)
	return New(c, Options{})
}

// TestF3_TrustedMergerMergesGreenQueuedPR is the POSITIVE CONTROL: a merger-tier
// actor queuing someone else's green PR must still merge. A gate that blocked
// this would be worthless.
func TestF3_TrustedMergerMergesGreenQueuedPR(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7, "alice", "bob")}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 1 || len(merged) != 1 || merged[0] != 7 {
		t.Fatalf("merged result=%v merge calls=%v, want PR 7 merged by trusted merger bob", result.Merged, merged)
	}
}

// TestF3_UntrustedMergerDoesNotMerge is the core regression: an actor with no
// merger tier must not be able to merge, even though every other gate is green
// and the self-merge ban is satisfied.
func TestF3_UntrustedMergerDoesNotMerge(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7, "alice", "mallory")}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want untrusted queuer mallory REFUSED (F3 regression)", result.Merged, merged)
	}
	if result.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", result.Skipped)
	}
	// Restart the engine: dedupe must survive process-local state being lost.
	c = newAutoMergeSweepClient(api.URL)
	_, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
	if err != nil || reason != autoMergeReasonUntrustedMerger {
		t.Fatalf("repeat sweep reason=%q err=%v", reason, err)
	}
	comments, _, err := c.gh.Issues.ListComments(context.Background(), "acme", "widget", 7, nil)
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments=%v err=%v, want one notice across sweeps", comments, err)
	}
	for _, text := range []string{"untrusted-merger", "@mallory", "sha7", "dashboard.authorized_users"} {
		if !strings.Contains(comments[0].GetBody(), text) {
			t.Errorf("notice %q missing %q", comments[0].GetBody(), text)
		}
	}
	// A new head gets its own notice, while repeated lane re-checks do not.
	if err := c.commentUntrustedMerger(context.Background(), "acme", "widget", 7, "newsha", "mallory"); err != nil {
		t.Fatal(err)
	}
	if err := c.commentUntrustedMerger(context.Background(), "acme", "widget", 7, "newsha", "mallory"); err != nil {
		t.Fatal(err)
	}
	comments, _, err = c.gh.Issues.ListComments(context.Background(), "acme", "widget", 7, nil)
	if err != nil || len(comments) != 2 {
		t.Fatalf("comments=%v err=%v, want one notice for each head", comments, err)
	}
}

// TestF3_SockpuppetPairDoesNotMerge covers the case the self-merge ban cannot
// see: two DISTINCT untrusted accounts, so author != queuer and the ban passes,
// but neither holds the merger tier.
func TestF3_SockpuppetPairDoesNotMerge(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7, "mallory", "mallory-alt")}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want sockpuppet pair REFUSED (F3 regression)", result.Merged, merged)
	}
}

// TestF3_SelfMergeBanStillHolds proves the earlier fix (#3413) is intact: a
// TRUSTED merger still cannot merge their OWN PR. Trust must not become a
// bypass of the self-merge ban.
func TestF3_SelfMergeBanStillHolds(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7, "bob", "BOB")}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want trusted merger BLOCKED from self-merge", result.Merged, merged)
	}
}

// TestF3_NoAuthorizerFailsClosed pins the fail-closed default: with no
// authorizer installed, an unclassifiable actor must NOT merge.
func TestF3_NoAuthorizerFailsClosed(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7, "alice", "bob")}, &merged)
	defer api.Close()

	c := newUngatedSweepClient(api.URL)
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepQueuedAutoMerges returned error: %v", err)
	}
	if len(result.Merged) != 0 || len(merged) != 0 {
		t.Fatalf("merged result=%v merge calls=%v, want fail-CLOSED with no authorizer", result.Merged, merged)
	}
}

// TestF3_TrySweepQueuedPRReportsUntrustedReason pins the skip reasons so the
// operator can tell an untrusted queuer apart from an unconfigured hive.
func TestF3_TrySweepQueuedPRReportsUntrustedReason(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7, "alice", "mallory")}, &merged)
	defer api.Close()

	gated := newAutoMergeSweepClient(api.URL)
	_, reason, err := gated.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
	if err != nil {
		t.Fatalf("trySweepQueuedPR returned error: %v", err)
	}
	if reason != autoMergeReasonUntrustedMerger {
		t.Fatalf("reason = %q, want %q", reason, autoMergeReasonUntrustedMerger)
	}

	ungated := newUngatedSweepClient(api.URL)
	_, reason, err = ungated.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
	if err != nil {
		t.Fatalf("trySweepQueuedPR returned error: %v", err)
	}
	if reason != autoMergeReasonNoMergerAuthz {
		t.Fatalf("reason = %q, want %q", reason, autoMergeReasonNoMergerAuthz)
	}
}

// TestF3_IsTrustedMergerFailsClosedOnEdgeCases covers the helper directly.
func TestF3_IsTrustedMergerFailsClosedOnEdgeCases(t *testing.T) {
	var nilClient *Engine
	if allowed, configured := nilClient.isTrustedMerger("bob"); allowed || configured {
		t.Fatalf("nil client = (%v,%v), want (false,false)", allowed, configured)
	}

	c := New(hgithub.NewClient("token", "acme", []string{"widget"}, nil, ""), Options{})
	if allowed, configured := c.isTrustedMerger("bob"); allowed || configured {
		t.Fatalf("no authorizer = (%v,%v), want (false,false)", allowed, configured)
	}

	c.SetMergerAuthorizer(func(login string) bool {
		return strings.EqualFold(login, "bob")
	})
	// An empty login must never be trusted, even with an authorizer installed.
	if allowed, configured := c.isTrustedMerger("   "); allowed || !configured {
		t.Fatalf("blank login = (%v,%v), want (false,true)", allowed, configured)
	}
	if allowed, _ := c.isTrustedMerger("bob"); !allowed {
		t.Fatal("bob should be trusted")
	}
	if allowed, _ := c.isTrustedMerger("mallory"); allowed {
		t.Fatal("mallory must not be trusted")
	}
}

func TestQueueApprovalRevokedBeforeLaneExecutionPostsNotice(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7, "alice", "bob")}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	ctx := context.Background()
	if err := c.queueApprovalStillHolds(ctx, "acme", "widget", 7, "sha7"); err != nil {
		t.Fatalf("trusted approval rejected: %v", err)
	}
	c.SetMergerAuthorizer(func(string) bool { return false })
	for range 2 {
		if err := c.queueApprovalStillHolds(ctx, "acme", "widget", 7, "sha7"); err == nil {
			t.Fatal("revoked merger must fail the lane's final authorization")
		}
	}
	comments, _, err := c.gh.Issues.ListComments(ctx, "acme", "widget", 7, nil)
	if err != nil || len(comments) != 1 || !strings.Contains(comments[0].GetBody(), "@bob") {
		t.Fatalf("comments=%v err=%v, want one notice for revoked merger bob", comments, err)
	}
}
