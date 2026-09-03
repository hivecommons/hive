package agent

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// #4596: an agent can sit at a login prompt forever while every file-level
// check hive performs reports success. Two things are pinned here:
//
//  1. The token-triggered restart is BOUNDED. It fires on "pane shows login
//     AND a valid shared credential exists", which is exactly the bug's
//     signature, and before this it retried at the cooldown interval with no
//     cap — forever, for a condition no restart can reach.
//  2. When it gives up, the message NAMES THE CAUSE. The reporter of #4596
//     retracted the issue twice against this failure because a valid
//     credential and a login menu look, from outside, like a credential
//     problem. The diagnosis has to distinguish the token file from the
//     session-state file beside it.

// claudeCredJSON is a credentials file shaped like the one Claude Code writes.
// expiresAt is milliseconds since the epoch; a future value is what makes
// claude.HasValidToken accept it.
func claudeCredJSON(t *testing.T, expiresAt time.Time) []byte {
	t.Helper()
	payload := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "sk-ant-oat-test",
			"refreshToken": "sk-ant-ort-test",
			"expiresAt":    expiresAt.UnixMilli(),
			"scopes":       []string{"user:inference"},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	return data
}

// claudeHomeAgent builds an agent whose AgentHome resolves to dir. UID 0 is the
// documented "no allocated UID" path in AgentHome, which reads $HOME — the only
// seam that avoids hardcoding /data/home in a test.
func claudeHomeAgent(t *testing.T, dir string) *AgentProcess {
	t.Helper()
	t.Setenv("HOME", dir)
	emptySharedPaths(t)
	if got := AgentHome("quality", 0, "claude"); got != dir {
		t.Fatalf("AgentHome did not resolve to the temp home: got %q want %q", got, dir)
	}
	return &AgentProcess{Name: "quality", UID: 0, Config: config.AgentConfig{Backend: "claude"}}
}

func writeClaudeCredential(t *testing.T, home string, expiresAt time.Time) string {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	path := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(path, claudeCredJSON(t, expiresAt), 0o660); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	return path
}

// --- the classifier ----------------------------------------------------------

func TestInspectClaudeSessionClassifies(t *testing.T) {
	dir := t.TempDir()

	// The exact eight-key skeleton recorded in #4596 — the file a second agent
	// leaves behind after overwriting a signed-in one. No oauthAccount.
	skeleton := `{"firstStartTime":"2026-08-22T17:14:00Z","hasResetAutoModeOptInForDefaultOffer":true,
	  "machineID":"abc","migrationVersion":3,"opusProMigrationComplete":true,
	  "seenNotifications":[],"sonnet1m45MigrationComplete":true,"userID":"u1"}`

	signedIn := `{"userID":"u1","hasCompletedOnboarding":true,
	  "oauthAccount":{"accountUuid":"a-1","emailAddress":"x@example.com","organizationUuid":"o-1"}}`

	cases := []struct {
		name       string
		contents   string
		write      bool
		want       claudeSessionState
		wantOAuth  bool
		wantSetup  bool
		wantString string
	}{
		{name: "absent", write: false, want: claudeSessionAbsent, wantString: "absent"},
		{name: "skeleton (the #4596 signature)", contents: skeleton, write: true,
			want: claudeSessionSkeleton, wantString: "no-signed-in-identity"},
		{name: "signed in", contents: signedIn, write: true,
			want: claudeSessionSignedIn, wantOAuth: true, wantSetup: true, wantString: "signed-in"},
		// An oauthAccount written back as an empty object is exactly as
		// unauthenticated as one that is absent, and the clobber produces both.
		{name: "empty oauthAccount object", contents: `{"oauthAccount":{}}`, write: true,
			want: claudeSessionSkeleton},
		{name: "null oauthAccount", contents: `{"oauthAccount":null}`, write: true,
			want: claudeSessionSkeleton},
		// A file we could not parse must NOT be reported as "the login identity
		// is missing" — that would be a guess, and this exists to replace guesses.
		{name: "unparseable is unknown, not skeleton", contents: `{not json`, write: true,
			want: claudeSessionUnknown, wantString: "unknown"},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "session", string(rune('a'+i)), ".claude.json")
			if tc.write {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			got := inspectClaudeSession(path)
			if got.State != tc.want {
				t.Fatalf("state = %v (%s), want %v", got.State, got.State, tc.want)
			}
			if got.HasOAuthAccount != tc.wantOAuth {
				t.Errorf("HasOAuthAccount = %v, want %v", got.HasOAuthAccount, tc.wantOAuth)
			}
			if got.HasCompletedSetup != tc.wantSetup {
				t.Errorf("HasCompletedSetup = %v, want %v", got.HasCompletedSetup, tc.wantSetup)
			}
			if tc.wantString != "" && got.State.String() != tc.wantString {
				t.Errorf("String() = %q, want %q", got.State.String(), tc.wantString)
			}
		})
	}
}

func TestInspectClaudeSessionUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — mode 0000 is still readable, so this cannot be measured")
	}
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(path, []byte(`{"oauthAccount":{"accountUuid":"a"}}`), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Distinct from absent: something IS there, and saying "absent" would send
	// the operator looking for a missing file that exists.
	if got := inspectClaudeSession(path); got.State != claudeSessionUnreadable {
		t.Fatalf("state = %v (%s), want unreadable", got.State, got.State)
	}
}

func TestInspectClaudeSessionEmptyPath(t *testing.T) {
	if got := inspectClaudeSession(""); got.State != claudeSessionUnknown {
		t.Fatalf("empty path: state = %v, want unknown", got.State)
	}
	if got := claudeSessionFile(""); got != "" {
		t.Fatalf("claudeSessionFile(\"\") = %q, want empty", got)
	}
}

// --- the restart cap ---------------------------------------------------------

func TestDecideTokenRestartIsBounded(t *testing.T) {
	// (a) The cap must be a small, sane number, pinned INDEPENDENTLY of the
	// mechanism exercised below. This assertion is the one that catches the
	// regression outright: a cap of, say, 1<<30 still "eventually gives up" in
	// the abstract while restarting the agent forever in practice, and an
	// earlier draft of this test — which derived its loop bound from the
	// constant — passed happily with exactly that value.
	if tokenRestartMaxAttempts < 1 || tokenRestartMaxAttempts > 10 {
		t.Fatalf("tokenRestartMaxAttempts = %d, want a small positive cap (1..10)",
			tokenRestartMaxAttempts)
	}

	// (b) The mechanism, probed a FIXED number of times. Deliberately not
	// derived from tokenRestartMaxAttempts: if the cap were removed or raised,
	// these probes would all fire and never reach giveUp, and this fails.
	const probes = 25
	a := &AgentProcess{Name: "quality"}
	now := time.Now()
	cooldown := time.Duration(tokenRestartCooldownSec) * time.Second

	fires, gaveUp := 0, false
	for i := 0; i < probes; i++ {
		switch a.decideTokenRestart(now) {
		case tokenRestartFire:
			fires++
			if gaveUp {
				t.Fatalf("probe %d fired again after having given up", i)
			}
		case tokenRestartGiveUp:
			gaveUp = true
		case tokenRestartWait:
			t.Fatalf("probe %d waited despite a full cooldown having elapsed", i)
		}
		now = now.Add(cooldown)
	}

	if !gaveUp {
		t.Fatalf("never gave up across %d cooldown-separated probes: the restart is unbounded (#4596)", probes)
	}
	if fires != tokenRestartMaxAttempts {
		t.Fatalf("fired %d times, want exactly %d before giving up", fires, tokenRestartMaxAttempts)
	}
	if a.tokenRestartAttempts != tokenRestartMaxAttempts {
		t.Fatalf("counter kept climbing after the cap: %d", a.tokenRestartAttempts)
	}
}

func TestDecideTokenRestartHonoursCooldown(t *testing.T) {
	a := &AgentProcess{Name: "quality"}
	now := time.Now()
	if got := a.decideTokenRestart(now); got != tokenRestartFire {
		t.Fatalf("first call: got %v, want fire", got)
	}
	// One second short of the cooldown must not fire, and must not burn an
	// attempt — otherwise a busy poller (every ~3s) would exhaust the cap in
	// seconds and give up before the restart it counted ever had a chance.
	if got := a.decideTokenRestart(now.Add(time.Duration(tokenRestartCooldownSec-1) * time.Second)); got != tokenRestartWait {
		t.Fatalf("inside cooldown: got %v, want wait", got)
	}
	if a.tokenRestartAttempts != 1 {
		t.Fatalf("a waiting call consumed an attempt: counter = %d, want 1", a.tokenRestartAttempts)
	}
}

// The counter is reset by the poller when the prompt clears. Pin that a reset
// genuinely re-arms the restart, so an agent that recovers and later needs a
// real nudge is not permanently barred by an old streak.
func TestTokenRestartCounterResetRearms(t *testing.T) {
	a := &AgentProcess{Name: "quality"}
	now := time.Now()
	cooldown := time.Duration(tokenRestartCooldownSec) * time.Second

	// Drive to the cap with a fixed safety bound rather than a
	// constant-derived loop.
	const maxProbes = 25
	for i := 0; i < maxProbes && a.decideTokenRestart(now) != tokenRestartGiveUp; i++ {
		now = now.Add(cooldown)
	}
	if got := a.decideTokenRestart(now); got != tokenRestartGiveUp {
		t.Fatalf("expected giveUp before reset, got %v", got)
	}

	// What pollTmuxOutputForAgent does when showsLogin goes false.
	a.tokenRestartAttempts = 0
	a.tokenRestartGaveUp = false

	if got := a.decideTokenRestart(now); got != tokenRestartFire {
		t.Fatalf("after reset: got %v, want fire", got)
	}
}

// --- the diagnosis -----------------------------------------------------------

