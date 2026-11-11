package agent

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/config"
)

// TestRepairClaudeCredentialBridge_PromotionKeepsSharedOwner: the promoted
// shared credential is written by the hive process, so without a chown it would
// belong to whoever runs hive. It must keep the owner and group of the file it
// replaces. The chown is a seam so this runs without root or real agent uids.
func TestRepairClaudeCredentialBridge_PromotionKeepsSharedOwner(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	now := time.Now()
	link, target := brokenBridgeFixture(t, shared, "coder",
		fakeClaudeCredential("fake-old-access", now.Add(-time.Hour), "fake-old-refresh"),
		fakeClaudeCredential("fake-new-access", now.Add(8*time.Hour), "fake-new-refresh"))
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)

	type chownCall struct {
		path     string
		uid, gid int
	}
	var calls []chownCall
	orig := chownClaudeCredential
	chownClaudeCredential = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path, uid, gid})
		return nil
	}
	t.Cleanup(func() { chownClaudeCredential = orig })

	rep := m.rebridgeClaudeCredential("coder", link, target, claudeBridgeTriggerWatchdog)
	if rep == nil || rep.kept != "agent" {
		t.Fatalf("repair = %+v, want kept=agent", rep)
	}
	if len(calls) != 1 || calls[0].uid != int(st.Uid) || calls[0].gid != int(st.Gid) {
		t.Fatalf("chown calls = %+v, want one call with the previous shared owner %d:%d", calls, st.Uid, st.Gid)
	}
	if filepath.Dir(calls[0].path) != filepath.Dir(target) {
		t.Errorf("chown applied to %q, want the replacement written beside %q", calls[0].path, target)
	}
}

// TestRepairClaudeCredentialBridge_StaleNamesNeverCollide: two repairs in the
// same second must not overwrite the first moved-aside file.
func TestRepairClaudeCredentialBridge_StaleNamesNeverCollide(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	fixed := time.Date(2026, 11, 7, 14, 54, 0, 0, time.UTC)
	orig := claudeBridgeNow
	claudeBridgeNow = func() time.Time { return fixed }
	t.Cleanup(func() { claudeBridgeNow = orig })

	sharedJSON := fakeClaudeCredential("fake-shared-access", time.Now().Add(8*time.Hour), "fake-shared-refresh")
	link, target := brokenBridgeFixture(t, shared, "tester", sharedJSON,
		fakeClaudeCredential("fake-first-access", time.Now().Add(time.Hour), "fake-first-refresh"))
	m.rebridgeClaudeCredential("tester", link, target, claudeBridgeTriggerWatchdog)

	// The CLI refreshes again within the same second.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte(fakeClaudeCredential("fake-second-access", time.Now().Add(time.Hour), "fake-second-refresh")), 0o600); err != nil {
		t.Fatal(err)
	}
	m.rebridgeClaudeCredential("tester", link, target, claudeBridgeTriggerWatchdog)

	if got := staleFiles(t, filepath.Dir(link)); len(got) != 2 {
		t.Fatalf("want two distinct moved-aside files, got %v", got)
	}
	assertBridgeRestored(t, link, target)
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

