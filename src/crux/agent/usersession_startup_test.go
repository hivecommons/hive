package agent

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent/panesignal"
)

const sessionStartupClaudeTrustPane = ` Accessing workspace:

 /data/session-workspaces/.sessions/0123456789ab/org/repo

 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.

 Claude Code'll be able to read, edit, and execute files here.

 ❯ No, exit
   Yes, I trust this folder

 Enter to confirm · Esc to cancel`

const sessionStartupClaudeNoticePane = ` Security notes:

 1. Claude can make mistakes
    You should always review Claude's responses, especially when
    running code.

 2. Due to prompt injection risks, only use it with code you trust
    For more details see:
    https://code.claude.com/docs/en/security

 Press Enter to continue…`

const sessionStartupClaudeThemePane = ` Let's get started.

 Choose the text style that looks best with your terminal
 To change this later, run /theme

 ❯ 1. Dark mode ✔
   2. Light mode`

const sessionStartupClaudePermissionPane = `│ Bash command                                             │
│   rm -rf build                                           │
│ Do you want to proceed?                                  │
│ ❯ 1. Yes                                                 │
│   2. Yes, and don't ask again for rm commands            │
│   3. No, and tell Claude what to do differently (esc)    │`

const sessionStartupClaudeLoginPane = ` Claude Code can be used with your Claude subscription or billed based on API usage.

 Select login method:

 ❯ 1. Claude account with subscription
   2. Anthropic Console account`

const sessionStartupCodexLoginPane = `  Welcome to Codex

  Sign in with ChatGPT to use Codex as part of your paid plan
› 1. Sign in with ChatGPT
  2. Provide your own API key
  Press Enter to continue`

const sessionStartupCopilotDevicePane = `To authenticate, visit https://github.com/login/device
Enter one-time code: ABCD-1234
Waiting for authorization...`

const sessionStartupCopilotTrustPane = `Confirm folder trust
  1. Yes
  2. Yes, and remember for future sessions
  3. No`

const sessionStartupCodexUpdatePane = `  ✨ Update available! 0.146.0 -> 0.147.0
› 1. Update now (runs ` + "`npm install -g @openai/codex`" + `)
  2. Skip
  3. Skip until next version
  Press enter to continue`

const sessionStartupCodexApprovalPane = `  Would you like to run the following command?
  $ cat notes.md
› 1. Yes, proceed (y)
  2. No, and tell Codex what to do differently (esc)`

const (
	sessionStartupClaudeReadyPane = "╭──────────╮\n│ ❯        │\n╰──────────╯\n  ? for shortcuts"
	sessionStartupCodexReadyPane  = "› Implement {feature}\n  gpt-5.6 default · /work"
	sessionStartupWorkingPane     = "● Reading the spec\n  ✻ Thinking… (12s · esc to interrupt)"
)

// TestClassifyUserSessionStartup covers what a session's CLI can show right
// after launch, per vendor: startup screens are answerable, permission prompts
// are refused, login screens are sign-in, and only a real input prompt (or a
// CLI already at work) is ready.
func TestClassifyUserSessionStartup(t *testing.T) {
	cases := []struct {
		name   string
		vendor string
		pane   string
		kind   crux.UserSessionStartupKind
		keys   []string
	}{
		{"nothing drawn yet", "claude", "  \n", crux.UserSessionStartupLoading, nil},
		{"copilot still loading", "copilot", "● Loading environment", crux.UserSessionStartupLoading, nil},
		{"claude trust", "claude", sessionStartupClaudeTrustPane, crux.UserSessionStartupScreen, []string{"Down", "Enter"}},
		{"claude first-run notice", "claude", sessionStartupClaudeNoticePane, crux.UserSessionStartupScreen, []string{"Enter"}},
		{"claude theme picker", "claude", sessionStartupClaudeThemePane, crux.UserSessionStartupScreen, []string{"Enter"}},
		{"copilot trust", "copilot", sessionStartupCopilotTrustPane, crux.UserSessionStartupScreen, []string{"1", "Enter"}},
		{"codex update menu", "codex", sessionStartupCodexUpdatePane, crux.UserSessionStartupScreen, []string{"3", "Enter"}},
		{"claude permission dialog", "claude", sessionStartupClaudePermissionPane, crux.UserSessionStartupRefused, nil},
		{"codex command approval", "codex", sessionStartupCodexApprovalPane, crux.UserSessionStartupRefused, nil},
		{"claude login menu", "claude", sessionStartupClaudeLoginPane, crux.UserSessionStartupSignIn, nil},
		{"codex sign-in menu", "codex", sessionStartupCodexLoginPane, crux.UserSessionStartupSignIn, nil},
		{"copilot device flow", "copilot", sessionStartupCopilotDevicePane, crux.UserSessionStartupSignIn, nil},
		{"claude input prompt", "claude", sessionStartupClaudeReadyPane, crux.UserSessionStartupReady, nil},
		{"codex input prompt", "codex", sessionStartupCodexReadyPane, crux.UserSessionStartupReady, nil},
		{"already working", "claude", sessionStartupWorkingPane, crux.UserSessionStartupReady, nil},
		// A menu's "Esc to cancel" hint reads as a busy footer; a login menu
		// carrying it must still be sign-in, never a working (ready) CLI.
		{"claude login menu with a cancel hint", "claude", sessionStartupClaudeLoginPane + "\n\n Enter to confirm · Esc to cancel", crux.UserSessionStartupSignIn, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			step, keys := classifyUserSessionStartup(c.vendor, c.pane)
			if step.Kind != c.kind {
				t.Fatalf("kind = %q (screen %q), want %q", step.Kind, step.Screen, c.kind)
			}
			if !reflect.DeepEqual(keys, c.keys) {
				t.Errorf("keys = %v, want %v", keys, c.keys)
			}
			if (c.kind == crux.UserSessionStartupScreen || c.kind == crux.UserSessionStartupRefused) && step.Screen == "" {
				t.Error("the screen is not named; the session tab could not say which one it is")
			}
		})
	}
}

