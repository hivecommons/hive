package agent

// Remote Control startup pin (#5607) — property tests.
//
// The invariant under test: every Claude-CLI launch surface hive provisions
// carries an EXPLICIT "remoteControlAtStartup": false, because an absent key
// delegates the decision to a server-side rollout that flipped to auto-ON.
// Presence of the key is the security property; merge-only semantics (an
// operator's explicit true survives) is the escape hatch.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

func readSettingsMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid JSON in %s: %v", path, err)
	}
	return parsed
}

func assertRemoteControlOff(t *testing.T, path string) {
	t.Helper()
	parsed := readSettingsMap(t, path)
	v, ok := parsed[remoteControlSettingKey]
	if !ok {
		t.Fatalf("%s: %s key ABSENT — an absent key delegates to the server-side rollout default, which is the #5607 exposure", path, remoteControlSettingKey)
	}
	if v != false {
		t.Fatalf("%s: %s = %v, want false", path, remoteControlSettingKey, v)
	}
}

// --- inference seed ----------------------------------------------------------

func TestInferenceSettingsSeed_PinsRemoteControlOff(t *testing.T) {
	seed := inferenceSettingsSeed()
	v, ok := seed[remoteControlSettingKey]
	if !ok {
		t.Fatalf("inferenceSettingsSeed missing %s — inference launches would fall back to the server-side rollout default", remoteControlSettingKey)
	}
	if v != false {
		t.Fatalf("inferenceSettingsSeed[%s] = %v, want false", remoteControlSettingKey, v)
	}
}

func TestEnsureClaudeSettings_PinsRemoteControlOff(t *testing.T) {
	orig := inferenceHomePrefixOverride
	inferenceHomePrefixOverride = filepath.Join(t.TempDir(), "inference-home") + "-"
	t.Cleanup(func() { inferenceHomePrefixOverride = orig })
	os.Remove(claudeInferenceSettingsPath)

	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	m.ensureClaudeSettings("rc-pin", 0)

	// Both settings surfaces the CLI reads for an inference agent:
	// userSettings in the per-agent HOME and the --settings flagSettings file.
	assertRemoteControlOff(t, filepath.Join(inferenceHomePath("rc-pin"), ".claude", "settings.json"))
	assertRemoteControlOff(t, claudeInferenceSettingsPath)
}

// --- shared interactive settings --------------------------------------------

