package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/kubestellar/hive/v2/pkg/advisory"
	"github.com/kubestellar/hive/v2/pkg/hub"
	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/classify"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/dashboard"
	"github.com/kubestellar/hive/v2/pkg/discord"
	"github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/governor"
	"github.com/kubestellar/hive/v2/pkg/knowledge"
	"github.com/kubestellar/hive/v2/pkg/notify"
	"github.com/kubestellar/hive/v2/pkg/policies"
	"github.com/kubestellar/hive/v2/pkg/proxy"
	"github.com/kubestellar/hive/v2/pkg/scheduler"
	"github.com/kubestellar/hive/v2/pkg/snapshot"
	"github.com/kubestellar/hive/v2/pkg/tokens"
)

var (
	gitHash   = "unknown"
	gitShort  = "unknown"
	gitBranch = "unknown"
)

func main() {
	defaultConfig := "/etc/hive/hive.yaml"
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" {
		defaultConfig = envCfg
	}
	configPath := flag.String("config", defaultConfig, "path to hive.yaml config file")
	flag.Parse()
	dashboard.SetGitVersion(gitHash, gitShort)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if os.Getenv("HIVE_MODE") == "hub" {
		runHub(logger)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Reconfigure logger with rolling file output
	logger = setupLogger(cfg.Governor.Logging.Dir, cfg.Governor.Logging.MaxSizeMB,
		cfg.Governor.Logging.MaxAgeDays, cfg.Governor.Logging.MaxBackups,
		cfg.Governor.Logging.Compress, cfg.Governor.Logging.Level)
	slog.SetDefault(logger)

	// Load or generate a unique Hive ID for this instance
	cfg.HiveID = loadOrGenerateHiveID(logger)
	os.Setenv("HIVE_ID", cfg.HiveID)

	logger.Info("hive starting",
		"org", cfg.Project.Org,
		"repos", cfg.Project.Repos,
		"agents", len(cfg.Agents),
		"hive_id", cfg.HiveID,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	var ghClient *github.Client
	var appAuth *github.AppAuth
	if cfg.GitHub.AppID != 0 {
		keyFile := cfg.GitHub.KeyFile
		if envKey := os.Getenv("GH_APP_KEY_FILE"); envKey != "" {
			keyFile = envKey
		}
		if keyFile == "" {
			keyFile = "/secrets/gh-app-key.pem"
		}
		var err error
		appAuth, err = github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, keyFile, logger)
		if err != nil {
			logger.Error("failed to init GitHub App auth", "error", err)
			os.Exit(1)
		}
		logger.Info("using GitHub App authentication", "app_id", cfg.GitHub.AppID)
		ghClient = github.NewClientFromApp(appAuth, cfg.Project.Org, cfg.Project.Repos, logger)

		if cfg.GitHub.DocsInstallationID != 0 {
			docsAuth, err := github.NewAppAuthWithCache(
				cfg.GitHub.AppID, cfg.GitHub.DocsInstallationID,
				keyFile, github.DocsTokenCachePath, logger,
			)
			if err != nil {
				logger.Warn("failed to init docs org token", "error", err)
			} else {
				if _, err := docsAuth.Token(ctx); err != nil {
					logger.Warn("failed to generate initial docs org token", "error", err)
				} else {
					logger.Info("docs org token cached", "installation_id", cfg.GitHub.DocsInstallationID)
				}
				go func() {
					const docsTokenRefreshInterval = 45 * time.Minute
					ticker := time.NewTicker(docsTokenRefreshInterval)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							if _, err := docsAuth.Token(ctx); err != nil {
								logger.Warn("docs token refresh failed", "error", err)
							}
						}
					}
				}()
			}
		}
	} else {
		ghToken := cfg.GitHub.Token
		if ghToken == "" {
			ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
		}
		if ghToken == "" {
			logger.Error("no GitHub token configured (set github.token or github.app_id in config)")
			os.Exit(1)
		}
		ghClient = github.NewClient(ghToken, cfg.Project.Org, cfg.Project.Repos, logger)
	}
	if len(cfg.Governor.Labels.Exempt) > 0 {
		ghClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
	}
	// Load user token for advisory posting (comments on issues as the logged-in user)
	var userGHClient atomic.Pointer[github.Client]
	if tokenData, err := os.ReadFile("/data/gh-user-token"); err == nil {
		userToken := strings.TrimSpace(string(tokenData))
		if userToken != "" {
			if username, err := github.ValidateToken(userToken); err == nil {
				uc := github.NewClient(userToken, cfg.Project.Org, cfg.Project.Repos, logger)
				userGHClient.Store(uc)
				logger.Info("user GitHub token loaded for advisory posting", "username", username)
			} else {
				logger.Warn("persisted user token is invalid or expired", "error", err)
			}
		}
	}

	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	sched := scheduler.New(cfg, logger)

	// Restore sparkline history from disk so it survives container restarts
	const sparklinePath = "/data/sparkline-history.json"
	if sparkData, err := os.ReadFile(sparklinePath); err == nil {
		var snapshots []governor.EvalSnapshot
		if err := json.Unmarshal(sparkData, &snapshots); err == nil && len(snapshots) > 0 {
			gov.SeedEvalHistory(snapshots)
			logger.Info("sparkline history restored", "entries", len(snapshots))
		}
	}

	// Restore mode history from disk so the mode timeline survives container restarts
	const modeHistoryPath = "/data/mode-history.json"
	if modeData, err := os.ReadFile(modeHistoryPath); err == nil {
		var changes []governor.ModeChange
		if err := json.Unmarshal(modeData, &changes); err == nil && len(changes) > 0 {
			gov.SeedModeHistory(changes)
			logger.Info("mode history restored", "entries", len(changes))
		}
	}

	// Restore token sparkline history from disk so token charts survive container restarts
	const tokenSparklinePath = "/data/token-sparkline-history.json"
	var pendingTokenSeed []dashboard.TokenSparklineEntry
	if tokenSparkData, err := os.ReadFile(tokenSparklinePath); err == nil {
		if err := json.Unmarshal(tokenSparkData, &pendingTokenSeed); err == nil && len(pendingTokenSeed) > 0 {
			logger.Info("token sparkline history loaded", "entries", len(pendingTokenSeed))
		}
	}

	if cfg.Knowledge.Enabled {
		layers := convertKnowledgeLayers(cfg.Knowledge.Layers)
		primerCfg := knowledge.PrimerConfig{
			MaxFacts:      cfg.Knowledge.Primer.MaxFacts,
			Priority:      cfg.Knowledge.Primer.Priority,
			MergeStrategy: cfg.Knowledge.Primer.MergeStrategy,
		}
		primer := knowledge.NewPrimer(layers, primerCfg, logger)
		sched.SetPrimer(primer)
		logger.Info("knowledge primer enabled",
			"layers", len(cfg.Knowledge.Layers),
			"max_facts", primerCfg.MaxFacts,
		)
	}

	notifier := notify.New(cfg.Notifications, logger)
	notifier.SetHiveID(cfg.HiveID)
	acmmLevel := inferACMMLevel(cfg)
	githubAppRequired := false

	// Find or create the pinned advisory issue. Any level can have advisory
	// agents whose findings should be posted to this issue.
	advisoryIssues := map[string]int{}
	if acmmLevel > 0 {
		primaryRepo := cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
			primaryRepo = cfg.Project.Repos[0]
		}
		if primaryRepo != "" {
			num, err := ghClient.EnsureAdvisoryIssue(ctx, primaryRepo)
			if err != nil {
				logger.Error("failed to ensure advisory issue", "repo", primaryRepo, "error", err)
				if strings.Contains(err.Error(), "403") {
					githubAppRequired = true
					logger.Warn("GitHub App not installed — issue creation returned 403, setting githubAppRequired flag")
				}
			} else {
				advisoryIssues[primaryRepo] = num
				os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num))
				logger.Info("advisory issue ready", "repo", primaryRepo, "number", num)
			}
		}
	}

	advisoryStore := advisory.NewStore()

	policyDir := cfg.Policies.LocalDir
	if policyDir == "" {
		policyDir = "/data/policies"
	}
	if cfg.Policies.Path != "" {
		policyDir = policyDir + "/" + cfg.Policies.Path
	}

	// Write brainstorm policy to disk so the agent can find it.
	// The policy is embedded in the binary but the agent searches the filesystem.
	brainstormPolicyDir := policyDir
	if brainstormPolicyDir == "" {
		brainstormPolicyDir = "/data/policies/examples/kubestellar/agents"
	}
	os.MkdirAll(brainstormPolicyDir, 0o755)
	if policyData, err := policies.DefaultPolicies.ReadFile("defaults/brainstorm-advisory.md"); err == nil {
		policyPath := filepath.Join(brainstormPolicyDir, "brainstorm-advisory.md")
		// Always overwrite — the embedded policy may have been updated
		// (e.g., inception reaping guard added in bug #113 fix).
		if err := os.WriteFile(policyPath, policyData, 0o644); err != nil {
			logger.Warn("failed to write brainstorm policy", "path", policyPath, "error", err)
		} else {
			logger.Info("wrote brainstorm policy to disk", "path", policyPath)
		}
	}

	projectCtx := agent.ProjectContext{
		Org:        cfg.Project.Org,
		Repos:      cfg.Project.Repos,
		ACMMLevel:  acmmLevel,
		PRsAllowed: cfg.Project.PRsAllowed(),
		PolicyDir:  policyDir,
	}
	agentMgr := agent.NewManager(cfg.EnabledAgents(), logger, projectCtx)

	const statePath = "/data/hive-state.json"
	var savedIssueCosts map[string]int64
	saved, stateErr := snapshot.LoadState(statePath, logger)
	if stateErr != nil {
		logger.Warn("failed to load persisted state", "error", stateErr)
	} else if saved != nil {
		savedIssueCosts = saved.IssueCosts
		for name, as := range saved.Agents {
			if _, inConfig := cfg.Agents[name]; !inConfig {
				logger.Info("skipping saved state for agent not in config", "agent", name)
				continue
			}
			if as.Paused {
				reason := as.PausedReason
				if reason == "" {
					reason = "persisted pause state"
				}
				trigger := as.PausedTrigger
				if trigger == "" {
					trigger = "state-restore"
				}
				_ = agentMgr.Pause(name, trigger, reason)
				if as.PausedAt != nil {
					agentMgr.SeedPauseState(name, *as.PausedAt, trigger, reason)
				}
			}
			if as.PinnedCLI != "" {
				_ = agentMgr.PinCLI(name, as.PinnedCLI)
			}
			if as.PinnedModel != "" {
				_ = agentMgr.PinModel(name, as.PinnedModel)
			}
			if as.ModelOverride != "" {
				agentMgr.SetModelOverride(name, as.ModelOverride)
				logger.Info("model override restored", "agent", name, "model", as.ModelOverride)
			}
			if as.BackendOverride != "" {
				agentMgr.SetBackendOverride(name, as.BackendOverride)
				logger.Info("backend override restored", "agent", name, "backend", as.BackendOverride)
			}
			if as.RestartCount > 0 {
				agentMgr.SeedRestartCount(name, as.RestartCount)
			}
			if as.LastKick != nil {
				agentMgr.SeedLastKick(name, *as.LastKick)
			}
			if len(as.KickHistory) > 0 {
				records := make([]agent.KickRecord, len(as.KickHistory))
				for i, ke := range as.KickHistory {
					records[i] = agent.KickRecord{Timestamp: ke.Timestamp, Agent: ke.Agent, Snippet: ke.Snippet}
				}
				agentMgr.SeedKickHistory(name, records)
			}
			if agentCfg, ok := cfg.Agents[name]; ok {
				if as.DisplayName != "" && agentCfg.DisplayName == "" {
					agentCfg.DisplayName = as.DisplayName
				}
				if as.Description != "" && agentCfg.Description == "" {
					agentCfg.Description = as.Description
				}
				if as.Enabled != nil {
					agentCfg.Enabled = *as.Enabled
				}
				if as.ClearOnKick != nil {
					agentCfg.ClearOnKick = *as.ClearOnKick
				}
				if as.StaleTimeout != nil {
					agentCfg.StaleTimeout = *as.StaleTimeout
				}
				if as.RestartStrategy != "" {
					agentCfg.RestartStrategy = as.RestartStrategy
				}
				cfg.Agents[name] = agentCfg
				_ = agentMgr.UpdateConfig(name, agentCfg)
			}
		}
		if saved.BudgetLimit > 0 {
			gov.SetBudgetLimit(saved.BudgetLimit)
		}
		if len(saved.BudgetIgnored) > 0 {
			gov.SetBudgetIgnored(saved.BudgetIgnored)
		}
		if len(saved.CadenceOverrides) > 0 {
			for modeName, agentCadences := range saved.CadenceOverrides {
				mode, ok := cfg.Governor.Modes[modeName]
				if !ok {
					continue
				}
				if mode.Cadences == nil {
					mode.Cadences = make(map[string]string)
				}
				for agentName, cadence := range agentCadences {
					mode.Cadences[agentName] = cadence
				}
				cfg.Governor.Modes[modeName] = mode
			}
			logger.Info("cadence overrides restored", "modes", len(saved.CadenceOverrides))
		}
		if saved.GovernorMode != "" {
			gov.SetMode(governor.Mode(saved.GovernorMode))
			logger.Info("governor mode restored", "mode", saved.GovernorMode)
		}
		if len(saved.LastKicks) > 0 {
			gov.SeedLastKicks(saved.LastKicks)
			logger.Info("governor last kicks restored", "agents", len(saved.LastKicks))
		}
		if saved.BudgetSpend > 0 || !saved.BudgetResetAt.IsZero() || len(saved.BudgetByAgent) > 0 {
			gov.SeedBudget(saved.BudgetSpend, saved.BudgetByAgent, saved.BudgetByModel, saved.BudgetResetAt)
			logger.Info("budget state restored", "spend", saved.BudgetSpend, "reset_at", saved.BudgetResetAt)
		}
		if len(saved.KickHistory) > 0 {
			records := make([]governor.KickRecord, len(saved.KickHistory))
			for i, ke := range saved.KickHistory {
				records[i] = governor.KickRecord{Timestamp: ke.Timestamp, Agent: ke.Agent}
			}
			gov.SeedKickHistory(records)
			logger.Info("kick history restored", "entries", len(records))
		}
		if !saved.LastEval.IsZero() {
			gov.SeedLastEval(saved.LastEval)
		}
		if saved.ACMMLevel != nil && cfg.ACMMLevel == nil {
			cfg.ACMMLevel = saved.ACMMLevel
			logger.Info("ACMM level restored", "level", *saved.ACMMLevel)
		}
		if saved.ConfigOverrides != nil {
			applyConfigOverrides(cfg, saved.ConfigOverrides)
			ghClient.SetRepos(cfg.Project.Repos)
			if len(cfg.Governor.Labels.Exempt) > 0 {
				ghClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
			}
			if uc := userGHClient.Load(); uc != nil {
				uc.SetRepos(cfg.Project.Repos)
			}
			logger.Info("migrated config overrides from state to hive.yaml",
				"repos", cfg.Project.Repos)

			// Write merged config to hive.yaml so overrides become the base config
			if err := cfg.Save(); err != nil {
				logger.Error("failed to save migrated config", "error", err)
			}

			// Strip config_overrides from state and re-save
			saved.ConfigOverrides = nil
			if err := snapshot.SaveState(statePath, saved, logger); err != nil {
				logger.Error("failed to re-save state after migration", "error", err)
			}
		}
	}

	if gov.GetBudget().WeeklyLimit == 0 && cfg.Governor.Budget.TotalTokens > 0 {
		gov.SetBudgetLimit(cfg.Governor.Budget.TotalTokens)
	}

	dashSrv := dashboard.NewServerWithAuth(cfg.Dashboard.Port, cfg.Dashboard.AuthToken, logger)

	// Seed token sparkline history now that the dashboard server exists
	if len(pendingTokenSeed) > 0 {
		dashSrv.SeedTokenSparklineHistory(pendingTokenSeed)
		logger.Info("token sparkline history restored", "entries", len(pendingTokenSeed))
	}

	beadStores := make(map[string]*beads.Store)
	for name, agentCfg := range cfg.EnabledAgents() {
		store, err := beads.NewStore(agentCfg.BeadsDir)
		if err != nil {
			logger.Warn("failed to init beads store", "agent", name, "error", err)
			continue
		}
		store.SetHiveID(cfg.HiveID)
		beadStores[name] = store
		logger.Info("beads store initialized", "agent", name, "count", store.Count())
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
			if _, exists := beadStores[name]; exists {
				continue // already loaded from config
			}
			agentBeadsDir := filepath.Join(beadsRootDir, name)
			beadsFile := filepath.Join(agentBeadsDir, "beads.json")
			if _, statErr := os.Stat(beadsFile); statErr != nil {
				continue // no beads.json in this directory
			}
			store, err := beads.NewStore(agentBeadsDir)
			if err != nil {
				logger.Warn("failed to load orphan beads store", "agent", name, "error", err)
				continue
			}
			store.SetHiveID(cfg.HiveID)
			beadStores[name] = store
			logger.Info("orphan beads store loaded from disk", "agent", name, "count", store.Count())
		}
	}

	initAgentConfigDrivenSystems(cfg)

	tokenCollector := tokens.NewCollector(cfg.Data.MetricsDir, logger)
	tokenCollector.SetClaudeSessionsDir(cfg.Data.ClaudeSessionsDir)
	tokenCollector.SetCopilotSessionsDir(cfg.Data.CopilotSessionsDir)
	if len(savedIssueCosts) > 0 {
		tokenCollector.SeedIssueCosts(savedIssueCosts)
		logger.Info("issue costs restored", "entries", len(savedIssueCosts))
	}
	tokenStop := make(chan struct{})
	go tokenCollector.Start(tokenStop)
	defer close(tokenStop)

	badgeURL := os.Getenv("HIVE_COVERAGE_BADGE_URL")
	if badgeURL == "" {
		badgeURL = "https://gist.githubusercontent.com/clubanderson/b9a9ae8469f1897a22d5a40629bc1e82/raw/coverage-badge.json"
	}
		primaryRepo := cfg.Project.PrimaryRepo
	if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
		primaryRepo = cfg.Project.Repos[0]
	}
	metricsCollector := dashboard.NewMetricsCollector(ghClient, cfg.Project.Org, primaryRepo, badgeURL, cfg.Project.AIAuthor, cfg.Project.Name, logger)
	go metricsCollector.Start(ctx)

	var lastActionable atomic.Pointer[github.ActionableResult]
	refreshDashboard := func() {
		actionable := lastActionable.Load()
		govState := gov.GetState()
		agentStatuses := agentMgr.AllStatuses()
		payload := dashboard.BuildFrontendStatus(
			govState,
			actionable,
			agentStatuses,
			cfg,
			tokenCollector,
			gov,
			beadStores,
			ghClient,
			ctx,
			metricsCollector,
		)
		if d := dashSrv.GetAdvisoryDigest(); d != nil {
			payload.AdvisoryDigest = d
		}
		dashSrv.UpdateStatus(payload)
	}

	const cachedActionablePath = "/data/last-actionable.json"
	if data, err := os.ReadFile(cachedActionablePath); err == nil {
		var cached github.ActionableResult
		if err := json.Unmarshal(data, &cached); err == nil {
			lastActionable.Store(&cached)
			gov.SeedQueueState(cached.Issues.Count, cached.PRs.Count, cached.Hold.Total, cached.Issues.SLAViolations)
			refreshDashboard()
			logger.Info("restored cached actionable data", "issues", cached.Issues.Count, "prs", cached.PRs.Count, "age", time.Since(cached.GeneratedAt).Round(time.Second))
		}
	}

	var knowledgeAPI *knowledge.KnowledgeAPI
	if cfg.Knowledge.Enabled {
		layers := convertKnowledgeLayers(cfg.Knowledge.Layers)
		knowledgeAPI = knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{
			Enabled: cfg.Knowledge.Enabled,
			Engine:  cfg.Knowledge.Engine,
		}, logger)
	}

	// Auto-connect configured vaults and start git-sync for Obsidian Git integration
	gitSyncer := knowledge.NewGitSyncer(logger)
	const seedDataDir = "/opt/hive/seed-data/wiki"
	for _, vc := range cfg.Knowledge.Vaults {
		if err := knowledge.InitVaultRepo(vc.Path, logger); err != nil {
			logger.Warn("failed to init vault directory", "name", vc.Name, "path", vc.Path, "error", err)
			continue
		}
		if err := knowledge.SeedVaultContent(vc.Path, seedDataDir, logger); err != nil {
			logger.Warn("failed to seed vault content", "name", vc.Name, "error", err)
		}
		if knowledgeAPI != nil {
			if err := knowledgeAPI.ConnectVault(vc.Path, vc.Name); err != nil {
				logger.Warn("failed to connect vault", "name", vc.Name, "path", vc.Path, "error", err)
				continue
			}
			logger.Info("vault auto-connected", "name", vc.Name, "path", vc.Path, "auto_index", vc.AutoIndex)
		}
		if vc.GitSync {
			// Find the store we just connected so the syncer can trigger reindex
			for _, vi := range knowledgeAPI.Vaults() {
				if vi.Name == vc.Name {
					// Re-fetch the FileStore by connecting info — the syncer needs it
					// to call Reindex() after each pull
					store := knowledgeAPI.GetVaultStore(vc.Path)
					if store != nil {
						gitSyncer.Add(vc.Name, vc.Path, store)
					}
					break
				}
			}
		}
	}

	// Auto-connect configured git sources (remote repos indexed as knowledge)
	for _, gsc := range cfg.Knowledge.GitSources {
		if knowledgeAPI == nil {
			// Knowledge not enabled but git sources configured — auto-enable
			knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
				Enabled: true,
				Engine:  "file",
			}, logger)
			logger.Info("auto-enabled knowledge API for git sources")
		}
		gsConfig := knowledge.GitSourceConfig{
			Name:    gsc.Name,
			URL:     gsc.URL,
			Branch:  gsc.Branch,
			Subpath: gsc.Subpath,
			Layer:   knowledge.LayerType(gsc.Layer),
		}
		if err := knowledgeAPI.ConnectGitSource(ctx, gsConfig); err != nil {
			logger.Warn("failed to connect git source",
				"name", gsc.Name,
				"url", gsc.URL,
				"subpath", gsc.Subpath,
				"error", err,
			)
		} else {
			logger.Info("git source connected",
				"name", gsc.Name,
				"url", gsc.URL,
				"subpath", gsc.Subpath,
				"layer", gsc.Layer,
			)
			// Register the FileStore with the scheduler's primer so agents
			// get primed with facts from this git source during kicks.
			if primer := sched.GetPrimer(); primer != nil {
				for _, gs := range knowledgeAPI.GitSources() {
					if gs.Name == gsc.Name && gs.Ready {
						store := knowledgeAPI.GetGitSourceStore(gsc.Name)
						if store != nil {
							primer.AddFileStore(gsc.Name, store, knowledge.LayerType(gsc.Layer))
						}
						break
					}
				}
			}
		}
	}

	go gitSyncer.Start(ctx)

	os.MkdirAll(nousSnapshotDir, 0o755)
	os.MkdirAll(nousGovernorDir, 0o755)
	nousState := loadNousState(logger)
	nousState.SnapshotDir = nousSnapshotDir

	// Auto-enable knowledge API for inception if not already configured.
	// Inception needs it to connect the inception wiki vault.
	if knowledgeAPI == nil {
		knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
			Enabled: true,
			Engine:  "file",
		}, logger)
		logger.Info("created file-based knowledge API for inception wiki")
	}
	inceptionEngine := knowledge.NewInceptionEngine("/data", knowledgeAPI, logger)
	sched.SetInception(inceptionEngine)

	// Brainstorm is on-demand only. Only restart with bootstrap during
	// capture phase — structure/scaffold phases don't need a fresh kick
	// and restarting would revert the phase back to capture.
	// Skip stale inceptions (> 10 min old) — these are leftovers from
	// previous runs that would interfere with new inceptions.
	const staleInceptionThreshold = 10 * time.Minute
	if state := inceptionEngine.GetState(); state != nil &&
		state.Phase != knowledge.PhaseComplete &&
		state.Phase != knowledge.PhaseScaffold {
		if time.Since(state.StartedAt) < staleInceptionThreshold {
			msg := sched.BuildAgentMessage("brainstorm", nil, nil)
			if err := agentMgr.RestartWithBootstrap(ctx, "brainstorm", msg); err != nil {
				logger.Warn("failed to resume brainstorm for active inception", "error", err)
			} else {
				logger.Info("brainstorm resumed for active inception", "phase", state.Phase)
			}
		} else {
			logger.Info("skipping stale inception resume — resetting",
				"phase", state.Phase,
				"age", time.Since(state.StartedAt).Round(time.Second),
			)
			_ = inceptionEngine.Reset()
			if err := agentMgr.Pause("brainstorm", "startup", "stale inception cleared — on-demand only"); err != nil {
				logger.Debug("brainstorm pause on startup", "error", err)
			}
		}
	} else {
		if err := agentMgr.Pause("brainstorm", "startup", "on-demand agent — triggered by inception only"); err != nil {
			logger.Debug("brainstorm pause on startup", "error", err)
		}
	}

	dashSrv.RegisterAPI(&dashboard.Dependencies{
		Config:           cfg,
		AgentMgr:         agentMgr,
		Governor:         gov,
		GHClient:         ghClient,
		GHAppAuth:        appAuth,
		Tokens:           tokenCollector,
		Knowledge:        knowledgeAPI,
		Inception:        inceptionEngine,
		Nous:             nousState,
		Scheduler:        sched,
		MetricsCollector: metricsCollector,
		BeadStores:       beadStores,
		Logger:           logger,
		Ctx:              ctx,
		RefreshFunc:      refreshDashboard,
		PersistFunc: func() {
			persistState(agentMgr, gov, cfg, tokenCollector, statePath, logger, dashSrv)
		},
		ReInitFunc: func() {
			initAgentConfigDrivenSystems(cfg)
		},
		SetUserClient: func(token string) {
			uc := github.NewClient(token, cfg.Project.Org, cfg.Project.Repos, logger)
			userGHClient.Store(uc)
			logger.Info("user GitHub client updated via device flow")
		},
		EnumerateFunc: func() {
			runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, &userGHClient, nil, logger)
		},
		AdvisoryResetFunc: func(newPrimaryRepo string) {
			logger.Info("advisory reset: primary repo changed, creating new advisory issue", "repo", newPrimaryRepo)
			if ghClient != nil {
				num, err := ghClient.EnsureAdvisoryIssue(ctx, newPrimaryRepo)
				if err != nil {
					logger.Error("failed to create advisory issue on new primary repo", "repo", newPrimaryRepo, "error", err)
					if strings.Contains(err.Error(), "403") {
						dashSrv.SetGitHubAppRequired(true)
					}
				} else {
					advisoryIssues[newPrimaryRepo] = num
					os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num))
					dashSrv.SetGitHubAppRequired(false)
					logger.Info("advisory issue ready on new primary repo", "repo", newPrimaryRepo, "number", num)
				}
			}
		},
	})

	dashSrv.SetGitHubAppRequired(githubAppRequired)

	if brainstormBeads, ok := beadStores["brainstorm"]; ok {
		inceptionWatcher := dashboard.NewInceptionWatcher(brainstormBeads, inceptionEngine, sched, agentMgr, gov, logger)
		go inceptionWatcher.Run(ctx)
	}

	if saved == nil {
		if levelStr := os.Getenv("HIVE_LEVEL"); levelStr != "" {
			const maxACMMLevel = 6
			level, err := strconv.Atoi(levelStr)
			if err != nil || level < 1 || level > maxACMMLevel {
				logger.Warn("invalid HIVE_LEVEL, skipping auto-apply", "value", levelStr)
			} else {
				logger.Info("first start detected, auto-applying ACMM pack", "level", level)
				result, err := dashSrv.ApplyPack(level)
				if err != nil {
					logger.Error("failed to auto-apply ACMM pack", "level", level, "error", err)
				} else {
					logger.Info("ACMM pack auto-applied",
						"level", level,
						"name", result.Name,
						"created", result.Created,
						"skipped", result.Skipped,
						"paused", result.Paused,
						"resumed", result.Resumed,
					)
				}
			}
		}
	} else {
		// Config file is authoritative on restarts; HIVE_LEVEL env var is
		// only a fallback for initial provisioning when no level is persisted.
		const maxACMMLevel = 6
		level := 0
		if cfg.ACMMLevel != nil && *cfg.ACMMLevel >= 1 && *cfg.ACMMLevel <= maxACMMLevel {
			level = *cfg.ACMMLevel
		} else if saved.ACMMLevel != nil && *saved.ACMMLevel >= 1 && *saved.ACMMLevel <= maxACMMLevel {
			level = *saved.ACMMLevel
		} else if levelStr := os.Getenv("HIVE_LEVEL"); levelStr != "" {
			if parsed, err := strconv.Atoi(levelStr); err == nil && parsed >= 1 && parsed <= maxACMMLevel {
				level = parsed
			} else {
				logger.Warn("invalid HIVE_LEVEL, skipping auto-apply", "value", levelStr)
			}
		}
		if level > 0 {
			action := "merging pack updates"
			if saved.ACMMLevel == nil || *saved.ACMMLevel != level {
				action = "re-applying pack (level changed)"
			}
			logger.Info("audit: "+action, "level", level, "saved_level", saved.ACMMLevel, "trigger", "startup")
			result, err := dashSrv.ApplyPack(level)
			if err != nil {
				logger.Error("failed to apply ACMM pack", "level", level, "error", err)
			} else {
				logger.Info("ACMM pack applied on startup",
					"level", level,
					"name", result.Name,
					"created", result.Created,
					"updated", result.Updated,
					"skipped", result.Skipped,
					"paused", result.Paused,
					"resumed", result.Resumed,
				)
			}
		}
	}

	if cfg.Policies.Repo != "" {
		localDir := cfg.Policies.LocalDir
		if localDir == "" {
			localDir = "/data/policies"
		}
		watcher := policies.NewWatcher(
			cfg.Policies.Repo,
			cfg.Policies.Branch,
			cfg.Policies.Path,
			localDir,
			cfg.Policies.PollInterval,
			logger,
		)
		if err := watcher.Start(ctx); err != nil {
			logger.Warn("policy watcher failed to start", "error", err)
		}
	}

	// Watch hive.yaml for external changes and reload config when modified
	configWatcher := config.NewWatcher(*configPath, func(newCfg *config.Config) {
		// Preserve runtime-only fields that are not in the YAML
		newCfg.HiveID = cfg.HiveID

		// Preserve ACMM level from the agent manager — it is the
		// authoritative source. The file may have a stale value if
		// a watcher reload races with a level-switch saveConfig().
		if cfg.ACMMLevel != nil {
			newCfg.ACMMLevel = cfg.ACMMLevel
		}

		// Swap the in-memory config pointer contents
		*cfg = *newCfg

		// Re-sync subsystems that cache config values
		ghClient.SetRepos(cfg.Project.Repos)
		if uc := userGHClient.Load(); uc != nil {
			uc.SetRepos(cfg.Project.Repos)
		}
		gov.UpdateConfig(cfg.Governor)
		initAgentConfigDrivenSystems(cfg)
		refreshDashboard()
	}, logger)
	dashSrv.SetSkipReloadFunc(configWatcher.SkipNext)
	go configWatcher.Start(ctx)

	githubProxy, err := proxy.NewGitHubProxy(logger, cfg.Project.Org, cfg.Project.Repos)
	if err != nil {
		logger.Error("failed to create github proxy", "error", err)
	} else {
		dashboard.SetProxyViolationsProvider(githubProxy.Violations)

		vllmEndpoint := envOrDefault("HIVE_VLLM_ENDPOINT", "http://vllm-svc.hive.svc.cluster.local:8000")
		llmdEndpoint := envOrDefault("HIVE_LLMD_ENDPOINT", "http://llm-d-epp.hive.svc.cluster.local:8000")
		agentMgr.SetInferenceCallbacks(
			func(agentName, backend, model string) {
				endpoint := vllmEndpoint
				if backend == "llm-d" {
					endpoint = llmdEndpoint
				}
				githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
					Backend:  backend,
					Endpoint: endpoint,
					Model:    model,
				})
			},
			func(agentName string) {
				githubProxy.ClearInferenceRoute(agentName)
			},
		)

		go func() {
			if err := githubProxy.Start(); err != nil {
				logger.Error("github proxy failed", "error", err)
			}
		}()
		go func() {
			if err := githubProxy.StartInferenceTranslator(); err != nil {
				logger.Error("inference translation server failed", "error", err)
			}
		}()
		logger.Info("github proxy started", "addr", githubProxy.ListenAddr())
	}

	go func() {
		if err := dashSrv.Start(); err != nil {
			logger.Error("dashboard server failed", "error", err)
		}
	}()

	if cfg.Notifications.Discord != nil && cfg.Notifications.Discord.BotToken != "" && cfg.Notifications.Discord.ChannelID != "" {
		discordBot := discord.NewBot(discord.Config{
			Token:          cfg.Notifications.Discord.BotToken,
			ChannelID:      cfg.Notifications.Discord.ChannelID,
			DashboardURL:   fmt.Sprintf("http://localhost:%d", cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
		}, logger)
		var agentNameList []string
		for name := range cfg.EnabledAgents() {
			agentNameList = append(agentNameList, name)
		}
		discordBot.SetAgentNames(agentNameList)
		if err := discordBot.Start(ctx); err != nil {
			logger.Warn("discord bot failed to start", "error", err)
		} else {
			logger.Info("discord bot started", "channel", cfg.Notifications.Discord.ChannelID)
		}
	}

	onDemandFromPack := config.OnDemandAgentsFromPacks()
	if len(onDemandFromPack) > 0 {
		logger.Info("on-demand agents from pack definitions", "agents", onDemandFromPack)
	}
	const agentLaunchDelaySec = 15
	agentIndex := 0
	for name, ac := range cfg.EnabledAgents() {
		isOnDemand := ac.OnDemand || onDemandFromPack[name]
		if isOnDemand {
			logger.Info("skipping on-demand agent at startup", "name", name)
			continue
		}
		if agentIndex > 0 {
			logger.Info("staggering agent launch", "name", name, "delay_sec", agentLaunchDelaySec)
			time.Sleep(time.Duration(agentLaunchDelaySec) * time.Second)
		}
		logger.Info("audit: starting agent", "name", name, "trigger", "startup")
		if err := agentMgr.Start(ctx, name); err != nil {
			logger.Warn("failed to start agent", "name", name, "error", err)
		}
		agentIndex++
	}

	// Start hub heartbeat push if configured (env var or config)
	hubURL := cfg.Hub.URL
	if envHub := os.Getenv("HIVE_HUB_URL"); envHub != "" {
		hubURL = envHub
		cfg.Hub.Enabled = true
		cfg.Hub.URL = envHub
	}
	if cfg.Hub.Enabled && hubURL != "" {
		go hub.StartHeartbeat(ctx, hubURL, func() *hub.HeartbeatPayload {
			if !cfg.Hub.Enabled {
				return nil
			}
			statuses := agentMgr.AllStatuses()
			govState := gov.GetState()
			agents := make([]hub.AgentSummary, 0, len(statuses))
			for name, proc := range statuses {
				agents = append(agents, hub.AgentSummary{Name: name, State: string(proc.State)})
			}
			acmmLvl := 0
			if cfg.ACMMLevel != nil {
				acmmLvl = *cfg.ACMMLevel
			}
			return &hub.HeartbeatPayload{
				HiveID:      cfg.HiveID,
				Org:         cfg.Project.Org,
				Repos:       cfg.Project.Repos,
				PrimaryRepo: cfg.Project.PrimaryRepo,
				ACMMLevel:   acmmLvl,
				Agents:      agents,
				Governor:    hub.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs},
				Contributors: func() hub.ContributorSummary {
					reg, active := dashSrv.ContributorSummary()
					return hub.ContributorSummary{Registered: reg, Active: active}
				}(),
				Leaderboard: func() []hub.LeaderboardEntry {
					lb := dashSrv.LeaderboardForHub()
					out := make([]hub.LeaderboardEntry, len(lb))
					for i, e := range lb {
						out[i] = hub.LeaderboardEntry{
							GitHubUsername:  e.GitHubUsername,
							AvatarURL:      e.AvatarURL,
							TrustTier:      e.TrustTier,
							TasksCompleted: e.TasksCompleted,
							TasksFailed:    e.TasksFailed,
							Active:         e.Active,
							CurrentTask:    e.CurrentTask,
						}
					}
					return out
				}(),
				Owner: func() string {
					if td, err := os.ReadFile("/data/gh-user-token"); err == nil {
						tok := strings.TrimSpace(string(td))
						if tok != "" {
							if u, err := github.ValidateToken(tok); err == nil {
								return u.Login
							}
						}
					}
					return ""
				}(),
				Health:       dashSrv.HealthSummary(),
				DashboardURL: func() string {
					if cfg.Hub.DashboardURL != "" {
						return cfg.Hub.DashboardURL
					}
					return fmt.Sprintf("http://localhost:%d", cfg.Dashboard.Port)
				}(),
				SnapshotURL:  cfg.Hub.SnapshotURL,
				IsPublic:     cfg.Hub.IsPublic,
				Version:           "3.0.0",
				GitHash:           gitShort,
				GitBranch:         gitBranch,
				GitHubAppRequired: dashSrv.IsGitHubAppRequired(),
			}
		}, time.Duration(cfg.Governor.EvalIntervalS)*time.Second, logger)

		go hub.StartTaskStatusPush(ctx, hubURL, func() *hub.TaskStatusPayload {
			reg, active := dashSrv.ContributorSummary()
			lb := dashSrv.LeaderboardForHub()
			out := make([]hub.LeaderboardEntry, len(lb))
			for i, e := range lb {
				out[i] = hub.LeaderboardEntry{
					GitHubUsername:  e.GitHubUsername,
					AvatarURL:      e.AvatarURL,
					TrustTier:      e.TrustTier,
					TasksCompleted: e.TasksCompleted,
					TasksFailed:    e.TasksFailed,
					Active:         e.Active,
					CurrentTask:    e.CurrentTask,
				}
			}
			return &hub.TaskStatusPayload{
				HiveID:       cfg.HiveID,
				Leaderboard:  out,
				Contributors: hub.ContributorSummary{Registered: reg, Active: active},
			}
		}, logger)
	}

	logger.Info("entering governor loop", "interval_seconds", cfg.Governor.EvalIntervalS)
	lastEvalInterval := cfg.Governor.EvalIntervalS
	ticker := time.NewTicker(time.Duration(cfg.Governor.EvalIntervalS) * time.Second)
	defer ticker.Stop()

	var agentTicker *time.Ticker
	if cfg.Dashboard.AgentPollIntervalS > 0 {
		agentTicker = time.NewTicker(time.Duration(cfg.Dashboard.AgentPollIntervalS) * time.Second)
		defer agentTicker.Stop()
		logger.Info("fast agent status enabled", "interval_seconds", cfg.Dashboard.AgentPollIntervalS)
	}

	dashSrv.MarkReady()

	const cliStartupDelay = 10 * time.Second
	logger.Info("waiting for CLI startup before first eval", "delay", cliStartupDelay)
	select {
	case <-time.After(cliStartupDelay):
	case <-ctx.Done():
		return
	}

	gov.ClearLastKicks()
	logger.Info("cleared last kicks for startup — all eligible agents will be kicked on first eval")
	runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, &userGHClient, nil, logger)
	persistState(agentMgr, gov, cfg, tokenCollector, statePath, logger, dashSrv)

	agentTickCh := func() <-chan time.Time {
		if agentTicker != nil {
			return agentTicker.C
		}
		return nil
	}()

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down, persisting state")
			persistState(agentMgr, gov, cfg, tokenCollector, statePath, logger, dashSrv)
			return
		case <-ticker.C:
			restarted := agentMgr.CheckAndRestartCrashedAgents(ctx)
			// If brainstorm crashed during inception, re-kick via SendKick.
			// SendKick waits for the CLI to be ready and sends the message
			// If brainstorm crashed during inception, re-kick with bootstrap.
			// The table parser in the watcher will catch questions from the
			// agent's output even if bd create doesn't execute.
			for _, name := range restarted {
				if name == "brainstorm" && inceptionEngine != nil {
					if state := inceptionEngine.GetState(); state != nil && state.Phase == knowledge.PhaseCapture {
						msg := sched.BuildAgentMessage("brainstorm", nil, sched.GetLastActionable())
						if err := agentMgr.RestartWithBootstrap(ctx, "brainstorm", msg); err != nil {
							logger.Warn("inception re-kick after crash failed", "error", err)
						} else {
							logger.Info("brainstorm re-kicked after crash", "phase", state.Phase)
						}
						gov.RecordKick("brainstorm")
					}
				}
			}
			runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, &userGHClient, restarted, logger)
			persistState(agentMgr, gov, cfg, tokenCollector, statePath, logger, dashSrv)
			if cfg.Governor.EvalIntervalS != lastEvalInterval && cfg.Governor.EvalIntervalS > 0 {
				logger.Info("eval interval changed, resetting ticker",
					"from", lastEvalInterval, "to", cfg.Governor.EvalIntervalS)
				ticker.Reset(time.Duration(cfg.Governor.EvalIntervalS) * time.Second)
				lastEvalInterval = cfg.Governor.EvalIntervalS
			}
		case <-agentTickCh:
			govState := gov.GetState()
			agentStatuses := agentMgr.AllStatuses()
			payload := dashboard.BuildAgentOnlyStatus(govState, agentStatuses, cfg)
			dashSrv.BroadcastAgentStatus(payload)
		}
	}
}

