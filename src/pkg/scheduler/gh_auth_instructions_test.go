package scheduler

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// kubestellar/hive#1861 asks that agents hold no GitHub token material at all.
// The proxy-side injection half of that issue is deferred (it needs attested
// identity first — #3841/N7, and the #3833 keypair), but the container path
// already reaches the goal without it: the image installs bin/gh-wrapper.sh AS
// `gh` (src/Dockerfile: COPY bin/gh-wrapper.sh /usr/local/bin/gh) and the wrapper
// exports the agent's tier-scoped token from HIVE_AGENT_TOKEN_CACHE itself,
// before the agent's command runs.
//
// So the kick prompt has no reason to hand the agent a token, and every reason
// not to. These pin that it does not — the container-path twin of the
// native-path guard in bin/test_gh_auth_native_no_cat.sh (#3842/#3889).

func ghAuthBlock(t *testing.T) string {
	t.Helper()
	s := New(&config.Config{
		Project: config.ProjectConfig{Org: "org", Repos: []string{"r"}},
	}, testLogger())
	block := s.ghAuthInstructions()
	// Positive control: we are looking at the real authentication block, so a
	// silent rename/emptying cannot make the assertions below vacuously pass.
	if !strings.Contains(block, "## Project Authentication") {
		t.Fatalf("ghAuthInstructions no longer returns the authentication block; got %q", block)
	}
	return block
}

// The load-bearing one. A token on a command line the agent types is a token in
// the agent's transcript and in whatever it echoes back — one prompt injection
// from exfiltration. This is the same exposure #3889 removed from the
// native-install prompt; it differs only in blast radius (tier-scoped here,
// fleet-wide there), not in kind.
func TestGHAuthInstructions_NeverHandsTheAgentAToken(t *testing.T) {
	block := ghAuthBlock(t)

	// No token cache path of any kind, per-agent or shared — not even inside a
	// prohibition. Naming the shared cache in order to forbid it still tells an
	// agent where it lives, and #1861's "Why now" is precisely an agent
	// DISCOVERING that path under pressure when its push failed. The prohibition
	// below works without a map to the thing being prohibited.
	for _, forbidden := range []string{
		"/var/run/hive-metrics/agent-tokens",
		"gh-app-token.cache",
		"gh-token-",
		"HIVE_AGENT_TOKEN_CACHE",
	} {
		if strings.Contains(block, forbidden) {
			t.Errorf("the kick prompt names a token path (%q). Agents must not be told where credentials live "+
				"(kubestellar/hive#1861): gh-wrapper.sh already applies the scoped token per invocation, so this "+
				"instruction is redundant AND puts a live credential in the agent's transcript.", forbidden)
		}
	}

	// No instruction to read a secret, whatever the path. Catches a reworded
	// reintroduction that the literal list above would miss.
	readsASecret := regexp.MustCompile(`(?i)\$\(\s*cat\b|\bcat\s+/var/run|\bexport\s+GH_TOKEN\s*=`)
	if m := readsASecret.FindString(block); m != "" {
		t.Errorf("the kick prompt tells the agent to read or export a credential (matched %q). "+
			"The hive authenticates gh and git on the agent's behalf; nothing here should ask the agent to "+
			"fetch, echo, or export one (kubestellar/hive#1861).", m)
	}
}

// The instruction must still SAY something about gh, or "no token handling"
// silently becomes "no guidance" and an agent invents its own auth — which is
// how the shared-cache read was discovered in the field (#1861's "Why now").
func TestGHAuthInstructions_StillTellsTheAgentGHIsAuthenticated(t *testing.T) {
	block := ghAuthBlock(t)

	for _, want := range []string{
		"gh CLI",                 // the tool is still addressed
		"already handled",        // and stated to be authenticated for them
		"do NOT export GH_TOKEN", // with the trap named explicitly
	} {
		if !strings.Contains(block, want) {
			t.Errorf("gh auth instructions missing %q — the agent needs to know gh works WITHOUT setup, "+
				"or it will improvise a credential", want)
		}
	}

	// git's half is unchanged and must stay: it is the same promise for the
	// other tool class, served by the credential helper.
	if !strings.Contains(block, "credential helper") {
		t.Error("the git bullet lost its credential-helper promise — both tool classes must state that auth is automatic")
	}
}

// A Copilot-backed agent that exports GH_TOKEN can lose its MODEL auth:
// bin/agent-launch.sh leaves GH_TOKEN unset precisely because "Copilot CLI uses
// GH_TOKEN for its own Copilot API auth, which rejects GitHub App
// server-to-server tokens". The old block told agents to export it four lines
// above admitting the Copilot CLI owns it. Keep the two consistent.
func TestGHAuthInstructions_DoesNotContradictItselfOnGHTOKEN(t *testing.T) {
	block := ghAuthBlock(t)

	if strings.Contains(block, "export GH_TOKEN=") {
		t.Fatal("the prompt sets GH_TOKEN while also telling the agent the Copilot CLI owns it — " +
			"agent-launch.sh deliberately leaves it unset for that reason")
	}
	if !strings.Contains(block, "missing GH_TOKEN") {
		t.Error("the prompt no longer reassures the agent that a missing GH_TOKEN is expected; " +
			"without it an agent reads the empty variable as a broken session and starts hunting for a token")
	}
}