// TestClassifyUserSessionStartup_EveryVendorRefusesPermissionPrompts: for each
// vendor the session picker offers, a permission prompt is refused and a
// login screen is sign-in. Neither ever carries keys.
func TestClassifyUserSessionStartup_EveryVendorRefusesPermissionPrompts(t *testing.T) {
	for _, v := range userSessionVendors {
		for name, pane := range map[string]string{
			"claude permission dialog": sessionStartupClaudePermissionPane,
			"codex command approval":   sessionStartupCodexApprovalPane,
		} {
			step, keys := classifyUserSessionStartup(v.id, pane)
			if step.Kind != crux.UserSessionStartupRefused || len(keys) != 0 {
				t.Errorf("%s / %s = (%q, keys %v), want refused with no keys", v.id, name, step.Kind, keys)
			}
		}
		for name, pane := range map[string]string{
			"claude login menu":   sessionStartupClaudeLoginPane,
			"codex sign-in menu":  sessionStartupCodexLoginPane,
			"copilot device flow": sessionStartupCopilotDevicePane,
		} {
			step, keys := classifyUserSessionStartup(v.id, pane)
			if step.Kind != crux.UserSessionStartupSignIn || len(keys) != 0 {
				t.Errorf("%s / %s = (%q, keys %v), want sign-in with no keys", v.id, name, step.Kind, keys)
			}
		}
	}
}

func TestUserSessionClientsTyped(t *testing.T) {
	for name, c := range map[string]struct {
		out  string
		want bool
	}{
		"no clients":                       {"", false},
		"writable, just attached":          {"0 1700000000 1700000000\n", false},
		"writable, terminal replies only":  {"0 1700000000 1700000001\n", false},
		"writable, typed":                  {"0 1700000000 1700000009\n", true},
		"read-only viewer activity":        {"1 1700000000 1700000090\n", false},
		"one viewer, one typing":           {"1 1700000000 1700000090\n0 1700000050 1700000060\n", true},
		"unparseable lines are not typing": {"garbage\n0 x y\n", false},
	} {
		if got := userSessionClientsTyped([]byte(c.out)); got != c.want {
			t.Errorf("%s: typed = %v, want %v", name, got, c.want)
		}
	}
}

// startupTmux is a fake tmux for one session whose pane shows pane and whose
// attached clients are clients.
func startupTmux(pane, clients string, listErr error) *fakeTmux {
	return &fakeTmux{handler: func(args []string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			return []byte("0 0 1700000000\n"), nil
		case "capture-pane":
			return []byte(pane), nil
		case "list-clients":
			return []byte(clients), listErr
		}
		return nil, nil
	}}
}

// sentKeys returns the key of every send-keys call, in order.
func sentKeys(f *fakeTmux) []string {
	var keys []string
	for _, c := range f.calls {
		if c.args[0] == "send-keys" {
			keys = append(keys, c.args[len(c.args)-1])
		}
	}
	return keys
}

