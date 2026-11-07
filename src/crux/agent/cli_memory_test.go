package agent

// CLI auto-memory policy (agents.cli_memory, #17032).
//
// Invariants under test: with the default policy every hive-provisioned
// Claude settings surface and Codex config carries the documented "memory
// off" switch, an operator's explicit value is never overwritten, and
// clear_on_kick removes only memory the agent alone owns — never a store
// reached through a bridge into shared state, never transcripts or config.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

func TestCodexConfigWithMemoriesDisabled(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "empty config gains a features table",
			in:   "",
			want: "[features]\nmemories = false\n",
		},
		{
			name: "top-level keys only: table appended after them",
			in:   "check_for_update_on_startup = false",
			want: "check_for_update_on_startup = false\n\n[features]\nmemories = false\n",
		},
		{
			name: "existing features table gains the key under its header",
			in:   "model = \"gpt-5-codex\"\n[features] # flags\nhooks = true\n[projects.\"/data/agents/ci\"]\ntrust_level = \"trusted\"\n",
			want: "model = \"gpt-5-codex\"\n[features] # flags\nmemories = false\nhooks = true\n[projects.\"/data/agents/ci\"]\ntrust_level = \"trusted\"\n",
		},
		{
			name: "operator's explicit memories = true is kept",
			in:   "[features]\nmemories = true\n",
			want: "[features]\nmemories = true\n",
		},
		{
			name: "top-level dotted key is kept (a second key would be invalid TOML)",
			in:   "features.memories = true\n[projects.x]\ntrust_level = \"trusted\"\n",
			want: "features.memories = true\n[projects.x]\ntrust_level = \"trusted\"\n",
		},
		{
			name: "top-level inline features table is kept",
			in:   "features = { hooks = true }\n",
			want: "features = { hooks = true }\n",
		},
		{
			name: "memories under another table and in a comment do not count",
			in:   "# memories = true\n[memories]\nuse_memories = false\n",
			want: "# memories = true\n[memories]\nuse_memories = false\n\n[features]\nmemories = false\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(codexConfigWithMemoriesDisabled([]byte(tc.in)))
			if got != tc.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, tc.want)
			}
			if again := string(codexConfigWithMemoriesDisabled([]byte(got))); again != got {
				t.Fatalf("not idempotent: second pass rewrote\n%q\nto\n%q", got, again)
			}
		})
	}
}

func TestCodexConfigSeededHonorsAutoMemory(t *testing.T) {
	if got := string(codexConfigSeeded(nil, false)); !strings.Contains(got, "[features]\nmemories = false\n") ||
		!strings.Contains(got, codexDisableUpdateCheckLine) {
		t.Fatalf("default policy seed = %q, want update check and memories disabled", got)
	}
	got := string(codexConfigSeeded(nil, true))
	if strings.Contains(got, "memories") {
		t.Fatalf("auto_memory: true must seed nothing about memories, got %q", got)
	}
	if !strings.Contains(got, codexDisableUpdateCheckLine) {
		t.Fatalf("update check must stay disabled regardless of the memory policy, got %q", got)
	}
}

func TestEnsureClaudeAutoMemoryDefault(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "claude"}}
	dir := t.TempDir()

	t.Run("default policy seeds the key and keeps existing keys", func(t *testing.T) {
		path := filepath.Join(dir, "default.json")
		if err := os.WriteFile(path, []byte(`{"enabledPlugins":{"x":true}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		m.ensureClaudeAutoMemoryDefault(agent, path)
		got := readSettingsMap(t, path)
		if v, ok := got[claudeAutoMemorySettingKey]; !ok || v != false {
			t.Fatalf("%s = %v (present %v), want false", claudeAutoMemorySettingKey, v, ok)
		}
		if _, ok := got["enabledPlugins"]; !ok {
			t.Fatal("seed dropped an unrelated key")
		}
	})

	t.Run("operator's explicit true survives", func(t *testing.T) {
		path := filepath.Join(dir, "explicit.json")
		if err := os.WriteFile(path, []byte(`{"autoMemoryEnabled":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		m.ensureClaudeAutoMemoryDefault(agent, path)
		if v := readSettingsMap(t, path)[claudeAutoMemorySettingKey]; v != true {
			t.Fatalf("%s = %v, want the operator's true", claudeAutoMemorySettingKey, v)
		}
	})

	t.Run("auto_memory true seeds nothing", func(t *testing.T) {
		on := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "claude", CLIMemory: config.CLIMemoryConfig{AutoMemory: true}}}
		path := filepath.Join(dir, "on.json")
		m.ensureClaudeAutoMemoryDefault(on, path)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("auto_memory: true wrote %s (stat err %v)", path, err)
		}
	})

	t.Run("missing settings dir is not created", func(t *testing.T) {
		path := filepath.Join(dir, "absent", "settings.json")
		m.ensureClaudeAutoMemoryDefault(agent, path)
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("seed created %s (stat err %v)", filepath.Dir(path), err)
		}
	})
}

