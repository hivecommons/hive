package agent

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

func TestCLIMemoryDrainSources_ClassifiesStoresByCLI(t *testing.T) {
	withSharedAgentHome(t)
	prev := codexHomePrefix
	codexHomePrefix = filepath.Join(t.TempDir(), ".codex-")
	t.Cleanup(func() { codexHomePrefix = prev })

	home := interactiveHomePath("scanner")
	for _, p := range []string{
		filepath.Join(home, ".claude", "projects", "-data-agents-scanner", "memory", "MEMORY.md"),
		filepath.Join(home, ".gemini", geminiMemoryFileName),
		filepath.Join(codexHomePath("scanner"), "memories", "memory_summary.md"),
		filepath.Join(codexHomePath("scanner"), "memories_v2", "index.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	got := cliMemoryDrainSources(ap, "claude")
	byCLI := map[string][]string{}
	for _, s := range got {
		byCLI[s.CLI] = append(byCLI[s.CLI], filepath.Base(s.Path))
		sort.Strings(byCLI[s.CLI])
	}
	if len(got) != 4 ||
		strings.Join(byCLI[cliMemoryClaude], ",") != "memory" ||
		strings.Join(byCLI[cliMemoryGemini], ",") != geminiMemoryFileName ||
		strings.Join(byCLI[cliMemoryCodex], ",") != "memories,memories_v2" {
		t.Fatalf("sources = %+v", got)
	}
}

func TestCLIMemoryDrainSources_DropsStoresReachedThroughSymlinkedAncestors(t *testing.T) {
	withSharedAgentHome(t)
	prev := codexHomePrefix
	codexHomePrefix = filepath.Join(t.TempDir(), ".codex-")
	t.Cleanup(func() { codexHomePrefix = prev })

	mk := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("- fact\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	home := interactiveHomePath("scanner")
	projects := filepath.Join(home, ".claude", "projects")
	// An honest store, and a second slug that is a symlink to it: inside the
	// agent's own home, so it is drained once, at its resolved path.
	own := filepath.Join(projects, "-data-agents-scanner", "memory")
	mk(filepath.Join(own, "MEMORY.md"))
	if err := os.Symlink(filepath.Join(projects, "-data-agents-scanner"), filepath.Join(projects, "-alias")); err != nil {
		t.Skip("symlinks unavailable")
	}
	// A slug pointed at another agent's store, and a Gemini store whose
	// .gemini directory is a symlink out of the home: both must be dropped.
	victim := filepath.Join(interactiveHomePath("victim"), ".claude", "projects", "-data-agents-victim", "memory")
	mk(filepath.Join(victim, "MEMORY.md"))
	if err := os.Symlink(filepath.Dir(victim), filepath.Join(projects, "-data-agents-victim")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	mk(filepath.Join(outside, geminiMemoryFileName))
	if err := os.Symlink(outside, filepath.Join(home, ".gemini")); err != nil {
		t.Fatal(err)
	}

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	got := cliMemoryDrainSources(ap, "claude")
	wantOwn, _ := filepath.EvalSymlinks(own)
	var paths []string
	for _, s := range got {
		paths = append(paths, s.Path)
		if s.Path != wantOwn || s.CLI != cliMemoryClaude {
			t.Errorf("source %+v reaches outside the agent's resolved home (want only %s)", s, wantOwn)
		}
	}
	if len(got) != 1 {
		t.Fatalf("sources = %v, want the honest store once", paths)
	}
}

func TestDrainCLIMemoryForKick_RunsDrainerBeforeWipeOnlyWhenEnabled(t *testing.T) {
	withSharedAgentHome(t)
	prev := codexHomePrefix
	codexHomePrefix = filepath.Join(t.TempDir(), ".codex-")
	t.Cleanup(func() { codexHomePrefix = prev })

	m := interactiveHomeTestManager(t)
	home := interactiveHomePath("scanner")
	mem := filepath.Join(home, ".claude", "projects", "-data-agents-scanner", "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(mem), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("- a fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls []CLIMemoryDrainRequest
	var sawFileAtDrain bool
	m.SetCLIMemoryDrainer(func(req CLIMemoryDrainRequest) CLIMemoryDrainResult {
		calls = append(calls, req)
		_, err := os.Stat(mem)
		sawFileAtDrain = err == nil
		return CLIMemoryDrainResult{Store: "/vault", Drained: 1}
	})

	off := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{
		Backend: "claude", CLIMemory: &config.CLIMemoryConfig{ClearOnKick: true},
	}}
	m.drainCLIMemoryForKick(off, "claude")
	if len(calls) != 0 {
		t.Fatalf("drainer ran with drain_to_knowledge off: %+v", calls)
	}

	on := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{
		Backend: "claude", CLIMemory: &config.CLIMemoryConfig{ClearOnKick: true, DrainToKnowledge: true},
	}}
	// Same order as deliverKickLocked: drain, then wipe.
	m.drainCLIMemoryForKick(on, "claude")
	m.clearCLIMemoryForKick(on, "claude")
	if len(calls) != 1 || calls[0].Agent != "scanner" || calls[0].Backend != "claude" || len(calls[0].Sources) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Sources[0].CLI != cliMemoryClaude || filepath.Base(calls[0].Sources[0].Path) != "memory" {
		t.Fatalf("source = %+v", calls[0].Sources[0])
	}
	if !sawFileAtDrain {
		t.Fatal("memory file was already gone when the drainer ran")
	}
	if _, err := os.Stat(mem); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("memory not wiped after drain: %v", err)
	}

	// Without an installed drainer the kick path must still be safe.
	m.SetCLIMemoryDrainer(nil)
	if err := os.MkdirAll(filepath.Dir(mem), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("- again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.drainCLIMemoryForKick(on, "claude")
	if len(calls) != 1 {
		t.Fatalf("removed drainer still invoked: %d calls", len(calls))
	}
}

// TestDrainCLIMemoryNow covers the on-demand entry point a Claude hook
// reaches through the hive (hivecommons/hive#17208): it narrows to the hook's
// project, refuses another agent's name, and is a no-op with the drain off.
func TestDrainCLIMemoryNow(t *testing.T) {
	withSharedAgentHome(t)
	prev := codexHomePrefix
	codexHomePrefix = filepath.Join(t.TempDir(), ".codex-")
	t.Cleanup(func() { codexHomePrefix = prev })

	m := interactiveHomeTestManager(t)
	m.uidMap = NewUIDMap()
	m.uidMap.Agents["scanner"] = 2001
	m.uidMap.Agents["builder"] = 2002
	home := interactiveHomePath("scanner")
	for _, p := range []string{
		filepath.Join(home, ".claude", "projects", "-data-agents-scanner-acme-widgets", "memory", "MEMORY.md"),
		filepath.Join(home, ".claude", "projects", "-data-agents-scanner-acme-gadgets", "memory", "MEMORY.md"),
		filepath.Join(home, ".gemini", geminiMemoryFileName),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("- a fact\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var calls []CLIMemoryDrainRequest
	m.SetCLIMemoryDrainer(func(req CLIMemoryDrainRequest) CLIMemoryDrainResult {
		calls = append(calls, req)
		return CLIMemoryDrainResult{Store: "vault", Drained: len(req.Sources)}
	})
	m.agents["scanner"] = &AgentProcess{Name: "scanner", UID: 2001, Config: config.AgentConfig{
		Backend: "claude", CLIMemory: &config.CLIMemoryConfig{DrainToKnowledge: true},
	}}

	t.Run("narrows to the hook's project", func(t *testing.T) {
		calls = nil
		res, ran, err := m.DrainCLIMemoryNow(CLIMemoryDrainNowRequest{
			Agent: "scanner", CallerUID: 2001, Cwd: "/data/agents/scanner/acme.widgets", Truncate: true,
		})
		if err != nil || !ran || res.Drained != 1 || len(calls) != 1 {
			t.Fatalf("res=%+v ran=%v err=%v calls=%+v", res, ran, err, calls)
		}
		got := calls[0]
		if !got.Truncate || len(got.Sources) != 1 || got.Sources[0].CLI != cliMemoryClaude ||
			filepath.Base(filepath.Dir(got.Sources[0].Path)) != "-data-agents-scanner-acme-widgets" {
			t.Fatalf("request = %+v", got)
		}
	})

	t.Run("without cwd drains every store", func(t *testing.T) {
		calls = nil
		if _, ran, err := m.DrainCLIMemoryNow(CLIMemoryDrainNowRequest{Agent: "scanner", CallerUID: 2001}); err != nil || !ran {
			t.Fatalf("ran=%v err=%v", ran, err)
		}
		if len(calls) != 1 || len(calls[0].Sources) != 3 || calls[0].Truncate {
			t.Fatalf("calls = %+v", calls)
		}
	})

	t.Run("refuses another agent's name", func(t *testing.T) {
		calls = nil
		for _, uid := range []int{2002, 4242} {
			_, ran, err := m.DrainCLIMemoryNow(CLIMemoryDrainNowRequest{Agent: "scanner", CallerUID: uid})
			if err == nil || ran {
				t.Fatalf("uid %d: ran=%v err=%v, want refusal", uid, ran, err)
			}
		}
		if _, _, err := m.DrainCLIMemoryNow(CLIMemoryDrainNowRequest{Agent: "", CallerUID: 2001}); err == nil {
			t.Fatal("an unnamed request must be refused")
		}
		if len(calls) != 0 {
			t.Fatalf("drainer ran for a refused request: %+v", calls)
		}
	})

	t.Run("write hook is a no-op without drain_on_write", func(t *testing.T) {
		calls = nil
		_, ran, err := m.DrainCLIMemoryNow(CLIMemoryDrainNowRequest{Agent: "scanner", CallerUID: 2001, OnWrite: true})
		if err != nil || ran || len(calls) != 0 {
			t.Fatalf("ran=%v err=%v calls=%+v", ran, err, calls)
		}
		m.agents["scanner"].Config.CLIMemory = &config.CLIMemoryConfig{DrainToKnowledge: true, DrainOnWrite: true}
		t.Cleanup(func() { m.agents["scanner"].Config.CLIMemory = &config.CLIMemoryConfig{DrainToKnowledge: true} })
		if _, ran, err := m.DrainCLIMemoryNow(CLIMemoryDrainNowRequest{Agent: "scanner", CallerUID: 2001, OnWrite: true}); err != nil || !ran || len(calls) != 1 {
			t.Fatalf("ran=%v err=%v calls=%+v", ran, err, calls)
		}
	})

	t.Run("no-op when the drain is off", func(t *testing.T) {
		calls = nil
		m.agents["scanner"].Config.CLIMemory = &config.CLIMemoryConfig{}
		t.Cleanup(func() { m.agents["scanner"].Config.CLIMemory = &config.CLIMemoryConfig{DrainToKnowledge: true} })
		res, ran, err := m.DrainCLIMemoryNow(CLIMemoryDrainNowRequest{Agent: "scanner", CallerUID: 2001})
		if err != nil || ran || res.Drained != 0 || len(calls) != 0 {
			t.Fatalf("res=%+v ran=%v err=%v calls=%+v", res, ran, err, calls)
		}
	})
}
