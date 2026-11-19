package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/cruxtest"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/scheduler"
)

type bootKnowledgeFake struct {
	deps bootKnowledgeDeps

	inited, seeded   []string
	initErr, seedErr error
	gitSyncer        *knowledge.GitSyncer
	beadSynthStarted bool
	promotionStarted bool
	graphStore       *knowledge.GraphStore
	graphErr         error
	graphWired       bool
	cleanupAudit     *dashboard.AuditLog
	nousDirs         int
	inceptionDir     string
	wikiReconnects   int
}

func newBootKnowledgeFake(t *testing.T) *bootKnowledgeFake {
	t.Helper()
	f := &bootKnowledgeFake{inceptionDir: t.TempDir()}
	f.deps = bootKnowledgeDeps{
		initVaultRepo: func(path string, logger *slog.Logger) error {
			f.inited = append(f.inited, path)
			if f.initErr != nil {
				return f.initErr
			}
			return knowledge.InitVaultRepo(path, logger)
		},
		seedVaultContent: func(path string, _ *slog.Logger) error { f.seeded = append(f.seeded, path); return f.seedErr },
		startGitSyncer:   func(_ context.Context, g *knowledge.GitSyncer) { f.gitSyncer = g },
		startBeadSynth:   func(context.Context, *knowledge.BeadSynthesizer) { f.beadSynthStarted = true },
		startPromotion:   func(context.Context, *knowledge.PromotionScheduler) { f.promotionStarted = true },
		openGraphStoreAsync: func(_ *slog.Logger, wire func(*knowledge.GraphStore, error)) {
			f.graphWired = true
			if f.graphStore == nil && f.graphErr == nil {
				f.graphErr = errors.New("no graph store in this test")
			}
			wire(f.graphStore, f.graphErr)
		},
		startWorkspaceCleanup: func(_ context.Context, _ *slog.Logger, a *dashboard.AuditLog) { f.cleanupAudit = a },
		ensureNousDirs:        func(*slog.Logger) { f.nousDirs++ },
		loadNousState:         func(*slog.Logger) *dashboard.NousState { return &dashboard.NousState{Mode: "observe"} },
		reconnectInceptionWiki: func(api *knowledge.KnowledgeAPI, logger *slog.Logger) {
			f.wikiReconnects++
			knowledge.ReconnectInceptionWiki(f.inceptionDir, api, logger)
		},
	}
	return f
}

func bootKnowledgeConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.HiveID = "boot-knowledge-test"
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"acme/widgets"}
	cfg.Agents = map[string]config.AgentConfig{
		"scanner":    {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
		"brainstorm": {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
	}
	cfg.Knowledge.BeadSynthesizer.VaultPath = filepath.Join(t.TempDir(), "bead-synth")
	return cfg
}

func newBootKnowledgeBoot(t *testing.T, cfg *config.Config) *boot {
	t.Helper()
	b, _ := newDepsTestBoot(t, cfg)
	b.sched = scheduler.New(cfg, b.logger)
	b.agentMgr = cruxtest.NewRuntime(cfg.Agents, b.logger, crux.ProjectContext{})
	b.dashSrv = dashboard.NewServer(0, b.logger)
	b.beadStores = map[string]*beads.Store{}
	return b
}

func TestBootKnowledgeWithMinimalConfigAutoEnablesFileAPI(t *testing.T) {
	cfg := bootKnowledgeConfig(t)
	b := newBootKnowledgeBoot(t, cfg)
	var log strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&log, nil))
	f := newBootKnowledgeFake(t)

	b.bootKnowledgeWith(f.deps)

	if b.knowledgeAPI == nil {
		t.Fatal("knowledge API must be auto-enabled")
	}
	if !strings.Contains(log.String(), "auto-enabled file-based knowledge API") {
		t.Fatalf("log:\n%s", log.String())
	}
	if f.gitSyncer == nil || len(f.gitSyncer.Names()) != 0 {
		t.Fatalf("git syncer = %v", f.gitSyncer)
	}
	if b.beadSynth != nil || f.beadSynthStarted {
		t.Fatal("no bead stores means no synthesizer")
	}
	if f.promotionStarted {
		t.Fatal("curator disabled by default")
	}
	if !f.graphWired || f.cleanupAudit != b.dashSrv.GetAudit() || f.nousDirs != 1 {
		t.Fatalf("background wiring: graph=%v cleanup=%v nous=%d", f.graphWired, f.cleanupAudit != nil, f.nousDirs)
	}
	if b.nousState == nil || b.nousState.SnapshotDir != nousSnapshotDir {
		t.Fatalf("nous state = %+v", b.nousState)
	}
	if f.wikiReconnects != 1 {
		t.Fatalf("past inception facts must be reconnected once at boot, got %d", f.wikiReconnects)
	}
	if !b.agentMgr.IsPaused("brainstorm") {
		t.Fatal("brainstorm must be paused on-demand at boot")
	}
}

