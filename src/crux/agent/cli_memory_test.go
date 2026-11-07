package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

func envValue(pairs []agentEnvPair, key string) (string, bool) {
	for _, p := range pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return "", false
}

// Default policy: Claude Code's auto-memory is pinned off for every Claude
// CLI launch (subscription and inference-routed), not for other CLIs.
func TestAgentEnvPairs_CLIMemoryPinsClaudeAutoMemoryOffByDefault(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	for _, backend := range []string{"claude", "vllm"} {
		ap := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: backend, Model: "sonnet"}}
		if v, ok := envValue(m.agentEnvPairs(ap), claudeDisableAutoMemoryEnv); !ok || v != "1" {
			t.Errorf("backend %s: %s=%q ok=%v, want 1", backend, claudeDisableAutoMemoryEnv, v, ok)
		}
	}
	ap := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "codex"}}
	if _, ok := envValue(m.agentEnvPairs(ap), claudeDisableAutoMemoryEnv); ok {
		t.Errorf("%s exported to a non-Claude CLI", claudeDisableAutoMemoryEnv)
	}
}

func TestAgentEnvPairs_CLIMemoryOptInLeavesClaudeDefault(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	ap := &AgentProcess{Name: "scanner", Config: config.AgentConfig{
		Backend:   "claude",
		CLIMemory: &config.CLIMemoryConfig{AutoMemory: boolPtr(true)},
	}}
	if v, ok := envValue(m.agentEnvPairs(ap), claudeDisableAutoMemoryEnv); ok {
		t.Errorf("auto_memory: true still exported %s=%q", claudeDisableAutoMemoryEnv, v)
	}
}

func TestCodexConfigWithMemoriesPinned(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		enabled bool
		want    string
	}{
		{
			name: "empty adds table",
			want: "[features]\nmemories = false\n",
		},
		{
			name: "top-level keys kept, table appended",
			in:   "check_for_update_on_startup = false\nmodel = \"gpt-5-codex\"\n",
			want: "check_for_update_on_startup = false\nmodel = \"gpt-5-codex\"\n\n[features]\nmemories = false\n",
		},
		{
			name: "existing table gets the key",
			in:   "model = \"x\"\n\n[features]\nweb_search = true\n\n[projects.\"/data/agents/ci\"]\ntrust_level = \"trusted\"\n",
			want: "model = \"x\"\n\n[features]\nweb_search = true\nmemories = false\n\n[projects.\"/data/agents/ci\"]\ntrust_level = \"trusted\"\n",
		},
		{
			name: "existing key replaced in place",
			in:   "[features]\nmemories = true\nweb_search = true\n",
			want: "[features]\nmemories = false\nweb_search = true\n",
		},
		{
			name:    "opt-in writes true",
			in:      "[features]\nmemories = false\n",
			enabled: true,
			want:    "[features]\nmemories = true\n",
		},
		{
			name: "memories key in another table is not ours",
			in:   "[projects.\"/x\"]\nmemories = true\n",
			want: "[projects.\"/x\"]\nmemories = true\n\n[features]\nmemories = false\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(codexConfigWithMemoriesPinned([]byte(c.in), c.enabled)); got != c.want {
				t.Errorf("merged config = %q, want %q", got, c.want)
			}
		})
	}
	// The combined seed carries both hive pins.
	if got := string(codexConfigSeed(nil, false)); got != "check_for_update_on_startup = false\n\n[features]\nmemories = false\n" {
		t.Errorf("codexConfigSeed(nil) = %q", got)
	}
}