func runEvalCycle(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	gov *governor.Governor,
	sched *scheduler.Scheduler,
	agentMgr *agent.Manager,
	dashSrv *dashboard.Server,
	notifier *notify.Notifier,
	beadStores map[string]*beads.Store,
	tokenCollector *tokens.Collector,
	metricsCollector *dashboard.MetricsCollector,
	nousState *dashboard.NousState,
	lastActionable *atomic.Pointer[github.ActionableResult],
	advisoryStore *advisory.Store,
	advisoryIssues map[string]int,
	userGHClient *atomic.Pointer[github.Client],
	restartedAgents []string,
	logger *slog.Logger,
) {
	if dashSrv.IsGitHubAppRequired() {
		primaryRepo := cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
			primaryRepo = cfg.Project.Repos[0]
		}
		if primaryRepo != "" && ghClient != nil {
			num, retryErr := ghClient.EnsureAdvisoryIssue(ctx, primaryRepo)
			if retryErr == nil {
				advisoryIssues[primaryRepo] = num
				os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num))
				logger.Info("advisory issue created on retry", "repo", primaryRepo, "number", num)
			}
		}
	}

	actionable, err := ghClient.EnumerateActionable(ctx)
	if err != nil {
		logger.Error("failed to enumerate actionable items", "error", err)
		return
	}
	lastActionable.Store(actionable)
	if data, err := json.Marshal(actionable); err == nil {
		atomicWrite("/data/last-actionable.json", data)
	}

	writeMergeEligible(actionable, actionable.Hold, cfg.Project.Org, logger)

	shaResult, shaErr := ghClient.EnforceSHAHold(ctx, github.SHAHoldConfig{
		PrimaryRepo:     cfg.Project.PrimaryRepo,
		AIAuthor:        cfg.Project.AIAuthor,
		InternalAuthors: []string{"kubestellar-hive[bot]", "github-actions[bot]", "dependabot[bot]", "copilot-swe-agent[bot]"},
	})
	if shaErr != nil {
		logger.Warn("SHA hold enforcement failed", "error", shaErr)
	} else {
		logger.Info("SHA hold enforcement complete",
			"held", shaResult.Held,
			"unheld", shaResult.Unheld,
			"skipped", shaResult.Skipped,
		)
	}

	agentsDue := gov.Evaluate(
		actionable.Issues.Count,
		actionable.PRs.Count,
		actionable.Hold.Total,
		actionable.Issues.SLAViolations,
	)

	// Restarted agents need a kick even if the governor wouldn't schedule one this cycle.
	if len(restartedAgents) > 0 {
		dueSet := make(map[string]bool, len(agentsDue))
		for _, a := range agentsDue {
			dueSet[a] = true
		}
		for _, a := range restartedAgents {
			if !dueSet[a] {
				agentsDue = append(agentsDue, a)
				logger.Info("adding restarted agent to kick list", "agent", a)
			}
		}
	}

	govState := gov.GetState()
	logger.Info("governor eval complete",
		"mode", govState.Mode,
		"issues", govState.QueueIssues,
		"prs", govState.QueuePRs,
		"agents_due", agentsDue,
	)

	// cadence.Paused (cadence: "pause" in config) means "don't kick this agent
	// in this mode" — it does NOT force-pause the agent. Manual pause/resume
	// via the dashboard is always respected; the governor only controls kicks.

	// Filter out on-demand agents — they are only triggered explicitly
	onDemandSet := config.OnDemandAgentsFromPacks()
	var filteredDue []string
	for _, name := range agentsDue {
		if ac, ok := cfg.Agents[name]; ok && ac.OnDemand {
			continue
		}
		if onDemandSet[name] {
			continue
		}
		filteredDue = append(filteredDue, name)
	}
	agentsDue = filteredDue

	sched.SetLastActionable(actionable)
	if len(agentsDue) > 0 {
		messages := sched.BuildKickMessages(actionable, agentsDue)
		for _, msg := range messages {
			logger.Info("audit: governor kicking agent", "agent", msg.Agent, "trigger", "governor-eval")
			if err := agentMgr.SendKick(msg.Agent, msg.Message); err != nil {
				logger.Warn("failed to send kick", "agent", msg.Agent, "error", err)
				continue
			}
			gov.RecordKick(msg.Agent)

			// Log token state at time of kick for cost attribution
			if tokenCollector != nil {
				if summary := tokenCollector.Summary(); summary != nil {
					agentTokens := summary.ByAgent[msg.Agent]
					logger.Info("kick token snapshot",
						"agent", msg.Agent,
						"agent_tokens", agentTokens,
						"total_tokens", summary.TotalTokens,
						"total_sessions", summary.SessionCount,
					)
				}
			}
		}
	}

	if actionable.Issues.SLAViolations > 0 {
		const doubleSLAMinutes = 60
		const maxSLANotificationsPerCycle = 3
		sent := 0
		for _, issue := range actionable.Issues.Items {
			if issue.AgeMinutes > doubleSLAMinutes {
				if sent >= maxSLANotificationsPerCycle {
					logger.Info("SLA notification cap reached, skipping remaining", "remaining", actionable.Issues.SLAViolations-sent)
					break
				}
				notifier.Send(
					"SLA 2x breach",
					fmt.Sprintf("%s#%d age %dm: %s\n%s", issue.Repo, issue.Number, issue.AgeMinutes, issue.Title, issue.URL),
					notify.PriorityHigh,
				)
				sent++
			}
		}
	}

	// Scan agent panes for login-required patterns and pause + notify if detected
	scanForLoginRequired(cfg, agentMgr, notifier, logger)

	agentStatuses := agentMgr.AllStatuses()

	statusPayload := dashboard.BuildFrontendStatus(
		govState,
		actionable,
		agentStatuses,
		cfg,
		tokenCollector,
		gov,
		beadStores,
		ghClient,
		ctx,
		metricsCollector,
	)
	// Ingest any JSONL findings agents wrote and persist them as beads.
	if advisoryStore != nil {
		findings, err := advisoryStore.ReadNewFindings()
		if err != nil {
			logger.Warn("failed to read advisory findings", "error", err)
		} else if len(findings) > 0 {
			// Log each new finding for the audit trail
			for _, f := range findings {
				logger.Info("advisory finding ingested",
					"agent", f.Agent,
					"severity", f.Severity,
					"type", f.Type,
					"title", f.Title,
					"file", f.File,
					"line", f.Line,
				)
			}
			if persisted := advisory.PersistAsBeads(findings, beadStores); persisted > 0 {
				logger.Info("advisory findings persisted as beads", "count", persisted)
			}
		}
	}

	// Reload bead stores from disk before building the digest. Agents write
	// beads via the bd CLI which persists directly to disk, so the in-memory
	// stores can become stale between eval cycles.
	for name, store := range beadStores {
		if err := store.Reload(); err != nil {
			logger.Warn("failed to reload beads from disk", "agent", name, "error", err)
		}
	}

	// Advisory digest: build from beads (the source of truth) before status broadcast.
	if len(beadStores) > 0 {
		digest := advisory.BuildDigestFromBeads(beadStores, string(govState.Mode))
		if advisoryStore != nil {
			advisoryStore.SetLatestDigest(digest)
		}
		dashSrv.SetAdvisoryDigest(digest)
		statusPayload.AdvisoryDigest = digest

		if digest.TotalCount > 0 {
			// Log severity breakdown and contributing agents
			bySeverity := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0, "info": 0}
			agentNames := make([]string, 0, len(digest.ByAgent))
			for agentName, findings := range digest.ByAgent {
				agentNames = append(agentNames, fmt.Sprintf("%s(%d)", agentName, len(findings)))
				for _, f := range findings {
					bySeverity[strings.ToLower(f.Severity)]++
				}
			}
			logger.Info("advisory digest built",
				"total_findings", digest.TotalCount,
				"critical", bySeverity["critical"],
				"high", bySeverity["high"],
				"medium", bySeverity["medium"],
				"low", bySeverity["low"],
				"agents", strings.Join(agentNames, ", "),
				"resolved_count", len(digest.RecentlyResolved),
			)

			md := advisory.FormatDigestMarkdown(digest)
			if md != "" {
				primaryRepo := cfg.Project.PrimaryRepo
				if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
					primaryRepo = cfg.Project.Repos[0]
				}
				if issueNum, ok := advisoryIssues[primaryRepo]; ok && issueNum > 0 {
					postClient := ghClient
					if uc := userGHClient.Load(); uc != nil {
						postClient = uc
					}
					if err := postClient.PostAdvisoryDigest(ctx, primaryRepo, issueNum, md); err != nil {
						logger.Warn("failed to post advisory digest", "repo", primaryRepo, "issue", issueNum, "error", err)
						if strings.Contains(err.Error(), "403") {
							dashSrv.SetGitHubAppRequired(true)
						}
					} else {
						logger.Info("posted advisory digest", "repo", primaryRepo, "issue", issueNum, "findings", digest.TotalCount)
					}
				}
			}
		}
	} else if d := dashSrv.GetAdvisoryDigest(); d != nil {
		statusPayload.AdvisoryDigest = d
	}

	dashSrv.UpdateStatus(statusPayload)

	if agentStats := dashboard.CollectAgentStats(statusPayload); len(agentStats) > 0 {
		gov.AttachAgentStats(agentStats)
	}

	if repoSnaps := dashboard.CollectRepoSnapshots(statusPayload); len(repoSnaps) > 0 {
		gov.AttachRepoSnapshots(repoSnaps)
	}

	if nousState != nil {
		var tokenSummary *tokens.AggregateSummary
		if tokenCollector != nil {
			tokenSummary = tokenCollector.Summary()
		}
		if err := nousState.RecordSnapshot(govState, actionable, agentsDue, agentStatuses, tokenSummary); err != nil {
			logger.Warn("failed to record nous snapshot", "error", err)
		}
	}
}

