package dashboard

import (
	"path/filepath"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Lifecycle replay for #3980, wiring a REAL ClaimLedger in as the
// Dependencies.IssueClaimed hook (the stub-based suppression test lives in
// contribute_ws_claimed_issue_test.go). This pins the dashboard-side contract
// against the actual ledger semantics across the claim's whole life: while the
// referencing PR is open its issue is withheld from every contributor, and the
// authoritative rescan after the PR closes releases the issue back into the
// offer pool — the cooldown path alone must not be what ends the suppression.

// openReferenceClaim mirrors the incident: PR #3898 saying "Refs #3498",
// deliberately non-closing.
func openReferenceClaim(repo string, number int) ghpkg.IssueClaim {
	return ghpkg.IssueClaim{
		Repo: repo, Issue: number,
		PRNumber: 3898, PRRepo: repo,
		PRURL:          "https://github.com/projectbluefin/dakota/pull/3898",
		PRAuthor:       "Danathar",
		ObservedAt:     time.Now(),
		ExternalAuthor: true,
		Reference:      true,
	}
}

func TestReferenceClaim_LedgerLifecycleReplay(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)

	ledger := ghpkg.NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), s.logger)
	ledger.Reconcile([]ghpkg.IssueClaim{openReferenceClaim("projectbluefin/dakota", 353)}, true)
	s.deps.IssueClaimed = ledger.Lookup

	// Round 1: #353 is reference-claimed by the open Refs-PR, so the
	// contributor is handed #360 instead of re-deriving "I already did this".
	connA := claimedIssueConn()
	hub.mu.Lock()
	hub.connections["conn-a"] = connA
	hub.mu.Unlock()
	msg := hub.selectTask(connA)
	if msg == nil || msg.Type != "task_assign" || msg.Number != 360 {
		t.Fatalf("reference-claimed #353 must be skipped for #360, got %+v", msg)
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
		t.Fatalf("expected task_unavailable/no_matching_work while the claim holds, got %+v", msg)
	}

	// Round 3: the Refs-PR closes; the authoritative rescan sees no open PRs,
	// releases the claim, and #353 re-enters the offer pool.
	ledger.Reconcile(nil, true)
	if msg := hub.selectTask(connB); msg == nil || msg.Type != "task_assign" || msg.Number != 353 {
		t.Fatalf("released issue must be offerable again, got %+v", msg)
	}
}

// TestReferenceClaim_EmptyLedgerPositiveControl proves the suppression above
// comes from the claim and nothing else: the same real-ledger wiring with an
// EMPTY ledger leaves selection exactly as before.
func TestReferenceClaim_EmptyLedgerPositiveControl(t *testing.T) {
	hub, s := covK2Hub(t)
	claimedIssueStatus(s)
	ledger := ghpkg.NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), s.logger)
	s.deps.IssueClaimed = ledger.Lookup

	msg := hub.selectTask(claimedIssueConn())
	if msg == nil || msg.Type != "task_assign" || msg.Number != 353 {
		t.Fatalf("with no claims, first eligible issue #353 must be offered, got %+v", msg)
	}
}
