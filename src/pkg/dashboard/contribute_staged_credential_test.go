package dashboard

import (
	"strings"
	"testing"
)

// contribute_staged_credential_test.go pins the fix for kubestellar/hive#5088.
//
// Container mode gives the CLI a COPY of the credential from ~/.claude (an
// allowlist since #7836, see contribute_claude_staging_test.go) in an ephemeral
// staging directory that the cleanup trap deletes on exit. That containment is
// deliberate — it is what stops a permissions-bypassed agent writing to the
// contributor's real credential (H6 / CWE-668) — and it stays.
//
// What it also did, silently, was throw away a login performed INSIDE the
// container. A contributor whose host credential had expired reached the CLI's
// login menu, completed the whole browser flow, worked a session, and was back
// at the login menu on the next run with nothing to show for it. The mechanism
// was working as designed; the design assumed the host credential was valid, and
// said nothing when it was not.
//
// This does not change the boundary. It makes the failure audible before the
// contributor spends a login on it.

// contributeHiveClaudeStagingBlock returns the claude branch of contribute-hive's
// CLI-staging case statement.
func contributeHiveClaudeStagingBlock(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	start := strings.Index(src, `stage_claude_home "${HOME}/.claude" "${CLI_STAGE}/.claude"`)
	if start < 0 {
		t.Fatal("the claude CLI-staging case was not found in the Justfile")
	}
	end := strings.Index(src[start:], "copilot)")
	if end < 0 {
		t.Fatal("the end of the claude staging case was not found in the Justfile")
	}
	return src[start : start+end]
}

// TestStagedClaudeCredentialIsChecked proves the gate exists at all: the recipe
// must consult the credential it just staged rather than launching blind.
func TestStagedClaudeCredentialIsChecked(t *testing.T) {
	block := contributeHiveClaudeStagingBlock(t)

	if !strings.Contains(block, "claude_staged_credential_usable") {
		t.Error("contribute-hive stages a Claude credential without checking it can authenticate (#5088)")
	}
	// It must check the STAGED copy — the file the container will actually read —
	// not the host path, which is not what gets mounted.
	if !strings.Contains(block, `"${CLI_STAGE}/.claude/.credentials.json"`) {
		t.Error("the check must read the staged credential the container will use, not the host's")
	}
}

// TestHeadlessRunFailsRatherThanWaitingAtALoginPrompt pins the harder half. A
// headless pod has no human to answer a login prompt, so warning and continuing
// would leave it sitting there — exactly the "healthy-looking but stalled"
// failure #2538 exists to prevent.
func TestHeadlessRunFailsRatherThanWaitingAtALoginPrompt(t *testing.T) {
	block := contributeHiveClaudeStagingBlock(t)

	idx := strings.Index(block, `"${CONTRIBUTOR_MODE:-}" == "headless"`)
	if idx < 0 {
		t.Fatal("the headless case is not distinguished, so a pod would wait at a login prompt (#5088)")
	}
	if !strings.Contains(block[idx:], "exit 1") {
		t.Error("a headless run with no usable credential must fail fast, not warn and continue")
	}
	// The refusal has to name the way out or it is an alarm rather than guidance.
	if !strings.Contains(block[idx:], "claude") {
		t.Error("the headless refusal does not tell the operator how to authenticate")
	}
}

// TestInteractiveWarningIsHonestAboutWhatItCosts pins the message content. The
// warning's whole value is telling the contributor that a login done inside the
// container is thrown away — a generic "no credential found" would send them
// straight into the login flow the warning exists to save them from.
func TestInteractiveWarningIsHonestAboutWhatItCosts(t *testing.T) {
	block := contributeHiveClaudeStagingBlock(t)

	for _, want := range []string{
		"only for this run",      // the cost, stated plainly
		"deleted",                // why: the staging copy does not survive
		"#5088",                  // where the reasoning lives
		"run claude on the host", // the fix
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the interactive warning does not mention %q", want)
		}
	}
	// It must NOT claim the in-container login fails — it succeeds, and saying
	// otherwise would be wrong in a way a contributor could catch immediately.
	if strings.Contains(block, "login will fail") {
		t.Error("the warning overstates the problem: an in-container login works, it just does not persist")
	}
}