// loginCommandForBackend returns the login instruction for a given CLI backend.
func loginCommandForBackend(backend string) string {
	switch backend {
	case "claude":
		return "Run: claude login"
	case "copilot":
		return "Run: copilot auth login"
	case "gemini":
		return "Run: gemini auth login"
	case "goose":
		return "Run: goose auth login"
	default:
		return "Run the login command for " + backend
	}
}

// scanForLoginRequired checks each running agent's tmux pane output for login-required
// patterns. When a match is found, the agent is paused and a notification is sent.
func scanForLoginRequired(
	cfg *config.Config,
	agentMgr *agent.Manager,
	notifier *notify.Notifier,
	logger *slog.Logger,
) {
	patterns := cfg.Governor.Sensing.LoginPatterns
	if len(patterns) == 0 {
		return
	}

	// Compile regex patterns, skipping empty and invalid ones
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			logger.Warn("invalid login pattern regex", "pattern", p, "error", err)
			continue
		}
		compiled = append(compiled, re)
	}
	if len(compiled) == 0 {
		return
	}

	const paneLines = 50 // number of recent lines to scan
	statuses := agentMgr.AllStatuses()
	for name, proc := range statuses {
		if proc.State != "running" {
			continue
		}

		output, err := agentMgr.GetOutput(name, paneLines)
		if err != nil || len(output) == 0 {
			continue
		}

		joined := strings.Join(output, "\n")
		for _, re := range compiled {
			if re.MatchString(joined) {
				logger.Warn("login required detected",
					"agent", name,
					"pattern", re.String(),
				)

				// Pause the agent instead of restarting
				if pauseErr := agentMgr.Pause(name, "login-detector", "login required detected"); pauseErr != nil {
					logger.Warn("failed to pause agent after login detection",
						"agent", name, "error", pauseErr)
				}

				// Determine the login instruction based on the agent's backend
				backend := cfg.Agents[name].Backend
				loginCmd := loginCommandForBackend(backend)

				notifier.Send(
					fmt.Sprintf("\U0001F511 Login required: %s", name),
					fmt.Sprintf(
						"Agent '%s' needs authentication. Open the agent's terminal "+
							"(tmux attach -t hive-%s) and run the login command for the CLI (%s). %s",
						name, name, backend, loginCmd,
					),
					notify.PriorityHigh,
				)

				break // one match per agent is enough
			}
		}
	}
}

