package agent

import "testing"

// TestMatchesAuthError_DefinitiveRejectionsOnly verifies that only genuine
// server-side credential rejections trigger a token purge, and that a transient
// interactive login/"re-authenticate" prompt (which the Copilot CLI can show on
// a slow cold start after an upgrade, while the stored token is still valid)
// does NOT — that was the cause of repeated forced re-logins on every upgrade.
func TestMatchesAuthError_DefinitiveRejectionsOnly(t *testing.T) {
	purge := []string{
		"Error: Bad credentials",
		"HTTP 401 Unauthorized",
		"token found but could not be validated",
		"Failed to fetch OAuth user login",
	}
	for _, s := range purge {
		if !matchesAuthError(s) {
			t.Errorf("definitive rejection should purge: %q", s)
		}
	}

	// These must NOT purge — they are login prompts / benign output that can
	// appear during a normal cold start with a valid token on disk.
	noPurge := []string{
		"Please re-authenticate to continue",              // the removed over-broad pattern
		"To sign in, use a web browser to open the page https://github.com/login/device",
		"Copilot CLI ready",
		"Enter the code below to authenticate",
		"",
	}
	for _, s := range noPurge {
		if matchesAuthError(s) {
			t.Errorf("non-rejection output must NOT purge the token: %q", s)
		}
	}
}
