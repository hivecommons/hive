package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// claude_credential_bridge_test.go: a Claude Code token refresh writes a temp
// file and renames it over $HOME/.claude/.credentials.json, which replaces the
// per-agent symlink to the shared login with a private regular file. These
// tests drive the three entry points that must repair it (agent launch, the
// credential watchdog, and the dashboard sign-in reload) through the same
// public paths production uses. Every token is a fake JSON blob written into a
// t.TempDir().

// fakeClaudeCredential is a credentials file in the on-disk shape Claude Code
// writes, with fake token strings.
func fakeClaudeCredential(access string, expiresAt time.Time, refresh string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":%q,"expiresAt":%d,"scopes":["user:inference"]}}`,
		access, refresh, expiresAt.UnixMilli())
}

// brokenBridgeFixture lays out a per-agent home whose credential symlink a
// token refresh has replaced: the shared credential holds sharedJSON and the
// agent's .claude/.credentials.json is a REGULAR file holding agentJSON, mode
// 0600 as the CLI writes it. Returns (link, target).
func brokenBridgeFixture(t *testing.T, shared, agentName, sharedJSON, agentJSON string) (string, string) {
	t.Helper()
	target := filepath.Join(shared, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if sharedJSON != "" {
		if err := os.WriteFile(target, []byte(sharedJSON), 0o660); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(shared, "agents", agentName, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte(agentJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return link, target
}

func assertBridgeRestored(t *testing.T, link, target string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("credential link missing after repair: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is still a regular file (mode %v); the bridge was not restored", link, info.Mode())
	}
	if got, _ := os.Readlink(link); got != target {
		t.Fatalf("credential link -> %q, want %q", got, target)
	}
}

// staleFiles lists the moved-aside credentials in dir.
func staleFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".credentials.json.stale-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSetupInteractiveHome_RebridgesCredentialReplacedByRefresh_KeepsNewerShared
// is the hive-wild-mole state: the agent's private copy is an older refresh
// (whose grant was later revoked) and the shared file holds a newer dashboard
// sign-in. Launch must move the private file aside, keep the shared file
// as it is, and restore the symlink. On the v6 tip bridgeHomeEntry refused
// to touch the real file, so the agent ran on the dead copy forever.
func TestSetupInteractiveHome_RebridgesCredentialReplacedByRefresh_KeepsNewerShared(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	now := time.Now()
	sharedJSON := fakeClaudeCredential("fake-signin-access", now.Add(8*time.Hour), "fake-signin-refresh")
	agentJSON := fakeClaudeCredential("fake-refreshed-access", now.Add(time.Hour), "fake-refreshed-refresh")
	link, target := brokenBridgeFixture(t, shared, "architect", sharedJSON, agentJSON)

	ap := &AgentProcess{Name: "architect", UID: 2001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	assertBridgeRestored(t, link, target)
	if got := readFileString(t, target); got != sharedJSON {
		t.Errorf("shared credential changed although it was the newer one:\n got %s\nwant %s", got, sharedJSON)
	}
	stale := staleFiles(t, filepath.Dir(link))
	if len(stale) != 1 {
		t.Fatalf("want exactly one moved-aside private file, got %v", stale)
	}
	if got := readFileString(t, stale[0]); got != agentJSON {
		t.Errorf("moved-aside file does not hold the private credential: %s", got)
	}
	if info, err := os.Stat(stale[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("moved-aside private file must keep its 0600 mode: info=%v err=%v", info, err)
	}
	if got := staleFiles(t, filepath.Dir(target)); len(got) != 0 {
		t.Errorf("shared credential was not replaced, so nothing beside it should be moved aside: %v", got)
	}
	ev := sink.find("claude_credential_rebridged")
	if ev == nil {
		t.Fatal("no claude_credential_rebridged audit entry for the repair")
	}
	if ev.Agent != "architect" || ev.Fields["trigger"] != "launch" || ev.Fields["kept"] != "shared" {
		t.Errorf("audit entry = %+v, want agent=architect trigger=launch kept=shared", ev)
	}
	for k, v := range ev.Fields {
		if s, ok := v.(string); ok && strings.Contains(s, "fake-") {
			t.Errorf("audit field %s carries token material: %q", k, s)
		}
	}
}

// TestSetupInteractiveHome_RebridgePromotesNewerUsablePrivateCredential: when
// the agent's private copy is the NEWER usable one (it just refreshed, and the
// refresh rotated the grant the shared file still holds), the repair promotes
// it into the shared file so the rest of the fleet is not stranded, and
// preserves the old shared file instead of losing it.
func TestSetupInteractiveHome_RebridgePromotesNewerUsablePrivateCredential(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	now := time.Now()
	sharedJSON := fakeClaudeCredential("fake-old-access", now.Add(-time.Hour), "fake-old-refresh")
	agentJSON := fakeClaudeCredential("fake-new-access", now.Add(8*time.Hour), "fake-new-refresh")
	link, target := brokenBridgeFixture(t, shared, "coder", sharedJSON, agentJSON)

	ap := &AgentProcess{Name: "coder", UID: 2002, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	assertBridgeRestored(t, link, target)
	if got := readFileString(t, target); got != agentJSON {
		t.Errorf("shared credential should now hold the newer private token:\n got %s\nwant %s", got, agentJSON)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o660 {
		t.Errorf("promoted shared credential must be 0660 (group-readable by every agent uid): info=%v err=%v", info, err)
	}
	oldShared := staleFiles(t, filepath.Dir(target))
	if len(oldShared) != 1 || readFileString(t, oldShared[0]) != sharedJSON {
		t.Errorf("replaced shared credential must be preserved beside it, got %v", oldShared)
	}
	if got := staleFiles(t, filepath.Dir(link)); len(got) != 1 {
		t.Errorf("private file must be moved aside, not deleted: %v", got)
	}
	if ev := sink.find("claude_credential_rebridged"); ev == nil || ev.Fields["kept"] != "agent" {
		t.Errorf("audit entry = %+v, want kept=agent", ev)
	}
}

// TestSetupInteractiveHome_RebridgeNeverPromotesUnusablePrivateCredential: a
// private copy with a later expiresAt but no usable grant must not overwrite
// the shared file; "newer" alone is not enough.
func TestSetupInteractiveHome_RebridgeNeverPromotesUnusablePrivateCredential(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	now := time.Now()
	sharedJSON := fakeClaudeCredential("fake-shared-access", now.Add(-2*time.Hour), "fake-shared-refresh")
	// Expired access token and no refresh grant: not usable.
	agentJSON := fakeClaudeCredential("fake-dead-access", now.Add(-time.Minute), "")
	link, target := brokenBridgeFixture(t, shared, "scanner", sharedJSON, agentJSON)

	ap := &AgentProcess{Name: "scanner", UID: 2003, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	assertBridgeRestored(t, link, target)
	if got := readFileString(t, target); got != sharedJSON {
		t.Errorf("an unusable private credential replaced the shared one: %s", got)
	}
}

// TestCredentialWatchdog_RebridgesWithinOneCycle: a refresh that replaces the
// link while the agent is running must be caught by the next watchdog tick,
// not only at the next launch.
func TestCredentialWatchdog_RebridgesWithinOneCycle(t *testing.T) {
	shared := withSharedAgentHome(t)
	t.Setenv(CredentialWatchdogIntervalEnv, "20ms")
	m, sink := testManagerWithSink(t)
	m.mu.Lock()
	m.agents["planner"] = &AgentProcess{Name: "planner", UID: 2004, Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Unlock()
	now := time.Now()
	link, target := brokenBridgeFixture(t, shared, "planner",
		fakeClaudeCredential("fake-shared-access", now.Add(8*time.Hour), "fake-shared-refresh"),
		fakeClaudeCredential("fake-private-access", now.Add(time.Hour), "fake-private-refresh"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.StartCredentialWatchdog(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// One cycle is 20ms; allow generous scheduling slack but far less than a
	// second cycle would need to matter.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertBridgeRestored(t, link, target)
	if ev := sink.find("claude_credential_rebridged"); ev == nil || ev.Fields["trigger"] != "watchdog" {
		t.Errorf("audit entry = %+v, want trigger=watchdog", ev)
	}
}

// TestReloadClaudeToken_RebridgesAfterSignIn: the dashboard sign-in writes the
// shared credential and calls ReloadClaudeToken. That must reach the agents a
// refresh had cut off, or the sign-in cannot help them (the hive-wild-mole
// 14:31Z and 14:50Z sign-ins).
func TestReloadClaudeToken_RebridgesAfterSignIn(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	m.mu.Lock()
	m.agents["reviewer"] = &AgentProcess{Name: "reviewer", UID: 2005, Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Unlock()
	now := time.Now()
	link, target := brokenBridgeFixture(t, shared, "reviewer",
		fakeClaudeCredential("fake-signin-access", now.Add(8*time.Hour), "fake-signin-refresh"),
		fakeClaudeCredential("fake-revoked-access", now.Add(-10*time.Minute), "fake-revoked-refresh"))

	m.ReloadClaudeToken()

	assertBridgeRestored(t, link, target)
	if ev := sink.find("claude_credential_rebridged"); ev == nil || ev.Fields["trigger"] != "sign-in" || ev.Fields["kept"] != "shared" {
		t.Errorf("audit entry = %+v, want trigger=sign-in kept=shared", ev)
	}
}

// TestReloadClaudeToken_LogoutDoesNotRebridge: after a logout the shared file
// is gone; moving a private credential aside then would leave the agent with
// nothing at all.
func TestReloadClaudeToken_LogoutDoesNotRebridge(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	m.mu.Lock()
	m.agents["reviewer"] = &AgentProcess{Name: "reviewer", UID: 2005, Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Unlock()
	link, _ := brokenBridgeFixture(t, shared, "reviewer", "",
		fakeClaudeCredential("fake-private-access", time.Now().Add(time.Hour), "fake-private-refresh"))

	m.ReloadClaudeToken()

	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Errorf("private credential must stay in place when there is no shared login: info=%v err=%v", info, err)
	}
}