func convertKnowledgeLayers(cfgLayers []config.KnowledgeLayer) []knowledge.LayerConfig {
	layers := make([]knowledge.LayerConfig, len(cfgLayers))
	for i, l := range cfgLayers {
		layers[i] = knowledge.LayerConfig{
			Type:   knowledge.LayerType(l.Type),
			Path:   l.Path,
			URL:    l.URL,
			Shared: l.Shared,
		}
	}
	return layers
}

// hiveIDFilePath is the persistent file where the Hive ID is stored across restarts.
const hiveIDFilePath = "/data/hive-id"

// loadOrGenerateHiveID reads the Hive ID from disk, or generates and persists a new one.
func loadOrGenerateHiveID(logger *slog.Logger) string {
	if envID := os.Getenv("HIVE_ID"); envID != "" {
		if err := os.WriteFile(hiveIDFilePath, []byte(envID+"\n"), 0o644); err == nil {
			logger.Info("hive ID set from HIVE_ID env var", "id", envID)
		}
		return envID
	}

	if data, err := os.ReadFile(hiveIDFilePath); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			logger.Info("hive ID loaded from disk", "id", id)
			return id
		}
	}

	id := "hive-" + randomName()

	if err := os.WriteFile(hiveIDFilePath, []byte(id+"\n"), 0o644); err != nil {
		logger.Warn("failed to persist hive ID", "error", err)
	} else {
		logger.Info("generated new hive ID", "id", id)
	}

	return id
}

