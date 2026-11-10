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

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agentmode"
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

// Only the sec-check role refinement of the contributor tier requests
// Dependabot alerts read (#17426, least privilege): every other tier's token
// must not carry it.
func TestScopedToken_OnlySecCheckTierRequestsVulnerabilityAlerts(t *testing.T) {
	var gotPerms map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotPerms = decodePermissions(t, r)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "scoped-token",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	auth := newWorkflowsTestAuth(t, server.URL)

	for _, tier := range []string{"advisor", "reviewer", "newcomer", "contributor", "trusted", "merger", crux.SharedUIDReadTokenTier} {
		if _, err := auth.ScopedToken(context.Background(), tier); err != nil {
			t.Fatalf("ScopedToken(%q): %v", tier, err)
		}
		if got, present := gotPerms["vulnerability_alerts"]; present {
			t.Errorf("tier %q requests vulnerability_alerts=%q; only the sec-check tier may", tier, got)
		}
	}
	if _, err := auth.ScopedToken(context.Background(), agentmode.SecCheckTier); err != nil {
		t.Fatalf("ScopedToken(sec-check): %v", err)
	}
	if _, err := auth.ScopedToken(context.Background(), agentmode.SecCheckTrustedTier); err != nil {
		t.Fatalf("ScopedToken(sec-check-trusted): %v", err)
	}
	if gotPerms["vulnerability_alerts"] != "read" {
		t.Errorf("sec-check-trusted tier vulnerability_alerts = %q, want read", gotPerms["vulnerability_alerts"])
	}
	if _, err := auth.ScopedToken(context.Background(), agentmode.SecCheckTier); err != nil {
		t.Fatalf("ScopedToken(sec-check): %v", err)
	}
	if gotPerms["vulnerability_alerts"] != "read" {
		t.Errorf("sec-check tier vulnerability_alerts = %q, want read", gotPerms["vulnerability_alerts"])
	}
	for _, perm := range []string{"contents", "issues", "pull_requests"} {
		if gotPerms[perm] != "write" {
			t.Errorf("sec-check tier %s = %q, want write (it is the contributor set plus alerts read)", perm, gotPerms[perm])
		}
	}
}

// The sec-check-trusted tier (#17496) is the trusted set plus Dependabot
// alerts read, and degrades to a token without only the alerts grant (keeping
// Workflows) when the installation has accepted Workflows but not alerts.
func TestScopedToken_SecCheckTrustedTierAlertsWithFallback(t *testing.T) {
	var requests []map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		perms := decodePermissions(t, r)
		requests = append(requests, perms)
		if perms["vulnerability_alerts"] != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"message": "The permissions requested are not granted to this installation."})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "scoped-token-trusted",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := newWorkflowsTestAuth(t, server.URL)
	if _, err := auth.ScopedToken(context.Background(), agentmode.SecCheckTrustedTier); err != nil {
		t.Fatalf("ScopedToken: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("mint requests = %d, want 2", len(requests))
	}
	first := requests[0]
	if first["vulnerability_alerts"] != "read" {
		t.Errorf("first mint vulnerability_alerts = %q, want read", first["vulnerability_alerts"])
	}
	for perm, want := range map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "checks": "read", "workflows": "write"} {
		if first[perm] != want {
			t.Errorf("first mint %s = %q, want %q (trusted set plus alerts read)", perm, first[perm], want)
		}
	}
	if _, present := requests[1]["vulnerability_alerts"]; present {
		t.Errorf("retry still requests vulnerability_alerts: %v", requests[1])
	}
	if requests[1]["workflows"] != "write" {
		t.Errorf("retry workflows = %q, want write (only the alerts grant is dropped)", requests[1]["workflows"])
	}
}

