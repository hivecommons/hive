package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Regression tests for #7469. The reviewer runs in ADVISORY mode, and the
// advisor tier is read-only on pull requests — so a reviewer could compute a
// verdict but never say it out loud. Its only consumer was the merge
// eligibility gate, which on a spoke that does not auto-merge means the review
// was computed and discarded.
//
// The reviewer tier upgrades exactly one permission. The tests below pin both
// halves: that it gained the ability to speak, and that it gained nothing else.

// mintTierPermissions captures the permission set a tier asks GitHub for.
func mintTierPermissions(t *testing.T, tier string) map[string]string {
	t.Helper()
	var got map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		got = decodePermissions(t, r)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "scoped-token-" + tier,
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := newWorkflowsTestAuth(t, server.URL)
	if _, err := auth.ScopedToken(context.Background(), tier); err != nil {
		t.Fatalf("ScopedToken(%q): %v", tier, err)
	}
	return got
}

// The capability the reviewer exists to use: pull_requests:write is what lets
// it submit a PR review carrying a body, and request reviewers to route work
// to the right human.
func TestScopedToken_ReviewerTierCanWritePullRequests(t *testing.T) {
	perms := mintTierPermissions(t, "reviewer")

	if got := perms["pull_requests"]; got != "write" {
		t.Errorf("reviewer tier requested pull_requests=%q, want %q — without it the reviewer cannot post the verdict it just computed", got, "write")
	}
	// Reviewing is grounded in reading the tree at the merge base; without
	// contents:read every tarball fetch 403s (#4289).
	if got := perms["contents"]; got != "read" {
		t.Errorf("reviewer tier requested contents=%q, want %q", got, "read")
	}
}

// The safety asymmetry. These are the properties that make the reviewer safe
// to run on cadence against a queue it does not own.
func TestScopedToken_ReviewerTierGainsNothingElse(t *testing.T) {
	perms := mintTierPermissions(t, "reviewer")

	// GitHub cannot separate "comment on an issue" from "create an issue" —
	// both are the issues permission. The advisor tier omits it deliberately,
	// and the reviewer tier must keep omitting it.
	if got, ok := perms["issues"]; ok {
		t.Errorf("reviewer tier requested issues=%q; it must be absent — the permission that would let it comment on issues is the same one that lets it create them", got)
	}
	// Merging and pushing both require contents:write.
	if got := perms["contents"]; got == "write" {
		t.Error("reviewer tier requested contents=write; merging and pushing must stay impossible")
	}
	if got, ok := perms["workflows"]; ok {
		t.Errorf("reviewer tier requested workflows=%q; it must be absent", got)
	}
	if got, ok := perms["administration"]; ok {
		t.Errorf("reviewer tier requested administration=%q; it must be absent", got)
	}
}

// The advisor tier must not have been widened by accident: agents that are
// advisory for reasons other than reviewing keep read-only pull requests.
func TestScopedToken_AdvisorTierStillReadOnly(t *testing.T) {
	perms := mintTierPermissions(t, "advisor")

	if got := perms["pull_requests"]; got != "read" {
		t.Errorf("advisor tier requested pull_requests=%q, want %q — only the reviewer role is widened", got, "read")
	}
	if got, ok := perms["issues"]; ok {
		t.Errorf("advisor tier requested issues=%q; it must stay absent", got)
	}
}
