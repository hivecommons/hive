package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestValidateLoginCodeRejectsCommandInjection is the security property of the
// whole endpoint. The pane is an interactive shell hosting a CLI: a code
// carrying a newline is SUBMITTED at that point and everything after it is
// typed as a fresh line, turning "paste my login code" into "run this".
func TestValidateLoginCodeRejectsCommandInjection(t *testing.T) {
	for _, tc := range []struct{ name, code string }{
		{"newline", "4/0Abc\nrm -rf /"},
		{"carriage return", "4/0Abc\rrm -rf /"},
		{"space then command", "4/0Abc; whoami"},
		{"tab", "4/0Abc\twhoami"},
		{"escape sequence", "4/0Abc\x1b[A"},
		{"nul", "4/0Abc\x00"},
		{"empty", ""},
		{"over length", strings.Repeat("a", maxLoginCodeLen+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateLoginCode(tc.code); err == nil {
				t.Fatalf("accepted %q — this value can carry a second command into the agent's shell", tc.code)
			}
		})
	}
}

// Real codes must still go through: a Google OAuth code and a GitHub device
// code are both printable and whitespace-free.
func TestValidateLoginCodeAcceptsRealCodes(t *testing.T) {
	for _, code := range []string{
		"4/0AVMBsJhV-9wKq2QpX8n_example-code",
		"ABCD-1234",
		"cuog9M96pk-QlnAiRsq_jA",
	} {
		if err := validateLoginCode(code); err != nil {
			t.Fatalf("rejected a legitimate authorization code %q: %v", code, err)
		}
	}
}

// SubmitLoginCode must type the code and submit it with EXACTLY one Enter —
// extra Enters answer whatever the CLI asks next (the codex "1. Update now"
// failure documented on tmuxSendEntersForAgent).
func TestSubmitLoginCodeTypesCodeAndSubmitsOnce(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "agy"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})

	origExists := tmuxSessionExists
	tmuxSessionExists = func(_ *Manager, _ *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = origExists })

	var typed []string
	termSeams(m).sendLiteral = func(_ *AgentProcess, text string) { typed = append(typed, text) }

	if err := m.SubmitLoginCode("scanner", "  4/0AVMBsJh-code  "); err != nil {
		t.Fatalf("SubmitLoginCode: %v", err)
	}
	if len(typed) != 1 || typed[0] != "4/0AVMBsJh-code" {
		t.Fatalf("typed = %q, want exactly the trimmed code once", typed)
	}
}

// A refused code must never be typed at all — no partial submission.
func TestSubmitLoginCodeRefusesWithoutTyping(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "agy"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})

	var typed []string
	termSeams(m).sendLiteral = func(_ *AgentProcess, text string) { typed = append(typed, text) }

	if err := m.SubmitLoginCode("scanner", "code\nrm -rf /"); err == nil {
		t.Fatal("accepted a newline-bearing code")
	}
	if len(typed) != 0 {
		t.Fatalf("typed %q despite refusing the code", typed)
	}
}

// TestAgyLoginPromptIsDetected: an agy agent parked at its OAuth hand-off
// reported state=running / needsLogin=false and raised no alert, because agy
// never prints the word "login" and no pattern matched its chrome.
func TestAgyLoginPromptIsDetected(t *testing.T) {
	pane := []string{
		" Your browser should open automatically. If not:",
		" https://accounts.google.com/o/oauth2/auth?access_type=offline&client_id=x",
		" If you aren't automatically redirected, paste the authorization code below:",
		" authorization code...",
	}
	if !paneShowsLoginPrompt(pane) {
		t.Fatal("agy's OAuth hand-off is not recognised as a login prompt — the agent reports healthy while doing no work")
	}
}

// The fragment "authorization code" alone must NOT trip the detector: an agent
// reading or writing about OAuth prints it in ordinary output (the #3959
// lesson that cost two earlier false-positive incidents).
func TestAgyPatternsDoNotMatchOrdinaryProse(t *testing.T) {
	for _, line := range []string{
		"  the handler exchanges the authorization code for a token",
		"  // authorization code grant, see RFC 6749",
		"  browser automation is out of scope for this task",
	} {
		if paneShowsLoginPrompt([]string{line}) {
			t.Fatalf("ordinary agent output read as a login prompt: %q", line)
		}
	}
}

// TestOMPLoginPromptIsDetected pins the boxed login screen reported in #8420:
// omp renders the shared browser hand-off entirely inside the tail of the pane,
// after its earlier /login directive has scrolled out of the poller's window.
func TestOMPLoginPromptIsDetected(t *testing.T) {
	pane := strings.Split(`╭─ Login to Anthropic (Claude Pro/Max) ──────────────────────────────╮
│ https://claude.ai/oauth/authorize?client_id=9d1c250a-a8a1-4dbc-8f52-e2c6f5cda0a0&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainf │
│ erence+user%3Asessions%3Aclaude_code+user%3Amcp_servers+user%3Afil                                            │
│ e_upload&code_challenge=abcDEF0123456789_-abcDEF0123456789&state=omp-login-state&code=true                  │
│ Ctrl+click to open                                                                                           │
│ Complete login in your browser. If the browser cannot reach this machine, paste the final redirect URL or   │
│ authorization code when prompted.                                                                            │
│ Waiting for browser authentication...                                                                        │
│ Paste the authorization code (or full redirect URL), then press Enter:                                       │
│ >                                                                                                            │`, "\n")
	if !paneShowsLoginPrompt(pane) {
		t.Fatal("omp's boxed OAuth hand-off is not recognised as a login prompt")
	}
}

// TestCodexLoginPromptIsDetected pins codex 0.156.1's browser sign-in
// screen: without these patterns the pane poller treats codex as healthy until
// the CLI times out to a shell and crash-loops instead of showing needs-login.
func TestCodexLoginPromptIsDetected(t *testing.T) {
	pane := strings.Split(`  Welcome to Codex, OpenAI's command-line coding agent
  Finish signing in via your browser
  If the link doesn't open automatically, open the following link to authenticate:
  https://auth.openai.com/oauth/authorize?response_type=code&client_id=app_example&redirect_uri=http%3A%2F%2Flocalhost%3A1455%2Fauth%2Fcallback
  On a remote or headless machine? Press esc and choose Sign in with Device Code.
  Press esc to cancel`, "\n")
	if !paneShowsLoginPrompt(pane) {
		t.Fatal("codex's browser sign-in screen is not recognised as a login prompt")
	}
}