// randomName generates a Docker-style adjective-noun name.
func randomName() string {
	adjectives := []string{
		"bold", "calm", "cool", "dark", "deep", "fair", "fast", "keen",
		"kind", "loud", "mild", "neat", "pale", "pure", "rare", "rich",
		"safe", "slim", "soft", "tall", "thin", "true", "vast", "warm",
		"wise", "able", "busy", "easy", "epic", "free", "glad", "good",
		"idle", "just", "lazy", "lean", "live", "long", "lost", "main",
		"next", "open", "real", "sure", "wild", "worn", "zero", "blue",
	}
	nouns := []string{
		"ant", "ape", "bat", "bee", "cow", "doe", "eel", "elk",
		"fox", "gnu", "hen", "jay", "kit", "lark", "moth", "newt",
		"owl", "pug", "ram", "ray", "seal", "swan", "toad", "wren",
		"bear", "colt", "crow", "deer", "dove", "duck", "fawn", "frog",
		"goat", "gull", "hare", "hawk", "ibis", "lynx", "mink", "mole",
		"orca", "pike", "puma", "slug", "stag", "wolf", "yak", "wasp",
	}

	buf := make([]byte, 2)
	if _, err := rand.Read(buf); err != nil {
		return "bold-ant"
	}
	adj := adjectives[int(buf[0])%len(adjectives)]
	noun := nouns[int(buf[1])%len(nouns)]
	return adj + "-" + noun
}

