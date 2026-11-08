package webstatic

import (
	"os"
	"strings"
	"testing"
)

// TestAgentCard_NamesBrokenCredentialBridge: when a Claude token refresh has
// replaced an agent's link to the shared login with a private file, the card
// must say so (the server sends credentialBridgeBroken) instead of a generic
// needs-login that sends the owner round a sign-in that cannot reach the
// agent. The agent-card Claude button runs the FLEET-WIDE sign-in, so it must
// say that too: on hive-wild-mole the per-agent "Login" button looked like a
// per-agent fix and was not one.
func TestAgentCard_NamesBrokenCredentialBridge(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"credential bridge broken (private stale file)",
		"a.credentialBridgeBroken",
		"Sign in Claude for all agents",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	// The old per-agent wording must be gone from the Claude login buttons.
	for _, stale := range []string{
		`title="Authenticate Claude Code CLI"`,
		`title="Re-authenticate Claude Code CLI"`,
	} {
		if strings.Contains(html, stale) {
			t.Errorf("index.html still labels the fleet-wide Claude sign-in as a per-agent login: %s", stale)
		}
	}
}
