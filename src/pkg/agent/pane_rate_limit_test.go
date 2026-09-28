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