func persistState(agentMgr *agent.Manager, gov *governor.Governor, cfg *config.Config, tc *tokens.Collector, path string, logger *slog.Logger, dashSrv *dashboard.Server) {
	statuses := agentMgr.AllStatuses()
	agents := make(map[string]snapshot.AgentState, len(statuses))
	for name, proc := range statuses {
		as := snapshot.AgentState{
			Paused:          proc.Paused,
			PinnedCLI:       proc.PinnedCLI,
			PinnedModel:     proc.PinnedModel,
			ModelOverride:   proc.ModelOverride,
			BackendOverride: proc.BackendOverride,
			RestartCount:    proc.RestartCount,
			LastKick:        proc.LastKick,
			PausedReason:    proc.PausedReason,
			PausedTrigger:   proc.PausedTrigger,
		}
		if !proc.PausedAt.IsZero() {
			t := proc.PausedAt
			as.PausedAt = &t
		}
		if len(proc.KickHistory) > 0 {
			as.KickHistory = make([]snapshot.AgentKickEntry, len(proc.KickHistory))
			for i, kr := range proc.KickHistory {
				as.KickHistory[i] = snapshot.AgentKickEntry{
					Timestamp: kr.Timestamp,
					Agent:     kr.Agent,
					Snippet:   kr.Snippet,
				}
			}
		}
		if agentCfg, ok := cfg.Agents[name]; ok {
			as.DisplayName = agentCfg.DisplayName
			as.Description = agentCfg.Description
			enabled := agentCfg.Enabled
			as.Enabled = &enabled
			clearOnKick := agentCfg.ClearOnKick
			as.ClearOnKick = &clearOnKick
			staleTimeout := agentCfg.StaleTimeout
			as.StaleTimeout = &staleTimeout
			as.RestartStrategy = agentCfg.RestartStrategy
			as.LaunchCmd = agentCfg.LaunchCmd
		}
		agents[name] = as
	}

	cadenceOverrides := make(map[string]map[string]string)
	for modeName, mode := range cfg.Governor.Modes {
		if len(mode.Cadences) > 0 {
			cadenceOverrides[modeName] = make(map[string]string, len(mode.Cadences))
			for agentName, cadence := range mode.Cadences {
				cadenceOverrides[modeName][agentName] = cadence
			}
		}
	}

	budget := gov.GetBudget()
	govState := gov.GetState()

	govKickHistory := gov.KickHistory()
	kickEntries := make([]snapshot.GovKickEntry, len(govKickHistory))
	for i, kr := range govKickHistory {
		kickEntries[i] = snapshot.GovKickEntry{Timestamp: kr.Timestamp, Agent: kr.Agent}
	}

	var issueCosts map[string]int64
	if tc != nil {
		issueCosts = tc.IssueCosts()
	}

	state := &snapshot.PersistedState{
		Agents:           agents,
		GovernorMode:     string(govState.Mode),
		BudgetLimit:      budget.WeeklyLimit,
		BudgetIgnored:    budget.IgnoredAgents,
		CadenceOverrides: cadenceOverrides,
		LastKicks:        govState.LastKick,
		BudgetSpend:      budget.CurrentSpend,
		BudgetResetAt:    budget.ResetAt,
		BudgetByAgent:    budget.ByAgent,
		BudgetByModel:    budget.ByModel,
		KickHistory:      kickEntries,
		IssueCosts:       issueCosts,
		LastEval:         govState.LastEval,
		ACMMLevel:        cfg.ACMMLevel,
	}

	if err := snapshot.SaveState(path, state, logger); err != nil {
		logger.Error("failed to persist state", "error", err)
	}

	if err := cfg.Save(); err != nil {
		logger.Error("failed to persist config to yaml", "error", err)
	}

	history := gov.EvalHistory()
	if len(history) > 0 {
		historyData, err := json.Marshal(history)
		if err == nil {
			atomicWrite("/data/sparkline-history.json", historyData)
		}
	}

	modeHistory := gov.ModeHistory()
	if len(modeHistory) > 0 {
		modeData, err := json.Marshal(modeHistory)
		if err == nil {
			atomicWrite("/data/mode-history.json", modeData)
		}
	}

	if dashSrv != nil {
		tokenHistory := dashSrv.TokenSparklineHistory()
		if len(tokenHistory) > 0 {
			tokenData, err := json.Marshal(tokenHistory)
			if err == nil {
				atomicWrite("/data/token-sparkline-history.json", tokenData)
			}
		}
	}
}