// withSharedClaudeDir provisions the shared home and its .claude dir (the
// entrypoint's job in production) and returns the settings path.
func withSharedClaudeDir(t *testing.T) string {
	t.Helper()
	shared := withSharedAgentHome(t)
	if err := os.MkdirAll(filepath.Join(shared, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sharedClaudeSettingsFile()
}

func rcTestAgent() *AgentProcess {
	return &AgentProcess{Name: "scanner", UID: 1001}
}

func TestEnsureClaudeRemoteControlDefault_SeedsFalse(t *testing.T) {
	path := withSharedClaudeDir(t)
	m := quietManager()

	m.ensureClaudeRemoteControlDefault(rcTestAgent())
	assertRemoteControlOff(t, path)

	// Idempotent: a second launch leaves the pin in place unchanged.
	m.ensureClaudeRemoteControlDefault(rcTestAgent())
	assertRemoteControlOff(t, path)
}

// TestEnsureClaudeRemoteControlDefault_SeedsBypassConsentSuppressor is the
// #14307 guard: interactive claude agents launch with
// --dangerously-skip-permissions, so the shared settings must carry the key
// that keeps Claude Code's "Bypass Permissions mode" screen from appearing.
func TestEnsureClaudeRemoteControlDefault_SeedsBypassConsentSuppressor(t *testing.T) {
	path := withSharedClaudeDir(t)
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o666); err != nil {
		t.Fatal(err)
	}

	quietManager().ensureClaudeRemoteControlDefault(rcTestAgent())

	parsed := readSettingsMap(t, path)
	if parsed["skipDangerousModePermissionPrompt"] != true {
		t.Fatalf("skipDangerousModePermissionPrompt = %v, want true", parsed["skipDangerousModePermissionPrompt"])
	}
	if parsed["theme"] != "dark" {
		t.Fatalf("sibling key clobbered: theme = %v", parsed["theme"])
	}
	assertRemoteControlOff(t, path)
}

func TestEnsureClaudeRemoteControlDefault_PreservesOperatorTrue(t *testing.T) {
	path := withSharedClaudeDir(t)
	if err := os.WriteFile(path, []byte(`{"remoteControlAtStartup":true,"theme":"dark"}`), 0o666); err != nil {
		t.Fatal(err)
	}

	quietManager().ensureClaudeRemoteControlDefault(rcTestAgent())

	parsed := readSettingsMap(t, path)
	if parsed[remoteControlSettingKey] != true {
		t.Fatalf("explicit operator opt-in was clobbered: %s = %v, want true", remoteControlSettingKey, parsed[remoteControlSettingKey])
	}
	if parsed["theme"] != "dark" {
		t.Fatalf("sibling key clobbered: theme = %v", parsed["theme"])
	}
}

func TestEnsureClaudeRemoteControlDefault_PreservesFleetKeys(t *testing.T) {
	// The shared file carries keys hive does not own and the fleet cannot run
	// without (the bypass-permissions consent suppressor above all). The pin
	// must be add-only.
	path := withSharedClaudeDir(t)
	existing := `{"skipDangerousModePermissionPrompt":true,"bypassPermissions":true,"hasCompletedOnboarding":true}`
	if err := os.WriteFile(path, []byte(existing), 0o666); err != nil {
		t.Fatal(err)
	}

	quietManager().ensureClaudeRemoteControlDefault(rcTestAgent())

	parsed := readSettingsMap(t, path)
	assertRemoteControlOff(t, path)
	for _, key := range []string{"skipDangerousModePermissionPrompt", "bypassPermissions", "hasCompletedOnboarding"} {
		if parsed[key] != true {
			t.Fatalf("fleet key %s = %v after pin, want true", key, parsed[key])
		}
	}
}

func TestEnsureClaudeRemoteControlDefault_SkipsWhenClaudeDirAbsent(t *testing.T) {
	shared := withSharedAgentHome(t) // no .claude dir: entrypoint owns creating it
	quietManager().ensureClaudeRemoteControlDefault(rcTestAgent())
	if _, err := os.Lstat(filepath.Join(shared, ".claude")); !os.IsNotExist(err) {
		t.Fatalf(".claude dir should not be invented by the pin (err=%v)", err)
	}
}

// --- runtime evidence check --------------------------------------------------

func TestRemoteControlBridgeUsed(t *testing.T) {
	withSharedAgentHome(t)
	m := quietManager()

	home := interactiveHomePath("scanner")
	if m.remoteControlBridgeUsed(home) {
		t.Fatal("missing session file should report bridge unused")
	}

	writeSession(t, home, `{"hasCompletedOnboarding":true}`)
	if m.remoteControlBridgeUsed(home) {
		t.Fatal("session without the marker should report bridge unused")
	}

	writeSession(t, home, `{"hasCompletedOnboarding":true,"hasUsedRemoteControl":true}`)
	if !m.remoteControlBridgeUsed(home) {
		t.Fatal("session with hasUsedRemoteControl:true should report bridge used — this is the launch-time evidence log for #5607")
	}
}

// --- hive-owned shared settings (#17059) -------------------------------------
//
// The shared settings.json carries hooks/apiKeyHelper, so it must be a regular
// file owned by the hive and not writable by the agents group before any
// claude launch reads it.

const hookPayload = `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch /tmp/should-not-run"}]}]}}`

func assertNoHooks(t *testing.T, path string) {
	t.Helper()
	if _, ok := readSettingsMap(t, path)["hooks"]; ok {
		t.Fatalf("%s still carries the planted hooks key", path)
	}
}

func assertNotAgentWritable(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s is %v, want a regular file", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		t.Fatalf("%s mode = %v, want no group/other write", path, perm)
	}
}

func TestEnsureClaudeRemoteControlDefault_ReplacesSymlinkedSettings(t *testing.T) {
	path := withSharedClaudeDir(t)
	planted := filepath.Join(t.TempDir(), "planted.json")
	if err := os.WriteFile(planted, []byte(hookPayload), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(planted, path); err != nil {
		t.Fatal(err)
	}

	quietManager().ensureClaudeRemoteControlDefault(rcTestAgent())

	assertNotAgentWritable(t, path)
	assertNoHooks(t, path)
	assertRemoteControlOff(t, path)
	rejected, err := os.Lstat(path + sharedClaudeSettingsRejectedSuffix)
	if err != nil || rejected.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("swapped-in symlink should be kept aside as %s (err=%v)", sharedClaudeSettingsRejectedSuffix, err)
	}
	if data, _ := os.ReadFile(planted); string(data) != hookPayload {
		t.Fatalf("symlink target was written through: %s", data)
	}
}

func TestEnsureClaudeRemoteControlDefault_TightensGroupWritableSettings(t *testing.T) {
	path := withSharedClaudeDir(t)
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	quietManager().ensureClaudeRemoteControlDefault(rcTestAgent())

	assertNotAgentWritable(t, path)
	if got := readSettingsMap(t, path)["theme"]; got != "dark" {
		t.Fatalf("hive-owned settings were not kept: theme = %v", got)
	}
	if _, err := os.Lstat(path + sharedClaudeSettingsRejectedSuffix); !os.IsNotExist(err) {
		t.Fatalf("a hive-owned file must be tightened in place, not rejected (err=%v)", err)
	}
}