// TestPreferAgentClaudeCredential pins the "newer AND usable" rule.
func TestPreferAgentClaudeCredential(t *testing.T) {
	now := time.Now()
	tok := func(exp time.Duration, refresh string) *claude.OAuthTokens {
		return &claude.OAuthTokens{AccessToken: "fake", RefreshToken: refresh, ExpiresAt: now.Add(exp).UnixMilli()}
	}
	cases := []struct {
		name          string
		agent, shared *claude.OAuthTokens
		want          bool
	}{
		{"agent newer and live", tok(8*time.Hour, "r"), tok(time.Hour, "r"), true},
		{"shared newer", tok(time.Hour, "r"), tok(8*time.Hour, "r"), false},
		{"tie keeps shared", tok(time.Hour, "r"), tok(time.Hour, "r"), false},
		{"agent expired but refreshable, shared absent", tok(-time.Hour, "r"), nil, true},
		{"agent unusable, shared absent", tok(-time.Hour, ""), nil, false},
		{"agent usable, shared unusable", tok(-time.Minute, "r"), tok(-2*time.Hour, ""), true},
		{"agent absent", nil, tok(time.Hour, "r"), false},
	}
	for _, c := range cases {
		if got := preferAgentClaudeCredential(c.agent, c.shared, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
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

	rep, err := m.repairClaudeCredentialBridge("reviewer", link, target)
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

	if _, err := m.repairClaudeCredentialBridge("scanner", link, target); !errors.Is(err, errClaudeBridgeLinkIsTarget) {
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

	rep, err := m.repairClaudeCredentialBridge("coder", target, target)
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

// withUnreadableClaudeCredential makes the repair's credential reader fail
// with EACCES for every path unreadable(path) accepts, as the hive server's
// uid does on an agent's 0600 file in a per-UID deployment. A test cannot
// produce that for real without root and a second uid.
func withUnreadableClaudeCredential(t *testing.T, unreadable func(path string) bool) {
	t.Helper()
	orig := claudeBridgeReadTokens
	claudeBridgeReadTokens = func(path string) (*claude.OAuthTokens, error) {
		if unreadable(path) {
			return nil, &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}
		return orig(path)
	}
	t.Cleanup(func() { claudeBridgeReadTokens = orig })
}

// TestRepairClaudeCredentialBridge_UnreadablePrivateCredentialLeftInPlace is
// the live per-UID incident: an agent's refresh left a fresh 0600 private
// file (the only live token, its refresh rotated the shared grant) that the
// hive server cannot read. On the v6 tip the read error looked like "no
// token": the file was moved aside, kept=shared, and the agent was relinked
// to the dead grant. Now nothing moves, nothing is linked, the dashboard
// still reports the bridge as broken, and the ERROR is logged once per file
// version rather than every watchdog cycle.
func TestRepairClaudeCredentialBridge_UnreadablePrivateCredentialLeftInPlace(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	var logs bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logs, nil))
	now := time.Now()
	sharedJSON := fakeClaudeCredential("fake-rotated-access", now.Add(3*time.Minute), "fake-rotated-refresh")
	agentJSON := fakeClaudeCredential("fake-fresh-access", now.Add(8*time.Hour), "fake-fresh-refresh")
	link, target := brokenBridgeFixture(t, shared, "quality", sharedJSON, agentJSON)
	agentDir := filepath.Dir(link)
	withUnreadableClaudeCredential(t, func(path string) bool { return filepath.Dir(path) == agentDir })

	rep, err := m.repairClaudeCredentialBridge("quality", link, target)
	if rep != nil || !errors.Is(err, errClaudeBridgeAgentUnreadable) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("repair = (%+v, %v), want (nil, errClaudeBridgeAgentUnreadable wrapping EACCES)", rep, err)
	}
	for cycle := 0; cycle < 3; cycle++ {
		if got := m.rebridgeClaudeCredential("quality", link, target, claudeBridgeTriggerWatchdog); got != nil {
			t.Fatalf("cycle %d: rebridge = %+v, want nil (left in place)", cycle, got)
		}
	}

	info, err := os.Lstat(link)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("private credential must stay a regular file at the link path: info=%v err=%v", info, err)
	}
	if got := readFileString(t, link); got != agentJSON {
		t.Errorf("private credential content changed: %s", got)
	}
	if got := staleFiles(t, agentDir); len(got) != 0 {
		t.Errorf("an unreadable private credential was moved aside: %v", got)
	}
	if got := readFileString(t, target); got != sharedJSON {
		t.Errorf("shared credential changed: %s", got)
	}
	if got := staleFiles(t, filepath.Dir(target)); len(got) != 0 {
		t.Errorf("shared credential was moved aside: %v", got)
	}
	if !claudeCredentialBridgeBroken("quality", 2007, "claude") {
		t.Error("the dashboard must keep reporting the bridge as broken while the private copy is in place")
	}
	if ev := sink.find("claude_credential_rebridged"); ev != nil {
		t.Errorf("nothing changed, so nothing may be audited as a re-bridge: %+v", ev)
	}
	const msg = "cannot read this agent's private credential"
	if n := strings.Count(logs.String(), msg); n != 1 {
		t.Fatalf("unreadable-credential error logged %d times over 3 cycles, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "permission denied") ||
		!strings.Contains(logs.String(), link) {
		t.Errorf("log must be an ERROR naming the path and the errno:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "fake-") {
		t.Errorf("log carries token material:\n%s", logs.String())
	}

	// The CLI refreshes again: a new file version is worth one more line.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(link, later, later); err != nil {
		t.Fatal(err)
	}
	m.rebridgeClaudeCredential("quality", link, target, claudeBridgeTriggerWatchdog)
	if n := strings.Count(logs.String(), msg); n != 2 {
		t.Errorf("a new private file version must be logged again: got %d lines, want 2", n)
	}
}

// TestRepairClaudeCredentialBridge_UnreadableAfterMoveIsPutBack: the file at
// the link was readable when checked, but the one actually renamed (the CLI
// replaced it in between) is not. It is put back at the link path, not left
// at a stale name with the agent relinked to the shared file.
func TestRepairClaudeCredentialBridge_UnreadableAfterMoveIsPutBack(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	now := time.Now()
	sharedJSON := fakeClaudeCredential("fake-shared-access", now.Add(time.Hour), "fake-shared-refresh")
	agentJSON := fakeClaudeCredential("fake-fresh-access", now.Add(8*time.Hour), "fake-fresh-refresh")
	link, target := brokenBridgeFixture(t, shared, "scanner", sharedJSON, agentJSON)
	withUnreadableClaudeCredential(t, func(path string) bool {
		return strings.Contains(filepath.Base(path), claudeCredentialStaleInfix)
	})

	rep, err := m.repairClaudeCredentialBridge("scanner", link, target)
	if rep != nil || !errors.Is(err, errClaudeBridgeAgentUnreadable) {
		t.Fatalf("repair = (%+v, %v), want (nil, errClaudeBridgeAgentUnreadable)", rep, err)
	}
	info, err := os.Lstat(link)
	if err != nil || !info.Mode().IsRegular() || readFileString(t, link) != agentJSON {
		t.Fatalf("private credential must be back at the link path: info=%v err=%v", info, err)
	}
	if got := staleFiles(t, filepath.Dir(link)); len(got) != 0 {
		t.Errorf("nothing may stay at a stale name: %v", got)
	}
	if got := readFileString(t, target); got != sharedJSON {
		t.Errorf("shared credential changed: %s", got)
	}
}

// TestRepairClaudeCredentialBridge_EmptyTokenPrivateNeverWins: a failed refresh
// writes a private file with empty tokens and expiresAt 0. It must never
// replace the shared login, usable or not; the agent is relinked to it.
func TestRepairClaudeCredentialBridge_EmptyTokenPrivateNeverWins(t *testing.T) {
	const emptyJSON = `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"scopes":[]}}`
	now := time.Now()
	cases := map[string]string{
		"usable shared":   fakeClaudeCredential("fake-live-access", now.Add(4*time.Hour), "fake-live-refresh"),
		"unusable shared": fakeClaudeCredential("fake-dead-access", now.Add(-time.Hour), ""),
	}
	for name, sharedJSON := range cases {
		t.Run(name, func(t *testing.T) {
			shared := withSharedAgentHome(t)
			m, sink := testManagerWithSink(t)
			link, target := brokenBridgeFixture(t, shared, "sec-check", sharedJSON, emptyJSON)

			rep := m.rebridgeClaudeCredential("sec-check", link, target, claudeBridgeTriggerWatchdog)
			if rep == nil || rep.kept != "shared" {
				t.Fatalf("repair = %+v, want kept=shared", rep)
			}
			if got := readFileString(t, target); got != sharedJSON {
				t.Errorf("an empty-token private file replaced the shared credential: %s", got)
			}
			if got := staleFiles(t, filepath.Dir(target)); len(got) != 0 {
				t.Errorf("shared credential must not be replaced or moved: %v", got)
			}
			assertBridgeRestored(t, link, target)
			if ev := sink.find("claude_credential_rebridged"); ev == nil || ev.Fields["kept"] != "shared" {
				t.Errorf("audit entry = %+v, want kept=shared", ev)
			}
		})
	}
}

// TestClaudeBridgePromotable_RejectsIncompleteCredentials pins the shared
// promotion rule (also applied by the root perm guard in entrypoint.sh): only a
// complete, usable credential may replace the shared login. On the v6 tip a
// token with no recorded expiresAt counted as usable and newer than an
// expired shared login, and so did one with no refresh token.
func TestClaudeBridgePromotable_RejectsIncompleteCredentials(t *testing.T) {
	now := time.Now()
	deadShared := &claude.OAuthTokens{AccessToken: "fake", ExpiresAt: now.Add(-time.Hour).UnixMilli()}
	cases := []struct {
		name  string
		agent *claude.OAuthTokens
		want  bool
	}{
		{"complete and live", &claude.OAuthTokens{AccessToken: "a", RefreshToken: "r", ExpiresAt: now.Add(time.Hour).UnixMilli()}, true},
		{"empty tokens, expiresAt 0", &claude.OAuthTokens{}, false},
		{"tokens but no expiresAt", &claude.OAuthTokens{AccessToken: "a", RefreshToken: "r"}, false},
		{"no refresh token", &claude.OAuthTokens{AccessToken: "a", ExpiresAt: now.Add(time.Hour).UnixMilli()}, false},
		{"no access token", &claude.OAuthTokens{RefreshToken: "r", ExpiresAt: now.Add(time.Hour).UnixMilli()}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := claudeBridgePromotable(c.agent, now); got != c.want {
			t.Errorf("%s: claudeBridgePromotable = %v, want %v", c.name, got, c.want)
		}
		if got := preferAgentClaudeCredential(c.agent, deadShared, now); got != c.want {
			t.Errorf("%s: preferAgentClaudeCredential over a dead shared login = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestPromoteClaudeCredential_RefusesUnusableBytes: the promotion re-checks the
// bytes it is about to write, so a source that changed after it was judged
// can never blank the shared login. Nothing is preserved or written.
func TestPromoteClaudeCredential_RefusesUnusableBytes(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "private.json")
	target := filepath.Join(dir, ".credentials.json")
	now := time.Now()
	sharedJSON := fakeClaudeCredential("fake-live-access", now.Add(time.Hour), "fake-live-refresh")
	if err := os.WriteFile(target, []byte(sharedJSON), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := promoteClaudeCredential(src, target, now); !errors.Is(err, errClaudeBridgeNotPromotable) {
		t.Fatalf("promote error = %v, want errClaudeBridgeNotPromotable", err)
	}
	if got := readFileString(t, target); got != sharedJSON {
		t.Errorf("shared credential changed: %s", got)
	}
	if got := staleFiles(t, dir); len(got) != 0 {
		t.Errorf("a refused promotion must not preserve or move anything: %v", got)
	}
}
