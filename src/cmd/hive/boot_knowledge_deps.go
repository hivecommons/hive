package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/knowledge"
)

const (
	// vaultSeedDataDir ships starter wiki content in the image for new vaults.
	vaultSeedDataDir = "/opt/hive/seed-data/wiki"
	// beadSynthVaultDefaultPath is where bead-derived facts land when
	// knowledge.bead_synthesizer.vault_path is unset.
	beadSynthVaultDefaultPath = "/data/vaults/bead-synth-wiki"
	// knowledgeGraphStorePath is the SQLite-backed knowledge graph on the PVC.
	knowledgeGraphStorePath = "/data/graph/knowledge.db"
	// inceptionDataDir roots the inception engine's persisted state.
	inceptionDataDir = "/data"
)

// bootKnowledgeDeps are bootKnowledge's long-lived effects (#7571, step 2):
// vault init/seed on disk, the background loops (git sync, bead synth,
// promotion, graph store open + wiring, workspace cleanup), the nous state
// read, the inception engine (reads /data), and the brainstorm restart. The
// knowledge API, vault connects, bead synthesizer, and inception decision
// run for real; cfg paths point a test at temp dirs.
type bootKnowledgeDeps struct {
	initVaultRepo         func(path string, logger *slog.Logger) error
	seedVaultContent      func(vaultPath string, logger *slog.Logger) error
	startGitSyncer        func(ctx context.Context, g *knowledge.GitSyncer)
	startBeadSynth        func(ctx context.Context, s *knowledge.BeadSynthesizer)
	startPromotion        func(ctx context.Context, p *knowledge.PromotionScheduler)
	openGraphStoreAsync   func(logger *slog.Logger, wire func(*knowledge.GraphStore, error))
	startWorkspaceCleanup func(ctx context.Context, logger *slog.Logger, audit *dashboard.AuditLog)
	ensureNousDirs        func(logger *slog.Logger)
	loadNousState         func(logger *slog.Logger) *dashboard.NousState
	newInceptionEngine    func(api *knowledge.KnowledgeAPI, logger *slog.Logger) *knowledge.InceptionEngine
	restartBrainstorm     func(ctx context.Context, mgr *agent.Manager, msg string) error
}

func defaultBootKnowledgeDeps() bootKnowledgeDeps {
	return bootKnowledgeDeps{
		initVaultRepo: knowledge.InitVaultRepo,
		seedVaultContent: func(vaultPath string, logger *slog.Logger) error {
			return knowledge.SeedVaultContent(vaultPath, vaultSeedDataDir, logger)
		},
		startGitSyncer: func(ctx context.Context, g *knowledge.GitSyncer) { go g.Start(ctx) },
		startBeadSynth: func(ctx context.Context, s *knowledge.BeadSynthesizer) { s.StartBackground(ctx) },
		startPromotion: func(ctx context.Context, p *knowledge.PromotionScheduler) { p.StartBackground(ctx) },
		openGraphStoreAsync: func(logger *slog.Logger, wire func(*knowledge.GraphStore, error)) {
			go func() { wire(knowledge.NewGraphStore(knowledgeGraphStorePath, logger)) }()
		},
		startWorkspaceCleanup: func(ctx context.Context, logger *slog.Logger, audit *dashboard.AuditLog) {
			go dashboard.StartWorkspaceCleanup(ctx, logger, audit)
		},
		ensureNousDirs: func(logger *slog.Logger) {
			if err := os.MkdirAll(nousSnapshotDir, 0o755); err != nil {
				logger.Warn("failed to create nous snapshot dir", "path", nousSnapshotDir, "error", err)
			}
			if err := os.MkdirAll(nousGovernorDir, 0o755); err != nil {
				logger.Warn("failed to create nous governor dir", "path", nousGovernorDir, "error", err)
			}
		},
		loadNousState: loadNousState,
		newInceptionEngine: func(api *knowledge.KnowledgeAPI, logger *slog.Logger) *knowledge.InceptionEngine {
			return knowledge.NewInceptionEngine(inceptionDataDir, api, logger)
		},
		restartBrainstorm: func(ctx context.Context, mgr *agent.Manager, msg string) error {
			return mgr.RestartWithBootstrap(ctx, "brainstorm", msg)
		},
	}
}
