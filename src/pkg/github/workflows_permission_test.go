package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// These are the regression tests for #4677: agents could not open PRs
// touching .github/workflows because no tier ever requested the workflows
// permission, so even an App installation that GRANTS Workflows read & write
// minted tokens without it and GitHub rejected the push server-side.
//
// Trusted-tier mints now request workflows:write, with an honest fallback:
// requested permissions must be a subset of what the installation grants, so
// on installs that have not accepted the new permission the first mint fails
// and the broker retries without workflows rather than breaking every
// trusted-tier token.

func newWorkflowsTestAuth(t *testing.T, url string) *AppAuth {
	t.Helper()
	key, _ := generateTestKey(t)
	return &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		apiURL:         url,
	}
}

func decodePermissions(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	var body struct {
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decoding token request body: %v", err)
	}
	return body.Permissions
}

// TestScopedToken_TrustedTiersRequestWorkflowsWrite pins that trusted and
// merger mints ask for workflows:write, and that write-capable-but-untrusted
// and advisory tiers do NOT — the pushbroker's protected-path rejection of
// .github/workflows/ for sandboxed contributors stays meaningful only if
// their tokens cannot write workflows either.
func TestScopedToken_TrustedTiersRequestWorkflowsWrite(t *testing.T) {
	cases := []struct {
		tier          string
		wantWorkflows string // "" means the permission must be absent
	}{
		{tier: "trusted", wantWorkflows: "write"},
		{tier: "merger", wantWorkflows: "write"},
		{tier: "contributor", wantWorkflows: ""},
		{tier: "newcomer", wantWorkflows: ""},
		{tier: "advisor", wantWorkflows: ""},
	}
	for _, tc := range cases {
		t.Run("tier_"+tc.tier, func(t *testing.T) {
			var gotPerms map[string]string
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				gotPerms = decodePermissions(t, r)
				json.NewEncoder(w).Encode(map[string]any{
					"token":      "scoped-token-" + tc.tier,
					"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
				})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			auth := newWorkflowsTestAuth(t, server.URL)
			if _, err := auth.ScopedToken(context.Background(), tc.tier); err != nil {
				t.Fatalf("ScopedToken(%q): %v", tc.tier, err)
			}
			if got := gotPerms["workflows"]; got != tc.wantWorkflows {
				t.Errorf("tier %q workflows permission = %q, want %q (#4677)", tc.tier, got, tc.wantWorkflows)
			}
		})
	}
}

// TestScopedToken_WorkflowsFallbackWhenInstallationLacksGrant simulates an App
// installation that has not accepted the Workflows permission: the first mint
// (requesting workflows:write) is refused, and the retry without it must
// succeed so trusted-tier agents keep working — degraded, logged, not broken.
func TestScopedToken_WorkflowsFallbackWhenInstallationLacksGrant(t *testing.T) {
	var requests []map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		perms := decodePermissions(t, r)
		requests = append(requests, perms)
		if perms["workflows"] != "" {
			// GitHub refuses to mint a token requesting a permission the
			// installation does not grant.
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{
				"message": "The permissions requested are not granted to this installation.",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "scoped-token-fallback",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := newWorkflowsTestAuth(t, server.URL)
	token, err := auth.ScopedToken(context.Background(), "trusted")
	if err != nil {
		t.Fatalf("ScopedToken with workflows fallback: %v", err)
	}
	if token != "scoped-token-fallback" {
		t.Fatalf("token = %q, want the fallback mint's token", token)
	}
	if len(requests) != 2 {
		t.Fatalf("mint requests = %d, want 2 (initial with workflows, retry without)", len(requests))
	}
	if requests[0]["workflows"] != "write" {
		t.Errorf("first mint workflows = %q, want %q", requests[0]["workflows"], "write")
	}
	if _, present := requests[1]["workflows"]; present {
		t.Errorf("retry mint still requests workflows: %v", requests[1])
	}
	// The retry must not silently drop anything else the tier needs.
	for _, perm := range []string{"contents", "issues", "pull_requests"} {
		if requests[1][perm] != "write" {
			t.Errorf("retry mint %s = %q, want %q", perm, requests[1][perm], "write")
		}
	}
}
