package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

func hookCommands(t *testing.T, settings map[string]any, event string) []string {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	var out []string
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		items, _ := gm["hooks"].([]any)
		for _, it := range items {
			im, _ := it.(map[string]any)
			cmd, _ := im["command"].(string)
			out = append(out, cmd)
		}
	}
	return out
}

func countCommand(cmds []string, want string) int {
	n := 0
	for _, c := range cmds {
		if c == want {
			n++
		}
	}
	return n
}

const operatorSettings = `{
  "theme": "dark",
  "hooks": {
    "SessionEnd": [{"hooks": [{"type": "command", "command": "/usr/local/bin/audit-session"}]}],
    "PostToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "prettier --write"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}]
  }
}`

func TestMergeClaudeMemoryDrainHooks_KeepsOperatorHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(operatorSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	m := quietManager()

	m.mergeClaudeMemoryDrainHooks("scanner", path, true, true)
	got := readSettingsMap(t, path)
	if got["theme"] != "dark" {
		t.Fatalf("sibling key clobbered: %v", got["theme"])
	}
	end := hookCommands(t, got, claudeHookSessionEnd)
	if countCommand(end, "/usr/local/bin/audit-session") != 1 || countCommand(end, claudeMemoryDrainHookCommand) != 1 {
		t.Fatalf("SessionEnd = %v", end)
	}
	post := hookCommands(t, got, claudeHookPostToolUse)
	if countCommand(post, "prettier --write") != 1 || countCommand(post, claudeMemoryDrainHookCommand) != 1 {
		t.Fatalf("PostToolUse = %v", post)
	}
	if stop := hookCommands(t, got, "Stop"); len(stop) != 1 || stop[0] != "notify-send done" {
		t.Fatalf("Stop = %v", stop)
	}
	groups := got["hooks"].(map[string]any)[claudeHookPostToolUse].([]any)
	if hive := groups[len(groups)-1].(map[string]any); hive["matcher"] != claudeMemoryDrainWriteMatcher {
		t.Fatalf("PostToolUse matcher = %v", hive["matcher"])
	}

	// Idempotent: a re-seed adds nothing and does not rewrite the file.
	before, _ := os.ReadFile(path)
	m.mergeClaudeMemoryDrainHooks("scanner", path, true, true)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("re-seed rewrote the file:\n%s\n---\n%s", before, after)
	}
}

func TestMergeClaudeMemoryDrainHooks_RemovesOnlyHiveEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(operatorSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	m := quietManager()
	m.mergeClaudeMemoryDrainHooks("scanner", path, true, true)

	// drain_on_write turned off: only the PostToolUse entry goes.
	m.mergeClaudeMemoryDrainHooks("scanner", path, true, false)
	got := readSettingsMap(t, path)
	if post := hookCommands(t, got, claudeHookPostToolUse); len(post) != 1 || post[0] != "prettier --write" {
		t.Fatalf("PostToolUse = %v", post)
	}
	if end := hookCommands(t, got, claudeHookSessionEnd); countCommand(end, claudeMemoryDrainHookCommand) != 1 {
		t.Fatalf("SessionEnd = %v", end)
	}

	// The drain turned off: Hive's SessionEnd entry goes, the operator's stays.
	m.mergeClaudeMemoryDrainHooks("scanner", path, false, false)
	got = readSettingsMap(t, path)
	if end := hookCommands(t, got, claudeHookSessionEnd); len(end) != 1 || end[0] != "/usr/local/bin/audit-session" {
		t.Fatalf("SessionEnd = %v", end)
	}
	if stop := hookCommands(t, got, "Stop"); len(stop) != 1 {
		t.Fatalf("Stop = %v", stop)
	}
}

func TestMergeClaudeMemoryDrainHooks_OffLeavesFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := quietManager()
	m.mergeClaudeMemoryDrainHooks("scanner", path, false, false)
	if data, _ := os.ReadFile(path); string(data) != `{"theme":"dark"}` {
		t.Fatalf("file rewritten with the drain off: %s", data)
	}

	// Seeded then retracted with no operator hooks: the hooks key goes too.
	m.mergeClaudeMemoryDrainHooks("scanner", path, true, false)
	if got := readSettingsMap(t, path); len(hookCommands(t, got, claudeHookSessionEnd)) != 1 {
		t.Fatalf("SessionEnd not seeded: %v", got)
	}
	m.mergeClaudeMemoryDrainHooks("scanner", path, false, false)
	if got := readSettingsMap(t, path); got["hooks"] != nil || got["theme"] != "dark" {
		t.Fatalf("after retract = %v", got)
	}
}

