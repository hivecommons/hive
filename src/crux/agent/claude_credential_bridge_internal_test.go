package agent

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// Fixture helpers shared by the tests in this file (the v6 #17253 suite).
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

// TestAgentSnapshot_ReportsCredentialBridgeBroken: the status the dashboard
// reads names the broken bridge, and stops naming it once it is repaired.
func TestAgentSnapshot_ReportsCredentialBridgeBroken(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	ap := &AgentProcess{Name: "architect", UID: 2001, Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Lock()
	m.agents["architect"] = ap
	m.mu.Unlock()
	link, _ := brokenBridgeFixture(t, shared, "architect",
		fakeClaudeCredential("fake-shared-access", time.Now().Add(8*time.Hour), "fake-shared-refresh"),
		fakeClaudeCredential("fake-private-access", time.Now().Add(time.Hour), "fake-private-refresh"))

	if snap := ap.snapshot(); !snap.CredentialBridgeBroken {
		t.Fatal("snapshot does not report the private file that replaced the credential link")
	}
	if got := m.RepairClaudeCredentialBridges(claudeBridgeTriggerWatchdog); len(got) != 1 || got[0] != "architect" {
		t.Fatalf("RepairClaudeCredentialBridges = %v, want [architect]", got)
	}
	if snap := ap.snapshot(); snap.CredentialBridgeBroken {
		t.Errorf("snapshot still reports a broken bridge after the repair (link %s)", link)
	}

	// Non-claude agents never report it, even with a stray regular file.
	other := &AgentProcess{Name: "copilot-one", UID: 2006, Config: config.AgentConfig{Backend: "copilot"}}
	brokenBridgeFixture(t, shared, "copilot-one", "", fakeClaudeCredential("fake-x", time.Now(), ""))
	if other.snapshot().CredentialBridgeBroken {
		t.Error("a copilot agent must not report a Claude credential bridge")
	}
}

// legacyDirLinkFixture lays out the pre-#17031 layout that the launch path
// retires but the credential watchdog still walks for a paused or stopped
// agent: the agent's whole ~/.claude is a directory symlink to the shared
// .claude, so the path the bridge calls the agent's credential IS the shared
// login. The shared credential holds sharedJSON. Returns the link path exactly
// as production computes it, the target, and the shared file's FileInfo
// captured before any repair runs.
func legacyDirLinkFixture(t *testing.T, shared, agentName string, uid int, sharedJSON string) (string, string, os.FileInfo) {
	t.Helper()
	sharedClaude := filepath.Join(shared, ".claude")
	if err := os.MkdirAll(sharedClaude, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sharedClaude, ".credentials.json")
	if err := os.WriteFile(target, []byte(sharedJSON), 0o660); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(shared, "agents", agentName)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sharedClaude, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	link, gotTarget, ok := claudeCredentialBridgePaths(agentName, uid, "claude")
	if !ok || gotTarget != target {
		t.Fatalf("claudeCredentialBridgePaths = (%q, %q, %v), want target %q", link, gotTarget, ok, target)
	}
	before, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	return link, target, before
}

// assertSharedCredentialUntouched: the shared login is still the very same
// regular file (same inode, not a symlink to anything, let alone itself),
// with the same bytes, and nothing was moved aside beside it.
func assertSharedCredentialUntouched(t *testing.T, target string, before os.FileInfo, wantJSON string) {
	t.Helper()
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("shared credential gone: %v", err)
	}
	if !info.Mode().IsRegular() {
		dest, _ := os.Readlink(target)
		t.Fatalf("shared credential is no longer a regular file (mode %v, link -> %q)", info.Mode(), dest)
	}
	if !os.SameFile(before, info) {
		t.Errorf("shared credential was replaced (new inode); it must not be renamed or rewritten")
	}
	if got := readFileString(t, target); got != wantJSON {
		t.Errorf("shared credential content changed:\n got %s\nwant %s", got, wantJSON)
	}
	if got := staleFiles(t, filepath.Dir(target)); len(got) != 0 {
		t.Errorf("shared credential was moved aside: %v", got)
	}
}

// TestRepairClaudeCredentialBridge_LegacyDirLinkUsableSharedUntouched: on the
// v6 tip the repair saw the shared file through the directory link as a
// "private copy", renamed it to .stale-<UTC>, promoted the same bytes back
// (new inode) and then failed the symlink with EEXIST, every watchdog cycle.
// It must leave the shared file alone and say why.
func TestRepairClaudeCredentialBridge_LegacyDirLinkUsableSharedUntouched(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	sharedJSON := fakeClaudeCredential("fake-shared-access", time.Now().Add(8*time.Hour), "fake-shared-refresh")
	link, target, before := legacyDirLinkFixture(t, shared, "reviewer", 2005, sharedJSON)

	rep, err := m.repairClaudeCredentialBridge("reviewer", link, target, claudeBridgeTriggerWatchdog)
	if rep != nil || !errors.Is(err, errClaudeBridgeLinkIsTarget) {
		t.Fatalf("repair = (%+v, %v), want (nil, errClaudeBridgeLinkIsTarget)", rep, err)
	}
	if got := m.rebridgeClaudeCredential("reviewer", link, target, claudeBridgeTriggerWatchdog); got != nil {
		t.Fatalf("rebridge = %+v, want nil (skipped)", got)
	}
	assertSharedCredentialUntouched(t, target, before, sharedJSON)
	if ev := sink.find("claude_credential_rebridged"); ev != nil {
		t.Errorf("a skipped legacy layout must not be audited as a re-bridge: %+v", ev)
	}
}

