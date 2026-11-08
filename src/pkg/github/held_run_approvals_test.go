package github

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// The contributor lane's held-run ledger: one pass per PR head, a refusal
// stands until the head changes or the App gains `actions: write`.
func TestHeldRunApprovalsOncePerHeadAndRefusal(t *testing.T) {
	var l HeldRunApprovals
	if l.Attempted("acme/widget", 7, "sha7") {
		t.Fatal("Attempted on an empty ledger = true")
	}
	if !l.Claim("acme/widget", 7, "sha7") {
		t.Fatal("first Claim = false, want true")
	}
	if l.Claim("ACME/Widget", 7, "SHA7") {
		t.Fatal("second Claim on the same head = true, want false")
	}
	if _, refused := l.Refused("acme/widget", 7, "sha7"); refused {
		t.Fatal("Refused before any refusal = true")
	}
	l.RecordRefusal("acme/widget", 7, "sha7", []int64{12, 11}, "read", true)
	runs, refused := l.Refused("acme/widget", 7, "sha7")
	if !refused || !slices.Equal(runs, []int64{12, 11}) {
		t.Fatalf("Refused = %v %v, want [12 11] true", runs, refused)
	}

	// Same grant, a lower one, an unreadable one: the refusal stands.
	for _, g := range []struct {
		level string
		known bool
	}{{"read", true}, {"", true}, {"write", false}} {
		if l.ReleaseOnGrant("acme/widget", 7, "sha7", g.level, g.known) {
			t.Fatalf("ReleaseOnGrant(%q, %v) released, want the refusal to stand", g.level, g.known)
		}
	}
	// The App gained actions: write: one more pass.
	if !l.ReleaseOnGrant("acme/widget", 7, "sha7", "write", true) {
		t.Fatal("ReleaseOnGrant(write) = false, want a release")
	}
	if l.Attempted("acme/widget", 7, "sha7") || !l.Claim("acme/widget", 7, "sha7") {
		t.Fatal("released head is not claimable again")
	}
	// Refused again while holding write: no further release.
	l.RecordRefusal("acme/widget", 7, "sha7", []int64{11}, "write", true)
	if l.ReleaseOnGrant("acme/widget", 7, "sha7", "admin", true) {
		t.Fatal("ReleaseOnGrant after a refusal recorded at write = true, want false")
	}

	// A new head re-arms the pass and forgets the old refusal.
	if _, refused := l.Refused("acme/widget", 7, "sha8"); refused {
		t.Fatal("Refused for a new head = true")
	}
	if !l.Claim("acme/widget", 7, "sha8") {
		t.Fatal("Claim on a new head = false, want true")
	}
	if _, refused := l.Refused("acme/widget", 7, "sha7"); refused {
		t.Fatal("old head's refusal survived the new head")
	}
	// A refusal recorded with an unknown grant releases once write shows.
	l.RecordRefusal("acme/widget", 7, "sha8", []int64{11}, "", false)
	if !l.ReleaseOnGrant("acme/widget", 7, "sha8", "write", true) {
		t.Fatal("ReleaseOnGrant after an unknown-grant refusal = false, want true")
	}
	// Other PRs are independent; a nil ledger is inert.
	if !l.Claim("acme/widget", 8, "sha7") {
		t.Fatal("Claim on another PR = false")
	}
	var nilLedger *HeldRunApprovals
	if !nilLedger.Claim("acme/widget", 7, "sha7") || nilLedger.Attempted("acme/widget", 7, "sha7") {
		t.Fatal("nil ledger is not inert")
	}
}

func TestHeldRunApprovalsEvictsStaleEntries(t *testing.T) {
	var l HeldRunApprovals
	l.Claim("acme/widget", 7, "sha7")
	l.mu.Lock()
	e := l.entries[heldRunApprovalKey("acme/widget", 7)]
	e.seen = time.Now().Add(-heldRunApprovalTTL - time.Minute)
	l.entries[heldRunApprovalKey("acme/widget", 7)] = e
	l.mu.Unlock()
	if !l.Claim("acme/widget", 9, "sha9") {
		t.Fatal("Claim on another PR = false")
	}
	if l.Attempted("acme/widget", 7, "sha7") {
		t.Fatal("stale entry survived the TTL")
	}
}

func TestInstallationPermissionCachesTheGrant(t *testing.T) {
	c := NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:1")
	if level, known := c.InstallationPermission(context.Background(), "actions"); known || level != "" {
		t.Fatalf("token client = %q %v, want unknown", level, known)
	}
	c.appAuth = &AppAuth{}
	calls := stubInstallation(t, map[string]string{"actions": "write", "issues": "write"}, nil)
	for i := 0; i < 3; i++ {
		if level, known := c.InstallationPermission(context.Background(), "Actions"); !known || level != "write" {
			t.Fatalf("read %d = %q %v, want write true", i, level, known)
		}
	}
	if *calls != 1 {
		t.Fatalf("installation reads = %d, want 1 within the TTL", *calls)
	}

	failing := NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:1")
	failing.appAuth = &AppAuth{}
	stubInstallation(t, nil, errors.New("boom"))
	if _, known := failing.InstallationPermission(context.Background(), "actions"); known {
		t.Fatal("failed read reported known")
	}
}