// recordCLIMemoryRemover swaps cliMemoryRemover for a direct delete that
// records each call, so tests exercise the selection logic without su-exec.
func recordCLIMemoryRemover(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	orig := cliMemoryRemover
	cliMemoryRemover = func(_ string, paths []string) error {
		calls = append(calls, slices.Clone(paths))
		for _, p := range paths {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		}
		return nil
	}
	t.Cleanup(func() { cliMemoryRemover = orig })
	return &calls
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if exists := err == nil; exists != want {
		t.Fatalf("%s exists = %v, want %v", path, exists, want)
	}
}

func clearOnKickAgent(name, backend string, uid int) *AgentProcess {
	return &AgentProcess{
		Name: name,
		UID:  uid,
		Config: config.AgentConfig{
			Backend:   backend,
			CLIMemory: config.CLIMemoryConfig{ClearOnKick: true},
		},
	}
}

func TestClearCLIMemory_ClaudePrivateHome(t *testing.T) {
	withSharedAgentHome(t)
	calls := recordCLIMemoryRemover(t)
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	agent := clearOnKickAgent("scanner", "claude", 1001)
	home := AgentHome(agent.Name, agent.UID, "claude")
	projects := filepath.Join(home, ".claude", "projects")
	memory := filepath.Join(projects, "-data-agents-scanner", "memory")
	transcript := filepath.Join(projects, "-data-agents-scanner", "session.jsonl")
	writeTestFile(t, filepath.Join(memory, "MEMORY.md"), "user works with PHI\n")
	writeTestFile(t, transcript, "{}\n")
	writeTestFile(t, filepath.Join(home, ".claude", "settings.json"), "{}")
	// A project dir symlinked in from elsewhere is not this agent's to clear.
	foreign := filepath.Join(t.TempDir(), "other-agent-project")
	writeTestFile(t, filepath.Join(foreign, "memory", "MEMORY.md"), "someone else's\n")
	if err := os.Symlink(foreign, filepath.Join(projects, "-linked")); err != nil {
		t.Fatal(err)
	}

	m.clearCLIMemoryLocked(agent)

	assertExists(t, memory, false)
	assertExists(t, transcript, true)
	assertExists(t, filepath.Join(home, ".claude", "settings.json"), true)
	assertExists(t, filepath.Join(foreign, "memory", "MEMORY.md"), true)
	if len(*calls) != 1 || !slices.Equal((*calls)[0], []string{memory}) {
		t.Fatalf("remover calls = %v, want exactly [[%s]]", *calls, memory)
	}
}

func TestClearCLIMemory_SkipsSharedClaudeBridge(t *testing.T) {
	shared := withSharedAgentHome(t)
	calls := recordCLIMemoryRemover(t)
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	agent := clearOnKickAgent("scanner", "claude", 1001)
	home := AgentHome(agent.Name, agent.UID, "claude")
	// Today's interactive layout: ~/.claude is a bridge to /data/home/.claude,
	// so every claude agent's memory lives in one shared tree.
	sharedMemory := filepath.Join(shared, ".claude", "projects", "-data-agents-reviewer", "memory", "MEMORY.md")
	writeTestFile(t, sharedMemory, "reviewer's notes\n")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(shared, ".claude"), filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}

	m.clearCLIMemoryLocked(agent)
	m.clearCLIMemoryLocked(agent)

	assertExists(t, sharedMemory, true)
	if len(*calls) != 0 {
		t.Fatalf("remover called through a shared bridge: %v", *calls)
	}
	if !agent.cliMemorySharedLogged {
		t.Fatal("skipping a shared store must be logged")
	}
}