func TestStartupStep_AnswersAStartupScreenAsTheSessionUID(t *testing.T) {
	f := startupTmux(sessionStartupClaudeTrustPane, "0 1700000000 1700000000\n", nil)
	r, _ := newTestSessionRuntimeWithPriv(t, f, nil)
	step := r.StartupStep(testSessionID, "claude", true)
	if step.Kind != crux.UserSessionStartupScreen || !step.Answered || step.Typed {
		t.Fatalf("step = %+v, want an answered startup screen", step)
	}
	if step.Screen != "claude workspace trust" {
		t.Errorf("screen = %q", step.Screen)
	}
	if got := sentKeys(f); !reflect.DeepEqual(got, []string{"Down", "Enter"}) {
		t.Errorf("keys sent = %v, want Down then Enter (\"Yes, I trust this folder\")", got)
	}
	for _, c := range f.calls {
		if c.user != testSessionSpecUser {
			t.Errorf("tmux %s ran as %q, want the session uid %q", c.args[0], c.user, testSessionSpecUser)
		}
		// Only the visible screen is read: an answered menu lingers in history.
		if c.args[0] == "capture-pane" && strings.Contains(strings.Join(c.args, " "), "-S") {
			t.Errorf("capture reads scrollback: %v", c.args)
		}
	}
}

// TestStartupStep_NeverAnswersOutsideTheBoundary is the safety boundary on the
// runtime: with answering switched ON, nothing is sent to a permission prompt,
// a login screen, a ready prompt, an unknown screen, or any screen once a
// person has typed.
func TestStartupStep_NeverAnswersOutsideTheBoundary(t *testing.T) {
	quiet := "0 1700000000 1700000000\n"
	cases := []struct {
		name    string
		vendor  string
		pane    string
		clients string
		listErr error
		kind    crux.UserSessionStartupKind
		typed   bool
	}{
		{"claude permission dialog", "claude", sessionStartupClaudePermissionPane, quiet, nil, crux.UserSessionStartupRefused, false},
		{"codex command approval", "codex", sessionStartupCodexApprovalPane, quiet, nil, crux.UserSessionStartupRefused, false},
		{"claude login menu", "claude", sessionStartupClaudeLoginPane, quiet, nil, crux.UserSessionStartupSignIn, false},
		{"codex sign-in menu", "codex", sessionStartupCodexLoginPane, quiet, nil, crux.UserSessionStartupSignIn, false},
		{"copilot device flow", "copilot", sessionStartupCopilotDevicePane, quiet, nil, crux.UserSessionStartupSignIn, false},
		{"input prompt", "claude", sessionStartupClaudeReadyPane, quiet, nil, crux.UserSessionStartupReady, false},
		{"unknown screen", "kilo", "Some screen nobody described\n[ OK ]", quiet, nil, crux.UserSessionStartupLoading, false},
		{"trust screen, a person typed", "claude", sessionStartupClaudeTrustPane, "0 1700000000 1700000030\n", nil, crux.UserSessionStartupScreen, true},
		{"trust screen, clients unknown", "claude", sessionStartupClaudeTrustPane, "", errors.New("no server"), crux.UserSessionStartupScreen, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := startupTmux(c.pane, c.clients, c.listErr)
			r, _ := newTestSessionRuntimeWithPriv(t, f, nil)
			step := r.StartupStep(testSessionID, c.vendor, true)
			if step.Kind != c.kind || step.Typed != c.typed {
				t.Fatalf("step = %+v, want kind %q typed %v", step, c.kind, c.typed)
			}
			if step.Answered {
				t.Error("step reports an answer")
			}
			if keys := sentKeys(f); len(keys) != 0 {
				t.Fatalf("keys %v were typed into the session", keys)
			}
		})
	}
}

func TestStartupStep_ObserveOnlySendsNothing(t *testing.T) {
	f := startupTmux(sessionStartupClaudeTrustPane, "", nil)
	r, _ := newTestSessionRuntimeWithPriv(t, f, nil)
	step := r.StartupStep(testSessionID, "claude", false)
	if step.Kind != crux.UserSessionStartupScreen || step.Answered {
		t.Fatalf("step = %+v", step)
	}
	if keys := sentKeys(f); len(keys) != 0 {
		t.Fatalf("observe-only step typed %v", keys)
	}
}

func TestStartupStep_GoneWhenTheSessionIsNotRunning(t *testing.T) {
	dead := &fakeTmux{handler: func(args []string) ([]byte, error) {
		if args[0] == "display-message" {
			return []byte("1 0 1700000000\n"), nil
		}
		return []byte(sessionStartupClaudeTrustPane), nil
	}}
	r, _ := newTestSessionRuntimeWithPriv(t, dead, nil)
	if step := r.StartupStep(testSessionID, "claude", true); step.Kind != crux.UserSessionStartupGone {
		t.Errorf("dead pane = %+v, want gone", step)
	}
	if keys := sentKeys(dead); len(keys) != 0 {
		t.Errorf("typed %v into a dead pane", keys)
	}
	missing := &fakeTmux{handler: func([]string) ([]byte, error) { return nil, errors.New("no server running") }}
	r, _ = newTestSessionRuntimeWithPriv(t, missing, nil)
	if step := r.StartupStep(testSessionID, "claude", true); step.Kind != crux.UserSessionStartupGone {
		t.Errorf("missing session = %+v, want gone", step)
	}
	if step := r.StartupStep("not-an-id", "claude", true); step.Kind != crux.UserSessionStartupGone {
		t.Errorf("bad id = %+v, want gone", step)
	}
}