// TestRepairClaudeCredentialBridge_LegacyDirLinkTokenlessSharedNotSelfLinked
// is the live incident: a logged-out (token-less) shared file is not
// promoted, so on the v6 tip the repair renamed it away and then created
// .credentials.json -> .credentials.json, a symlink to itself, logging out
// every agent. No symlink may be created and the file must stay in place.
func TestRepairClaudeCredentialBridge_LegacyDirLinkTokenlessSharedNotSelfLinked(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	const sharedJSON = `{}`
	link, target, before := legacyDirLinkFixture(t, shared, "scanner", 2003, sharedJSON)

	if _, err := m.repairClaudeCredentialBridge("scanner", link, target, claudeBridgeTriggerWatchdog); !errors.Is(err, errClaudeBridgeLinkIsTarget) {
		t.Fatalf("repair error = %v, want errClaudeBridgeLinkIsTarget", err)
	}
	assertSharedCredentialUntouched(t, target, before, sharedJSON)
}

// TestRepairClaudeCredentialBridges_LegacyDirLinkSurvivesWatchdogCyclesWarnsOnce:
// the watchdog walks every managed agent each cycle, paused ones included.
// Several cycles over the legacy layout leave the shared login untouched,
// re-bridge nothing, and log the skip once rather than every cycle.
func TestRepairClaudeCredentialBridges_LegacyDirLinkSurvivesWatchdogCyclesWarnsOnce(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	var logs bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logs, nil))
	m.mu.Lock()
	m.agents["planner"] = &AgentProcess{Name: "planner", UID: 2004, Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Unlock()
	sharedJSON := fakeClaudeCredential("fake-shared-access", time.Now().Add(8*time.Hour), "fake-shared-refresh")
	_, target, before := legacyDirLinkFixture(t, shared, "planner", 2004, sharedJSON)

	for cycle := 0; cycle < 4; cycle++ {
		if got := m.RepairClaudeCredentialBridges(claudeBridgeTriggerWatchdog); len(got) != 0 {
			t.Fatalf("cycle %d re-bridged %v, want none", cycle, got)
		}
	}
	assertSharedCredentialUntouched(t, target, before, sharedJSON)
	if n := strings.Count(logs.String(), "legacy whole-directory symlink"); n != 1 {
		t.Errorf("legacy-layout warning logged %d times over 4 cycles, want 1:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "repair failed") {
		t.Errorf("the legacy layout must be skipped, not reported as a failed repair:\n%s", logs.String())
	}
}

// TestRepairClaudeCredentialBridge_RefusesLinkEqualToTarget: the last line of
// defence. A link path equal to its own target is refused before anything is
// moved: nothing renamed, nothing created.
func TestRepairClaudeCredentialBridge_RefusesLinkEqualToTarget(t *testing.T) {
	withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	target := sharedClaudeBridgeTarget()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	const sharedJSON = `{}`
	if err := os.WriteFile(target, []byte(sharedJSON), 0o660); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}

	rep, err := m.repairClaudeCredentialBridge("coder", target, target, claudeBridgeTriggerWatchdog)
	if rep != nil || !errors.Is(err, errClaudeBridgeLinkIsTarget) {
		t.Fatalf("repair = (%+v, %v), want (nil, errClaudeBridgeLinkIsTarget)", rep, err)
	}
	assertSharedCredentialUntouched(t, target, before, sharedJSON)
}

// TestClaudeCredentialBridgeBroken_LegacyDirLinkIsNotBroken: the dashboard's
// "bridge broken" signal follows the repair's rule. The legacy layout is not
// a broken bridge (the agent reads the shared login directly), while a real
// per-agent .claude holding a private regular file still is.
func TestClaudeCredentialBridgeBroken_LegacyDirLinkIsNotBroken(t *testing.T) {
	shared := withSharedAgentHome(t)
	legacyDirLinkFixture(t, shared, "architect", 2001, `{}`)
	if claudeCredentialBridgeBroken("architect", 2001, "claude") {
		t.Error("the legacy whole-directory layout must not report a broken credential bridge")
	}

	brokenBridgeFixture(t, shared, "coder", "",
		fakeClaudeCredential("fake-private-access", time.Now().Add(time.Hour), "fake-private-refresh"))
	if !claudeCredentialBridgeBroken("coder", 2002, "claude") {
		t.Error("a private regular file in a real per-agent .claude must still report a broken bridge")
	}
}

// TestSetupInteractiveHome_LegacyDirLinkLaunchRetiresBeforeRebridge pins the
// launch ordering: setupPerAgentCLIHome retires the directory link and makes
// a real per-agent .claude BEFORE it runs the credential repair, so a launch
// never sees the shared file through the old link. The shared login stays the
// same file and the agent ends up bridged to it.
func TestSetupInteractiveHome_LegacyDirLinkLaunchRetiresBeforeRebridge(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	sharedJSON := fakeClaudeCredential("fake-shared-access", time.Now().Add(8*time.Hour), "fake-shared-refresh")
	link, target, before := legacyDirLinkFixture(t, shared, "coder", 2002, sharedJSON)

	m.setupInteractiveHome(&AgentProcess{Name: "coder", UID: 2002, Config: config.AgentConfig{Backend: "claude"}}, "claude")

	assertSharedCredentialUntouched(t, target, before, sharedJSON)
	if info, err := os.Lstat(filepath.Dir(link)); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("legacy .claude directory link not retired: info=%v err=%v", info, err)
	}
	assertBridgeRestored(t, link, target)
}