func TestClearCLIMemory_SkipsSharedHomes(t *testing.T) {
	shared := withSharedAgentHome(t)
	calls := recordCLIMemoryRemover(t)
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	sharedMemory := filepath.Join(shared, ".claude", "projects", "p", "memory", "MEMORY.md")
	writeTestFile(t, sharedMemory, "fleet-wide\n")

	// HIVE_SHARED_AGENT_HOME=1: every per-UID agent runs out of /data/home.
	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	m.clearCLIMemoryLocked(clearOnKickAgent("scanner", "claude", 1001))
	// UID 0 agents share the hive's own HOME.
	t.Setenv("HOME", shared)
	m.clearCLIMemoryLocked(clearOnKickAgent("scanner", "claude", 0))

	assertExists(t, sharedMemory, true)
	if len(*calls) != 0 {
		t.Fatalf("remover called on a shared HOME: %v", *calls)
	}
}

func TestClearCLIMemory_Codex(t *testing.T) {
	calls := recordCLIMemoryRemover(t)
	orig := codexHomePrefix
	codexHomePrefix = filepath.Join(t.TempDir(), ".codex-")
	t.Cleanup(func() { codexHomePrefix = orig })
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	agent := clearOnKickAgent("coder", codexBackend, 1002)
	root := codexHomePath(agent.Name)
	writeTestFile(t, filepath.Join(root, "memories", "summary.md"), "x")
	writeTestFile(t, filepath.Join(root, "memories_v2", "summary.md"), "x")
	writeTestFile(t, filepath.Join(root, "config.toml"), "[features]\nmemories = true\n")

	m.clearCLIMemoryLocked(agent)

	assertExists(t, filepath.Join(root, "memories"), false)
	assertExists(t, filepath.Join(root, "memories_v2"), false)
	assertExists(t, filepath.Join(root, "config.toml"), true)
	if len(*calls) != 1 {
		t.Fatalf("remover calls = %v, want one batched call", *calls)
	}
}

func TestClearCLIMemory_GeminiPrivateHome(t *testing.T) {
	withSharedAgentHome(t)
	calls := recordCLIMemoryRemover(t)
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	agent := clearOnKickAgent("scanner", "gemini", 1001)
	home := AgentHome(agent.Name, agent.UID, "gemini")
	memory := filepath.Join(home, ".gemini", "GEMINI.md")
	writeTestFile(t, memory, "## Gemini Added Memories\n- one-off\n")
	writeTestFile(t, filepath.Join(home, ".gemini", "settings.json"), "{}")

	m.clearCLIMemoryLocked(agent)

	assertExists(t, memory, false)
	assertExists(t, filepath.Join(home, ".gemini", "settings.json"), true)
	if len(*calls) != 1 {
		t.Fatalf("remover calls = %v, want 1", *calls)
	}
}

func TestClearCLIMemory_OffByDefault(t *testing.T) {
	withSharedAgentHome(t)
	calls := recordCLIMemoryRemover(t)
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	agent := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	memory := filepath.Join(AgentHome(agent.Name, agent.UID, "claude"), ".claude", "projects", "p", "memory")
	writeTestFile(t, filepath.Join(memory, "MEMORY.md"), "kept\n")

	m.clearCLIMemoryLocked(agent)

	assertExists(t, memory, true)
	if len(*calls) != 0 {
		t.Fatalf("clear_on_kick: false must not clear, remover calls = %v", *calls)
	}
}

// The clear runs on the kick path before any input reaches the pane, so the
// kick (and the /clear before it) never sees the old memory.
func TestDeliverKickLocked_ClearsCLIMemoryBeforeInput(t *testing.T) {
	withSharedAgentHome(t)
	m, agent, _ := kickLogTestManager(t, "")
	agent.UID = 1001
	agent.Config.CLIMemory.ClearOnKick = true
	memory := filepath.Join(AgentHome(agent.Name, agent.UID, "claude"), ".claude", "projects", "p", "memory")
	writeTestFile(t, filepath.Join(memory, "MEMORY.md"), "stale\n")

	var events []string
	orig := cliMemoryRemover
	cliMemoryRemover = func(_ string, paths []string) error {
		events = append(events, "clear-memory")
		return nil
	}
	t.Cleanup(func() { cliMemoryRemover = orig })
	termSeams(m).sendKeys = func(_ *AgentProcess, keys ...string) {
		events = append(events, "sendkeys:"+strings.Join(keys, "+"))
	}

	m.deliverKickLocked(agent, "next task", "send-kick")

	if len(events) < 2 || events[0] != "clear-memory" {
		t.Fatalf("events = %v, want the memory clear before any pane input", events)
	}
}