func TestEnsureClaudeRemoteControlDefault_SeededSettingsNotGroupWritable(t *testing.T) {
	path := withSharedClaudeDir(t)
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	quietManager().ensureClaudeRemoteControlDefault(rcTestAgent())

	assertNotAgentWritable(t, path)
	assertRemoteControlOff(t, path)
}

func TestEnsureClaudeRemoteControlDefault_ReplacesForeignOwnedSettings(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chowning the fixture to another uid requires root; the owner check is covered by TestUntrustedSharedClaudeSettingsReason")
	}
	path := withSharedClaudeDir(t)
	if err := os.WriteFile(path, []byte(hookPayload), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, foreignSettingsUID(), -1); err != nil {
		t.Fatal(err)
	}

	quietManager().ensureClaudeRemoteControlDefault(rcTestAgent())

	assertNotAgentWritable(t, path)
	assertNoHooks(t, path)
	assertRemoteControlOff(t, path)
	if data, err := os.ReadFile(path + sharedClaudeSettingsRejectedSuffix); err != nil || string(data) != hookPayload {
		t.Fatalf("foreign-owned settings should be kept aside for review (err=%v, data=%s)", err, data)
	}
}

// foreignSettingsUID returns a uid that is neither root, the test's euid nor
// DevUID — i.e. what an agent UID looks like to the owner check.
func foreignSettingsUID() int {
	uid := os.Geteuid() + 4242
	for uid == DevUID || uid == 0 {
		uid++
	}
	return uid
}

type fakeSettingsInfo struct {
	mode os.FileMode
	uid  uint32
}

func (f fakeSettingsInfo) Name() string       { return claudeSettingsFileName }
func (f fakeSettingsInfo) Size() int64        { return 0 }
func (f fakeSettingsInfo) Mode() os.FileMode  { return f.mode }
func (f fakeSettingsInfo) ModTime() time.Time { return time.Time{} }
func (f fakeSettingsInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeSettingsInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

func TestUntrustedSharedClaudeSettingsReason(t *testing.T) {
	self := uint32(os.Geteuid())
	cases := []struct {
		name    string
		info    fakeSettingsInfo
		trusted bool
	}{
		{"hive-owned regular file", fakeSettingsInfo{mode: 0o644, uid: self}, true},
		{"root-owned regular file", fakeSettingsInfo{mode: 0o644, uid: 0}, true},
		{"dev-owned regular file", fakeSettingsInfo{mode: 0o644, uid: uint32(DevUID)}, true},
		{"agent-owned regular file", fakeSettingsInfo{mode: 0o644, uid: uint32(foreignSettingsUID())}, false},
		{"symlink", fakeSettingsInfo{mode: os.ModeSymlink | 0o777, uid: self}, false},
		{"directory", fakeSettingsInfo{mode: os.ModeDir | 0o755, uid: self}, false},
	}
	for _, tc := range cases {
		reason := untrustedSharedClaudeSettingsReason(tc.info)
		if (reason == "") != tc.trusted {
			t.Errorf("%s: reason = %q, want trusted=%v", tc.name, reason, tc.trusted)
		}
	}
	if reason := untrustedSharedClaudeSettingsReason(fakeSettingsInfo{mode: 0o644, uid: uint32(foreignSettingsUID())}); !strings.Contains(reason, "uid") {
		t.Errorf("foreign-owner reason should name the uid, got %q", reason)
	}
}

func TestIsSharedClaudeSettingsFile(t *testing.T) {
	cases := map[string]bool{
		"/data/home/.claude/settings.json":          true,
		"/data/home/.claude/settings.json.rejected": false,
		"/data/home/.claude/projects/settings.json": false,
		"/data/home/.gemini/settings.json":          false,
	}
	for path, want := range cases {
		if got := isSharedClaudeSettingsFile(path); got != want {
			t.Errorf("isSharedClaudeSettingsFile(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestFixEntryLeavesSharedClaudeSettingsMode runs fixEntry as the watcher's
// own uid (so its chmod arm is reachable) and checks the shared settings file
// is not widened while a sibling in the same directory still is.
func TestFixEntryLeavesSharedClaudeSettingsMode(t *testing.T) {
	origDev, origNode := DevUID, NodeGID
	DevUID, NodeGID = os.Getuid(), os.Getgid()
	t.Cleanup(func() { DevUID, NodeGID = origDev, origNode })

	dir := filepath.Join(t.TempDir(), claudeHomeDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, claudeSettingsFileName)
	sibling := filepath.Join(dir, "sibling.json")
	for _, p := range []string{settings, sibling} {
		if err := os.WriteFile(p, []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		fixEntry(p, fi, quietLogger())
	}
	if got := statMode(t, settings); got != 0o644 {
		t.Errorf("shared settings.json mode = %v, want 0644 untouched", got)
	}
	if got := statMode(t, sibling); got&0o020 == 0 {
		t.Errorf("sibling mode = %v; fixEntry's chmod arm was not exercised", got)
	}
}
