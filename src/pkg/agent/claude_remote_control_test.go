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
	"testing"

	"github.com/hivecommons/hive/pkg/config"
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