const mergeEligiblePath = "/var/run/hive-metrics/merge-eligible.json"

func writeMergeEligible(actionable *github.ActionableResult, hold github.HoldResult, org string, logger *slog.Logger) {
	holdSet := make(map[string]bool)
	for _, h := range hold.Items {
		key := fmt.Sprintf("%s/%d", h.Repo, h.Number)
		holdSet[key] = true
	}

	type eligiblePR struct {
		Number    int      `json:"number"`
		Repo      string   `json:"repo"`
		Title     string   `json:"title"`
		Author    string   `json:"author"`
		Labels    []string `json:"labels,omitempty"`
		Mergeable bool     `json:"mergeable"`
		DCO       string   `json:"dco"`
	}

	var eligible []eligiblePR
	for _, pr := range actionable.PRs.Items {
		if pr.Draft {
			continue
		}
		key := fmt.Sprintf("%s/%d", pr.Repo, pr.Number)
		if holdSet[key] {
			continue
		}
		fullRepo := pr.Repo
		if !strings.Contains(fullRepo, "/") && org != "" {
			fullRepo = org + "/" + fullRepo
		}
		dco := "unknown"
		for _, l := range pr.Labels {
			if l == "dco-signoff: yes" {
				dco = "yes"
			} else if l == "dco-signoff: no" {
				dco = "no"
			}
		}
		eligible = append(eligible, eligiblePR{
			Number:    pr.Number,
			Repo:      fullRepo,
			Title:     pr.Title,
			Author:    pr.Author,
			Labels:    pr.Labels,
			Mergeable: pr.Mergeable,
			DCO:       dco,
		})
	}

	payload := map[string]any{
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"merge_eligible": eligible,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		logger.Warn("failed to marshal merge-eligible", "error", err)
		return
	}
	_ = os.MkdirAll("/var/run/hive-metrics", 0o755)
	atomicWrite(mergeEligiblePath, data)
	logger.Info("merge-eligible.json updated", "eligible", len(eligible), "total_prs", len(actionable.PRs.Items))
}

