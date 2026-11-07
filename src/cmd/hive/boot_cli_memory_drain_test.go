package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge/climemory"
)

// drainCapturingRuntime is a hosted runtime that records the drainer the
// boot installs so the test can invoke it the way the manager does on a kick.
type drainCapturingRuntime struct {
	hostedRuntime
	fn crux.CLIMemoryDrainer
}

func (r *drainCapturingRuntime) SetCLIMemoryDrainer(fn crux.CLIMemoryDrainer) { r.fn = fn }

// sidecarLikeRuntime deliberately lacks SetCLIMemoryDrainer.
type sidecarLikeRuntime struct{ hostedRuntime }

func TestCLIMemoryDrainEnabled(t *testing.T) {
	b := &boot{}
	if b.cliMemoryDrainEnabled() {
		t.Fatal("nil config must not enable the drain")
	}
	cfg := bootKnowledgeConfig(t)
	b.cfg = cfg
	if b.cliMemoryDrainEnabled() {
		t.Fatal("default config must not enable the drain")
	}
	ac := cfg.Agents["scanner"]
	ac.CLIMemory = &config.CLIMemoryConfig{DrainToKnowledge: true}
	cfg.Agents["scanner"] = ac
	if !b.cliMemoryDrainEnabled() {
		t.Fatal("a per-agent drain_to_knowledge must enable the drain")
	}
	cfg.Agents["scanner"] = config.AgentConfig{Enabled: false, Backend: "copilot", CLIMemory: &config.CLIMemoryConfig{DrainToKnowledge: true}}
	if b.cliMemoryDrainEnabled() {
		t.Fatal("a disabled agent must not enable the drain")
	}
	cfg.AgentsCLIMemory.DrainToKnowledge = true
	if !b.cliMemoryDrainEnabled() {
		t.Fatal("the fleet-wide flag must enable the drain")
	}
}

func TestInstallCLIMemoryDrainerWritesDraftsIntoBeadSynthVault(t *testing.T) {
	cfg := bootKnowledgeConfig(t)
	cfg.AgentsCLIMemory.DrainToKnowledge = true
	cfg.Knowledge.BeadSynthesizer.TargetLayer = "team"
	b := newBootKnowledgeBoot(t, cfg)
	var log strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&log, nil))
	rt := &drainCapturingRuntime{hostedRuntime: b.agentMgr}
	b.agentMgr = rt
	b.bootKnowledgeWith(newBootKnowledgeFake(t).deps)
	if b.knowledgeAPI == nil {
		t.Fatal("knowledge API must be auto-enabled")
	}

	b.installCLIMemoryDrainer()

	vaultPath := b.beadSynthVaultPath()
	if b.knowledgeAPI.GetVaultStore(vaultPath) == nil {
		t.Fatal("the bead-synth vault must be connected even with no bead stores")
	}
	if rt.fn == nil {
		t.Fatalf("drainer must be installed on the in-process runtime; log:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "CLI memory drain installed") || !strings.Contains(log.String(), "layer=team") {
		t.Fatalf("log:\n%s", log.String())
	}

	memDir := filepath.Join(t.TempDir(), ".claude", "projects", "-data-agents-scanner-acme-widgets", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("- The widgets build needs CGO_ENABLED=1 on CI runners\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := rt.fn(crux.CLIMemoryDrainRequest{Agent: "scanner", Backend: "claude", Sources: []crux.CLIMemorySource{{CLI: climemory.CLIClaude, Path: memDir}}})
	if res.Err != nil || res.Store != beadSynthVaultName || res.Drained != 1 {
		t.Fatalf("drain result = %+v", res)
	}
	entries, err := filepath.Glob(filepath.Join(vaultPath, "cli-memory-*.md"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("vault entries = %v, err=%v", entries, err)
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state: draft", "repo: acme/widgets", "agent: scanner"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("entry missing %q:\n%s", want, data)
		}
	}
	if again := rt.fn(crux.CLIMemoryDrainRequest{Agent: "scanner", Backend: "claude", Sources: []crux.CLIMemorySource{{CLI: climemory.CLIClaude, Path: memDir}}}); again.Drained != 0 || again.Dupes != 1 {
		t.Fatalf("second drain = %+v, want dedupe", again)
	}

	// Calling again keeps the already-connected vault and reinstalls.
	rt.fn = nil
	b.installCLIMemoryDrainer()
	if rt.fn == nil {
		t.Fatal("reinstall must set the drainer again")
	}
}

func TestInstallCLIMemoryDrainerStandsDown(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		b := newBootKnowledgeBoot(t, cfg)
		rt := &drainCapturingRuntime{hostedRuntime: b.agentMgr}
		b.agentMgr = rt
		b.installCLIMemoryDrainer()
		if rt.fn != nil {
			t.Fatal("drain off must not install a drainer")
		}
	})
	t.Run("sidecar runtime warns", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		cfg.AgentsCLIMemory.DrainToKnowledge = true
		b := newBootKnowledgeBoot(t, cfg)
		var log strings.Builder
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		b.agentMgr = &sidecarLikeRuntime{hostedRuntime: b.agentMgr}
		b.installCLIMemoryDrainer()
		if !strings.Contains(log.String(), "sidecar runtime cannot drain") {
			t.Fatalf("log:\n%s", log.String())
		}
	})
	t.Run("no knowledge API warns", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		cfg.AgentsCLIMemory.DrainToKnowledge = true
		b := newBootKnowledgeBoot(t, cfg)
		var log strings.Builder
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		rt := &drainCapturingRuntime{hostedRuntime: b.agentMgr}
		b.agentMgr = rt
		b.installCLIMemoryDrainer()
		if rt.fn != nil || !strings.Contains(log.String(), "knowledge API is unavailable") {
			t.Fatalf("installed=%v log:\n%s", rt.fn != nil, log.String())
		}
	})
	t.Run("vault path blocked by a file warns", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		cfg.AgentsCLIMemory.DrainToKnowledge = true
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg.Knowledge.BeadSynthesizer.VaultPath = filepath.Join(blocker, "vault")
		b := newBootKnowledgeBoot(t, cfg)
		var log strings.Builder
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		rt := &drainCapturingRuntime{hostedRuntime: b.agentMgr}
		b.agentMgr = rt
		b.bootKnowledgeWith(newBootKnowledgeFake(t).deps)
		b.installCLIMemoryDrainer()
		if rt.fn != nil || !strings.Contains(log.String(), "failed to create vault for CLI memory drain") {
			t.Fatalf("installed=%v log:\n%s", rt.fn != nil, log.String())
		}
	})
}