func TestBootKnowledgeWithVaultsConnectAndRegisterWithPrimer(t *testing.T) {
	cfg := bootKnowledgeConfig(t)
	cfg.Knowledge.Enabled = true
	cfg.Knowledge.Engine = "file"
	vault := filepath.Join(t.TempDir(), "wiki")
	cfg.Knowledge.Vaults = []config.VaultConfig{
		{Name: "team-wiki", Path: vault, GitSync: true},
		{Name: "no-sync", Path: filepath.Join(t.TempDir(), "other")},
	}
	b := newBootKnowledgeBoot(t, cfg)
	primer := knowledge.NewPrimer(nil, knowledge.PrimerConfig{}, b.logger)
	b.sched.SetPrimer(primer)
	f := newBootKnowledgeFake(t)

	b.bootKnowledgeWith(f.deps)

	if len(f.inited) != 2 || len(f.seeded) != 2 {
		t.Fatalf("init=%v seed=%v", f.inited, f.seeded)
	}
	if got := len(b.knowledgeAPI.Vaults()); got != 2 {
		t.Fatalf("vaults connected = %d", got)
	}
	if got := strings.Join(primer.FileStoreNames(), ","); got != "team-wiki,no-sync" {
		t.Fatalf("primer stores = %q", got)
	}
	if got := strings.Join(f.gitSyncer.Names(), ","); got != "team-wiki" {
		t.Fatalf("git-synced vaults = %q", got)
	}
}

func TestBootKnowledgeWithVaultInitFailureSkipsVault(t *testing.T) {
	cfg := bootKnowledgeConfig(t)
	cfg.Knowledge.Enabled = true
	cfg.Knowledge.Vaults = []config.VaultConfig{{Name: "broken", Path: filepath.Join(t.TempDir(), "v")}}
	b := newBootKnowledgeBoot(t, cfg)
	var log strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&log, nil))
	f := newBootKnowledgeFake(t)
	f.initErr = errors.New("read-only fs")

	b.bootKnowledgeWith(f.deps)

	if len(f.seeded) != 0 || len(b.knowledgeAPI.Vaults()) != 0 {
		t.Fatalf("broken vault must be skipped: seeded=%v vaults=%d", f.seeded, len(b.knowledgeAPI.Vaults()))
	}
	if !strings.Contains(log.String(), "failed to init vault directory") {
		t.Fatalf("log:\n%s", log.String())
	}
}

func TestBootKnowledgeWithDocumentImportFailureIsWarning(t *testing.T) {
	cfg := bootKnowledgeConfig(t)
	cfg.Knowledge.Documents = []config.DocSourceConfigYAML{{Name: "missing", FilePath: filepath.Join(t.TempDir(), "nope.md"), Layer: "personal"}}
	b := newBootKnowledgeBoot(t, cfg)
	var log strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&log, nil))

	b.bootKnowledgeWith(newBootKnowledgeFake(t).deps)

	if !strings.Contains(log.String(), "auto-enabled knowledge API for document sources") || !strings.Contains(log.String(), "failed to import document source") {
		t.Fatalf("log:\n%s", log.String())
	}
}