func TestDiagnoseStuckLoginNamesTheSessionStateFile(t *testing.T) {
	home := t.TempDir()
	agent := claudeHomeAgent(t, home)
	m := &Manager{logger: slog.Default()}

	credPath := writeClaudeCredential(t, home, time.Now().Add(6*time.Hour))
	// The skeleton a clobber leaves behind: valid credential, no identity.
	sessionPath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(sessionPath, []byte(`{"userID":"u1","machineID":"m1"}`), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	got := m.diagnoseStuckLogin(agent)

	// The whole point is that the operator can tell these two files apart, so
	// both paths must appear, and the message must say the credential is FINE.
	for _, want := range []string{credPath, sessionPath, "oauthAccount", "4596"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnosis does not mention %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "valid") {
		t.Errorf("diagnosis should say the credential is valid:\n%s", got)
	}
	// It must also say what will NOT help, because "restart it" is the first
	// thing an operator tries — and is what hive itself was doing on a loop.
	if !strings.Contains(strings.ToLower(got), "restarting cannot fix") {
		t.Errorf("diagnosis should say restarting cannot fix it:\n%s", got)
	}
}

func TestDiagnoseStuckLoginSignedInSaysCauseIsElsewhere(t *testing.T) {
	home := t.TempDir()
	agent := claudeHomeAgent(t, home)
	m := &Manager{logger: slog.Default()}

	writeClaudeCredential(t, home, time.Now().Add(6*time.Hour))
	if err := os.WriteFile(filepath.Join(home, ".claude.json"),
		[]byte(`{"hasCompletedOnboarding":true,"oauthAccount":{"accountUuid":"a-1"}}`), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	got := m.diagnoseStuckLogin(agent)
	// Both files are good, so the message must NOT blame the session state —
	// a false #4596 diagnosis would send the next investigator down the wrong
	// path, which is the exact cost this change is trying to remove.
	if strings.Contains(got, "4596") {
		t.Errorf("healthy session state must not be blamed on #4596:\n%s", got)
	}
	if !strings.Contains(got, "not on-disk state") {
		t.Errorf("diagnosis should point away from on-disk state:\n%s", got)
	}
}

func TestDiagnoseStuckLoginNoCredential(t *testing.T) {
	home := t.TempDir()
	agent := claudeHomeAgent(t, home)
	m := &Manager{logger: slog.Default()}

	// No credential written at all: the restart fired because the SHARED path
	// had a token, which this agent does not resolve to.
	got := m.diagnoseStuckLogin(agent)
	if !strings.Contains(got, "no valid credential") {
		t.Errorf("expected a no-credential diagnosis:\n%s", got)
	}
	if !strings.Contains(got, "interactive login") {
		t.Errorf("expected the diagnosis to name the remedy:\n%s", got)
	}
}

func TestDiagnoseStuckLoginNonClaudeBackendStaysGeneric(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	emptySharedPaths(t)
	m := &Manager{logger: slog.Default()}
	agent := &AgentProcess{Name: "quality", UID: 0, Config: config.AgentConfig{Backend: "copilot"}}

	got := m.diagnoseStuckLogin(agent)
	// Copilot does not split auth across two files, so a claude-shaped
	// diagnosis would be fabricated.
	if strings.Contains(got, "4596") || strings.Contains(got, "oauthAccount") {
		t.Errorf("non-claude backend must not get the claude diagnosis:\n%s", got)
	}
	if !strings.Contains(got, "copilot") {
		t.Errorf("diagnosis should name the backend:\n%s", got)
	}
}

// TestDiagnoseStuckLoginUnreadableDoesNotBlameTheAgent pins the perspective
// of the unreadable-session diagnosis. Under the per-agent home layout the
// agent's CLI rewrites .claude.json agent-owned at 0600, which the hive
// process cannot read — the NORMAL state of a healthy signed-in agent. An
// earlier wording claimed "the CLI cannot load the signed-in identity",
// which is a statement about the AGENT's view made from a process that
// merely could not look; it sent a real investigation chasing a permission
// problem on a file the agent read fine.
func TestDiagnoseStuckLoginUnreadableDoesNotBlameTheAgent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — mode 0000 is still readable, so this cannot be measured")
	}
	home := t.TempDir()
	agent := claudeHomeAgent(t, home)
	m := &Manager{logger: slog.Default()}

	credPath := writeClaudeCredential(t, home, time.Now().Add(6*time.Hour))
	sessionPath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(sessionPath, []byte(`{"oauthAccount":{"accountUuid":"a-1"}}`), 0o000); err != nil {
		t.Fatalf("write session: %v", err)
	}

	got := m.diagnoseStuckLogin(agent)
	for _, want := range []string{credPath, sessionPath, "hive process"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnosis does not mention %q:\n%s", want, got)
		}
	}
	// The message must not assert what the agent's own CLI can or cannot
	// load — that is exactly the overclaim being removed.
	if strings.Contains(got, "cannot load") {
		t.Errorf("diagnosis must not claim the agent's CLI cannot load the identity:\n%s", got)
	}
	if !strings.Contains(got, "cannot be determined") {
		t.Errorf("diagnosis should say the identity is undetermined from the hive's view:\n%s", got)
	}
}
