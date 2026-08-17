package dashboard

import (
	"path/filepath"
	"testing"
	"time"

	ghpkg "github.com/kubestellar/hive/v2/pkg/github"
)

// #3980 regression suite for the contribute queue: an issue whose only open
// work is a PR referencing it WITHOUT a closing keyword (`Refs #N`) was
// invisible to the claim ledger, so selectTask re-offered it to the same
// contributor every 4h cooldown window forever, burning a full task cycle per
// re-offer. selectTask consults the ledger through Dependencies.IssueClaimed;
// with soft references recorded as claims (pkg/github FetchClaims), the same
// hook now suppresses this shape too. These tests wire a REAL ClaimLedger in
// as the hook, so the dashboard-side contract is pinned against the actual
// ledger semantics rather than a hand-rolled stub.

// softClaim is the #3980 incident signal: an open PR from the contributor's
// own client referencing the issue with `Refs #N` — deliberately non-closing
// because the issue is maintainer-gated.
func softClaim(repo string, number int) ghpkg.IssueClaim {
	return ghpkg.IssueClaim{
		Repo: repo, Issue: number,
		PRNumber: 3898, PRRepo: repo,
		PRURL:           "https://github.com/projectbluefin/dakota/pull/3898",
		PRAuthor:        "Danathar",
		ObservedAt:      time.Now(),
		FirstObservedAt: time.Now(),
		ExternalAuthor:  true,
		SoftReference:   true,
	}
}

// TestSoftClaimIssue_ReplaysThreeNineEightZero replays the full loop: while
// the Refs-PR is open its issue must be skipped (the next admissible issue is
// offered, then no_matching_work), and the moment an authoritative rescan
// releases the claim — the PR merged or closed — the issue is offerable again.
func TestSoftClaimIssue_ReplaysThreeNineEightZero(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)

	ledger := ghpkg.NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), s.logger)
	ledger.Reconcile([]ghpkg.IssueClaim{softClaim("projectbluefin/dakota", 353)}, true)
	s.deps.IssueClaimed = ledger.Lookup

	// Round 1: #353 is soft-claimed by the open Refs-PR, so the contributor
	// must be handed #360 instead of burning a cycle re-deriving "I already
	// did this".
	connA := claimedIssueConn()
	hub.mu.Lock()
	hub.connections["conn-a"] = connA
	hub.mu.Unlock()
	msg := hub.selectTask(connA)
	if msg == nil || msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %+v", msg)
	}
	if msg.Number != 360 {
		t.Fatalf("soft-claimed issue was offered anyway: got #%d, want #360", msg.Number)
	}

	// Round 2: with #360 active and #353 still claimed, a second contributor
	// gets an explicit negative-ack — never the claimed issue.
	connB := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "ct-b", ContributorID: "c-b", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
	hub.mu.Lock()
	hub.connections["conn-b"] = connB
	hub.mu.Unlock()
	if msg := hub.selectTask(connB); msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableNoMatchingWork {
		t.Fatalf("expected task_unavailable/no_matching_work while claim holds, got %+v", msg)
	}

	// Round 3: the Refs-PR closes; the authoritative rescan sees no open PRs
	// and releases the claim, so #353 re-enters the offer pool.
	ledger.Reconcile(nil, true)
	if msg := hub.selectTask(connB); msg == nil || msg.Type != "task_assign" || msg.Number != 353 {
		t.Fatalf("released issue must be offerable again, got %+v", msg)
	}
}

// TestSoftClaimIssue_PositiveControl proves the suppression comes from the
// claim and nothing else: an EMPTY ledger wired through the same hook leaves
// selection exactly as before, offering the first eligible issue.
func TestSoftClaimIssue_PositiveControl(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)
	ledger := ghpkg.NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), s.logger)
	s.deps.IssueClaimed = ledger.Lookup

	msg := hub.selectTask(claimedIssueConn())
	if msg == nil || msg.Type != "task_assign" || msg.Number != 353 {
		t.Fatalf("with no claims, first eligible issue #353 must be offered, got %+v", msg)
	}
}
