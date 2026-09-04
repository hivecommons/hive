package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/tokens"
)

func (w *spokeWire) wireSpokeMetricsAndKnowledgeAPI() {
	// Count stores that fail to open. They are dropped from w.beadStores entirely,
	// which makes an incomplete ledger indistinguishable from a smaller one — and
	// the dependency admission gate must not read a lookup miss in a truncated
	// ledger as "this candidate declared no dependencies".
	w.beadStoreLoadFailures = 0
	for name, agentCfg := range w.cfg.EnabledAgents() {
		store, err := beads.NewStore(agentCfg.BeadsDir)
		if err != nil {
			w.logger.Warn("failed to init beads store", "agent", name, "error", err)
			w.beadStoreLoadFailures++
			continue
		}
		store.SetHiveID(w.cfg.HiveID)
		w.beadStores[name] = store
		w.logger.Info("beads store initialized", "agent", name, "count", store.Count())
	}

	// Scan /data/beads/ for agent directories that have beads.json files on
	// disk but are not covered by the enabled-agent loop above. This handles
	// agents that were disabled between restarts or added by a previous ACMM
	// pack that is no longer active.
	const beadsRootDir = "/data/beads"
	if entries, err := os.ReadDir(beadsRootDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if _, exists := w.beadStores[name]; exists {
				continue // already loaded from config
			}
			agentBeadsDir := filepath.Join(beadsRootDir, name)
			beadsFile := filepath.Join(agentBeadsDir, "beads.json")
			if _, statErr := os.Stat(beadsFile); statErr != nil {
				continue // no beads.json in this directory
			}
			store, err := beads.NewStore(agentBeadsDir)
			if err != nil {
				w.logger.Warn("failed to load orphan beads store", "agent", name, "error", err)
				w.beadStoreLoadFailures++
				continue
			}
			store.SetHiveID(w.cfg.HiveID)
			w.beadStores[name] = store
			w.logger.Info("orphan beads store loaded from disk", "agent", name, "count", store.Count())
		}
	}

	if w.cfg.Retro.Enabled {
		if _, exists := w.beadStores[retro.Actor]; !exists {
			retroStore, err := beads.NewStore(filepath.Join(beadsRootDir, retro.Actor))
			if err != nil {
				w.logger.Warn("failed to init retro beads store", "error", err)
				w.beadStoreLoadFailures++
			} else {
				retroStore.SetHiveID(w.cfg.HiveID)
				w.beadStores[retro.Actor] = retroStore
				w.logger.Info("retro beads store initialized", "count", retroStore.Count())
			}
		}
	}

	initAgentConfigDrivenSystems(w.cfg)

	w.tokenCollector = tokens.NewCollector(w.cfg.Data.MetricsDir, w.logger)
	w.tokenCollector.SetClaudeSessionsDir(w.cfg.Data.ClaudeSessionsDir)
	w.tokenCollector.SetCopilotSessionsDir(w.cfg.Data.CopilotSessionsDir)
	w.tokenCollector.SetBobSessionsDir(w.cfg.Data.BobSessionsDir)
	tokenStop := make(chan struct{})
	go w.tokenCollector.Start(tokenStop)
	w.addCleanup(func() { close(tokenStop) })

	badgeURL := os.Getenv("HIVE_COVERAGE_BADGE_URL")
	if badgeURL == "" {
		badgeURL = "https://gist.githubusercontent.com/clubanderson/b9a9ae8469f1897a22d5a40629bc1e82/raw/coverage-badge.json"
	}
	primaryRepo := w.cfg.Project.PrimaryRepo
	if primaryRepo == "" && len(w.cfg.Project.Repos) > 0 {
		primaryRepo = w.cfg.Project.Repos[0]
	}
	w.metricsCollector = dashboard.NewMetricsCollector(w.ghClient, w.cfg.Project.Org, primaryRepo, badgeURL, w.cfg.Project.AIAuthor, w.cfg.Project.Name, w.logger)
	go w.metricsCollector.Start(w.ctx)

	// Fleet-stats collector: computes this hive's AI-author contribution counts
	// (merged/rejected PRs, CVE-referencing PRs) across its org on a slow timer
	// and caches them, so each heartbeat can attach a fresh-but-cheap snapshot
	// the hub aggregates into the public landing page's live fleet-stats strip.
	// ai_author is optional config and hosted hives are provisioned without it,
	// so most spokes had an empty author — which silently disabled the collector
	// entirely (Start() returns early) and left the public fleet-stats strip
	// blank. Fall back to the bot token's own GitHub login: that IS the account
	// the agents open PRs as, so it is the correct author to count. Never fall
	// back to an org-wide search with no author filter — that would sweep in
	// human PRs and overstate what the fleet's agents actually did.
	//
	// Use EffectiveAIAuthor(), not the raw Project.AIAuthor field. App-authored
	// hives deliberately leave ai_author EMPTY and derive their identity from
	// the installed App ("<slug>[bot]") — that is what keeps App-bot mode
	// durable across restarts. Reading the raw field saw "" for every one of
	// them and disabled the collector fleet-wide, while the PAT fallback below
	// could not rescue it either: those hives authenticate as a GitHub App and
	// have github.token empty, so there was no token to identify. The result
	// was a fleet where essentially no spoke ever attempted a collect.
	fleetStatsAuthor := w.cfg.EffectiveAIAuthor()
	fleetStatsToken := w.cfg.GitHub.Token
	if fleetStatsToken == "" {
		fleetStatsToken = os.Getenv("HIVE_GITHUB_TOKEN")
	}
	if fleetStatsAuthor == "" && fleetStatsToken != "" {
		if botUser, err := github.ValidateToken(fleetStatsToken, w.cfg.GitHub.ResolvedAPIURL()); err == nil && botUser.Login != "" {
			fleetStatsAuthor = botUser.Login
			w.logger.Info("fleet stats: ai_author unset, using bot token identity",
				"author", fleetStatsAuthor)
		} else if err != nil {
			w.logger.Warn("fleet stats: ai_author unset and bot identity lookup failed; "+
				"this hive will not contribute to the public fleet-stats total",
				"error", err)
		}
	}
	if fleetStatsAuthor == "" || w.cfg.Project.Org == "" {
		w.logger.Warn("fleet stats collector disabled: author or org is empty; "+
			"set project.ai_author in hive.yaml so this hive contributes to the fleet total",
			"author", fleetStatsAuthor, "org", w.cfg.Project.Org)
	}
	w.fleetStatsCollector = collect.NewFleetStatsCollector(w.ghClient, fleetStatsAuthor, w.cfg.Project.Org, w.logger)
	// Persist the collected counts on the /data PVC (same store as sessions and
	// cost/fact history) so a restart resumes from the last-known counts instead
	// of nil. Without this, a fleet-wide upgrade clears every spoke's in-memory
	// counts and the public landing-page total collapses until all spokes
	// re-collect (#2329, building on the hub-side #2328 defensive aging fix).
	w.fleetStatsCollector.EnablePersistence("/data/fleet-stats.json")
	go w.fleetStatsCollector.Start(w.ctx)

	// Per-repo output-activity collector: reads the local audit log (no GitHub
	// calls) and summarizes issues/PRs/comments/merges/claims/reviews per repo
	// with recency, so the hub can tell — from the heartbeat alone — whether each
	// hive is producing output back to its work source. Persisted to the /data
	// PVC so a restart resumes the last summary; the collector loop reads
	// /data/audit.jsonl every few minutes.
	w.activityCollector = collect.NewActivityCollector(w.dashSrv.GetAudit(), "", w.logger)
	w.activityCollector.EnablePersistence("/data/activity.json")
	go w.activityCollector.Start(w.ctx)

	// Per-repo cost collector: joins the same audited output events against
	// the token collector's per-message usage timeline, on the same ticker
	// interval as the activity collector above, and caches the result for
	// /api/repo-cost. Before this (#4943), the interval join — including
	// the same expensive audit read the activity collector does — ran on
	// every 60s dashboard poll, per open browser tab, instead of once per
	// collection interval.
	w.repoCostCollector = collect.NewRepoCostCollector(w.dashSrv.GetAudit(), w.tokenCollector, "", w.logger)
	w.repoCostCollector.EnablePersistence("/data/repo-cost.json")
	go w.repoCostCollector.Start(w.ctx)

	// Persistent hourly metrics behind the Operations + Leaderboard sparklines
	// (queue depth, tasks/hour, fleet size, per-contributor completions). The
	// store loads any prior 7-day history from the /data PVC on first use and the
	// rollup goroutine samples + buckets hourly, so a rolling upgrade resumes the
	// trend instead of flattening it. Bound to w.ctx so it shuts down cleanly with
	// the rest of the background loops (no goroutine leak). See contribute_metrics.go.
	w.dashSrv.StartContributeMetrics(w.ctx)
	w.refreshDashboard = func() {
		// Capture the mutation epoch BEFORE reading any state: if a mutation
		// (e.g. a restart-count or budget-window reset) lands while this
		// snapshot is being built, UpdateStatusIfFresh drops it so the stale
		// values never overwrite what the mutation's own refresh will publish
		// (#4348 — the restart-count flicker).
		buildEpoch := w.dashSrv.BeginStatusSnapshot()
		actionable := w.lastActionable.Load()
		govState := w.gov.GetState()
		agentStatuses := w.agentMgr.AllStatuses()
		payload := dashboard.BuildFrontendStatus(
			govState,
			actionable,
			agentStatuses,
			w.cfg,
			w.tokenCollector,
			w.gov,
			w.beadStores,
			w.ghClient,
			w.ctx,
			w.metricsCollector,
		)
		if d := w.dashSrv.GetAdvisoryDigest(); d != nil {
			payload.AdvisoryDigest = d
		}
		w.dashSrv.UpdateStatusIfFresh(payload, buildEpoch)
	}

	const cachedActionablePath = "/data/last-actionable.json"
	if data, err := os.ReadFile(cachedActionablePath); err == nil {
		var cached github.ActionableResult
		if err := json.Unmarshal(data, &cached); err == nil {
			w.lastActionable.Store(&cached)
			w.gov.SeedQueueState(cached.Issues.Count, cached.PRs.Count, cached.Hold.Total, cached.Issues.SLAViolations)
			w.refreshDashboard()
			w.logger.Info("restored cached actionable data", "issues", cached.Issues.Count, "prs", cached.PRs.Count, "age", time.Since(cached.GeneratedAt).Round(time.Second))
		}
	}
	if w.cfg.Knowledge.Enabled {
		layers := convertKnowledgeLayers(w.cfg.Knowledge.Layers)
		// The curator block was previously dropped here, so NewPromoter always
		// received a zero CuratorConfig and AutoPromoteThreshold never reached
		// the promoter in production. Passing it through is what makes the
		// threshold gate real for the scheduled sweep (#5430).
		w.knowledgeAPI = knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{
			Enabled: w.cfg.Knowledge.Enabled,
			Engine:  w.cfg.Knowledge.Engine,
			Curator: curatorConfigFromHive(w.cfg.Knowledge.Curator),
		}, w.logger)
	}

}