// An installation that accepted Dependabot alerts but not Workflows (#4677,
// #17496): GitHub's 422 does not name the refused permission, so the broker
// must not conclude the alerts grant is missing. The final token keeps
// vulnerability_alerts:read and drops only workflows.
func TestScopedToken_SecCheckTrustedTierKeepsAlertsWhenWorkflowsRefused(t *testing.T) {
	var requests []map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		perms := decodePermissions(t, r)
		requests = append(requests, perms)
		if perms["workflows"] != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"message": "The permissions requested are not granted to this installation."})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "scoped-token-alerts",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := newWorkflowsTestAuth(t, server.URL)
	token, err := auth.ScopedToken(context.Background(), agentmode.SecCheckTrustedTier)
	if err != nil {
		t.Fatalf("ScopedToken: %v", err)
	}
	if token != "scoped-token-alerts" {
		t.Fatalf("token = %q, want the alerts-capable mint's token", token)
	}
	if len(requests) != 3 {
		t.Fatalf("mint requests = %d, want 3 (with both, without alerts, without workflows)", len(requests))
	}
	last := requests[len(requests)-1]
	if last["vulnerability_alerts"] != "read" {
		t.Errorf("final mint vulnerability_alerts = %q, want read (the grant is accepted; only workflows was refused)", last["vulnerability_alerts"])
	}
	if _, present := last["workflows"]; present {
		t.Errorf("final mint still requests workflows: %v", last)
	}
	for _, perm := range []string{"contents", "issues", "pull_requests"} {
		if last[perm] != "write" {
			t.Errorf("final mint %s = %q, want write", perm, last[perm])
		}
	}
}

// An installation that accepted neither Workflows nor Dependabot alerts still
// mints the bare trusted set for the sec-check-trusted tier.
func TestScopedToken_SecCheckTrustedTierDropsBothWhenNeitherGranted(t *testing.T) {
	var requests []map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		perms := decodePermissions(t, r)
		requests = append(requests, perms)
		if perms["workflows"] != "" || perms["vulnerability_alerts"] != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"message": "The permissions requested are not granted to this installation."})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "scoped-token-bare",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := newWorkflowsTestAuth(t, server.URL)
	token, err := auth.ScopedToken(context.Background(), agentmode.SecCheckTrustedTier)
	if err != nil {
		t.Fatalf("ScopedToken: %v", err)
	}
	if token != "scoped-token-bare" {
		t.Fatalf("token = %q, want the bare trusted mint's token", token)
	}
	if len(requests) != 4 {
		t.Fatalf("mint requests = %d, want 4", len(requests))
	}
	last := requests[len(requests)-1]
	for _, perm := range []string{"workflows", "vulnerability_alerts"} {
		if _, present := last[perm]; present {
			t.Errorf("final mint still requests %s: %v", perm, last)
		}
	}
	if last["contents"] != "write" {
		t.Errorf("final mint contents = %q, want write", last["contents"])
	}
}

// The sec-check tier requests Dependabot alerts read and degrades to a token
// without it when the installation has not accepted the permission.
func TestScopedToken_SecCheckVulnerabilityAlertsWithFallback(t *testing.T) {
	var requests []map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		perms := decodePermissions(t, r)
		requests = append(requests, perms)
		if perms["vulnerability_alerts"] != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"message": "The permissions requested are not granted to this installation."})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "scoped-token-contributor",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := newWorkflowsTestAuth(t, server.URL)
	if _, err := auth.ScopedToken(context.Background(), agentmode.SecCheckTier); err != nil {
		t.Fatalf("ScopedToken: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("mint requests = %d, want 2", len(requests))
	}
	if requests[0]["vulnerability_alerts"] != "read" {
		t.Errorf("first mint vulnerability_alerts = %q, want read", requests[0]["vulnerability_alerts"])
	}
	if _, present := requests[1]["vulnerability_alerts"]; present {
		t.Errorf("retry still requests vulnerability_alerts: %v", requests[1])
	}
	if requests[1]["pull_requests"] != "write" {
		t.Errorf("retry pull_requests = %q, want write", requests[1]["pull_requests"])
	}
}

// A repo-scope 422 (renamed / removed repository, #7869) is not a missing
// permission grant: the contributor mint must fail on the first attempt so
// the hub's one-mint-per-repo fall-through accounting still holds.
func TestScopedToken_SecCheckRepoScopeErrorDoesNotRetry(t *testing.T) {
	var requests int
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]any{"message": "There is at least one repository that does not exist or is not accessible to the parent installation."})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := newWorkflowsTestAuth(t, server.URL)
	if _, err := auth.ScopedTokenForRepos(context.Background(), agentmode.SecCheckTier, []string{"gone"}); err == nil {
		t.Fatal("ScopedTokenForRepos: want the repo-scope error, got nil")
	}
	if requests != 1 {
		t.Fatalf("mint requests = %d, want 1 (a repo-scope 422 must not trigger the permission fallback)", requests)
	}
}
