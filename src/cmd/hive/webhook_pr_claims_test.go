package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#14814: the governor's PRClaimsHook must write the ledger,
// persist it, and drop the claimed issue from the cached actionable snapshot
// on the webhook — not on the next enumeration.
func TestPRClaimsHookRecordsAndFiltersSnapshot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "claims.json")
	ledger := github.NewClaimLedger(path, logger)

	var target atomic.Pointer[github.ActionableResult]
	target.Store(&github.ActionableResult{GeneratedAt: now, Issues: github.IssueResult{
		Count: 2, Items: []github.Issue{{Repo: "repo", Number: 14765}, {Repo: "repo", Number: 14766}},
	}})
	var patched []int
	patch := &snapshotPatcher{target: &target, logger: logger, onPatch: func(n int) { patched = append(patched, n) }}

	hook := prClaimsHookFor(func() *github.ClaimLedger { return ledger }, patch, logger)
	hook(github.PRClaimDelivery{
		Repo: "repo", Number: 14772, Action: "opened", State: github.PRStateOpen, At: now,
		Claims: []github.IssueClaim{{Repo: "repo", Issue: 14765, PRNumber: 14772, PRRepo: "repo", PRAuthor: "owner", ExternalAuthor: true, ObservedAt: now}},
	})

	if _, ok := ledger.Lookup("repo", 14765); !ok {
		t.Fatal("claim not recorded in the ledger")
	}
	cur := target.Load()
	if cur.Issues.Count != 1 || len(cur.Issues.Items) != 1 || cur.Issues.Items[0].Number != 14766 {
		t.Fatalf("snapshot after hook = %+v, want only #14766", cur.Issues)
	}
	if len(patched) != 1 || patched[0] != 1 {
		t.Fatalf("onPatch calls = %v, want [1]", patched)
	}
	// Persisted: a fresh ledger from the same path sees the claim.
	reloaded, err := github.LoadClaimLedger(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Lookup("repo", 14765); !ok {
		t.Fatal("claim not persisted")
	}

	// A delivery with nothing to apply does not touch the snapshot.
	hook(github.PRClaimDelivery{Repo: "repo", Number: 99, Action: "edited", State: github.PRStateOpen, At: now})
	if len(patched) != 1 {
		t.Fatalf("onPatch calls after no-op delivery = %v", patched)
	}

	// Nil ledger resolver / nil patcher are safe.
	prClaimsHookFor(nil, nil, logger)(github.PRClaimDelivery{Repo: "repo", Number: 1})
	prClaimsHookFor(func() *github.ClaimLedger { return nil }, nil, logger)(github.PRClaimDelivery{Repo: "repo", Number: 1})
	if (*snapshotPatcher)(nil).suppressClaimed(ledger, logger) != 0 {
		t.Fatal("nil patcher must be a no-op")
	}
}
