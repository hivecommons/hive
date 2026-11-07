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
