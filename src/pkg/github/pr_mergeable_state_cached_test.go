package github

import (
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// TestCachedPRMergeableStateIsHeadScoped: the stage route reads a held PR's
// mergeability from the detail the enumeration already fetched, never with
// a request, and only for exactly the head it asks about. An older head's
// state, an uncomputed state and an empty cache say nothing.
func TestCachedPRMergeableStateIsHeadScoped(t *testing.T) {
	c := &Client{org: "acme"}
	const number = 4242
	store := func(key, head, state string) {
		t.Helper()
		pr := &gh.PullRequest{Number: gh.Ptr(number), Head: &gh.PullRequestBranch{SHA: gh.Ptr(head)}}
		if state != "" {
			pr.MergeableState = gh.Ptr(state)
		}
		c.storePRDetail(key, number, pr)
	}
	t.Cleanup(func() {
		sharedPRDetailCache.invalidate("acme/held-conflict-cache", number)
		sharedPRDetailCache.invalidate("held-conflict-cache", number)
	})

	if s, ok := c.CachedPRMergeableState("acme/held-conflict-cache", number, "head1"); ok {
		t.Fatalf("empty cache answered %q", s)
	}

	// The enumeration stores under the configured (bare) repo name.
	store("held-conflict-cache", "head1", "dirty")
	if s, ok := c.CachedPRMergeableState("acme/held-conflict-cache", number, "head1"); !ok || s != "dirty" {
		t.Fatalf("current head = %q, %v; want dirty", s, ok)
	}
	if s, ok := c.CachedPRMergeableState("held-conflict-cache", number, "head1"); !ok || s != "dirty" {
		t.Fatalf("bare repo name = %q, %v; want dirty", s, ok)
	}
	if s, ok := c.CachedPRMergeableState("acme/held-conflict-cache", number, "head2"); ok {
		t.Fatalf("a newer head read the older head's state %q", s)
	}
	if s, ok := c.CachedPRMergeableState("other/held-conflict-cache", number, "head1"); ok {
		t.Fatalf("another org's repo read this one's state %q", s)
	}

	// A full-name entry for the new head wins; GitHub has not computed it.
	store("acme/held-conflict-cache", "head2", "unknown")
	sharedPRDetailCache.invalidate("held-conflict-cache", number)
	if s, ok := c.CachedPRMergeableState("acme/held-conflict-cache", number, "head2"); ok {
		t.Fatalf("unknown mergeability answered %q", s)
	}
	store("acme/held-conflict-cache", "head2", "")
	if s, ok := c.CachedPRMergeableState("acme/held-conflict-cache", number, "head2"); ok {
		t.Fatalf("missing mergeability answered %q", s)
	}
	store("acme/held-conflict-cache", "head2", "Clean")
	if s, ok := c.CachedPRMergeableState("acme/held-conflict-cache", number, "head2"); !ok || s != "clean" {
		t.Fatalf("clean head = %q, %v; want clean", s, ok)
	}

	var nilClient *Client
	if _, ok := nilClient.CachedPRMergeableState("acme/held-conflict-cache", number, "head2"); ok {
		t.Fatal("nil client answered")
	}
	if _, ok := c.CachedPRMergeableState("acme/held-conflict-cache", number, " "); ok {
		t.Fatal("empty head answered")
	}
}