func TestBootKnowledgeWithBeadStoresBuildAndStartSynthesizer(t *testing.T) {
	cfg := bootKnowledgeConfig(t)
	b := newBootKnowledgeBoot(t, cfg)
	b.beadStores["scanner"] = newTestBeadStore(t)
	primer := knowledge.NewPrimer(nil, knowledge.PrimerConfig{}, b.logger)
	b.sched.SetPrimer(primer)
	f := newBootKnowledgeFake(t)

	b.bootKnowledgeWith(f.deps)

	if b.beadSynth == nil || !f.beadSynthStarted {
		t.Fatal("bead synthesizer not built/started")
	}
	if got := strings.Join(primer.FileStoreNames(), ","); got != "bead-synth-wiki" {
		t.Fatalf("primer stores = %q", got)
	}
	if _, err := os.Stat(cfg.Knowledge.BeadSynthesizer.VaultPath); err != nil {
		t.Fatalf("synth vault dir not created: %v", err)
	}

	t.Run("disabled synthesizer is built but not started", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		off := false
		cfg.Knowledge.BeadSynthesizer.Enabled = &off
		b := newBootKnowledgeBoot(t, cfg)
		b.beadStores["scanner"] = newTestBeadStore(t)
		f := newBootKnowledgeFake(t)
		b.bootKnowledgeWith(f.deps)
		if b.beadSynth == nil || f.beadSynthStarted {
			t.Fatalf("synth=%v started=%v", b.beadSynth != nil, f.beadSynthStarted)
		}
	})
}

func TestBootKnowledgeWithCuratorSchedule(t *testing.T) {
	t.Run("enabled starts promotion", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		on := true
		cfg.Knowledge.Curator.Enabled = &on
		cfg.Knowledge.Curator.Schedule = "0 3 * * *"
		b := newBootKnowledgeBoot(t, cfg)
		f := newBootKnowledgeFake(t)
		b.bootKnowledgeWith(f.deps)
		if !f.promotionStarted {
			t.Fatal("promotion scheduler not started")
		}
	})
	t.Run("schedule without enabled hints opt-in", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		cfg.Knowledge.Curator.Schedule = "0 3 * * *"
		b := newBootKnowledgeBoot(t, cfg)
		var log strings.Builder
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		f := newBootKnowledgeFake(t)
		b.bootKnowledgeWith(f.deps)
		if f.promotionStarted || !strings.Contains(log.String(), "scheduled promotion is disabled") {
			t.Fatalf("started=%v log:\n%s", f.promotionStarted, log.String())
		}
	})
}

func TestBootKnowledgeWithGraphStoreWiring(t *testing.T) {
	t.Run("open failure is a warning", func(t *testing.T) {
		b := newBootKnowledgeBoot(t, bootKnowledgeConfig(t))
		var log strings.Builder
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		f := newBootKnowledgeFake(t)
		f.graphErr = errors.New("locked")
		b.bootKnowledgeWith(f.deps)
		if !strings.Contains(log.String(), "failed to open knowledge graph store") {
			t.Fatalf("log:\n%s", log.String())
		}
		if b.knowledgeAPI.GraphStore() != nil {
			t.Fatal("graph store must not be attached on open failure")
		}
	})
	t.Run("open success wires primer, api, and synthesizer", func(t *testing.T) {
		cfg := bootKnowledgeConfig(t)
		b := newBootKnowledgeBoot(t, cfg)
		b.beadStores["scanner"] = newTestBeadStore(t)
		b.sched.SetPrimer(knowledge.NewPrimer(nil, knowledge.PrimerConfig{}, b.logger))
		var log strings.Builder
		b.logger = slog.New(slog.NewTextHandler(&log, nil))
		f := newBootKnowledgeFake(t)
		gs, err := knowledge.NewGraphStore(filepath.Join(t.TempDir(), "g.db"), b.logger)
		if err != nil {
			t.Skipf("graph store unavailable here: %v", err)
		}
		f.graphStore = gs

		b.bootKnowledgeWith(f.deps)

		if b.knowledgeAPI.GraphStore() != gs {
			t.Fatal("graph store not attached to knowledge API")
		}
		if !strings.Contains(log.String(), "knowledge graph store opened") {
			t.Fatalf("log:\n%s", log.String())
		}
	})
}