func TestMergeClaudeMemoryDrainHooks_SplitsMixedOperatorGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	mixed := `{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":"/usr/local/bin/audit-session"},{"type":"command","command":` +
		`"hive memory-drain --cli claude --agent \"$HIVE_AGENT\""}]}]}}`
	if err := os.WriteFile(path, []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}
	m := quietManager()
	m.mergeClaudeMemoryDrainHooks("scanner", path, true, false)
	if data, _ := os.ReadFile(path); string(data) != mixed {
		t.Fatalf("an existing Hive entry must not be added twice: %s", data)
	}
	m.mergeClaudeMemoryDrainHooks("scanner", path, false, false)
	if end := hookCommands(t, readSettingsMap(t, path), claudeHookSessionEnd); len(end) != 1 || end[0] != "/usr/local/bin/audit-session" {
		t.Fatalf("SessionEnd = %v", end)
	}
}

func TestEnsureClaudeRemoteControlDefault_SeedsMemoryDrainHooks(t *testing.T) {
	path := withSharedClaudeDir(t)
	m := quietManager()
	m.agents = map[string]*AgentProcess{
		"scanner": {Name: "scanner", Config: config.AgentConfig{CLIMemory: &config.CLIMemoryConfig{DrainToKnowledge: true}}},
		"builder": {Name: "builder", Config: config.AgentConfig{CLIMemory: &config.CLIMemoryConfig{DrainOnWrite: true}}},
	}

	m.ensureClaudeRemoteControlDefault(rcTestAgent())
	got := readSettingsMap(t, path)
	assertRemoteControlOff(t, path)
	if end := hookCommands(t, got, claudeHookSessionEnd); countCommand(end, claudeMemoryDrainHookCommand) != 1 {
		t.Fatalf("SessionEnd = %v", end)
	}
	// drain_on_write without drain_to_knowledge does nothing.
	if post := hookCommands(t, got, claudeHookPostToolUse); len(post) != 0 {
		t.Fatalf("PostToolUse = %v", post)
	}

	m.agents["builder"].Config.CLIMemory.DrainToKnowledge = true
	m.ensureClaudeRemoteControlDefault(rcTestAgent())
	if post := hookCommands(t, readSettingsMap(t, path), claudeHookPostToolUse); countCommand(post, claudeMemoryDrainHookCommand) != 1 {
		t.Fatalf("PostToolUse = %v", post)
	}

	m.agents = nil
	m.ensureClaudeRemoteControlDefault(rcTestAgent())
	got = readSettingsMap(t, path)
	if got["hooks"] != nil {
		t.Fatalf("hooks left behind with the drain off: %v", got["hooks"])
	}
	assertRemoteControlOff(t, path)
}

func TestEnsureClaudeSettings_SeedsMemoryDrainHooks(t *testing.T) {
	orig := inferenceHomePrefixOverride
	inferenceHomePrefixOverride = filepath.Join(t.TempDir(), "inference-home") + "-"
	t.Cleanup(func() { inferenceHomePrefixOverride = orig })
	os.Remove(claudeInferenceSettingsPath)
	t.Cleanup(func() { os.Remove(claudeInferenceSettingsPath) })

	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	m.agents["drainer"] = &AgentProcess{Name: "drainer", Config: config.AgentConfig{
		CLIMemory: &config.CLIMemoryConfig{DrainToKnowledge: true, DrainOnWrite: true},
	}}
	m.ensureClaudeSettings("drainer", 0)

	for _, p := range []string{filepath.Join(inferenceHomePath("drainer"), ".claude", "settings.json"), claudeInferenceSettingsPath} {
		got := readSettingsMap(t, p)
		if countCommand(hookCommands(t, got, claudeHookSessionEnd), claudeMemoryDrainHookCommand) != 1 ||
			countCommand(hookCommands(t, got, claudeHookPostToolUse), claudeMemoryDrainHookCommand) != 1 {
			t.Fatalf("%s hooks = %v", p, got["hooks"])
		}
		assertRemoteControlOff(t, p)
	}
}
