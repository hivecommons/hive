package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Regression tests for #7469 and #7487. #7469 gave the reviewer tier
// pull_requests:write so an ADVISORY reviewer could say its verdict out loud.
// #7487 narrowed it back to read: hive already had a sanctioned route to the
// same outcome — the review-request relay in review_request_watcher.go, where
// the GOVERNOR submits the review with the App token, so it is authored by the
// App bot, lands on the audit/activity trail, and passes through the
// fail-closed canary scan. A direct-token review does none of those things.
//
// The tests below pin that the reviewer tier is read-only on pull requests and
// that it gained nothing else. The pull_requests assertion pins the VALUE, not
// merely the key's presence: an assertion that only checked presence is what
// would let a silent re-widening back to write slip through.

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

// #7487: the reviewer reads the pull request and routes its verdict through
// the audited relay; it does not write to the PR with its own token. Pinning
// the exact value — not the key's presence — is the point of this test.
func TestScopedToken_ReviewerTierPullRequestsIsReadOnly(t *testing.T) {
	perms := mintTierPermissions(t, "reviewer")

	if got := perms["pull_requests"]; got != "read" {
		t.Errorf("reviewer tier requested pull_requests=%q, want %q — the reviewer posts its verdict through the review-request relay, which is authored by the App bot, recorded on the audit trail, and canary-scanned; a direct-token write bypasses all three", got, "read")
	}
	// Reviewing is grounded in reading the tree at the merge base; without
	// contents:read every tarball fetch 403s (#4289).
	if got := perms["contents"]; got != "read" {
		t.Errorf("reviewer tier requested contents=%q, want %q", got, "read")
	}
	if got := perms["metadata"]; got != "read" {
		t.Errorf("reviewer tier requested metadata=%q, want %q", got, "read")
	}
}

// The reviewer tier must stay permission-identical to the advisor tier it is
// named apart from. The role plumbing (agentmode.TokenTierForRole) is what the
// distinct tier name exists for; the permission level is not part of it.
func TestScopedToken_ReviewerTierMatchesAdvisorPermissions(t *testing.T) {
	reviewer := mintTierPermissions(t, "reviewer")
	advisor := mintTierPermissions(t, "advisor")

	for _, key := range []string{"pull_requests", "contents", "metadata", "issues", "workflows", "administration", "checks"} {
		if reviewer[key] != advisor[key] {
			t.Errorf("reviewer tier %s=%q but advisor tier %s=%q — the reviewer tier must not widen beyond the advisor tier (#7487)", key, reviewer[key], key, advisor[key])
		}
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
	// #7487: no direct write on the pull request either. The governor
	// performs the write, not the agent.
	if got := perms["pull_requests"]; got == "write" {
		t.Error("reviewer tier requested pull_requests=write; the review must go through the audited relay so it is App-authored, observable, and canary-scanned")
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