func atomicWrite(path string, data []byte) {
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

func applyConfigOverrides(cfg *config.Config, o *snapshot.ConfigOverrides) {
	if len(o.ProjectRepos) > 0 {
		cfg.Project.Repos = o.ProjectRepos
	}
	if o.EvalIntervalS != nil {
		cfg.Governor.EvalIntervalS = *o.EvalIntervalS
	}
	if len(o.Thresholds) > 0 {
		for name, threshold := range o.Thresholds {
			if mode, ok := cfg.Governor.Modes[name]; ok {
				mode.Threshold = threshold
				cfg.Governor.Modes[name] = mode
			}
		}
	}
	if len(o.SensingGHRate) > 0 {
		cfg.Governor.Sensing.GHRatePatterns = o.SensingGHRate
	}
	if len(o.SensingCLIExclude) > 0 {
		cfg.Governor.Sensing.CLIExcludePatterns = o.SensingCLIExclude
	}
	if len(o.SensingLogin) > 0 {
		cfg.Governor.Sensing.LoginPatterns = o.SensingLogin
	}
	if o.SensingTTL != nil {
		cfg.Governor.Sensing.TTLSeconds = *o.SensingTTL
	}
	if o.SensingPullback != nil {
		cfg.Governor.Sensing.PullbackSeconds = *o.SensingPullback
	}
	if len(o.ExemptLabels) > 0 {
		cfg.Governor.Labels.Exempt = o.ExemptLabels
	}
	if o.NtfyServer != "" || o.NtfyTopic != "" {
		if cfg.Notifications.Ntfy == nil {
			cfg.Notifications.Ntfy = &config.NtfyConfig{}
		}
		if o.NtfyServer != "" {
			cfg.Notifications.Ntfy.Server = o.NtfyServer
		}
		if o.NtfyTopic != "" {
			cfg.Notifications.Ntfy.Topic = o.NtfyTopic
		}
	}
	if o.DiscordWebhook != "" {
		if cfg.Notifications.Discord == nil {
			cfg.Notifications.Discord = &config.DiscordConfig{}
		}
		cfg.Notifications.Discord.Webhook = o.DiscordWebhook
	}
	if o.HealthcheckInterval != nil {
		cfg.Governor.Health.HealthcheckInterval = *o.HealthcheckInterval
	}
	if o.RestartCooldown != nil {
		cfg.Governor.Health.RestartCooldown = *o.RestartCooldown
	}
	if o.ModelLock != nil {
		cfg.Governor.Health.ModelLock = *o.ModelLock
	}
	if o.LogMaxSizeMB != nil {
		cfg.Governor.Logging.MaxSizeMB = *o.LogMaxSizeMB
	}
	if o.LogMaxAgeDays != nil {
		cfg.Governor.Logging.MaxAgeDays = *o.LogMaxAgeDays
	}
	if o.LogMaxBackups != nil {
		cfg.Governor.Logging.MaxBackups = *o.LogMaxBackups
	}
	if o.LogCompress != nil {
		cfg.Governor.Logging.Compress = *o.LogCompress
	}
	if o.LogLevel != "" {
		cfg.Governor.Logging.Level = o.LogLevel
	}
}

const (
	nousGovernorDir = "/var/run/nous/governor"
	nousSnapshotDir = "/data/nous/snapshots"
)

func loadNousState(logger *slog.Logger) *dashboard.NousState {
	state := &dashboard.NousState{
		Mode:   "observe",
		Scope:  "governor",
		Phase:  "collecting",
		Status: make(map[string]interface{}),
		Config: make(map[string]interface{}),
	}

	if ledgerData, err := os.ReadFile(nousGovernorDir + "/ledger.json"); err == nil {
		var ledger struct {
			Iterations []map[string]interface{} `json:"iterations"`
		}
		if err := json.Unmarshal(ledgerData, &ledger); err == nil {
			state.Ledger = ledger.Iterations
			logger.Info("nous ledger loaded", "iterations", len(state.Ledger))
		}
	}

	if principlesData, err := os.ReadFile(nousGovernorDir + "/principles.json"); err == nil {
		var pFile struct {
			Principles []json.RawMessage `json:"principles"`
		}
		if err := json.Unmarshal(principlesData, &pFile); err == nil {
			for _, raw := range pFile.Principles {
				var p map[string]interface{}
				if json.Unmarshal(raw, &p) == nil {
					state.Principles = append(state.Principles, dashboard.NousPrinciple{
						ID:         stringFromMap(p, "id"),
						Text:       stringFromMap(p, "statement"),
						Confidence: confidenceToFloat(stringFromMap(p, "confidence")),
						Source:     stringFromMap(p, "category"),
					})
				}
			}
			logger.Info("nous principles loaded", "count", len(state.Principles))
		}
	}

	snapshotCount := 0
	if entries, err := os.ReadDir(nousSnapshotDir); err == nil {
		snapshotCount = len(entries)
	}

	iterationCount := len(state.Ledger)
	if iterationCount > 0 {
		state.Phase = "observing"
	}

	state.Status = map[string]interface{}{
		"status":          "active",
		"mode":            state.Mode,
		"scope":           state.Scope,
		"phase":           state.Phase,
		"snapshots":       snapshotCount,
		"snapshotCount":   snapshotCount,
		"iterations":      iterationCount,
		"principles":      len(state.Principles),
		"principleCount":  len(state.Principles),
		"baseline_target": dashboard.NousBaselineTarget,
		"snapshotTarget":  dashboard.NousBaselineTarget,
		"baseline_pct":    float64(snapshotCount) * 100 / dashboard.NousBaselineTarget,
	}

	return state
}

func stringFromMap(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func confidenceToFloat(s string) float64 {
	switch s {
	case "high":
		return 0.9
	case "medium":
		return 0.7
	case "low":
		return 0.4
	default:
		return 0.5
	}
}

const logFilename = "hive.log"

func setupLogger(dir string, maxSizeMB, maxAgeDays, maxBackups int, compress bool, level string) *slog.Logger {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("failed to create log directory, falling back to stdout only", "dir", dir, "error", err)
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(level)}))
	}

	lj := &lumberjack.Logger{
		Filename:   filepath.Join(dir, logFilename),
		MaxSize:    maxSizeMB,
		MaxAge:     maxAgeDays,
		MaxBackups: maxBackups,
		Compress:   compress,
	}

	tee := io.MultiWriter(os.Stdout, lj)
	return slog.New(slog.NewJSONHandler(tee, &slog.HandlerOptions{Level: parseLogLevel(level)}))
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// initAgentConfigDrivenSystems wires up config-driven agent metadata to subsystems
// that previously relied on hardcoded agent name maps (classifier, discord, token detector).
func initAgentConfigDrivenSystems(cfg *config.Config) {
	var lanes []classify.LaneConfig
	var agentNames []string
	detectKeywords := make(map[string][]string)
	discordIdentities := make(map[string]discord.AgentIdentity)
	discordAliases := make(map[string]string)

	for name, agent := range cfg.Agents {
		agentNames = append(agentNames, name)

		if len(agent.LaneKeywords) > 0 {
			lanes = append(lanes, classify.LaneConfig{
				Name:     name,
				Keywords: agent.LaneKeywords,
			})
		}
		if len(agent.DetectKeywords) > 0 {
			detectKeywords[name] = agent.DetectKeywords
		}
		if agent.Emoji != "" || agent.Color != "" {
			discordIdentities[name] = discord.AgentIdentity{
				Emoji: agent.Emoji,
				Color: parseColorInt(agent.Color),
			}
		}
		for _, alias := range agent.Aliases {
			discordAliases[alias] = name
		}
	}

	if len(lanes) > 0 {
		classify.SetLanes(lanes)
	}
	if len(detectKeywords) > 0 {
		tokens.SetDetectKeywords(detectKeywords)
	}
	tokens.SetAgentNames(agentNames)
	discord.SetAgentIdentities(discordIdentities)
	if len(discordAliases) > 0 {
		discord.SetAgentAliases(discordAliases)
	}
}

// inferACMMLevel returns the configured ACMM level, defaulting to L1 (advisory-only).
func inferACMMLevel(cfg *config.Config) int {
	if cfg.ACMMLevel != nil {
		return *cfg.ACMMLevel
	}
	return 1
}

// parseColorInt converts a hex color string like "#3498db" to an int.
func parseColorInt(color string) int {
	color = strings.TrimPrefix(color, "#")
	if color == "" {
		return 0x95a5a6
	}
	var result int
	fmt.Sscanf(color, "%x", &result)
	return result
}

func runHub(logger *slog.Logger) {
	port := 3001
	if p := os.Getenv("HIVE_HUB_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	logger.Info("starting in HUB mode", "port", port)

	hubSrv := hub.NewHubServer(port, logger, gitShort)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("hub received signal, shutting down gracefully", "signal", sig)
		const shutdownTimeout = 10 * time.Second
		if err := hubSrv.Shutdown(shutdownTimeout); err != nil {
			logger.Error("hub graceful shutdown failed", "error", err)
		}
	}()

	if err := hubSrv.Start(port); err != nil && err != http.ErrServerClosed {
		logger.Error("hub server failed", "error", err)
		os.Exit(1)
	}
	logger.Info("hub server stopped")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
