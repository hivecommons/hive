package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
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
	prevSpool := cliMemorySpoolDir
	cliMemorySpoolDir = t.TempDir()
	t.Cleanup(func() { cliMemorySpoolDir = prevSpool })
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

// TestInstalledDrainerTruncatesOnlyFullyStoredFiles drives the installed
// drainer with Truncate the way the on-demand drain does
// (hivecommons/hive#17208): a file whose facts all reached the vault is
// emptied, a file with a fact over the per-drain cap is left alone.
func TestInstalledDrainerTruncatesOnlyFullyStoredFiles(t *testing.T) {
	cfg := bootKnowledgeConfig(t)
	cfg.AgentsCLIMemory.DrainToKnowledge = true
	prevSpool := cliMemorySpoolDir
	cliMemorySpoolDir = t.TempDir()
	t.Cleanup(func() { cliMemorySpoolDir = prevSpool })
	b := newBootKnowledgeBoot(t, cfg)
	rt := &drainCapturingRuntime{hostedRuntime: b.agentMgr}
	b.agentMgr = rt
	b.bootKnowledgeWith(newBootKnowledgeFake(t).deps)
	b.installCLIMemoryDrainer()
	if rt.fn == nil {
		t.Fatal("drainer must be installed")
	}

	memDir := filepath.Join(t.TempDir(), ".claude", "projects", "-data-agents-scanner-acme-widgets", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stored := filepath.Join(memDir, "a.md")
	if err := os.WriteFile(stored, []byte("- The widgets build needs CGO_ENABLED=1 on CI runners\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var big strings.Builder
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&big, "- Widget fact number %d is distinct from every other one\n", i)
	}
	partial := filepath.Join(memDir, "b.md")
	if err := os.WriteFile(partial, []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	src := []crux.CLIMemorySource{{CLI: climemory.CLIClaude, Path: memDir}}

	res := rt.fn(crux.CLIMemoryDrainRequest{Agent: "scanner", Backend: "claude", Sources: src, Truncate: true})
	if res.Err != nil || res.Truncated != 1 || res.Kept != 1 {
		t.Fatalf("truncating drain = %+v", res)
	}
	if b, _ := os.ReadFile(stored); len(b) != 0 {
		t.Fatalf("fully stored file not emptied: %q", b)
	}
	if b, _ := os.ReadFile(partial); len(b) != big.Len() {
		t.Fatal("file with facts missing from the vault was truncated")
	}

	// The kick path never sets Truncate: clear_on_kick owns the wipe there.
	kickDir := filepath.Join(t.TempDir(), ".claude", "projects", "-data-agents-scanner", "memory")
	if err := os.MkdirAll(kickDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kickFile := filepath.Join(kickDir, "MEMORY.md")
	if err := os.WriteFile(kickFile, []byte("- Kick drains leave the store to clear_on_kick\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := rt.fn(crux.CLIMemoryDrainRequest{Agent: "scanner", Backend: "claude", Sources: []crux.CLIMemorySource{{CLI: climemory.CLIClaude, Path: kickDir}}}); res.Truncated != 0 {
		t.Fatalf("a drain without Truncate emptied files: %+v", res)
	}
	if b, _ := os.ReadFile(kickFile); len(b) == 0 {
		t.Fatal("a drain without Truncate must not touch the store")
	}
}

// drainNowHost is a fake crux.CLIMemoryDrainNowHost.
type drainNowHost struct {
	got []crux.CLIMemoryDrainNowRequest
	res crux.CLIMemoryDrainResult
	ran bool
	err error
}

func (h *drainNowHost) DrainCLIMemoryNow(req crux.CLIMemoryDrainNowRequest) (crux.CLIMemoryDrainResult, bool, error) {
	h.got = append(h.got, req)
	return h.res, h.ran, h.err
}

type drainAuditRecord struct {
	actor, action, agent string
	fields               map[string]any
}

type drainAuditSink struct{ recs []drainAuditRecord }

func (s *drainAuditSink) Record(actor, action, agent string, fields map[string]any) {
	s.recs = append(s.recs, drainAuditRecord{actor, action, agent, fields})
}

func TestCLIMemoryDrainRequestHandlerAuthAndAudit(t *testing.T) {
	cases := []struct {
		name    string
		host    *drainNowHost
		ok, ran bool
		outcome string
	}{
		{"drained", &drainNowHost{ran: true, res: crux.CLIMemoryDrainResult{Store: beadSynthVaultName, Drained: 2, Truncated: 1}}, true, true, "drained"},
		{"drain off is a no-op", &drainNowHost{}, true, false, "noop"},
		{"another agent's name is refused", &drainNowHost{err: errors.New("an agent may only drain its own memory")}, false, false, "refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			audit := &drainAuditSink{}
			resp := cliMemoryDrainRequestHandler(tc.host, audit)(github.MemoryDrainRequest{Agent: "scanner", Cwd: "/data/agents/scanner/w", Truncate: true}, 2001)
			if resp.OK != tc.ok || resp.Ran != tc.ran {
				t.Fatalf("resp = %+v", resp)
			}
			if len(tc.host.got) != 1 || tc.host.got[0] != (crux.CLIMemoryDrainNowRequest{Agent: "scanner", CallerUID: 2001, Cwd: "/data/agents/scanner/w", Truncate: true}) {
				t.Fatalf("host got %+v", tc.host.got)
			}
			if len(audit.recs) != 1 || audit.recs[0].action != auditCLIMemoryDrainRequested || audit.recs[0].agent != "scanner" ||
				audit.recs[0].fields["outcome"] != tc.outcome || audit.recs[0].fields["uid"] != "2001" {
				t.Fatalf("audit = %+v", audit.recs)
			}
			if tc.ok && resp.Drained != tc.host.res.Drained {
				t.Fatalf("resp = %+v", resp)
			}
		})
	}
}
