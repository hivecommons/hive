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

// TestScopedToken_AdvisoryTiersRequestContentsRead is the regression test for
// #4289: advisory-tier agents ("advisor", "newcomer") must be able to READ
// repo contents — auditing the repo is their core function — while remaining
// unable to write (contents:write must be absent so GitHub rejects any push
// server-side with 403). The advisor tier must also not request issues at
// all, to prevent issue creation.
func TestScopedToken_AdvisoryTiersRequestContentsRead(t *testing.T) {
	cases := []struct {
		tier       string
		wantIssues string // "" means the issues permission must be absent
	}{
		{tier: "advisor", wantIssues: ""},
		{tier: "newcomer", wantIssues: "write"},
	}

	for _, tc := range cases {
		t.Run("tier_"+tc.tier, func(t *testing.T) {
			key, _ := generateTestKey(t)

			var gotPerms map[string]string
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Permissions map[string]string `json:"permissions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decoding token request body: %v", err)
				}
				gotPerms = body.Permissions
				json.NewEncoder(w).Encode(map[string]any{
					"token":      "scoped-token-" + tc.tier,
					"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
				})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			auth := &AppAuth{
				appID:          1,
				installationID: 2,
				key:            key,
				logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
				apiURL:         server.URL,
			}

			if _, err := auth.ScopedToken(context.Background(), tc.tier); err != nil {
				t.Fatalf("ScopedToken(%q): %v", tc.tier, err)
			}

			if got := gotPerms["contents"]; got != "read" {
				t.Errorf("tier %q contents permission = %q, want %q (advisory agents need a repo read path, #4289)", tc.tier, got, "read")
			}
			if got := gotPerms["metadata"]; got != "read" {
				t.Errorf("tier %q metadata permission = %q, want %q", tc.tier, got, "read")
			}
			if got := gotPerms["issues"]; got != tc.wantIssues {
				t.Errorf("tier %q issues permission = %q, want %q", tc.tier, got, tc.wantIssues)
			}
			// The write-prevention boundary: contents must never be "write"
			// at these tiers — GitHub enforces the push block server-side.
			for perm, level := range gotPerms {
				if level == "write" && perm != "issues" {
					t.Errorf("tier %q requested %s:write — advisory tiers must not hold any repo write capability", tc.tier, perm)
				}
			}
		})
	}
}