func TestSeedCopilotMemorySetting_OwnsTheKey(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "copilot"}}

	m.setupInteractiveHome(ap, "copilot")

	path := filepath.Join(interactiveHomePath("scanner"), ".copilot", copilotSettingsFileName)
	read := func() map[string]any {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("parse settings: %v", err)
		}
		return got
	}
	if got := read(); got[copilotMemorySettingKey] != false {
		t.Fatalf("default launch: %s = %v, want false", copilotMemorySettingKey, got[copilotMemorySettingKey])
	}

	// An operator key the CLI wrote itself is preserved; the policy flips
	// only its own key when the operator opts in.
	if err := os.WriteFile(path, []byte(`{"memory":false,"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ap.Config.CLIMemory = &config.CLIMemoryConfig{AutoMemory: boolPtr(true)}
	m.setupInteractiveHome(ap, "copilot")
	got := read()
	if got[copilotMemorySettingKey] != true || got["theme"] != "dark" {
		t.Fatalf("opt-in: %v", got)
	}
}

func TestClearCLIMemoryForKick_WipesOnlyThisAgentsStores(t *testing.T) {
	shared := withSharedAgentHome(t)
	prev := codexHomePrefix
	codexHomePrefix = filepath.Join(t.TempDir(), ".codex-")
	t.Cleanup(func() { codexHomePrefix = prev })

	m := interactiveHomeTestManager(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{
		Backend:   "claude",
		CLIMemory: &config.CLIMemoryConfig{ClearOnKick: true},
	}}
	home := interactiveHomePath("scanner")
	mustWrite := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	claudeMem := filepath.Join(home, ".claude", "projects", "-data-agents-scanner", "memory", "MEMORY.md")
	claudeTranscript := filepath.Join(home, ".claude", "projects", "-data-agents-scanner", "abc.jsonl")
	geminiMem := filepath.Join(home, ".gemini", geminiMemoryFileName)
	geminiSettings := filepath.Join(home, ".gemini", "settings.json")
	codexMem := filepath.Join(codexHomePath("scanner"), "memories", "notes.md")
	codexMemV2 := filepath.Join(codexHomePath("scanner"), "memories_v2", "index.json")
	codexConfig := filepath.Join(codexHomePath("scanner"), "config.toml")
	otherAgentMem := filepath.Join(interactiveHomePath("reviewer"), ".claude", "projects", "-data-agents-reviewer", "memory", "MEMORY.md")
	sharedMem := filepath.Join(shared, ".claude", "projects", "-data-agents-coder", "memory", "MEMORY.md")
	for _, p := range []string{claudeMem, claudeTranscript, geminiMem, geminiSettings, codexMem, codexMemV2, codexConfig, otherAgentMem, sharedMem} {
		mustWrite(p)
	}

	m.clearCLIMemoryForKick(ap, "claude")

	for _, p := range []string{claudeMem, geminiMem, codexMem, codexMemV2} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s should have been wiped (err=%v)", p, err)
		}
	}
	for _, p := range []string{claudeTranscript, geminiSettings, codexConfig, otherAgentMem, sharedMem} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s must survive the wipe: %v", p, err)
		}
	}

	// Idempotent on an already-clean home.
	m.clearCLIMemoryForKick(ap, "claude")
}

func TestClearCLIMemoryForKick_OffByDefaultAndNeverSharedHome(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	home := interactiveHomePath("scanner")
	mem := filepath.Join(home, ".claude", "projects", "-data-agents-scanner", "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(mem), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No policy: nothing is touched.
	m.clearCLIMemoryForKick(&AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}, "claude")
	if _, err := os.Stat(mem); err != nil {
		t.Fatalf("default policy wiped memory: %v", err)
	}

	// Shared-home escape hatch: the store is fleet-shared, so no wipe even
	// when the policy asks for one.
	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	on := &config.CLIMemoryConfig{ClearOnKick: true}
	if got := cliMemoryStorePaths(&AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{CLIMemory: on}}, "claude"); got != nil {
		t.Errorf("shared home returned stores to wipe: %v", got)
	}
	if got := cliMemoryStorePaths(&AgentProcess{Name: "scanner", UID: 0, Config: config.AgentConfig{CLIMemory: on}}, "claude"); got != nil {
		t.Errorf("uid 0 returned stores to wipe: %v", got)
	}
}