// TestAPIKeyAuthenticationIsNotTreatedAsMissingCredential covers the other way a
// contributor can authenticate. ANTHROPIC_API_KEY is forwarded into the
// container by the provider-env block, so a contributor using it needs no
// .credentials.json at all.
//
// Checking only the OAuth file made an API-key contributor look unauthenticated:
// interactive runs warned about a login menu they would never see, and — far
// worse — headless runs exited refusing to start a run that would have worked.
// The check has to sit here rather than at the forwarding site, because the
// headless refusal exits long before that code is reached.
func TestAPIKeyAuthenticationIsNotTreatedAsMissingCredential(t *testing.T) {
	block := contributeHiveClaudeStagingBlock(t)

	if !strings.Contains(block, "ANTHROPIC_API_KEY") {
		t.Fatal("the credential gate ignores ANTHROPIC_API_KEY, so an API-key contributor is " +
			"warned interactively and hard-failed headless for a run that would have worked (#5088)")
	}
	// It must SHORT-CIRCUIT the gate, not merely be mentioned: a present key means
	// no warning and no refusal, whatever the OAuth file says.
	guard := `if [[ -z "${ANTHROPIC_API_KEY:-}" ]] && ! claude_staged_credential_usable`
	if !strings.Contains(block, guard) {
		t.Error("a present ANTHROPIC_API_KEY must skip the gate entirely")
	}
}

// TestCredentialGateIsForwardedKeysOnly guards against the inverse mistake. The
// gate may only accept a key this recipe actually forwards into the container —
// accepting one it does not forward would wave through a run that then cannot
// authenticate, which is worse than the false warning being fixed.
func TestCredentialGateIsForwardedKeysOnly(t *testing.T) {
	src := justfileSource(t)
	forwardIdx := strings.Index(src, "for name in ANTHROPIC_API_KEY")
	if forwardIdx < 0 {
		t.Fatal("the provider-env forwarding list was not found")
	}
	line := src[forwardIdx:]
	if end := strings.Index(line, "\n"); end > 0 {
		line = line[:end]
	}
	if !strings.Contains(line, "ANTHROPIC_API_KEY") {
		t.Error("ANTHROPIC_API_KEY is accepted by the gate but not forwarded into the container")
	}
}

// TestStagedCredentialCheckToleratesRefreshTokens guards the one nuance that
// separates this from crying wolf. An expired ACCESS token that still carries a
// refresh token is fine: Claude Code refreshes it silently and no login prompt
// appears. Warning there would train contributors to ignore the warning.
func TestStagedCredentialCheckToleratesRefreshTokens(t *testing.T) {
	src := justfileSource(t)
	start := strings.Index(src, "claude_staged_credential_usable() {")
	if start < 0 {
		t.Fatal("the claude_staged_credential_usable helper was not found")
	}
	helper := src[start : start+strings.Index(src[start:], "\n      }")]

	if !strings.Contains(helper, "refreshToken") {
		t.Error("an expired access token with a refresh token must count as usable — " +
			"Claude Code refreshes it silently and no login prompt appears (#5088)")
	}
	if !strings.Contains(helper, "accessToken") || !strings.Contains(helper, "expiresAt") {
		t.Error("the check must mirror pkg/claude's ReadAccessToken rule (accessToken + expiresAt)")
	}
	// A missing jq must not be read as "no credential" — that would fire the
	// warning on every machine without jq, for a credential that is fine.
	if !strings.Contains(helper, "command -v jq") {
		t.Error("the helper must degrade to silence when jq is unavailable, not to a false warning")
	}
}
