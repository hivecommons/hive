package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/hivecommons/hive/pkg/campaignexport"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

const (
	// vaultSeedDataDir ships starter wiki content in the image for new vaults.
	vaultSeedDataDir = "/opt/hive/seed-data/wiki"
	// beadSynthVaultDefaultPath is where bead-derived facts land when
	// knowledge.bead_synthesizer.vault_path is unset.
	beadSynthVaultDefaultPath = "/data/vaults/bead-synth-wiki"
	// knowledgeGraphStorePath is the SQLite-backed knowledge graph on the PVC.
	knowledgeGraphStorePath = "/data/graph/knowledge.db"
	// inceptionDataDir roots what the retired inception engine left on the
	// PVC: the inception-wiki vault, its state file and campaign archives.
	inceptionDataDir = "/data"
)

// bootKnowledgeDeps are bootKnowledge's long-lived effects (#7571, step 2):
// vault init/seed on disk, the background loops (git sync, bead synth,
// promotion, graph store open + wiring, workspace cleanup), the nous state
// read, the inception-wiki reconnect and the retired-campaigns export (both
// read /data). The knowledge API, vault connects and bead synthesizer run for
// real; cfg paths point a test at temp dirs.
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
	// reconnectInceptionWiki keeps facts from past inception runs (the
	// inception-wiki vault) served after the inception engine's retirement;
	// nil skips it.
	reconnectInceptionWiki func(api *knowledge.KnowledgeAPI, logger *slog.Logger)
	// exportRetiredCampaigns writes the one-time read-only export of retired
	// inception campaign archives and campaign Jam data; nil skips it.
	exportRetiredCampaigns func(cfg *config.Config, logger *slog.Logger)
	// startConnectorSyncer runs the knowledge.connectors syncer (#11069);
	// nil leaves it built but not started.
	startConnectorSyncer func(ctx context.Context, s *connector.Syncer)
	// startPublishMirror runs the knowledge.publish mirror (#11076); nil
	// leaves it built but not started.
	startPublishMirror func(ctx context.Context, m *connector.Mirror)
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
		reconnectInceptionWiki: func(api *knowledge.KnowledgeAPI, logger *slog.Logger) {
			knowledge.ReconnectInceptionWiki(inceptionDataDir, api, logger)
		},
		exportRetiredCampaigns: exportRetiredCampaigns,
		startConnectorSyncer:   func(ctx context.Context, s *connector.Syncer) { go s.Run(ctx) },
		startPublishMirror:     func(ctx context.Context, m *connector.Mirror) { go m.Start(ctx) },
	}
}

// exportRetiredCampaigns bundles retired inception campaign archives and the
// campaign Jam store into /data/exports/campaigns-<date>.tar.gz (0600) the
// first time either is found, and logs the path once. The sources are only
// read; they stay in place for one release. A failure is logged and never
// blocks boot.
func exportRetiredCampaigns(cfg *config.Config, logger *slog.Logger) {
	var metrics, logs, agents string
	if cfg != nil {
		metrics, logs, agents = cfg.Data.MetricsDir, cfg.Data.LogsDir, cfg.Data.AgentsDir
	}
	path, err := campaignexport.Export(inceptionDataDir, campaignexport.JamPath(metrics, logs, agents), time.Now())
	if err != nil {
		logger.Warn("retired campaigns export failed; the original files are untouched", "error", err)
		return
	}
	if path != "" {
		logger.Warn("retired campaigns exported", "path", path,
			"detail", "inception campaigns, campaign Jam and recheck were retired in favour of Inception (spek) sessions; the original files stay in place for one release")
	}
}