func (w *spokeWire) wireSpokeKnowledgeSources() {
	// Auto-connect configured vaults and start git-sync for Obsidian Git integration
	w.gitSyncer = knowledge.NewGitSyncer(w.logger)
	const seedDataDir = "/opt/hive/seed-data/wiki"
	for _, vc := range w.cfg.Knowledge.Vaults {
		if err := knowledge.InitVaultRepo(vc.Path, w.logger); err != nil {
			w.logger.Warn("failed to init vault directory", "name", vc.Name, "path", vc.Path, "error", err)
			continue
		}
		if err := knowledge.SeedVaultContent(vc.Path, seedDataDir, w.logger); err != nil {
			w.logger.Warn("failed to seed vault content", "name", vc.Name, "error", err)
		}
		if w.knowledgeAPI != nil {
			if err := w.knowledgeAPI.ConnectVault(vc.Path, vc.Name); err != nil {
				w.logger.Warn("failed to connect vault", "name", vc.Name, "path", vc.Path, "error", err)
				continue
			}
			w.logger.Info("vault auto-connected", "name", vc.Name, "path", vc.Path, "auto_index", vc.AutoIndex)
			if w.primer = w.sched.GetPrimer(); w.primer != nil {
				store := w.knowledgeAPI.GetVaultStore(vc.Path)
				if store != nil {
					w.primer.AddFileStore(vc.Name, store, knowledge.LayerPersonal)
					w.logger.Info("vault registered with primer", "name", vc.Name)
				}
			}
		}
		if vc.GitSync {
			// Find the store we just connected so the syncer can trigger reindex
			for _, vi := range w.knowledgeAPI.Vaults() {
				if vi.Name == vc.Name {
					// Re-fetch the FileStore by connecting info — the syncer needs it
					// to call Reindex() after each pull
					store := w.knowledgeAPI.GetVaultStore(vc.Path)
					if store != nil {
						w.gitSyncer.Add(vc.Name, vc.Path, store)
					}
					break
				}
			}
		}
	}

	// Auto-connect configured git sources (remote repos indexed as knowledge)
	for _, gsc := range w.cfg.Knowledge.GitSources {
		if w.knowledgeAPI == nil {
			// Knowledge not enabled but git sources configured — auto-enable
			w.knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
				Enabled: true,
				Engine:  "file",
			}, w.logger)
			w.logger.Info("auto-enabled knowledge API for git sources")
		}
		gsConfig := knowledge.GitSourceConfig{
			Name:    gsc.Name,
			URL:     gsc.URL,
			Branch:  gsc.Branch,
			Subpath: gsc.Subpath,
			Layer:   knowledge.LayerType(gsc.Layer),
		}
		if err := w.knowledgeAPI.ConnectGitSource(w.ctx, gsConfig); err != nil {
			w.logger.Warn("failed to connect git source",
				"name", gsc.Name,
				"url", gsc.URL,
				"subpath", gsc.Subpath,
				"error", err,
			)
		} else {
			w.logger.Info("git source connected",
				"name", gsc.Name,
				"url", gsc.URL,
				"subpath", gsc.Subpath,
				"layer", gsc.Layer,
			)
			// Register the FileStore with the scheduler's w.primer so agents
			// get primed with facts from this git source during kicks.
			if w.primer = w.sched.GetPrimer(); w.primer != nil {
				for _, gs := range w.knowledgeAPI.GitSources() {
					if gs.Name == gsc.Name && gs.Ready {
						store := w.knowledgeAPI.GetGitSourceStore(gsc.Name)
						if store != nil {
							w.primer.AddFileStore(gsc.Name, store, knowledge.LayerType(gsc.Layer))
						}
						break
					}
				}
			}
		}
	}

	// Auto-import configured document sources (PDFs, URLs as knowledge)
	for _, doc := range w.cfg.Knowledge.Documents {
		if w.knowledgeAPI == nil {
			w.knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
				Enabled: true,
				Engine:  "file",
			}, w.logger)
			w.logger.Info("auto-enabled knowledge API for document sources")
		}
		docConfig := knowledge.DocSourceConfig{
			Name:     doc.Name,
			URL:      doc.URL,
			FilePath: doc.FilePath,
			Layer:    knowledge.LayerType(doc.Layer),
		}
		meta, err := w.knowledgeAPI.ImportDocument(w.ctx, docConfig)
		if err != nil {
			w.logger.Warn("failed to import document source",
				"name", doc.Name,
				"error", err,
			)
		} else {
			w.logger.Info("document source imported",
				"name", doc.Name,
				"facts", meta.FactCount,
				"content_type", meta.ContentType,
			)
		}
	}

	go w.gitSyncer.Start(w.ctx)

	// Auto-enable knowledge API when not explicitly configured.
	// Both bead-synth-wiki and inception require it.
	if w.knowledgeAPI == nil {
		w.knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
			Enabled: true,
			Engine:  "file",
		}, w.logger)
		w.logger.Info("auto-enabled file-based knowledge API")
	}
	if len(w.beadStores) > 0 {
		synthVaultPath := w.cfg.Knowledge.BeadSynthesizer.VaultPath
		if synthVaultPath == "" {
			synthVaultPath = "/data/vaults/bead-synth-wiki"
		}
		if err := os.MkdirAll(synthVaultPath, 0o755); err != nil {
			w.logger.Warn("failed to create bead-synth vault dir", "path", synthVaultPath, "error", err)
		}
		if w.knowledgeAPI != nil {
			if connErr := w.knowledgeAPI.ConnectVault(synthVaultPath, "bead-synth-wiki"); connErr != nil {
				w.logger.Warn("failed to auto-connect bead-synth vault", "path", synthVaultPath, "error", connErr)
			} else {
				w.logger.Info("auto-connected bead-synth vault", "path", synthVaultPath)
				if w.primer = w.sched.GetPrimer(); w.primer != nil {
					store := w.knowledgeAPI.GetVaultStore(synthVaultPath)
					if store != nil {
						beadLayer := knowledge.LayerType(w.cfg.Knowledge.BeadSynthesizer.TargetLayer)
						if beadLayer == "" {
							beadLayer = knowledge.LayerPersonal
						}
						w.primer.AddFileStore("bead-synth-wiki", store, beadLayer)
						w.logger.Info("bead-synth vault registered with primer", "layer", beadLayer)
					}
				}
			}
		}
		var rawGH *gh.Client
		if w.ghClient != nil {
			rawGH = w.ghClient.GoGitHub()
		}

		var kRetention *knowledge.RetentionPolicy
		if rp := w.cfg.Knowledge.BeadSynthesizer.RetentionPolicy; rp != nil {
			kRetention = &knowledge.RetentionPolicy{
				MaxBeads:               rp.MaxBeads,
				ArchiveAfterSynthDays:  rp.ArchiveAfterSynthDays,
				HighPriorityRetainDays: rp.HighPriorityRetainDays,
				PreserveWithDeps:       rp.PreserveWithDeps,
			}
		} else {
			kRetention = &knowledge.RetentionPolicy{
				PreserveWithDeps: true,
			}
		}

		w.beadSynth = knowledge.NewBeadSynthesizer(w.beadStores, w.knowledgeAPI, knowledge.BeadSynthesizerConfig{
			Schedule:         w.cfg.Knowledge.BeadSynthesizer.Schedule,
			MinConfidence:    w.cfg.Knowledge.BeadSynthesizer.MinConfidence,
			TargetLayer:      w.cfg.Knowledge.BeadSynthesizer.TargetLayer,
			MaxFactsPerCycle: w.cfg.Knowledge.BeadSynthesizer.MaxFactsPerCycle,
			VaultPath:        synthVaultPath,
			Org:              w.cfg.Project.Org,
			Repos:            w.cfg.Project.Repos,
			RetentionPolicy:  kRetention,
		}, w.logger, rawGH)

		if cleaned, err := w.beadSynth.CleanupVault(); err != nil {
			w.logger.Warn("vault cleanup failed", "error", err)
		} else if cleaned > 0 {
			w.logger.Info("cleaned up low-quality bead-synth facts", "removed", cleaned)
		}

		if w.cfg.Knowledge.BeadSynthesizer.IsEnabled() && w.knowledgeAPI != nil {
			w.beadSynth.StartBackground(w.ctx)
			w.logger.Info("bead-to-wiki synthesizer started",
				"schedule", w.cfg.Knowledge.BeadSynthesizer.Schedule,
				"target_layer", w.cfg.Knowledge.BeadSynthesizer.TargetLayer,
				"vault_path", synthVaultPath,
				"bead_stores", len(w.beadStores),
			)
		}
	}

	// Scheduled knowledge promotion (#5430). knowledge.curator.schedule used to
	// be parsed, defaulted to "daily", and never read. It now drives a real
	// sweep — but ONLY when knowledge.curator.enabled is explicitly true.
	// StartBackground is a no-op otherwise, and logs a notice if a schedule was
	// configured without the opt-in so the mismatch is visible rather than
	// silent. Do not replace the IsEnabled() guard with a schedule check: that
	// would enable unreviewed promotion on every hive that omits the key.
	if w.knowledgeAPI != nil && w.cfg.Knowledge.Curator.IsEnabled() {
		w.promotionScheduler = knowledge.NewPromotionScheduler(
			w.knowledgeAPI.Promoter(),
			curatorConfigFromHive(w.cfg.Knowledge.Curator),
			w.logger,
		)
		w.promotionScheduler.StartBackground(w.ctx)
	} else if w.cfg.Knowledge.Curator.Schedule != "" {
		w.logger.Info("knowledge.curator.schedule is set but scheduled promotion is disabled",
			"schedule", w.cfg.Knowledge.Curator.Schedule,
			"hint", "set knowledge.curator.enabled: true to opt in",
		)
	}

	// Open the graph store in a background goroutine. NewGraphStore acquires
	// a SQLite file lock that blocks if the old pod still holds it. Deferring
	// this lets the HTTP server start so the readiness probe passes, which
	// tells Kubernetes to terminate the old pod and release the lock.
	const graphStorePath = "/data/graph/knowledge.db"
	go func() {
		graphStore, graphErr := knowledge.NewGraphStore(graphStorePath, w.logger)
		if graphErr != nil {
			w.logger.Warn("failed to open knowledge graph store", "path", graphStorePath, "error", graphErr)
			return
		}
		w.logger.Info("knowledge graph store opened", "path", graphStorePath)
		if w.primer = w.sched.GetPrimer(); w.primer != nil {
			w.primer.SetGraphStore(graphStore)
		}
		if w.knowledgeAPI != nil {
			w.knowledgeAPI.SetGraphStore(graphStore)
			if w.primer = w.sched.GetPrimer(); w.primer != nil {
				w.knowledgeAPI.WireContext7Suggester(w.primer)
			}
		}
		if w.beadSynth != nil {
			w.beadSynth.SetGraphStore(graphStore)
		}
		if w.knowledgeAPI != nil {
			for _, ls := range w.knowledgeAPI.FileStores() {
				if n, err := graphStore.SyncFromFileStore(ls); err != nil {
					w.logger.Warn("graph sync failed", "store", ls.Name(), "error", err)
				} else if n > 0 {
					w.logger.Info("graph synced from vault", "store", ls.Name(), "triples", n)
				}
			}
		}
	}()

	go dashboard.StartWorkspaceCleanup(w.ctx, w.logger, w.dashSrv.GetAudit())

	if err := os.MkdirAll(nousSnapshotDir, 0o755); err != nil {
		w.logger.Warn("failed to create nous snapshot dir", "path", nousSnapshotDir, "error", err)
	}
	if err := os.MkdirAll(nousGovernorDir, 0o755); err != nil {
		w.logger.Warn("failed to create nous governor dir", "path", nousGovernorDir, "error", err)
	}
	w.nousState = loadNousState(w.logger)
	w.nousState.SnapshotDir = nousSnapshotDir

	w.inceptionEngine = knowledge.NewInceptionEngine("/data", w.knowledgeAPI, w.logger)
	w.sched.SetInception(w.inceptionEngine)

}