// TestSeedOnboarding_ClaudeOnlyAndOnlyWhenAsked: a Claude Code session HOME is
// seeded (as the session uid, never over an existing file) with onboarding
// done and the session's own directory trusted, and with nothing that
// identifies an account.
func TestSeedOnboarding_ClaudeOnlyAndOnlyWhenAsked(t *testing.T) {
	const cwd = "/data/session-workspaces/.sessions/0123456789ab/org/repo"
	seeds := func(spec crux.UserSessionSpec) []fakePrivCall {
		r, p := newTestSessionRuntimeWithPriv(t, &fakeTmux{}, nil)
		r.seedOnboarding(spec, testSessionUID, cwd)
		var out []fakePrivCall
		for _, c := range p.calls {
			if len(c.argv) > 2 && c.argv[2] == userSessionSeedOnceScript {
				out = append(out, c)
			}
		}
		return out
	}
	if got := seeds(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude"}); len(got) != 0 {
		t.Errorf("seeded without SeedOnboarding: %v", got)
	}
	if got := seeds(crux.UserSessionSpec{ID: testSessionID, Vendor: "codex", SeedOnboarding: true}); len(got) != 0 {
		t.Errorf("seeded a codex session: %v", got)
	}
	got := seeds(crux.UserSessionSpec{ID: testSessionID, Vendor: "claude", SeedOnboarding: true})
	if len(got) != 1 {
		t.Fatalf("seed calls = %d, want 1", len(got))
	}
	call := got[0]
	if call.user != testSessionSpecUser {
		t.Errorf("seed ran as %q, want the session uid %q", call.user, testSessionSpecUser)
	}
	if target := call.argv[len(call.argv)-1]; target != userSessionHome(testSessionID)+"/"+userSessionClaudeConfigName {
		t.Errorf("seed target = %q", target)
	}
	var seed map[string]any
	if err := json.Unmarshal([]byte(call.stdin), &seed); err != nil {
		t.Fatalf("seed is not JSON: %v", err)
	}
	if seed[claudeOnboardingKey] != true {
		t.Errorf("seed lacks %s: %v", claudeOnboardingKey, seed)
	}
	projects, _ := seed[userSessionClaudeProjectsKey].(map[string]any)
	project, _ := projects[cwd].(map[string]any)
	if project[userSessionClaudeTrustKey] != true || len(projects) != 1 {
		t.Errorf("seed trusts %v, want only %s", projects, cwd)
	}
	for _, key := range []string{"oauthAccount", "userID", "primaryApiKey", "bypassPermissionsModeAccepted", "customApiKeyResponses"} {
		if _, ok := seed[key]; ok {
			t.Errorf("seed carries %q", key)
		}
	}
	// The script refuses to replace a file that exists or to follow a link.
	for _, want := range []string{`[ -e "$1" ]`, `[ -L "$1" ]`, "set -C", "umask 077"} {
		if !strings.Contains(userSessionSeedOnceScript, want) {
			t.Errorf("seed script lost %q", want)
		}
	}
}

// TestSessionStartupUsesTheAgentsDetector pins the sharing: the screens a
// session answers come out of the agents' own table, with the agents' keys.
func TestSessionStartupUsesTheAgentsDetector(t *testing.T) {
	for _, c := range []struct{ vendor, pane string }{
		{"claude", sessionStartupClaudeTrustPane},
		{"copilot", sessionStartupCopilotTrustPane},
		{"codex", sessionStartupCodexUpdatePane},
	} {
		key, label, ok := blockingPromptKey(c.vendor, c.pane)
		if !ok {
			t.Fatalf("%s: the agents' detector does not know this fixture", c.vendor)
		}
		scr, ok := panesignal.SessionStartupScreen(c.vendor, c.pane)
		if !ok || scr.Label != label {
			t.Fatalf("%s: session screen = (%+v, %v), agents' label %q", c.vendor, scr, ok, label)
		}
		if want := []string{key, "Enter"}; !reflect.DeepEqual(scr.Keys, want) {
			t.Errorf("%s: session keys = %v, agents send %v", c.vendor, scr.Keys, want)
		}
	}
}
