package agent

import "testing"

// A gateway model-entitlement 403 is not a login prompt (#4400).
//
// Claude Code prefixes EVERY API error with its login hint, so an upstream
// refusal renders as
//
//	● Please run /login · API Error: 403 {... "team not allowed to access model" ...}
//
// on an agent that is fully logged in. That matched the login detector, which
// badged the agent as needing login and — via the poller's
// `showsLogin && configHasTokens()` branch — auto-restarted it straight back
// into the same 403. The reporter saw an agent that "keeps crashing" and was
// pointed at the one action that could not help.
//
// The line below is the verbatim shape from the log attached to #4400, with the
// model list trimmed exactly as the CLI trimmed it.
const reported403Line = `● Please run /login · API Error: 403 {"type":"error","error":{"type":"api_error","message":"inference backend returned 403: {"error":{"message":"team not allowed to access model. This team can only access models=['gemini-2.5-pro', 'gemini-2.5-flash', 'gcp/gemini-3.1-pro-preview', 'aws/claude-sonnet-4-6', 'aws/claude-opus-4..."}}`

func TestModelEntitlement403IsNotALoginPrompt(t *testing.T) {
	if paneShowsLoginPrompt([]string{reported403Line}) {
		t.Error("a 403 model-entitlement refusal was read as a login prompt — " +
			"this is what badged a logged-in agent and auto-restarted it into the same error (#4400)")
	}
}

func TestModelEntitlement403SurroundedByOrdinaryOutput(t *testing.T) {
	// The real pane carries the error among other lines; the guard must be
	// per-line, not "the whole pane looks like an error".
	lines := []string{
		"❯ You produced a plan but executed nothing. Execute it yourself NOW.",
		reported403Line,
		"✻ Crunched for 0s",
	}
	if paneShowsLoginPrompt(lines) {
		t.Error("403 line among ordinary output still read as a login prompt")
	}
}

// --- the signal that must NOT be suppressed -----------------------------------

func TestGenuineLoginPromptStillDetected(t *testing.T) {
	// Claude Code's real logged-out screen, with no upstream error attached.
	// These reach the detector through lineHasLoginDirective (an imperative
	// verb alongside "/login"), not through loginPromptPatterns.
	for _, line := range []string{
		"Please run /login",
		"● Please run /login",
		"Please run /login to continue",
		"Type /login to sign in",
	} {
		if !paneShowsLoginPrompt([]string{line}) {
			t.Errorf("genuine login prompt no longer detected: %q", line)
		}
	}
}

func TestUnauthenticated401StillCountsAsLogin(t *testing.T) {
	// 401 is AUTHENTICATION — the caller is not identified, and /login is
	// exactly the fix. Suppressing this would trade one misdiagnosis for the
	// opposite one, so it is pinned.
	line := `● Please run /login · API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`
	if !paneShowsLoginPrompt([]string{line}) {
		t.Error("a 401 authentication error must still be treated as needing login")
	}
}

func TestOtherLoginPatternsUnaffected(t *testing.T) {
	// Entries from loginPromptPatterns itself — the OAuth / device-flow chrome
	// this detector keys on. (The config-level defaultLoginPatterns list, with
	// "gh auth login" and friends, is a separate mechanism and is not what
	// paneShowsLoginPrompt reads.)
	for _, line := range []string{
		"sign in to use",
		"Select login method",
		"github.com/login/device",
		"Enter one-time code",
	} {
		if !paneShowsLoginPrompt([]string{line}) {
			t.Errorf("login pattern no longer detected: %q", line)
		}
	}
}

// --- the pre-existing false-positive guards must keep working ------------------

func TestOrdinaryOutputMentioningLoginStillIgnored(t *testing.T) {
	// lineHasLoginDirective's existing reason for existing.
	for _, line := range []string{
		"POST /login returns 302",
		"/help  /login  /model",
		"the /login endpoint is rate limited",
	} {
		if paneShowsLoginPrompt([]string{line}) {
			t.Errorf("ordinary output read as a login prompt: %q", line)
		}
	}
}

// --- the classifier itself ------------------------------------------------------

func TestLineShowsUpstreamAuthorizationError(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"403 status", "API Error: 403 something", true},
		{"litellm wording", "team not allowed to access model. This team can only access models=[...]", true},
		{"litellm wording, mixed case", "Team Not Allowed To Access Model", true},
		{"401 is authentication, not authorization", "API Error: 401 invalid x-api-key", false},
		{"500 is neither", "API Error: 500 internal", false},
		{"ordinary output", "checking model access for the team", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lineShowsUpstreamAuthorizationError(c.line); got != c.want {
				t.Errorf("lineShowsUpstreamAuthorizationError(%q) = %v, want %v", c.line, got, c.want)
			}
		})
	}
}

func TestEmptyPaneIsNotALoginPrompt(t *testing.T) {
	if paneShowsLoginPrompt(nil) || paneShowsLoginPrompt([]string{""}) {
		t.Error("an empty pane must not report a login prompt")
	}
}

func TestBobAPIKeyVerificationFailureNeedsLogin(t *testing.T) {
	lines := []string{
		"How would you like to authenticate for this project?",
		`🛑 Failed to login. Message: Failed to fetch user profile - HTTP 401:  - {"message":"API Key verification failed: Invalid or expired API Key","error":"unauthorized"}`,
	}
	if !paneShowsBobAPIKeyRejected(lines) {
		t.Fatal("Bob HTTP 401 API-key verification failure must be classified as a credential/login problem")
	}
}

func TestBobAPIKeyVerificationFailureJSONOrderNeedsLogin(t *testing.T) {
	line := `🛑 Failed to login. Message: Failed to fetch user profile - HTTP 401:  - {"error":"unauthorized","message":"API Key verification failed: Invalid or expired API Key"}`
	if !paneShowsBobAPIKeyRejected([]string{line}) {
		t.Fatal("Bob API-key detector must match the observed alternate JSON field order")
	}
}

func TestBobAPIKeyDetectorIgnoresOrdinaryAPIKeyText(t *testing.T) {
	if paneShowsBobAPIKeyRejected([]string{"document how to rotate an API key before it expires"}) {
		t.Fatal("ordinary API key discussion must not be treated as a Bob credential failure")
	}
}
