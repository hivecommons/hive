package agent

import (
	"strings"
	"testing"
)

// The Copilot CLI start-up banner for a credential GitHub refused to CHECK,
// as captured live on hive-qual0 (2026-09-28 14:41 UTC).
const rateLimitedStartupPane = `Authentication token found but could not be validated.
GitHub returned: API rate limit exceeded for user ID 407614. If you reach out to GitHub Support for help, please include the request ID 5DE4:420C8 and timestamp 2026-09-28 14:41:15 UTC. For mo
Check your network connection and credentials, then restart Copilot. Use /login to re-authenticate.
 /data/agents/quality`

func TestPaneShowsStartupRateLimit(t *testing.T) {
	lines := strings.Split(rateLimitedStartupPane, "\n")
	if !paneShowsStartupRateLimit(lines) {
		t.Fatal("rate-limited validation banner not recognised")
	}
	if paneShowsStartupRateLimit([]string{"Authentication token found but could not be validated.", "GitHub returned: Bad credentials"}) {
		t.Fatal("a genuine credential rejection must not read as a rate limit")
	}
	if paneShowsStartupRateLimit(nil) {
		t.Fatal("empty pane must not read as a rate limit")
	}
}

// A rate-limited banner ends with "Use /login to re-authenticate" but must NOT
// be a login prompt: treating it as one spent the token-restart cap inside the
// rate-limit window and stranded the agent on the give-up latch.
func TestPaneShowsLoginPrompt_RateLimitedBannerIsNotLogin(t *testing.T) {
	lines := strings.Split(rateLimitedStartupPane, "\n")
	if paneShowsLoginPrompt(lines) {
		t.Fatal("rate-limited validation banner must not classify as a login prompt")
	}
	// The same advice without the rate-limit line still is one.
	plain := []string{"Check your network connection and credentials, then restart Copilot. Use /login to re-authenticate."}
	if !paneShowsLoginPrompt(plain) {
		t.Fatal("bare /login advice must still classify as a login prompt")
	}
}

func TestClassifyAuthPaneLoginURLAndBackend401(t *testing.T) {
	cases := []struct {
		name             string
		pane             string
		wantNeedsLogin   bool
		wantLoginURL     string
		wantBackendError bool
	}{
		{
			name:           "real device URL",
			pane:           "To sign in, use a web browser to open the page https://github.com/login/device\nEnter one-time code ABCD-EFGH",
			wantNeedsLogin: true,
			wantLoginURL:   "https://github.com/login/device",
		},
		{
			name:             "inference proxy database 401",
			pane:             `● Please run /login · API Error: 401 {"type":"error","error":{"type":"api_error","message":"inference backend returned 401: {"error":{"message":"Authentication Error, Error in connector: Error querying the database: FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute","type":"auth_error"}}}}`,
			wantBackendError: true,
		},
		{
			name:           "plain login directive without URL",
			pane:           "● Please run /login",
			wantNeedsLogin: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyAuthPane(strings.Split(tc.pane, "\n"))
			if got.NeedsLogin != tc.wantNeedsLogin {
				t.Fatalf("NeedsLogin = %v, want %v (state=%+v)", got.NeedsLogin, tc.wantNeedsLogin, got)
			}
			if got.LoginURL != tc.wantLoginURL {
				t.Fatalf("LoginURL = %q, want %q (state=%+v)", got.LoginURL, tc.wantLoginURL, got)
			}
			if got.BackendAuthError != tc.wantBackendError {
				t.Fatalf("BackendAuthError = %v, want %v (state=%+v)", got.BackendAuthError, tc.wantBackendError, got)
			}
			if tc.wantBackendError && got.BackendAuthErrorMsg == "" {
				t.Fatalf("BackendAuthErrorMsg empty (state=%+v)", got)
			}
		})
	}
}

// The diagnostic path must not rewrite the token store over a rate-limited
// validation: the credential was never rejected.
func TestMatchesAuthError_IgnoresRateLimitedValidation(t *testing.T) {
	if matchesAuthError(rateLimitedStartupPane) {
		t.Fatal("rate-limited validation banner must not count as an auth error")
	}
	if !matchesAuthError("Authentication token found but could not be validated.\nGitHub returned: Bad credentials") {
		t.Fatal("genuine rejection must still count as an auth error")
	}
}
