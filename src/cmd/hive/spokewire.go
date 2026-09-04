package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	// automaxprocs sets GOMAXPROCS to match the container's CPU quota (Linux
	// CFS) at init. Without it the Go runtime sizes its P count to the whole
	// NODE's core count, so on a many-core IKS worker a pod limited to a few
	// CPUs spawns far more runnable Ps than its CFS quota can service; when the
	// quota is exhausted mid-period EVERY goroutine — including the netpoller
	// that answers the :3002 liveness probe and the heartbeat loop — is
	// throttled until the next CFS period, which stacks on top of the NFS
	// stalls to push probe latency past the kubelet timeout. Matching GOMAXPROCS
	// to the quota removes that self-inflicted throttling.
	//
	// This is called explicitly rather than via the package's blank import
	// because that import's init writes a line to the default logger (stderr)
	// unconditionally. `hive` re-execs itself as a Git transport shim, and the
	// setup path captures a child's stdout and stderr into a single buffer to
	// parse (e.g. `symbolic-ref --short origin/HEAD`), so an init-time banner
	// is indistinguishable from Git's answer and corrupts the parsed branch
	// name. Setting it with a no-op logger keeps the GOMAXPROCS behaviour and
	// drops the banner.

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/discord"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/github/requestwatch"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/proxy"
	"github.com/hivecommons/hive/pkg/pushbroker"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/toolapprove"
	"github.com/hivecommons/hive/pkg/tracing"
	"github.com/hivecommons/hive/pkg/trajectory"
	"github.com/hivecommons/hive/pkg/watchdog"
	"github.com/hivecommons/hive/pkg/watsonx"
)

// wireSpokeSubsystems runs the spoke-mode subsystem wiring after the common
// process, config path, logging, and hub-mode preamble has completed. It keeps
// the original startup order byte-for-byte inside the spoke path while main()
// becomes an ordered dispatcher. Follow-up phase-2c PRs should peel contiguous
// sections from this function into narrower wireX constructors.
func wireSpokeSubsystems(configPath string, logger *slog.Logger, startTime time.Time) {
	// Use LoadWithDashboardOverlay (not plain Load) so the dashboard overlay's
	// removed_agents tombstones are populated into cfg.RemovedAgents at boot —
	// BEFORE the startup ApplyPack below reconciles the ACMM roster. Plain Load
	// never reads the overlay, so on restart the tombstone was invisible and
	// ApplyPack re-added deleted pack agents (brainstorm/guide) every time
	// (#2439). Same return signature as Load; falls back to the seed when no
	// overlay exists or the pod is not in Kubernetes.
	cfg, err := config.LoadWithDashboardOverlay(configPath)
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
	_ = os.Setenv("HIVE_ID", cfg.HiveID) // valid key/value; Setenv cannot fail on Unix

	// Observability (#2439): report the removed-agents tombstone LoadWithDashboardOverlay
	// adopted from the dashboard overlay at boot, BEFORE the startup ApplyPack below. On
	// a non-sticking-removal report this line is the first check — an empty set here on a
	// hive that removed an agent means the tombstone did not persist across the restart.
	logger.Info("boot: loaded removed-agents tombstone",
		"hive_id", cfg.HiveID,
		"count", len(cfg.RemovedAgents),
		"agents", cfg.RemovedAgents,
	)

	// Surface config provenance: when the persisted runtime config exists, init
	// containers restore it over the ConfigMap seed on restart, so edits made
	// only to the seed (or only to the live file) silently lose to it.
	//
	// Checks the legacy name too: during the migration a hive may still carry
	// only /data/hive.yaml.bak, and the whole point of this log line is to warn
	// that such a file is shadowing the seed. Note this path was previously
	// built as configPath + ".bak", which only ever resolved to the real
	// location when HIVE_CONFIG happened to live under /data — a literal grep
	// for "hive.yaml.bak" could not find it either.
	for _, runtimePath := range []string{config.RuntimeConfigFile, config.RuntimeConfigFileLegacy} {
		if _, statErr := os.Stat(runtimePath); statErr == nil {
			logger.Info("persisted runtime config present — restored over the seed on pod restart; fixes must land in the live config so the next save refreshes it",
				"path", runtimePath,
				"github_installation_id", cfg.GitHub.InstallationID,
			)
			break
		}
	}

	// HIVE_CONFIG names a DIFFERENT file from the one we actually loaded.
	//
	// This is only ever the entrypoint's read-only escape hatch failing to
	// land. When the config path cannot be written, entrypoint.sh exports
	// HIVE_CONFIG=/data/hive.yaml.runtime and logs "config path is read-only —
	// using ... directly"; HIVE_CONFIG is read above only as the DEFAULT of
	// -config, and the image's CMD passes that flag explicitly
	// (Dockerfile: CMD ["--config", "/etc/hive/hive.yaml"]), so the explicit
	// value won and the redirect did nothing.
	//
	// That is #4973, and it is silently destructive rather than merely wrong:
	// the stale file loads, then Config.Save() writes the whole in-memory
	// config back over /data/hive.yaml.runtime, destroying the state the
	// operator had persisted there. An ACMM level set from the dashboard came
	// back at its provisioned value after a restart, twice, with /data intact.
	//
	// entrypoint.sh now appends `--config "$HIVE_CONFIG"` to the argv so the
	// last-occurrence-wins rule in flag.Parse carries the redirect, which means
	// this branch should be unreachable. It is kept — at WARN, naming both
	// paths — because the failure it reports is invisible from every other
	// vantage point: /api/config/provenance reads HIVE_CONFIG directly, so it
	// reports the file the entrypoint chose while the process runs on the one
	// it did not, and the two disagree with no way to tell from the outside.
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" && envCfg != configPath {
		logger.Warn("config path disagreement: HIVE_CONFIG names a different file than the one loaded — an explicit -config (the image CMD) outranked the entrypoint's redirect; persisted state in HIVE_CONFIG may be overwritten by the next save",
			"hive_config_env", envCfg,
			"loaded_config", configPath,
		)
	}

	logger.Info("hive starting",
		"org", cfg.Project.Org,
		"repos", cfg.Project.Repos,
		"agents", len(cfg.Agents),
		"hive_id", cfg.HiveID,
	)
	startupRepoTargetIssue := config.ValidateRepoTargets(cfg)
	if startupRepoTargetIssue != nil {
		logger.Warn("repo target misconfigured — owner action required",
			"issue", startupRepoTargetIssue.Message,
			"hive_id", cfg.HiveID,
			"org", cfg.Project.Org,
			"repos", cfg.Project.Repos,
			"primary_repo", cfg.Project.PrimaryRepo,
		)
	}
	repoTargetMisconfigured := func() bool {
		return config.ValidateRepoTargets(cfg) != nil
	}
	repoTargetIssueMessage := func() string {
		if issue := config.ValidateRepoTargets(cfg); issue != nil {
			return issue.Message
		}
		return ""
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize OpenTelemetry tracing. Off by default: with no otel block
	// (or otel.enabled=false) this installs a no-op provider with zero export
	// overhead. Never fatal — a tracing setup error must not stop hive.
	otelCfg := cfg.EffectiveOTel()
	traceShutdown, traceErr := tracing.Init(ctx, tracing.Config{
		Enabled:     otelCfg.Enabled,
		Endpoint:    otelCfg.Endpoint,
		Headers:     otelCfg.Headers,
		ServiceName: otelCfg.ServiceNameOrDefault(),
		Insecure:    otelCfg.Insecure,
		SampleRatio: otelCfg.SampleRatio,
		HiveID:      cfg.HiveID,
		Branch:      cfg.Policies.Branch,
		// Reach anchors (#3973): gitShort is the ldflags-baked commit of THIS
		// binary (already canonicalized to 7 chars above), and the image ref is
		// the Deployment-declared image (cached — warmed by the release-channel
		// read at startup; "" outside a cluster). Spans attribute to the code
		// that actually runs, not to the merge/publish event (#3816).
		Commit: gitShort,
		Image:  hub.SelfDeploymentImage(),
	})
	if traceErr != nil {
		logger.Warn("tracing init failed; continuing without tracing", "error", traceErr)
	} else if otelCfg.Enabled {
		logger.Info("otel tracing enabled", "endpoint", otelCfg.Endpoint, "service_name", otelCfg.ServiceNameOrDefault())
	}
	// Component reach counters (#3993): resume this commit's counters from the
	// PVC before any span can start. Independent of the otel block above —
	// counters increment with or without an exporter (design D2 of #3973), so
	// this runs unconditionally and a load failure only costs history, never
	// counting. Counters persisted by a DIFFERENT commit are dropped inside
	// LoadReachState: a new binary starts fresh keys naturally.
	if err := tracing.LoadReachState(reachStatePath, gitShort, logger); err != nil {
		logger.Warn("reach state load failed; starting with fresh counters", "error", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), traceShutdownTimeout)
		defer shutdownCancel()
		if err := traceShutdown(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown error", "error", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	// preShutdownHooks run in the signal handler before the context is canceled,
	// in registration order, while every connection and tmux server is still
	// live. Registrations happen later in startup, once the subsystems they
	// touch exist.
	//
	// This was a single atomic.Pointer[func()] until kubestellar/hive#5390. A
	// lone pointer makes registration DESTRUCTIVE: the second Store silently
	// discards the first hook, and the loss is invisible — nothing fails, a
	// shutdown side effect simply stops happening. That is precisely the trap
	// the WebSocket drain walked into, since the slot was already held by
	// #4296's kick-log archive. A slice makes adding a hook additive by
	// construction, so the next one cannot repeat the mistake.
	var preShutdownHooks shutdownHooks
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		preShutdownHooks.run()
		cancel()
	}()

	ghAuth := initGitHubAuth(ctx, cfg, logger)
	ghClient, appAuth := ghAuth.Client, ghAuth.AppAuth
	// appAuthFailure, when non-empty, is the operator-facing reason GitHub auth
	// is unavailable. It is surfaced through the existing
	// GitHubAppRequired/PermIssue banner rather than killing the process.
	appAuthFailure := ghAuth.Failure
	// appAuthState classifies that failure so the banner and the hub's journey
	// nudges can tell an operator-side fault (no key was ever delivered) from a
	// user-actionable one (the App is not installed).
	appAuthState := ghAuth.State
	if ghClient != nil && len(cfg.Governor.Labels.Exempt) > 0 {
		ghClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
		ghClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
	}
	// Unconditional (nil-safe, zero value = no filtering): the issue filter
	// gates which issues become actionable at all, so it must be installed
	// even when no exempt labels are configured.
	ghClient.SetIssueFilter(cfg.Project.IssueFilter)
	// The user write-token client (userGHClient) was removed: every GitHub write
	// — issues, PRs, comments, merges, and the advisory digest — now goes through
	// the hive's App installation token (ghClient / kubestellar-hive[bot]). The
	// user token only ever served as an advisory-digest fallback writer, which is
	// no longer wanted (and forced the excessive "repo" login scope, issue #1927).
	// Dashboard login now requests no scope and no user write-token is persisted.

	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	// Default mode thresholds scale with how many repos the hive watches, so
	// the mode ladder means the same thing on a 3-repo hive as on a 39-repo
	// one (#3498). Explicit thresholds are unaffected.
	gov.SetRepoCount(cfg.Project.RepoCount())
	sched := scheduler.New(cfg, logger)

	// Wire the GitHub prompt-source resolver so agents may source their kick
	// prompt from a repo (agent.prompt_source). Fetching reuses the hive's App
	// token via ghClient and is gated to the seed-only allowlist — the closure
	// captures cfg so a live config reload updates the allowlist on the next kick.
	// A nil ghClient must be passed as a nil Fetcher interface (not a typed-nil
	// *github.Client) so the resolver's nil-fetcher fallback path triggers.
	var promptFetcher promptsrc.Fetcher
	if ghClient != nil {
		promptFetcher = ghClient
	}
	sched.SetGitHubPromptResolver(promptsrc.NewResolver(
		promptFetcher,
		func(slug string) bool { return cfg.GitHubPromptAllowed(slug) },
		logger,
	))

	// Wire the whole-agent definition_source resolver so agents imported with
	// "keep linked" re-fetch their portable AgentDefinition from the repo on
	// reload/kick and re-apply its operator-safe fields (never security/seed-only
	// fields — see pkg/defsrc). Same seed-only allowlist gate and graceful
	// fallback as the prompt resolver.
	var defFetcher defsrc.Fetcher
	if ghClient != nil {
		defFetcher = ghClient
	}
	definitionResolver := defsrc.NewResolver(
		defFetcher,
		func(slug string) bool { return cfg.GitHubDefinitionAllowed(slug) },
		logger,
	)
	// Apply live definitions once at startup so a repo edit made while the hive
	// was down is reflected before the first kick.
	defsrc.ApplyToConfig(context.Background(), cfg, definitionResolver, logger)

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

	// Restore fact count history from disk so the knowledge sparkline survives restarts
	const factHistoryPath = "/data/fact-history.json"
	var pendingFactSeed []dashboard.FactHistoryEntry
	if factData, err := os.ReadFile(factHistoryPath); err == nil {
		if err := json.Unmarshal(factData, &pendingFactSeed); err == nil && len(pendingFactSeed) > 0 {
			logger.Info("fact history loaded", "entries", len(pendingFactSeed))
		}
	}

	// Restore estimated-cost history from disk so the cost sparkline survives restarts
	const costHistoryPath = "/data/cost-history.json"
	var pendingCostSeed []dashboard.CostHistoryEntry
	if costData, err := os.ReadFile(costHistoryPath); err == nil {
		if err := json.Unmarshal(costData, &pendingCostSeed); err == nil && len(pendingCostSeed) > 0 {
			logger.Info("cost history loaded", "entries", len(pendingCostSeed))
		}
	}

	// #4298: restore per-budget-window history so past resets survive a restart.
	// A missing or unparseable file is ordinary on a hive upgrading into this
	// feature — it simply starts with no history rather than failing to boot.
	const budgetWindowHistoryPath = "/data/budget-window-history.json"
	var pendingBudgetWindowSeed []collect.BudgetWindowEntry
	if budgetData, err := os.ReadFile(budgetWindowHistoryPath); err == nil {
		if err := json.Unmarshal(budgetData, &pendingBudgetWindowSeed); err == nil && len(pendingBudgetWindowSeed) > 0 {
			logger.Info("budget window history loaded", "entries", len(pendingBudgetWindowSeed))
		}
	}

	// #4263: restore convergence soak telemetry so a fixed-commit off/shadow/
	// enforce comparison survives restarts. Missing or unparseable is ordinary
	// on a hive that never ran with the toggle on — start empty, never fail.
	const convergenceSoakHistoryPath = "/data/convergence-soak-history.json"
	var pendingConvergenceSoakSeed []dashboard.ConvergenceSoakEntry
	if soakData, err := os.ReadFile(convergenceSoakHistoryPath); err == nil {
		if err := json.Unmarshal(soakData, &pendingConvergenceSoakSeed); err == nil && len(pendingConvergenceSoakSeed) > 0 {
			logger.Info("convergence soak history loaded", "entries", len(pendingConvergenceSoakSeed))
		}
	}

	// Restore governor/repo/beads/system trend history from disk so those
	// sparklines survive restarts and render for any viewer (previously kept
	// only in the browser's localStorage).
	const trendHistoryPath = "/data/trend-history.json"
	var pendingTrendSeed []dashboard.TrendHistoryEntry
	if trendData, err := os.ReadFile(trendHistoryPath); err == nil {
		if err := json.Unmarshal(trendData, &pendingTrendSeed); err == nil && len(pendingTrendSeed) > 0 {
			logger.Info("trend history loaded", "entries", len(pendingTrendSeed))
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
	// A hive that booted without usable GitHub credentials raises the banner
	// immediately, seeded with the classification made at startup. Otherwise
	// these stay empty and are filled in later by the live probes below.
	githubAppRequired := appAuthFailure != ""
	// githubAppDiag/githubAppState carry the classified reason App auth failed,
	// so the banner can name the true cause (and the hub can avoid escalating
	// against a hive whose credentials the operator never delivered).
	githubAppDiag := appAuthFailure
	githubAppState := appAuthState
	// Config truth outranks live probes: an App with no installation cannot
	// mint, period. A cached token can keep clients green for up to an hour
	// after an installation is cleared, and waiting for the first failed mint
	// left the banner down and the hub green exactly when the operator needed
	// the opposite (the fast-model-actuation incident).
	if cfg.GitHub.ConfiguredButUninstalled() {
		githubAppRequired = true
		githubAppState = github.AppStateNotInstalled
		githubAppDiag = "GitHub App " + strconv.FormatInt(cfg.GitHub.AppID, 10) +
			" has no installation for this org — install it (the spoke adopts the installation automatically)"
	}

	// Invocation-attribution trail (pkg/github/attribution.go): stamp hive-
	// created PRs/issues with what the hive invoked, and audit every such
	// creation. Wired in stages as dependencies come up: the trailer gate now
	// (cfg exists, and the advisory-issue ensure just below must respect the
	// toggle), the per-agent resolver after the agent manager exists, and the
	// audit sink after the dashboard server exists. cfg is the live pointer
	// (the config watcher swaps contents in place), so the toggle is read
	// fresh per creation — a dashboard flip takes effect immediately.
	if ghClient != nil {
		ghClient.SetAttributionHooks(github.AttributionHooks{
			TrailerEnabled: func() bool { return cfg.Governor.AttributionTrailerEnabled() },
		})
	}

	// Find or create the pinned advisory issue. Any level can have advisory
	// agents whose findings should be posted to this issue.
	advisoryIssues := map[string]int{}
	if acmmLevel > 0 && ghClient != nil {
		primaryRepo := cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
			primaryRepo = cfg.Project.Repos[0]
		}
		if primaryRepo != "" {
			num, err := ghClient.EnsureAdvisoryIssue(ctx, primaryRepo)
			if err != nil {
				logger.Error("failed to ensure advisory issue", "repo", primaryRepo, "error", err)
				// GitHub returns 403 for rate limiting too — a transient
				// condition that must not raise the "App Not Installed"
				// banner (matches the guard on the repo-change path).
				if isGitHubRateLimitText(err) {
					logger.Warn("GitHub API rate limit hit during advisory issue ensure", "repo", primaryRepo)
				} else {
					// Do NOT decide from the error string. This call site used
					// to raise the banner on a substring match for "403"/"401"
					// and set githubAppRequired=true BEFORE classifying, then
					// never lower it again when classification came back OK or
					// inconclusive — which is why the banner showed on boot and
					// vanished on the first Re-check with nothing fixed.
					// classifyGitHubAppFailure is the same verdict Re-check
					// uses, and it declines to raise on AppStateUnknown.
					raise, diag, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger)
					if raise {
						githubAppRequired = true
						githubAppDiag, githubAppState = diag, state
						logger.Warn("GitHub App authentication failed at startup",
							"state", state.String(),
							"operator_actionable", state.OperatorActionable(),
							"error", err)
					} else {
						logger.Warn("advisory issue ensure failed but GitHub App auth verified healthy — not raising the App banner",
							"repo", primaryRepo, "state", state.String(), "error", err)
					}
				}
			} else {
				advisoryIssues[primaryRepo] = num
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
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
	if err := os.MkdirAll(brainstormPolicyDir, 0o755); err != nil {
		logger.Warn("failed to create brainstorm policy dir", "path", brainstormPolicyDir, "error", err)
	}
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
		Org:             cfg.Project.Org,
		Repos:           cfg.Project.Repos,
		PrimaryRepoName: cfg.Project.PrimaryRepo,
		ACMMLevel:       acmmLevel,
		PRsAllowed:      cfg.Project.PRsAllowed(),
		PolicyDir:       policyDir,
		AppAuthoredPRs:  cfg.GitHub.AppAuthoredPRsEnabled(),
	}
	if cfg.GitHub.IsGHE() {
		projectCtx.GHHost = cfg.GitHub.HostLabel()
	}
	agentMgr := agent.NewManager(cfg.EnabledAgents(), logger, projectCtx)
	// SIGTERM (pod roll, hive upgrade) destroys every tmux server and with it
	// the in-flight kick's scrollback; archive it to /data first (#4296).
	archiveOnShutdown := func() { agentMgr.ArchiveAllKickLogs("shutdown") }
	preShutdownHooks.add("archive-kick-logs", archiveOnShutdown)
	agentMgr.SetSandboxConfig(cfg.AgentSandbox)

	// Say out loud when the sandbox opt-in is configured but inert. The gate is
	// two-part (global agent_sandbox.enabled AND a per-agent sandbox.enabled),
	// and the dashboard's Security tab writes only the global half — so an
	// owner can turn the sandbox on, be told the setting was updated, and still
	// have every agent running unconfined on the operator's own host.
	//
	// That silence is the part of #4918 that is safe to fix here. The gate
	// itself is load-bearing: a sandboxed agent runs a different execution
	// model and startSandboxKickLocked has no tmux fallback, so collapsing it
	// would convert working agents into permanently failing ones. Telling an
	// operator who believes they are covered that they are not costs nothing.
	logAgentSandboxPosture(logger, cfg)
	// Treat any configured gateway name as an inference-routable backend so an
	// agent with backend: <gateway> routes through it. Resolution is live
	// (reads cfg on each call) so gateways added from the Model Gateways tab
	// take effect without a restart.
	//
	// Wired HERE — immediately after the manager is constructed — and not in
	// the proxy/dashboard wiring further down, because SetBackendOverride
	// validates backend names against this predicate. The persisted-state
	// replay (restoreAgentRuntimeState, below) re-applies saved backend
	// overrides long before the dashboard wiring runs, and with the predicate
	// still unset every gateway-named override was rejected there — silently,
	// while the model override beside it restored fine. That is the #3961
	// asymmetric revert: an agent switched to a gateway backend came back on
	// its config backend but with the switched model still applied, producing
	// launch-dead hybrids like `pi --model gpt-5.6-luna`.
	agentMgr.SetGatewayBackendChecker(func(backend string) bool {
		return cfg.Governor.ResolveGateway(backend) != nil &&
			!strings.EqualFold(backend, "") // empty is the default, not a named backend
	})
	// Resolve the bob API key at LAUNCH time, not here: cfg is the live config
	// pointer (the config watcher swaps its contents in place on reload), so a
	// key added via the Secret mount, the PVC file, or a config edit takes
	// effect on the next agent launch with no hive restart. Only the key's
	// LOCATION is ever in cfg; the value is read from file/env on each call and
	// is never logged.
	agentMgr.SetBobAPIKeyResolver(func() string {
		return cfg.Governor.Bob.ResolveAPIKey()
	})
	// Hive-wide default explain mode, resolved per kick/launch off the live cfg
	// pointer for the same reason as the bob key above: an operator debugging a
	// misbehaving fleet turns explanation on from Settings → Governor and needs
	// it on the NEXT kick, not after a restart. Governor config wins over
	// HIVE_EXPLAIN_MODE; the env var stays as the fallback (#4712).
	agentMgr.SetExplainModeDefaultResolver(func() string {
		return cfg.Governor.ResolveExplainModeDefault()
	})
	// The launch path also needs to know WHICH FILE the key came from, so it can
	// check that file is readable by the agent UID rather than only by the hive
	// process. Returns a loggable source string, never the key value.
	agentMgr.SetBobKeySourceResolver(func() string {
		return cfg.Governor.Bob.ResolveAPIKeySource()
	})
	// Log only WHERE the key came from (or that none is set) so a misconfigured
	// hive is diagnosable without the value ever reaching the logs.
	if src := cfg.Governor.Bob.ResolveAPIKeySource(); src != "" {
		logger.Info("bob api key detected", "source", src)
	} else {
		logger.Info("no bob api key configured; agents with backend \"bob\" will not launch",
			"remedy", "set governor.bob.api_key_file or the "+config.DefaultBobAPIKeyEnv+" env var")
	}
	if appAuth != nil {
		agentMgr.SetAppAuth(appAuth)
		agentMgr.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: appAuth})
	}
	// Start the per-agent token refresh loop UNCONDITIONALLY. It no-ops until
	// App auth is wired, and on hosted spokes that wiring happens AFTER boot
	// (heartbeat delivery / config API reinit / config reload). Gating this on
	// appAuth != nil at boot meant those hives never refreshed per-agent token
	// caches: agent sessions outlived their scoped token, gh 401'd and printed
	// "gh auth login", and the login-detector auto-paused the agent (#4072).
	go agentMgr.StartAgentTokenRefresh(ctx)
	// Start the credential watchdog UNCONDITIONALLY. It self-gates per backend
	// on the presence of an agent using that backend each tick, so it is a
	// no-op on gateway/inference-only hives. On Copilot/Claude hives it turns a
	// missing or expired durable credential — the "stuck at login after an
	// upgrade roll" outage — into an immediate Audit Log signal instead of a
	// silent multi-hour stall.
	go agentMgr.StartCredentialWatchdog(ctx)
	// Keep the Copilot CLI's config.json copilotTokens populated from the
	// durable user token, so agents never sit stuck at "Please use /login"
	// while a valid token exists (CLI 1.0.78 does not re-populate the emptied
	// store from the injected env token on its own). Self-gates on a copilot
	// backend and only writes when the store is empty; never runs a login.
	go agentMgr.StartCopilotSessionRefresh(ctx)
	if ghClient != nil {
		agentMgr.SetSandboxPRClient(ghClient)
	}

	// PR-open-as-the-App-bot: agents push their branch (App-token credential
	// helper) then drop a request file; the hive opens the PR here with the App
	// token so it is authored by "<slug>[bot]", not the Copilot login user.
	// Gated on a real client + usable App — with no App there is no bot to author
	// as, and requests simply accumulate rather than opening under a wrong
	// identity. ghClient uses the App installation token (see ghAuth wiring).

	// Approval desk (RFC #4000): the single tool-approval decision point plus
	// its durable operator-lane inbox. Both are nil unless
	// `tool_approval.enabled` is set — the default — so this costs nothing and
	// changes nothing on a hive that has not opted in. Built here, before the
	// auto-merge sweep is started below, because the sweep is the one producer
	// wired in this slice. Also handed to the dashboard for the Approvals panel.
	approvalDesk, approvalInbox := buildApprovalDesk(cfg, logger)
	// Create the agent-facing request queues REGARDLESS of App state. The
	// watchers below stay gated (no App, no bot to author as), but the queues
	// must exist either way or the "requests simply accumulate" behavior above
	// is a fiction: hive-open-pr / hive-open-issue run in the AGENT's shell and
	// hard-fail on a missing directory, discarding the finding instead of
	// queueing it. App setup routinely completes after boot (operator saves the
	// installation ID, /gh-setup persists it, auto-discovery finds it later), so
	// this gap silently disarms agent writes on a hive that looks healthy.
	github.PrepareRequestDirs(logger)

	if ghClient != nil && cfg.GitHub.HasUsableApp() {
		// Attribution resolver: effective backend/model from the manager
		// (runtime overrides included), falling back to the configured values
		// for an agent the manager does not know; tool version resolved
		// lazily per backend and cached. Only launch descriptors flow here —
		// never tokens, keys, or prompt content.
		ghClient.SetAttributionResolver(func(agentName string) github.InvocationMeta {
			backend, model, effort, known := agentMgr.InvocationMetadata(agentName)
			if !known {
				if ac, inCfg := cfg.Agents[agentName]; inCfg {
					backend, model = ac.Backend, ac.Model
					// Same resolver the Manager uses, not a second copy of the
					// rule: a hardcoded default here would drift silently the
					// moment agy's default effort changed.
					effort = agent.ResolveReasoningEffort(backend, model)
				}
			}
			tool, toolVersion := github.ResolveToolVersion(backend)
			return github.InvocationMeta{
				Agent:   agentName,
				Backend: backend,
				// bob self-selects (no catalog): requested model is honestly
				// "auto" — see github.RequestedModel for the known follow-up
				// on discovering bob's internal routing.
				Model:       github.RequestedModel(backend, model),
				Effort:      effort,
				Tool:        tool,
				ToolVersion: toolVersion,
			}
		})
		// authz enforces the SAME per-agent ACMM write-gate + forge-resistance as
		// the direct `gh pr create` path — the request-file route grants no extra
		// privilege. A denied request is quarantined, never opened.
		// holdLabel (F6): at hold-gated ACMM levels (L3/L4/L5) every agent-opened
		// PR must carry the "hold" label so the merge gate holds it for human
		// approval. Outreach content is public speech on the project's behalf, so
		// it remains human-reviewed at L6 too. This is decided server-side from the
		// authenticated agent identity and authoritative hive level
		// (GetACMMLevel), NOT from a client flag — the gh-wrapper.sh tail that used
		// to add the label was dead code after `exec hive-open-pr`. L1/L2 open no
		// agent PRs (manual); non-outreach L6 PRs retain their existing automerge
		// behavior.
		holdLabel := func(agentName string) bool {
			return shouldHoldAgentPR(agentName, agentMgr.GetACMMLevel())
		}
		// #5117: tell the client which accounts are ours, so the
		// self-authorization gate recognises an issue filed under
		// project.ai_author's plain user account as hive-filed rather than
		// mistaking it for a human's. The App bot is recognised without this;
		// hiveIdentity() is the same resolver the duplicate-PR guard uses.
		ghClient.SetHiveIdentity(hiveIdentity(cfg))
		startRequestWatchers(ctx, requestwatch.New(ghClient, agentMgr.AuthorizePROpen, agentMgr.AuthorizeIssueOpen, holdLabel, nil), logger)
		// Review relay: agents request PR reviews by dropping a file (hive-review)
		// instead of running `gh pr review` in their own shell, which the hive
		// never observes. The watcher submits the review with the App token and
		// records it on the audit/activity trail, gated by the same
		// forge-resistance + push-capability (CanPush) check as opening a PR —
		// reviewing is a PR-write, so AuthorizePROpen is the correct gate.
		ghClient.StartReviewRequestWatcher(ctx, agentMgr.AuthorizePROpen, nil)
		// Merge relay: agents request merges by dropping a file (hive-merge)
		// instead of calling the GitHub MCP merge_pull_request tool, whose GraphQL
		// mutation GitHub rejects for App tokens ("Resource not accessible by
		// integration"). The hive merges over REST with the App token, gated by
		// the same forge-resistance + a CanMerge ACMM check.
		// bindMergeAuthz layers the F4 target-binding (CWE-863) on top of the
		// manager's agent/UID/CanMerge check: the merge must name a pinned head
		// SHA (no unpinned "merge whatever HEAD is now") AND the (repo, number)
		// must appear in the governor's current merge-eligible list — so an
		// injected agent cannot land an arbitrary reachable PR of its choosing.
		// Fix #2: on a terminal merge failure caused by a failing REQUIRED check,
		// re-engage the fix loop instead of abandoning the PR. The hook records a
		// re-engagement under the escalation store's per-red-SHA cap (shared with
		// the reaper so a PR is never double-dispatched beyond its budget) and
		// returns whether the cap still allowed a dispatch. The PR is already
		// surfaced into CI_FAILING by writeMergeEligible each eval tick; the hook
		// is the loop-safety authority that decides when to STOP nudging.
		ghClient.SetMergeReEngageHook(mergeReEngageHook(cfg))
		ghClient.StartMergeRequestWatcher(ctx, bindMergeAuthz(agentMgr.AuthorizeMerge), nil)

		// SECURITY (audit F3): re-verify the merger tier inside the sweep. The
		// dashboard's queue endpoint gates on requireMergerOrOwnerRole, but the
		// sweep merges a minute later off the label + App-authored approval body
		// alone, so without this ANY actor who can get the label applied merges
		// anything, and a sockpuppet pair defeats the self-merge ban. Resolved
		// against the SAME allowlist the dashboard uses so there is one notion of
		// trust; read through cfg on every call so a config reload takes effect.
		autoMergeOpts := automerge.Options{
			Logger:           logger,
			MergerAuthorizer: trustedMergerFunc(cfg),
		}

		// commitGreen's required-checks gate (self-merge sweep, see
		// automerge_sweep.go): install the operator-declared
		// auto_merge.required_checks list, if any, so gating does not depend
		// on GitHub's branch-protection API — the Hive App token lacks
		// administration:read, so that API call reliably errors and would
		// otherwise fail closed to the coarser isMetaCheck/isIgnorableCICheck
		// allowlist. Unset/empty leaves the API/allowlist fallback chain
		// intact (SetRequiredChecks(nil) is a safe no-op).
		if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
			autoMergeOpts.RequiredChecks = set
		}

		// Self-authored auto-merge: the App merges its OWN open, CI-green PRs
		// directly over the REST API, without a human "Approved ... for Hive
		// auto-merge" queue review and without waiting on tide. Prow forbids
		// self-approval (lgtm+approved must come from someone other than the
		// author), and the author here is always the App itself, so the
		// human-queue path (StartMergeRequestWatcher above / the governor
		// sweep) can never clear for the App's own PRs — this is the only
		// route that lands them. See AutoMergeConfig and
		// SweepSelfAuthoredAutoMerges for the full rationale and the safety
		// properties preserved (green required checks, head-SHA re-verified
		// immediately before merge, squash method, all tiers included).
		// Default ON; `auto_merge.self_authored: false` disables it. ALSO
		// gated on ACMM level (config.SelfMergeMinACMMLevel): l4.md/l5.md
		// both forbid the App merging its own PRs, so an L4/L5 hive must
		// never start this loop regardless of the flag above — see
		// AutoMergeConfig.SelfAuthoredAutoMergeAllowed. StartSelfAuthoredAutoMergeSweep
		// itself no-ops (with a one-time INFO log) when acmmAllowed is false.
		// Approval desk (RFC #4000). Installed BEFORE the sweep starts so the
		// first tick already consults it. A nil desk (the default —
		// `tool_approval.enabled` is false) installs no hook, leaving the
		// sweep's behavior byte-identical to the pre-desk build.
		if approvalDesk != nil && approvalInbox != nil {
			autoMergeOpts.ApprovalDesk = newSelfMergeDeskHook(approvalDesk, approvalInbox, cfg, logger)
		}
		automerge.StartSelfAuthoredAutoMergeSweep(ctx, ghClient, cfg.AutoMerge.MaxMerges, cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(cfg.ACMMLevel), cfg.ACMMLevel, autoMergeOpts)
	}

	// Opt-in mint credential: when mint.enabled, build a Minter from the config
	// (signing key + issuer + TTL) and attach it so each per-agent token refresh
	// ALSO issues a scoped short-lived OIDC token alongside the GitHub App token.
	// Default off — an absent/disabled `mint:` block leaves the credential path
	// byte-identical. Fail-safe: a mint setup error is logged, never fatal.
	if cfg.Mint.Enabled {
		if agentMinter, err := buildAgentMinter(cfg, logger); err != nil {
			logger.Warn("mint enabled but minter setup failed; agents keep App token only", "error", err)
		} else {
			agentMgr.SetAgentMint(agentMinter)
			logger.Info("mint credential enabled", "issuer", cfg.Mint.Issuer, "hive_id", cfg.HiveID)
		}
	}

	go agent.StartPermissionsWatcher(logger)

	const statePath = "/data/hive-state.json"
	saved, stateErr := snapshot.LoadState(statePath, logger)
	if stateErr != nil {
		logger.Warn("failed to load persisted state", "error", stateErr)
	} else if saved != nil {
		restoreAgentRuntimeState(saved, cfg, agentMgr, logger)
		// Re-establish the fleet breaker AFTER per-agent pauses are restored
		// above: the agents it held are already back in the paused state (with
		// PausedTrigger == fleet-breaker from their persisted pause), so this
		// only re-attaches the breaker so a later release resumes exactly them.
		// An engaged breaker must REMAIN engaged across restart — it does not
		// auto-release, and it does not re-pause or resume anything here.
		if saved.Breaker != nil && saved.Breaker.Engaged {
			agentMgr.RestoreBreaker(true, saved.Breaker.Paused)
			logger.Info("fleet breaker restored from state", "held", len(saved.Breaker.Paused))
		}
		if saved.BudgetLimit > 0 {
			gov.SetBudgetLimit(saved.BudgetLimit)
		}
		if saved.BudgetIgnoreAll {
			gov.SetBudgetIgnoreAll(true)
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
					mode.Cadences = make(map[string]config.Cadence)
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
			gov.SeedBudgetWindowBaseline(saved.BudgetWindowBaseline)
			logger.Info("budget state restored", "spend", saved.BudgetSpend, "reset_at", saved.BudgetResetAt, "window_baseline", saved.BudgetWindowBaseline)
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
				ghClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
			}
			ghClient.SetIssueFilter(cfg.Project.IssueFilter)
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
	// SIGTERM (pod roll, hive self-upgrade) kills the process and every
	// contributor WebSocket with it, and until #5390 it did so without a word:
	// the peer saw a bare 1006, indistinguishable from a network fault, which is
	// what made #5090 take days to diagnose. Send each contributor a 1012
	// (CloseServiceRestart) first so the relay knows to reconnect immediately —
	// into the replacement pod, which maxSurge=1/maxUnavailable=0 has already
	// brought to readiness before this signal was delivered.
	//
	// Registered as its OWN hook rather than folded into archiveOnShutdown: the
	// two are unrelated, and the drain must not be able to prevent the archive
	// from running. addUrgent, not add, because it is the time-critical half —
	// the sooner the frame is on the wire the sooner the relay reconnects,
	// whereas the kick-log archive does PVC I/O on NFS and nobody is waiting on
	// it. The hub is resolved lazily inside the closure because the contributor
	// hub is not constructed until registerContributeRoutes runs, below.
	preShutdownHooks.addUrgent("drain-contributor-websockets", func() {
		dashSrv.DrainContributorsForShutdown()
	})
	var beadStores map[string]*beads.Store

	// Wire ioscan input enforcement (opt-in via ioscan.enabled) to the dashboard
	// audit log so a blocked/redacted issue title surfaces in the existing
	// audit-trail UI with no new sink. The closure keeps pkg/scheduler decoupled
	// from pkg/dashboard — the scheduler only knows a func(action, detail, agent).
	sched.SetAuditFunc(func(action, detail, agent string) {
		dashSrv.AuditLog(agent, action, detail, agent)
	})
	sched.SetAdvisoryFunc(func(title, detail, agentName string) {
		store := beadStores[agentName]
		if store == nil {
			store = beadStores["scanner"]
		}
		if store == nil {
			store = beadStores["supervisor"]
		}
		if store == nil {
			for _, candidate := range beadStores {
				store = candidate
				break
			}
		}
		if store != nil {
			if b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, agentName, ""); err == nil {
				_ = store.SetMetadata(b.ID, "ioscan_classifier", detail)
			}
		}
	})
	if cfg.Ioscan.IsEnabled() && cfg.Ioscan.Classifier.Enabled {
		endpoint, apiKey, model := cfg.Governor.ResolveReviewer()
		if cfg.Ioscan.Classifier.Model != "" {
			model = cfg.Ioscan.Classifier.Model
		}
		if model == "" {
			model = ioscan.DefaultClassifierModel
		}
		classifier, cerr := ioscan.NewLLMClassifier(ioscan.LLMClassifierConfig{
			Endpoint: endpoint,
			APIKey:   apiKey,
			Model:    model,
		})
		if cerr != nil {
			logger.Warn("ioscan classifier enabled but not running", "reason", cerr.Error())
		} else {
			sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), ioscan.Thresholds{
				Warn:  cfg.Ioscan.Classifier.WarnThreshold,
				Block: cfg.Ioscan.Classifier.BlockThreshold,
			})
			logger.Info("ioscan semantic classifier enabled", "model", model)
		}
	}
	agentMgr.SetSandboxAuditCallback(func(agentName, action, detail string) {
		dashSrv.AuditLog(agentName, action, detail, agentName)
		if action == "sandbox_broker_rejected" {
			if store, ok := beadStores[agentName]; ok && store != nil {
				if b, err := store.Create("Sandbox push broker rejected changes", beads.TypeAdvisory, beads.PriorityHigh, agentName, ""); err == nil {
					_ = store.SetMetadata(b.ID, "sandbox_broker_rejection", detail)
				}
			}
		}
	})

	// Persist per-user dashboard sessions on the PVC (/data) so direct-route
	// users aren't logged out by pod restarts. NOTE: use /data explicitly, NOT
	// filepath.Dir(configPath) — the config lives at /etc/hive/hive.yaml, which
	// is an ephemeral emptyDir (the ConfigMap seed mount), so a sessions file
	// there is wiped on every pod roll. That was the "re-login on every visit"
	// bug on direct-route spokes. /data is the CephFS PVC (same place cost/fact
	// history persist).
	dashSrv.EnableSessionPersistence("/data/dashboard-sessions.json")

	// Lifecycle timeline journeys persist on the PVC too (#5656): the ring is
	// the panel's only memory of merged/blocked outcomes, so a pod roll must
	// not zero the fleet counters. Enabled before any producer records.
	dashSrv.EnableLifecyclePersistence("/data/lifecycle-timeline.json")

	// The scheduler's classifier pass records KindClassified journeys the
	// moment lane routing decides an issue's lane — same store, no extra work.
	sched.SetLifecycleRecorder(dashSrv.LifecycleTimeline())

	// Attribution audit sink: every hive-mediated PR/issue creation lands in
	// the dashboard audit log (audit.jsonl + ring) UNCONDITIONALLY — the
	// trailer toggle never gates this. Creations before this point (the
	// startup advisory-issue ensure) fall back to the hive log inside
	// recordCreationAudit, so no creation goes unrecorded. The same stream
	// feeds the lifecycle timeline: agent_pr_created → pr_opened and
	// pr_merged → merged (both automerge sweep paths, MergePR from the
	// dashboard queue and the merge watcher), see recordLifecycleFromAudit.
	if ghClient != nil {
		ghClient.SetAttributionAudit(func(action, detail, agent string) {
			dashSrv.AuditLog("system", action, detail, agent)
			recordLifecycleFromAudit(dashSrv, cfg.Project.Org, action, detail, agent)
		})
	}

	// Seed token sparkline history now that the dashboard server exists
	if len(pendingTokenSeed) > 0 {
		dashSrv.SeedTokenSparklineHistory(pendingTokenSeed)
		logger.Info("token sparkline history restored", "entries", len(pendingTokenSeed))
	}

	if len(pendingFactSeed) > 0 {
		dashSrv.SeedFactHistory(pendingFactSeed)
		logger.Info("fact history restored", "entries", len(pendingFactSeed))
	}

	if len(pendingCostSeed) > 0 {
		dashSrv.SeedCostHistory(pendingCostSeed)
		logger.Info("cost history restored", "entries", len(pendingCostSeed))
	}

	if len(pendingBudgetWindowSeed) > 0 {
		dashSrv.SeedBudgetWindowHistory(pendingBudgetWindowSeed)
		logger.Info("budget window history restored", "entries", len(pendingBudgetWindowSeed))
	}
	if len(pendingConvergenceSoakSeed) > 0 {
		dashSrv.SeedConvergenceSoak(pendingConvergenceSoakSeed)
		logger.Info("convergence soak history restored", "entries", len(pendingConvergenceSoakSeed))
	}

	if len(pendingTrendSeed) > 0 {
		dashSrv.SeedTrendHistory(pendingTrendSeed)
		logger.Info("trend history restored", "entries", len(pendingTrendSeed))
	}

	beadStores = make(map[string]*beads.Store)
	// Count stores that fail to open. They are dropped from beadStores entirely,
	// which makes an incomplete ledger indistinguishable from a smaller one — and
	// the dependency admission gate must not read a lookup miss in a truncated
	// ledger as "this candidate declared no dependencies".
	beadStoreLoadFailures := 0
	for name, agentCfg := range cfg.EnabledAgents() {
		store, err := beads.NewStore(agentCfg.BeadsDir)
		if err != nil {
			logger.Warn("failed to init beads store", "agent", name, "error", err)
			beadStoreLoadFailures++
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
				beadStoreLoadFailures++
				continue
			}
			store.SetHiveID(cfg.HiveID)
			beadStores[name] = store
			logger.Info("orphan beads store loaded from disk", "agent", name, "count", store.Count())
		}
	}

	if cfg.Retro.Enabled {
		if _, exists := beadStores[retro.Actor]; !exists {
			retroStore, err := beads.NewStore(filepath.Join(beadsRootDir, retro.Actor))
			if err != nil {
				logger.Warn("failed to init retro beads store", "error", err)
				beadStoreLoadFailures++
			} else {
				retroStore.SetHiveID(cfg.HiveID)
				beadStores[retro.Actor] = retroStore
				logger.Info("retro beads store initialized", "count", retroStore.Count())
			}
		}
	}

	initAgentConfigDrivenSystems(cfg)

	tokenCollector := tokens.NewCollector(cfg.Data.MetricsDir, logger)
	tokenCollector.SetClaudeSessionsDir(cfg.Data.ClaudeSessionsDir)
	tokenCollector.SetCopilotSessionsDir(cfg.Data.CopilotSessionsDir)
	tokenCollector.SetBobSessionsDir(cfg.Data.BobSessionsDir)
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
	fleetStatsAuthor := cfg.EffectiveAIAuthor()
	fleetStatsToken := cfg.GitHub.Token
	if fleetStatsToken == "" {
		fleetStatsToken = os.Getenv("HIVE_GITHUB_TOKEN")
	}
	if fleetStatsAuthor == "" && fleetStatsToken != "" {
		if botUser, err := github.ValidateToken(fleetStatsToken, cfg.GitHub.ResolvedAPIURL()); err == nil && botUser.Login != "" {
			fleetStatsAuthor = botUser.Login
			logger.Info("fleet stats: ai_author unset, using bot token identity",
				"author", fleetStatsAuthor)
		} else if err != nil {
			logger.Warn("fleet stats: ai_author unset and bot identity lookup failed; "+
				"this hive will not contribute to the public fleet-stats total",
				"error", err)
		}
	}
	if fleetStatsAuthor == "" || cfg.Project.Org == "" {
		logger.Warn("fleet stats collector disabled: author or org is empty; "+
			"set project.ai_author in hive.yaml so this hive contributes to the fleet total",
			"author", fleetStatsAuthor, "org", cfg.Project.Org)
	}
	fleetStatsCollector := collect.NewFleetStatsCollector(ghClient, fleetStatsAuthor, cfg.Project.Org, logger)
	// Persist the collected counts on the /data PVC (same store as sessions and
	// cost/fact history) so a restart resumes from the last-known counts instead
	// of nil. Without this, a fleet-wide upgrade clears every spoke's in-memory
	// counts and the public landing-page total collapses until all spokes
	// re-collect (#2329, building on the hub-side #2328 defensive aging fix).
	fleetStatsCollector.EnablePersistence("/data/fleet-stats.json")
	go fleetStatsCollector.Start(ctx)

	// Per-repo output-activity collector: reads the local audit log (no GitHub
	// calls) and summarizes issues/PRs/comments/merges/claims/reviews per repo
	// with recency, so the hub can tell — from the heartbeat alone — whether each
	// hive is producing output back to its work source. Persisted to the /data
	// PVC so a restart resumes the last summary; the collector loop reads
	// /data/audit.jsonl every few minutes.
	activityCollector := collect.NewActivityCollector(dashSrv.GetAudit(), "", logger)
	activityCollector.EnablePersistence("/data/activity.json")
	go activityCollector.Start(ctx)

	// Per-repo cost collector: joins the same audited output events against
	// the token collector's per-message usage timeline, on the same ticker
	// interval as the activity collector above, and caches the result for
	// /api/repo-cost. Before this (#4943), the interval join — including
	// the same expensive audit read the activity collector does — ran on
	// every 60s dashboard poll, per open browser tab, instead of once per
	// collection interval.
	repoCostCollector := collect.NewRepoCostCollector(dashSrv.GetAudit(), tokenCollector, "", logger)
	repoCostCollector.EnablePersistence("/data/repo-cost.json")
	go repoCostCollector.Start(ctx)

	// Persistent hourly metrics behind the Operations + Leaderboard sparklines
	// (queue depth, tasks/hour, fleet size, per-contributor completions). The
	// store loads any prior 7-day history from the /data PVC on first use and the
	// rollup goroutine samples + buckets hourly, so a rolling upgrade resumes the
	// trend instead of flattening it. Bound to ctx so it shuts down cleanly with
	// the rest of the background loops (no goroutine leak). See contribute_metrics.go.
	dashSrv.StartContributeMetrics(ctx)

	var lastActionable atomic.Pointer[github.ActionableResult]
	refreshDashboard := func() {
		// Capture the mutation epoch BEFORE reading any state: if a mutation
		// (e.g. a restart-count or budget-window reset) lands while this
		// snapshot is being built, UpdateStatusIfFresh drops it so the stale
		// values never overwrite what the mutation's own refresh will publish
		// (#4348 — the restart-count flicker).
		buildEpoch := dashSrv.BeginStatusSnapshot()
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
		dashSrv.UpdateStatusIfFresh(payload, buildEpoch)
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
		// The curator block was previously dropped here, so NewPromoter always
		// received a zero CuratorConfig and AutoPromoteThreshold never reached
		// the promoter in production. Passing it through is what makes the
		// threshold gate real for the scheduled sweep (#5430).
		knowledgeAPI = knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{
			Enabled: cfg.Knowledge.Enabled,
			Engine:  cfg.Knowledge.Engine,
			Curator: curatorConfigFromHive(cfg.Knowledge.Curator),
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
			if primer := sched.GetPrimer(); primer != nil {
				store := knowledgeAPI.GetVaultStore(vc.Path)
				if store != nil {
					primer.AddFileStore(vc.Name, store, knowledge.LayerPersonal)
					logger.Info("vault registered with primer", "name", vc.Name)
				}
			}
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

	// Auto-import configured document sources (PDFs, URLs as knowledge)
	for _, doc := range cfg.Knowledge.Documents {
		if knowledgeAPI == nil {
			knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
				Enabled: true,
				Engine:  "file",
			}, logger)
			logger.Info("auto-enabled knowledge API for document sources")
		}
		docConfig := knowledge.DocSourceConfig{
			Name:     doc.Name,
			URL:      doc.URL,
			FilePath: doc.FilePath,
			Layer:    knowledge.LayerType(doc.Layer),
		}
		meta, err := knowledgeAPI.ImportDocument(ctx, docConfig)
		if err != nil {
			logger.Warn("failed to import document source",
				"name", doc.Name,
				"error", err,
			)
		} else {
			logger.Info("document source imported",
				"name", doc.Name,
				"facts", meta.FactCount,
				"content_type", meta.ContentType,
			)
		}
	}

	go gitSyncer.Start(ctx)

	// Auto-enable knowledge API when not explicitly configured.
	// Both bead-synth-wiki and inception require it.
	if knowledgeAPI == nil {
		knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
			Enabled: true,
			Engine:  "file",
		}, logger)
		logger.Info("auto-enabled file-based knowledge API")
	}

	var beadSynth *knowledge.BeadSynthesizer
	if len(beadStores) > 0 {
		synthVaultPath := cfg.Knowledge.BeadSynthesizer.VaultPath
		if synthVaultPath == "" {
			synthVaultPath = "/data/vaults/bead-synth-wiki"
		}
		if err := os.MkdirAll(synthVaultPath, 0o755); err != nil {
			logger.Warn("failed to create bead-synth vault dir", "path", synthVaultPath, "error", err)
		}
		if knowledgeAPI != nil {
			if connErr := knowledgeAPI.ConnectVault(synthVaultPath, "bead-synth-wiki"); connErr != nil {
				logger.Warn("failed to auto-connect bead-synth vault", "path", synthVaultPath, "error", connErr)
			} else {
				logger.Info("auto-connected bead-synth vault", "path", synthVaultPath)
				if primer := sched.GetPrimer(); primer != nil {
					store := knowledgeAPI.GetVaultStore(synthVaultPath)
					if store != nil {
						beadLayer := knowledge.LayerType(cfg.Knowledge.BeadSynthesizer.TargetLayer)
						if beadLayer == "" {
							beadLayer = knowledge.LayerPersonal
						}
						primer.AddFileStore("bead-synth-wiki", store, beadLayer)
						logger.Info("bead-synth vault registered with primer", "layer", beadLayer)
					}
				}
			}
		}
		var rawGH *gh.Client
		if ghClient != nil {
			rawGH = ghClient.GoGitHub()
		}

		var kRetention *knowledge.RetentionPolicy
		if rp := cfg.Knowledge.BeadSynthesizer.RetentionPolicy; rp != nil {
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

		beadSynth = knowledge.NewBeadSynthesizer(beadStores, knowledgeAPI, knowledge.BeadSynthesizerConfig{
			Schedule:         cfg.Knowledge.BeadSynthesizer.Schedule,
			MinConfidence:    cfg.Knowledge.BeadSynthesizer.MinConfidence,
			TargetLayer:      cfg.Knowledge.BeadSynthesizer.TargetLayer,
			MaxFactsPerCycle: cfg.Knowledge.BeadSynthesizer.MaxFactsPerCycle,
			VaultPath:        synthVaultPath,
			Org:              cfg.Project.Org,
			Repos:            cfg.Project.Repos,
			RetentionPolicy:  kRetention,
		}, logger, rawGH)

		if cleaned, err := beadSynth.CleanupVault(); err != nil {
			logger.Warn("vault cleanup failed", "error", err)
		} else if cleaned > 0 {
			logger.Info("cleaned up low-quality bead-synth facts", "removed", cleaned)
		}

		if cfg.Knowledge.BeadSynthesizer.IsEnabled() && knowledgeAPI != nil {
			beadSynth.StartBackground(ctx)
			logger.Info("bead-to-wiki synthesizer started",
				"schedule", cfg.Knowledge.BeadSynthesizer.Schedule,
				"target_layer", cfg.Knowledge.BeadSynthesizer.TargetLayer,
				"vault_path", synthVaultPath,
				"bead_stores", len(beadStores),
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
	if knowledgeAPI != nil && cfg.Knowledge.Curator.IsEnabled() {
		promotionScheduler := knowledge.NewPromotionScheduler(
			knowledgeAPI.Promoter(),
			curatorConfigFromHive(cfg.Knowledge.Curator),
			logger,
		)
		promotionScheduler.StartBackground(ctx)
	} else if cfg.Knowledge.Curator.Schedule != "" {
		logger.Info("knowledge.curator.schedule is set but scheduled promotion is disabled",
			"schedule", cfg.Knowledge.Curator.Schedule,
			"hint", "set knowledge.curator.enabled: true to opt in",
		)
	}

	// Open the graph store in a background goroutine. NewGraphStore acquires
	// a SQLite file lock that blocks if the old pod still holds it. Deferring
	// this lets the HTTP server start so the readiness probe passes, which
	// tells Kubernetes to terminate the old pod and release the lock.
	const graphStorePath = "/data/graph/knowledge.db"
	go func() {
		graphStore, graphErr := knowledge.NewGraphStore(graphStorePath, logger)
		if graphErr != nil {
			logger.Warn("failed to open knowledge graph store", "path", graphStorePath, "error", graphErr)
			return
		}
		logger.Info("knowledge graph store opened", "path", graphStorePath)
		if primer := sched.GetPrimer(); primer != nil {
			primer.SetGraphStore(graphStore)
		}
		if knowledgeAPI != nil {
			knowledgeAPI.SetGraphStore(graphStore)
			if primer := sched.GetPrimer(); primer != nil {
				knowledgeAPI.WireContext7Suggester(primer)
			}
		}
		if beadSynth != nil {
			beadSynth.SetGraphStore(graphStore)
		}
		if knowledgeAPI != nil {
			for _, ls := range knowledgeAPI.FileStores() {
				if n, err := graphStore.SyncFromFileStore(ls); err != nil {
					logger.Warn("graph sync failed", "store", ls.Name(), "error", err)
				} else if n > 0 {
					logger.Info("graph synced from vault", "store", ls.Name(), "triples", n)
				}
			}
		}
	}()

	go dashboard.StartWorkspaceCleanup(ctx, logger, dashSrv.GetAudit())

	if err := os.MkdirAll(nousSnapshotDir, 0o755); err != nil {
		logger.Warn("failed to create nous snapshot dir", "path", nousSnapshotDir, "error", err)
	}
	if err := os.MkdirAll(nousGovernorDir, 0o755); err != nil {
		logger.Warn("failed to create nous governor dir", "path", nousGovernorDir, "error", err)
	}
	nousState := loadNousState(logger)
	nousState.SnapshotDir = nousSnapshotDir

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

	// Provider rotation (RFC #3958): opt-in automatic failover when a
	// provider's subscription/credit is exhausted. Nil when disabled.
	var rotationMgr *rotation.Manager
	if cfg.Governor.Rotation.Enabled {
		rotationMgr = rotation.NewManager(cfg.Governor.Rotation)
		rotationMgr.Start(ctx)
		logger.Info("provider rotation enabled",
			"threshold_pct", cfg.Governor.Rotation.EffectiveThreshold(),
			"providers", len(cfg.Governor.Rotation.Providers))
	}

	// Agent self-healing watchdog (RFC #4665): liveness/readiness
	// reconciliation on the governor tick. Config problems fall back to the
	// RFC defaults loudly — a typo must not disable self-healing silently.
	wdSettings, wdCfgErrs := watchdog.SettingsFrom(cfg.Governor.Watchdog)
	for _, e := range wdCfgErrs {
		logger.Warn("watchdog config problem", "error", e)
	}
	var wd *watchdog.Reconciler
	if wdSettings.Enabled() {
		wdFleet := agent.WatchdogFleet{
			M: agentMgr,
			// Queue depth for the readiness gate: an agent producing nothing
			// while nothing is queued is correct, not unhealthy. Read live
			// from the governor so it reflects the current sweep.
			Queued: func() (int, bool) {
				st := gov.GetState()
				return st.QueueIssues + st.QueuePRs, true
			},
		}
		wd = watchdog.New(wdSettings, wdFleet, dashSrv, logger,
			watchdog.WithAuthProbes(watchdogAuthProbes(cfg)))
		if saved != nil && len(saved.Watchdog) > 0 {
			wd.Restore(saved.Watchdog)
		}
		// Dead-session recovery moves under the watchdog's bounded ladder ONLY
		// when the watchdog may actually act. In observe mode the manager's
		// crash loop keeps its existing job, so there is never a window in
		// which neither component restarts a dead agent.
		agentMgr.SetDeadSessionRecoveryOwner(wdSettings.MayAct())
		logger.Info("agent watchdog enabled (RFC #4665)",
			"mode", string(wdSettings.Mode),
			"probe_interval", wdSettings.ProbeInterval,
			"crash_loop_after", wdSettings.CrashLoopAfter,
			"auth_probe", wdSettings.AuthProbe,
			"dead_session_recovery", map[bool]string{true: "watchdog", false: "crash-loop"}[wdSettings.MayAct()])
		if wdSettings.Mode == watchdog.ModeObserve {
			logger.Info("agent watchdog is in OBSERVE mode: it will classify agents, publish conditions and record what it WOULD have done, but will not restart or pause anything. Set governor.watchdog.mode: heal to enable healing.")
		}
	} else {
		logger.Info("agent watchdog disabled by config", "mode", string(wdSettings.Mode))
	}

	// Linear write credential for ISSUES_ONLY+ agents (GitHub-issue parity):
	// prefer the connected Linear agent app's OAuth token, so agent writes are
	// authored by the same "Hive" app identity that acknowledges sessions —
	// the analogue of App-bot authorship on GitHub — and fall back to the
	// work-source API key from hive.yaml. Resolved live off the dashboard's
	// install store and the cfg pointer so a workspace connected after boot
	// reaches agents on their next launch / hourly token refresh. Values are
	// never logged. Wired before RegisterAPI so the resolver is in place
	// before any agent launches.
	agentMgr.SetLinearCredentialResolver(func() agent.LinearCredential {
		if tok := dashSrv.LinearAgentAccessToken(); tok != "" {
			return agent.LinearCredential{AccessToken: tok}
		}
		if cfg.Governor.WorkSource.Type == "linear" {
			return agent.LinearCredential{APIKey: strings.TrimSpace(cfg.Governor.WorkSource.Linear.APIKey)}
		}
		return agent.LinearCredential{}
	})

	// In-flight ledger + session PR link (Linear GitHub-parity follow-ups):
	// the scheduler withholds work a Linear session is already working, and
	// the pr-request watcher narrates opened PRs into the session.
	sched.SetInflightLookup(dashSrv.LinearSessionHolder)
	if ghClient != nil {
		ghClient.SetPROpenedHook(func(agentName, repo string, number int, url string) {
			dashSrv.LinearAgentPROpened(agentName, repo, number, url)
			// Same typed hook feeds the lifecycle timeline: the watcher fires
			// it on the exact path that opened the PR, with the agent name the
			// audit stream attributes to the governor flow (#5656). The store
			// dedupes with the audit-sink bridge by (ref, kind).
			recordPROpened(dashSrv, cfg.Project.Org, agentName, repo, number, url)
		})
	}

	dashSrv.RegisterAPI(&dashboard.Dependencies{
		Config:   cfg,
		AgentMgr: agentMgr,
		// Provider gateways (#5565 slice 3): concrete openrouter/watsonx/
		// linearagent adapters behind the dashboard's consumer-defined
		// interfaces — this composition root is the only non-test place that
		// names the concrete types.
		Watsonx:              watsonxGateway{},
		OpenRouter:           openRouterGateway{},
		NewLinearAgent:       newLinearAgentGateway(logger),
		LinearStoredViewerID: linearStoredViewerID,
		Governor:             gov,
		GHClient:             ghClient,
		GHAppAuth:            appAuth,
		GHTokenScopes:        ghAuth.TokenScopes,
		Tokens:               tokenCollector,
		Knowledge:            knowledgeAPI,
		Inception:            inceptionEngine,
		Nous:                 nousState,
		Scheduler:            sched,
		MetricsCollector:     metricsCollector,
		RotationMgr:          rotationMgr,
		// #3972: hand the ACMM advisor the SAME cached fleet-stats collector
		// the heartbeat reads, so its merge-success signal reuses the existing
		// 30-minute collect loop instead of issuing a second GitHub fetch.
		FleetStats:            fleetStatsCollector,
		Activity:              activityCollector,
		RepoCost:              repoCostCollector,
		BeadSynthesizer:       beadSynth,
		BeadStores:            beadStores,
		BeadStoreLoadFailures: beadStoreLoadFailures,
		// RFC #4000 approval desk. Nil unless `tool_approval.enabled`, in which
		// case the Approvals panel renders as "not enabled".
		ApprovalDesk:  approvalDesk,
		ApprovalInbox: approvalInbox,
		Logger:        logger,
		Ctx:           ctx,
		RefreshFunc:   refreshDashboard,
		// #3768: give the contribute queue read access to the duplicate-PR
		// claim ledger, so an issue any open PR (hive-authored or a human
		// contributor's) already claims to fix is never offered to another
		// contributor. Lazy: the ledger loads on first use, same as the
		// eval-cycle guard.
		IssueClaimed: func(repo string, number int) (github.IssueClaim, bool) {
			return getClaimLedger(logger).Lookup(repo, number)
		},
		HookFire: func(ctx context.Context, p hooks.Payload) {
			hookDispatcher().Fire(ctx, p)
		},
		PersistFunc: func() {
			persistState(agentMgr, gov, cfg, statePath, logger, dashSrv, wd)
		},
		ReInitFunc: func() {
			initAgentConfigDrivenSystems(cfg)
		},
		EnumerateFunc: func() {
			runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, nil, approvalDesk, logger)
		},
		AdvisoryResetFunc: func(newPrimaryRepo string) {
			logger.Info("advisory reset: primary repo changed, creating new advisory issue", "repo", newPrimaryRepo)
			if ghClient != nil {
				num, err := ghClient.EnsureAdvisoryIssue(ctx, newPrimaryRepo)
				if err != nil {
					logger.Error("failed to create advisory issue on new primary repo", "repo", newPrimaryRepo, "error", err)
					if isGitHubRateLimitText(err) {
						logger.Warn("GitHub API rate limit hit during advisory issue creation", "repo", newPrimaryRepo)
					} else {
						// #2224 replaced error-string classification everywhere
						// else but missed this site, which raised the banner on
						// a bare "403"/"401" substring and recorded no state at
						// all — so the UI fell back to "App Not Installed" even
						// for an operator-side key fault. Classify properly.
						raise, diag, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger)
						if raise {
							dashSrv.SetGitHubAppRequired(true)
							dashSrv.SetGitHubAppState(state.String())
							if diag != "" {
								dashSrv.SetGitHubAppPermIssue(diag)
							}
							logger.Warn("GitHub App authentication failed creating advisory issue",
								"repo", newPrimaryRepo, "state", state.String(),
								"operator_actionable", state.OperatorActionable())
						}
					}
				} else {
					advisoryIssues[newPrimaryRepo] = num
					_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
					dashSrv.SetGitHubAppRequired(false)
					dashSrv.ClearPendingGitHubAppInstall()
					logger.Info("advisory issue ready on new primary repo", "repo", newPrimaryRepo, "number", num)
				}
			}
		},
		ReinitGitHubFunc: func(newAppID, newInstallationID int64, keyFile string) error {
			newAppAuth, err := github.NewAppAuth(newAppID, newInstallationID, keyFile, logger, cfg.GitHub.ResolvedAPIURL())
			if err != nil {
				return fmt.Errorf("initializing app auth: %w", err)
			}
			newClient := github.NewClientFromAppWithBotLogin(newAppAuth, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.BotLogin())
			if len(cfg.Governor.Labels.Exempt) > 0 {
				newClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
				newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
			}
			newClient.SetIssueFilter(cfg.Project.IssueFilter)

			ghClient = newClient
			appAuth = newAppAuth
			agentMgr.SetAppAuth(newAppAuth)
			// Deliver fresh per-agent scoped tokens to already-running agents
			// immediately — the periodic refresh loop only ticks every 40m,
			// far too long for agents whose caches are empty or stale (#4072).
			go agentMgr.RefreshAgentTokens(ctx)
			dashSrv.UpdateGitHubClient(newClient, newAppAuth)
			logger.Info("github client reinitialized via config API", "app_id", newAppID, "installation_id", newInstallationID)

			primaryRepo := cfg.Project.PrimaryRepo
			if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
				primaryRepo = cfg.Project.Repos[0]
			}
			if primaryRepo != "" {
				num, advErr := ghClient.EnsureAdvisoryIssue(ctx, primaryRepo)
				if advErr != nil {
					logger.Warn("advisory issue creation failed after reinit", "repo", primaryRepo, "error", advErr)
				} else {
					advisoryIssues[primaryRepo] = num
					_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
					logger.Info("advisory issue ready after reinit", "repo", primaryRepo, "number", num)
				}
			}
			return nil
		},
		// Same key resolution as boot (initGitHubAuth) and the heartbeat apply
		// path: without it, the dashboard Set ID handler gated reinit on the
		// raw key_file, which is deliberately empty on hub-delivered per-app-id
		// keys (#2459).
		ResolveAppKeyFileFunc: func(configured string, appID int64) string {
			return appKeys.Resolve(configured, os.Getenv("GH_APP_KEY_FILE"), appID)
		},
	})

	// Forge App tab inventory: the resolved active key path and the per-app-id
	// PVC keys live here in cmd/hive, so they are injected as a provider (the
	// SetGitHubAppRecheckFn pattern). Fingerprints and paths only — the
	// provider never touches key material.
	dashSrv.SetForgeAppInventoryFn(func() dashboard.ForgeAppInventory {
		held := appKeys.HeldFingerprints()
		keys := make([]dashboard.ForgeAppKey, 0, len(held))
		for idStr, fp := range held {
			keys = append(keys, dashboard.ForgeAppKey{
				AppID:       idStr,
				Path:        appKeys.PerAppIDKeyPathFor(idStr),
				Fingerprint: fp,
			})
		}
		return dashboard.ForgeAppInventory{
			ActiveKeyFile: appKeys.Resolve(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID),
			HeldKeys:      keys,
		}
	})

	dashSrv.SetGitHubAppRequired(githubAppRequired)
	// Order matters: SetGitHubAppRequired(false) clears both fields, so the
	// classified state is applied only after it, and only when a failure was
	// actually detected.
	if githubAppRequired {
		dashSrv.SetGitHubAppState(githubAppState.String())
		if githubAppDiag != "" {
			dashSrv.SetGitHubAppPermIssue(githubAppDiag)
		}
	}

	// Wire up the manual re-check callback for the dashboard button.
	{
		recheckRepo := cfg.Project.PrimaryRepo
		if recheckRepo == "" && len(cfg.Project.Repos) > 0 {
			recheckRepo = cfg.Project.Repos[0]
		}
		if recheckRepo != "" {
			dashSrv.SetGitHubAppRecheckFn(func() bool {
				// The Re-check button is the first thing an owner clicks on a
				// degraded hive. Report the real cause instead of the generic
				// "not accessible" — there is no client to check WITH, so the
				// credentials themselves are what must be fixed.
				if ghClient == nil {
					logger.Warn("github app recheck: hive is running without GitHub credentials", "detail", appAuthFailure)
					dashSrv.AuditLog("system", "github_app_check", "result=no GitHub client: "+appAuthFailure, "")
					return false
				}
				// #4360: ask about repo COVERAGE before attempting a read.
				// A repo the installation does not cover answers 404, which is
				// indistinguishable from "no such repo" and used to be reported
				// as "app not installed / no read" — sending the operator after
				// credentials that were never broken. Checking first means the
				// specific, correct message wins over the generic one.
				if raise, diag, state := classifyGitHubAppRepoCoverage(ctx, ghClient.AppAuth(), cfg.Project.Org, cfg.Project.Repos, logger); raise {
					dashSrv.SetGitHubAppPermIssue(diag)
					dashSrv.SetGitHubAppState(state.String())
					logger.Warn("github app recheck: installation does not cover every configured repo",
						"org", cfg.Project.Org, "state", state.String(), "detail", diag)
					dashSrv.AuditLog("system", "github_app_check", "result=repos not in installation: "+diag, "")
					return false
				}
				num, err := ghClient.EnsureAdvisoryIssue(ctx, recheckRepo)
				if err != nil {
					logger.Debug("github app recheck: not accessible", "repo", recheckRepo, "error", err)
					dashSrv.AuditLog("system", "github_app_check", "result=not accessible (app not installed / no read)", "")
					return false
				}
				advisoryIssues[recheckRepo] = num
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
				// Finding the advisory issue only proves the app is installed
				// (reads succeed on public repos even with a token from the
				// wrong installation). Verify write capability before letting
				// the handler clear the banner, so Re-check can't produce a
				// clears-then-returns flip-flop.
				// Before reporting a wrong-account installation, try to fix it:
				// this is the exact case rediscovery exists for. Cached by TTL,
				// so a repeated re-check does not re-hit the API.
				healGitHubAppInstallation(ctx, ghClient.AppAuth(), cfg, logger)
				// Shared verdict with the boot and advisory-digest paths. Re-check
				// previously branched on `diag != ""` while boot branched on the
				// error string, which is how the two came to disagree about the
				// same hive; routing both through classifyGitHubAppFailure means
				// they cannot drift again.
				if raise, diag, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger); raise {
					dashSrv.SetGitHubAppPermIssue(diag)
					dashSrv.SetGitHubAppState(state.String())
					logger.Warn("github app recheck: app detected but write not verified",
						"repo", recheckRepo, "state", state.String(),
						"operator_actionable", state.OperatorActionable(), "detail", diag)
					dashSrv.AuditLog("system", "github_app_check", "result=installed but write NOT verified: "+diag, "")
					return false
				}
				// #2353: the classifier above only proves the installation
				// authenticates and grants issues:write — NOT that this repo can
				// actually be written. Finding the advisory issue is a READ, which
				// succeeds even when the repo is not in the App installation's
				// selected repos. Perform a REAL write probe before clearing the
				// banner, so re-check cannot falsely "verify write" for a repo the
				// App can only read (the recheck false-positive).
				if werr := ghClient.ProbeIssueWrite(ctx, recheckRepo, num); werr != nil {
					if strings.Contains(werr.Error(), "403") && strings.Contains(werr.Error(), "Resource not accessible by integration") {
						msg, state := classifyGitHubAppWriteForbidden(ctx, ghClient.AppAuth(), cfg.Project.Org, recheckRepo)
						dashSrv.SetGitHubAppPermIssue(msg)
						dashSrv.SetGitHubAppState(state.String())
						logger.Warn("github app recheck: write probe returned 403 — not clearing the banner",
							"repo", recheckRepo, "state", state.String(), "detail", msg)
						dashSrv.AuditLog("system", "github_app_check", "result=write probe FORBIDDEN: "+msg, "")
						return false
					}
					// A non-403 probe failure is inconclusive (rate limit,
					// transient network). Do NOT clear the banner on a write we
					// could not confirm, but also do NOT accuse anyone.
					logger.Warn("github app recheck: write probe inconclusive — leaving banner as-is",
						"repo", recheckRepo, "error", werr)
					dashSrv.AuditLog("system", "github_app_check", "result=write probe inconclusive", "")
					return false
				}
				logger.Info("github app recheck: app detected, write verified", "repo", recheckRepo, "number", num)
				dashSrv.AuditLog("system", "github_app_check", "result=OK (installed, write verified)", "")
				return true
			})
		}
	}

	// If the App credentials are present but github.installation_id is still
	// empty, discover it automatically. This covers the delayed approval path:
	// a non-admin requests installation, an org admin approves later, and the
	// spoke adopts the installation ID without requiring anyone to paste it.
	{
		const githubAppDiscoveryInterval = 5 * time.Minute
		tryDiscover := func() {
			if cfg.GitHub.InstallationID != 0 {
				return
			}
			_, _ = dashSrv.AutoDiscoverGitHubInstallationID(ctx, false)
		}
		go func() {
			tryDiscover()
			ticker := time.NewTicker(githubAppDiscoveryInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					tryDiscover()
				}
			}
		}()
	}

	// Self-heal the "GitHub App not installed" banner. This handles:
	//  1. GitHub App credentials arrived after startup (via heartbeat/webhook)
	//  2. ReinitGitHubFunc succeeded but cleared githubAppRequired before
	//     EnsureAdvisoryIssue could run against the new client
	//  3. A TRANSIENT startup/runtime 4xx (rate-limit blip, brief token-refresh
	//     window, momentary permission propagation delay) latched the banner even
	//     though the app is really installed and can write. Previously the retry
	//     loop exited permanently after the first advisory-issue READ succeeded,
	//     so a later transient write failure that re-set the flag was never
	//     re-evaluated — the banner stuck until the pod was restarted.
	//
	// The loop therefore runs for the lifetime of the process (it does NOT
	// return after the first success) and, whenever the banner is currently
	// showing, re-runs the SAME read+write verification as the manual "Re-check"
	// button (githubAppRecheckFn, which calls diagnoseGitHubApp) and clears
	// the flag on success. When the banner is not showing there is nothing to do,
	// so the tick is a cheap no-op that makes no GitHub API calls.
	{
		primaryRepo := cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
			primaryRepo = cfg.Project.Repos[0]
		}
		if primaryRepo != "" {
			// githubAppSelfHealInterval mirrors the heartbeat cadence so a stale
			// banner clears within one heartbeat window of the app becoming
			// healthy, without adding meaningful GitHub API load (the check only
			// runs while the banner is actually showing).
			const githubAppSelfHealInterval = 2 * time.Minute
			go func() {
				ticker := time.NewTicker(githubAppSelfHealInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						// Nothing to heal unless the banner is showing.
						if !dashSrv.IsGitHubAppRequired() {
							continue
						}
						_, _ = dashSrv.AutoDiscoverGitHubInstallationID(ctx, false)
						// Re-run the same read+write verification the manual
						// Re-check button uses. It clears the flag on success
						// (installed AND write-verified) and leaves it set on a
						// genuine failure (not installed / insufficient perms).
						if dashSrv.RecheckGitHubApp() {
							if num, exists := advisoryIssues[primaryRepo]; exists {
								_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
							}
							logger.Info("github app self-heal: banner cleared, app installed and write verified", "repo", primaryRepo)
						} else {
							logger.Debug("github app self-heal: still not verified, banner remains", "repo", primaryRepo)
						}
					}
				}
			}()
		}
	}

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
	configWatcher := config.NewWatcher(configPath, func(newCfg *config.Config) {
		// Preserve runtime-only fields that are not in the YAML
		newCfg.HiveID = cfg.HiveID

		// Preserve ACMM level from the agent manager — it is the
		// authoritative source. The file may have a stale value if
		// a watcher reload races with a level-switch saveConfig().
		if cfg.ACMMLevel != nil {
			newCfg.ACMMLevel = cfg.ACMMLevel
		}

		// Preserve removed-agent tombstones across the swap as a union of the
		// live cfg and the incoming reload. LoadWithDashboardOverlay now carries
		// the overlay's tombstones into newCfg, but a removal that landed in the
		// live cfg after this reload's snapshot (or an overlay too short/stale to
		// echo it back yet) must not be lost — otherwise the next persistState
		// saver rewrites every layer tombstone-free and the deleted agents
		// reappear (#2439). Union keeps any tombstone present in either side.
		for _, name := range cfg.RemovedAgents {
			newCfg.MarkAgentRemoved(name)
		}
		newCfg.PruneRemovedAgents()

		// Observability (#2439): this is the ~2-min interval reload path, so keep it
		// at DEBUG to avoid spamming a healthy hive. When a removal is not sticking,
		// enabling DEBUG shows the tombstone surviving each swap — an empty count here
		// while the agent keeps reappearing localizes the leak to this union-preserve.
		logger.Debug("reload: preserved removed-agents",
			"hive_id", cfg.HiveID,
			"count", len(newCfg.RemovedAgents),
			"agents", newCfg.RemovedAgents,
		)

		// Capture the outgoing GitHub App identity before the swap so we can
		// tell whether the reload changed it.
		prevGitHub := cfg.GitHub

		// Swap the in-memory config pointer contents
		*cfg = *newCfg

		// Re-sync subsystems that cache config values
		ghClient.SetRepos(cfg.Project.Repos)
		gov.UpdateConfig(cfg.Governor)
		// A reload can add or archive repos, which moves every scaled default
		// threshold — re-sync it alongside the repo list above.
		gov.SetRepoCount(cfg.Project.RepoCount())
		agentMgr.SetSandboxConfig(cfg.AgentSandbox)
		// Re-run the posture check on reload, not only at boot: flipping the
		// Security tab's sandbox toggle writes the config and lands here, which
		// is the exact moment an operator forms the belief that they are now
		// sandboxed. See logAgentSandboxPosture.
		logAgentSandboxPosture(logger, cfg)

		// Hot-reload the state-triggered hooks (RFC #4001). Recompiles only
		// when the `hooks:` list actually changed, and swaps the registry in
		// place so per-hook rate-limit windows SURVIVE the reload — otherwise
		// a reload loop would be a way to clear the anti-storm ceiling.
		buildHookDispatcher(cfg, hookSinks{
			Notifier: notifier,
			AgentMgr: agentMgr,
			Timeline: dashSrv.LifecycleTimeline(),
			Audit:    dashSrv.AgentAuditSink(),
		}, logger)

		// Hot-reload the state-triggered hooks (RFC #4001). Recompiles only
		// when the `hooks:` list actually changed, and swaps the registry in
		// place so per-hook rate-limit windows SURVIVE the reload — otherwise
		// a reload loop would be a way to clear the anti-storm ceiling.
		buildHookDispatcher(cfg, hookSinks{
			Notifier: notifier,
			AgentMgr: agentMgr,
			Timeline: dashSrv.LifecycleTimeline(),
			Audit:    dashSrv.AgentAuditSink(),
			// #4000 ↔ #4001 seam: an `enqueue-approval` hook lands in the same
			// durable operator inbox the desk uses. nil when the desk is off,
			// which keeps the dispatcher's "no approval queue wired" error
			// honest rather than failing on every firing.
			Approvals: newHookApprovalAdapter(approvalInbox, toolapprove.ACMMLevelOf(cfg)),
		}, logger)

		// Re-apply live agent definitions (definition_source) on reload so an
		// operator's edit to a linked repo propagates. Merges only operator-safe
		// fields; a fetch failure keeps each agent's baked definition. Runs before
		// initAgentConfigDrivenSystems so downstream systems see the merged config.
		defsrc.ApplyToConfig(context.Background(), cfg, definitionResolver, logger)
		if err := cfg.ExpandAgentReplicas(); err != nil {
			logger.Warn("failed to expand agent replicas after config reload", "error", err)
		}
		addedAgents := agentMgr.ReconcileAgents(cfg.EnabledAgents())
		for _, added := range addedAgents {
			if ac, ok := cfg.Agents[added]; ok && !ac.OnDemand {
				go func(name string) {
					logger.Info("audit: starting reconciled agent", "name", name, "trigger", "config-reload")
					if err := agentMgr.Start(ctx, name); err != nil {
						logger.Warn("failed to start reconciled agent", "name", name, "error", err)
					}
				}(added)
			}
		}
		gov.UpdateAgents(cfg.EnabledAgents())

		initAgentConfigDrivenSystems(cfg)

		// Rebuild GitHub App auth when its identity changed. AppAuth captures
		// app_id/installation_id at construction, so without this a corrected
		// installation_id in hive.yaml keeps minting tokens for the OLD
		// installation until the pod restarts.
		//
		// RESOLVE the key file rather than reading cfg.GitHub.KeyFile raw. An
		// unset key_file is the CORRECT steady state on a hosted spoke — the
		// heartbeat apply path deliberately does not persist one, because the
		// path is derivable from app_id and a stored value outlives the App it
		// was derived for. Gating on the raw field therefore skipped the rebuild
		// entirely on exactly the hives that need it: a corrected
		// installation_id saved to hive.yaml kept minting tokens for the old
		// installation until the pod restarted. Startup (resolveAppKeyFile
		// above), the heartbeat rebuild, and the dashboard's Set ID handler
		// (#2459) all already resolve here; this was the last raw reader.
		//
		// Comparing RESOLVED paths also catches a change the raw comparison
		// cannot see: a per-app-id key arriving on the PVC changes which key
		// this process should sign with while cfg.GitHub.KeyFile stays "".
		prevKeyFile := appKeys.Resolve(prevGitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), prevGitHub.AppID)
		nextKeyFile := appKeys.Resolve(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID)
		if prevGitHub.AppID != cfg.GitHub.AppID ||
			prevGitHub.InstallationID != cfg.GitHub.InstallationID ||
			prevKeyFile != nextKeyFile ||
			prevGitHub.APIURL != cfg.GitHub.APIURL {
			if cfg.GitHub.HasUsableApp() && nextKeyFile != "" {
				newAppAuth, appErr := github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, nextKeyFile, logger, cfg.GitHub.ResolvedAPIURL())
				if appErr != nil {
					logger.Error("github app auth rebuild after config reload failed", "error", appErr)
				} else {
					newClient := github.NewClientFromAppWithBotLogin(newAppAuth, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.BotLogin())
					if len(cfg.Governor.Labels.Exempt) > 0 {
						newClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
						newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
					}
					newClient.SetIssueFilter(cfg.Project.IssueFilter)
					ghClient = newClient
					appAuth = newAppAuth
					agentMgr.SetAppAuth(newAppAuth)
					// Immediate per-agent token delivery — see #4072.
					go agentMgr.RefreshAgentTokens(ctx)
					agentMgr.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: newAppAuth})
					agentMgr.SetSandboxPRClient(newClient)
					dashSrv.UpdateGitHubClient(newClient, newAppAuth)
					logger.Info("github app auth rebuilt after config reload",
						"app_id", cfg.GitHub.AppID,
						"installation_id", cfg.GitHub.InstallationID,
						"key_file", nextKeyFile,
					)
				}
			}
		}

		refreshDashboard()
	}, logger)
	dashSrv.SetSkipReloadFunc(configWatcher.SkipNext)
	go configWatcher.Start(ctx)

	// Persist operator pause/resume into the on-disk config so it survives
	// restarts and upgrades. Without this, a pod restart rebuilt every agent
	// un-paused, silently undoing an operator's pause on the next upgrade.
	// Concurrent pauses (e.g. an ACMM pack pausing several agents in a loop,
	// or login-detector firing while the operator pauses) each did an
	// unsynchronized cfg.Agents map write + cfg.Save(). The saves clobbered
	// each other (last writer wins), so only some pauses reached the PVC and
	// the rest were silently lost on the next restart. Serialize the
	// read-modify-save under a dedicated mutex so every pause transition is
	// durably persisted.
	agentMgr.SetPersistPauseCallback(func(name string, paused bool) {
		configWatcher.SkipNext() // don't let our own write trigger a reload
		changed, err := cfg.SetAgentPausedAndSave(name, paused)
		if err != nil {
			logger.Warn("failed to persist agent pause state", "agent", name, "paused", paused, "error", err)
		}
		_ = changed
	})

	// Persist the fully-expanded prompt text of every kick so owners can review
	// what their agents were actually told, over a day/week window, in the
	// per-agent "Prompts" tab. Redaction and truncation happen inside the
	// store, before anything is written to the PVC.
	agentMgr.SetRecordPromptCallback(dashSrv.RecordPrompt)

	// Feed agent lifecycle events (start, stop, launch FAILURE, backend/model
	// change) into the durable audit store behind the dashboard's Audit Log.
	// Injected as an interface because pkg/dashboard already imports pkg/agent,
	// so the manager cannot reach the audit store directly without an import
	// cycle. Motivating case: an agent configured with a backend its hive image
	// did not support failed at every launch for a day, visible only as a WARN
	// line inside the pod.
	agentMgr.SetAuditSink(dashSrv.AgentAuditSink())

	// Compile the operator's state-triggered hooks (RFC #4001). Every sink the
	// vetted actions act through exists by this point: the notifier, the agent
	// manager's AUDITED pause, the lifecycle timeline, and the same audit store
	// the dashboard writes. The approvals sink stays nil until #4000's queue
	// lands — an enqueue-approval hook then reports a wiring failure per firing
	// rather than silently dropping the request.
	//
	// Fail-closed: an invalid hooks list logs and leaves the previous set
	// armed; it never crashes the process or silently disarms working hooks.
	buildHookDispatcher(cfg, hookSinks{
		Notifier: notifier,
		AgentMgr: agentMgr,
		Timeline: dashSrv.LifecycleTimeline(),
		Audit:    dashSrv.AgentAuditSink(),
		// #4000 ↔ #4001 seam: see the reload site above.
		Approvals: newHookApprovalAdapter(approvalInbox, toolapprove.ACMMLevelOf(cfg)),
	}, logger)

	// Emit the governor_mode_change transition post-commit. Installed once:
	// the observer reads the dispatcher through hookDispatcher() on each
	// firing, so a later config reload that arms or disarms hooks is picked up
	// without re-registering.
	installGovernorModeChangeEmitter(gov)
	installAgentPauseEmitter(agentMgr)

	// Register custom GHE hostnames with the proxy allowlist so mode
	// enforcement applies to GitHub Enterprise API and web requests.
	for _, rawURL := range []string{cfg.GitHub.ResolvedAPIURL(), cfg.GitHub.ResolvedBaseURL()} {
		if parsed, err := url.Parse(rawURL); err == nil && parsed.Host != "" {
			proxy.RegisterGitHubHost(parsed.Host)
		}
	}

	dashboard.SetBackendAuthProvider(agentMgr.BackendAuthAvailable)
	// Per-agent probe supersedes the backend-level one: under the per-agent-UID
	// layout each agent has its own HOME, so the shared credential path is empty
	// even for authenticated agents (see pkg/agent/authprobe.go).
	dashboard.SetAgentAuthProvider(agentMgr.AgentAuthAvailable)

	canaryLeakHandler := func(leak ioscan.CanaryLeak) {
		detail := fmt.Sprintf("rule=%s, agent=%s, source=%s", ioscan.CanaryLeakRule, leak.Agent, leak.Source)
		dashSrv.AuditLog(leak.Agent, "ioscan_canary_leak", detail, leak.Agent)
		if store, ok := beadStores[leak.Agent]; ok && store != nil {
			if b, berr := store.Create("Canary token leaked via "+leak.Source, beads.TypeAdvisory, beads.PriorityCritical, leak.Agent, ""); berr == nil {
				_ = store.SetMetadata(b.ID, "rule", ioscan.CanaryLeakRule)
				_ = store.SetMetadata(b.ID, "source", leak.Source)
			}
		}
	}
	if ghClient != nil {
		ghClient.SetCanaryScanner(cfg.Ioscan.IsEnabled() && cfg.Ioscan.Canaries, cfg.Ioscan.FailClosed(), ioscan.DefaultCanaries, canaryLeakHandler)
	}

	githubProxy, err := proxy.NewGitHubProxy(logger, cfg.Project.Org, cfg.Project.Repos)
	if err != nil {
		logger.Error("failed to create github proxy", "error", err)
	} else {
		githubProxy.SetCanaryScanner(cfg.Ioscan.IsEnabled() && cfg.Ioscan.Canaries, cfg.Ioscan.FailClosed(), ioscan.DefaultCanaries, canaryLeakHandler)
		// #1861: the proxy resolves an identified agent to its hub-held scoped
		// token via the package-level registry WriteAgentToken feeds (NOT via
		// the appAuth instance, which is replaced on key rotation — a closure
		// over it would strand the proxy on the stale instance). Wired
		// unconditionally: with HIVE_PROXY_INJECT_GH_AUTH unset (the default)
		// the proxy never consults the source and the registry stays empty.
		githubProxy.SetAgentTokenSource(github.AgentProxyToken)
		dashboard.SetProxyViolationsProvider(githubProxy.Violations)
		// Lets the dashboard narrow the LiteLLM model dropdown to the set the
		// configured key is entitled to, learned by the proxy from a key-info
		// probe or a "team not allowed" 403.
		dashboard.SetEntitledModelsProvider(githubProxy.EntitledModels)
		// Surface a stale/invalid inference gateway key (repeated 401s on every
		// inference call) as a hive health signal: the proxy latches the failure
		// after several consecutive rejections and clears it on the next success,
		// and the heartbeat builder reports it to the hub (both as an immediate
		// advisory-staleness cause and as a dedicated inference-auth alert).
		dashboard.SetInferenceAuthProvider(githubProxy.InferenceAuthError)
		// #4294: the provider spending-limit signal, read by the eval cycle to
		// raise an advisory and stop kicking agents at a gateway that is
		// refusing on a money limit.
		dashboard.SetInferenceBudgetProvider(githubProxy.InferenceBudgetExceeded)

		// Wire the inference token sink so the translator records per-agent
		// usage (from the gateway's OpenAI usage block) into the same metrics
		// dir the token collector scans. Without this, bare-mode inference
		// agents (litellm/vllm/llm-d) never write a scannable session file and
		// their consumption reads as zero.
		githubProxy.SetTokenSink(tokens.NewInferenceSink(cfg.Data.MetricsDir, logger))

		// With the sink active, the proxy also MITMs the Copilot completion host
		// (api.githubcopilot.com) to record Copilot token usage live per
		// response — so Copilot cost shows up while an agent runs instead of only
		// tallying at session shutdown. Tell the collector to defer Copilot token
		// accrual to the sink ONLY for sessions active from NOW on (the moment
		// live capture starts). Sessions that ended earlier were never sniffed by
		// the proxy, so the scanner keeps counting their shutdown tokens —
		// otherwise all pre-existing Copilot spend would vanish.
		tokenCollector.SetCopilotLiveCapture(time.Now().UnixMilli())

		vllmEndpoints := parseEndpointList(envOrDefault("HIVE_VLLM_ENDPOINT", "http://hive-vllm-svc.hive-inference.svc.cluster.local:8000"))
		llmdEndpoints := parseEndpointList(envOrDefault("HIVE_LLMD_ENDPOINT", "http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000"))
		inferenceEndpoints := map[string][]string{
			"vllm":  vllmEndpoints,
			"llm-d": llmdEndpoints,
		}
		// litellm has no in-cluster default: register it only when an
		// endpoint is configured (yaml or HIVE_LITELLM_ENDPOINT), so an
		// unconfigured backend doesn't show up in model discovery. A URL
		// saved later from the governor LiteLLM tab is registered at
		// runtime via dashSrv.UpdateInferenceEndpoint.
		if cfg.Governor.LiteLLM.LocalProxy {
			inferenceEndpoints["litellm"] = []string{litellmLocalProxyURL()}
		} else if litellmEndpoint := cfg.Governor.LiteLLM.ResolveEndpoint(); litellmEndpoint != "" {
			inferenceEndpoints["litellm"] = parseEndpointList(litellmEndpoint)
		}
		// Register every explicitly-configured named gateway's endpoint by
		// gateway NAME so the Model Gateways tab's per-gateway model discovery
		// and per-gateway routing resolve on boot (the legacy "litellm" block
		// is already registered above; ResolvedGateways only synthesizes it
		// when no explicit gateways are set, so this loop never double-adds it).
		for _, gw := range cfg.Governor.Gateways {
			if ep := strings.TrimSpace(gw.Endpoint); ep != "" {
				inferenceEndpoints[gw.Name] = parseEndpointList(ep)
			}
		}
		dashSrv.SetInferenceEndpoints(inferenceEndpoints)
		// The gateway-name predicate (SetGatewayBackendChecker) is wired right
		// after the manager is constructed — see the comment there (#3961): it
		// must be live before the persisted-state replay re-applies saved
		// backend overrides, which happens well before this point.
		agentMgr.SetInferenceCallbacks(
			func(agentName, backend, model string) {
				// Named model gateway (OpenRouter, a second LiteLLM, etc.): resolve
				// endpoint/key/model from the gateway and route through it. Built-in
				// backend names (litellm/vllm/llm-d) are handled below; a gateway
				// literally named "litellm" resolves here to the same legacy block
				// via ResolvedGateways, so behavior is identical.
				if gw := cfg.Governor.ResolveGateway(backend); gw != nil && !config.IsInferenceBackend(backend) {
					endpoint := gw.Endpoint
					if endpoint == "" {
						logger.Warn("gateway backend selected but no endpoint configured",
							"agent", agentName, "gateway", backend, "model", model)
						return
					}
					if model == "" {
						model = gw.DefaultModel
					}
					// watsonx authenticates the OpenAI-compatible model gateway
					// with a short-lived IAM bearer minted from the IBM Cloud API
					// key (NOT the raw key), and scopes billing/limits by a
					// project id sent as X-IBM-Project-ID. Mint (cached) and set
					// both here; every other kind sends the resolved key verbatim.
					apiKey, extraHeaders := resolveGatewayAuth(gw, agentName, backend, logger)
					githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:      backend,
						Endpoint:     endpoint,
						Model:        model,
						APIKey:       apiKey,
						CABundle:     gw.CABundle,
						ExtraHeaders: extraHeaders,
					})
					return
				}
				if backend == "litellm" {
					// Resolve endpoint/key at call time so a URL saved from
					// the governor LiteLLM tab (or a rotated key) takes
					// effect without a hive restart. cfg is the live config
					// pointer — the config watcher swaps its contents in
					// place on reload.
					lc := cfg.Governor.LiteLLM
					// Endpoint/model resolution lives in a pure function so the
					// decision tree (local proxy / legacy block / explicit-gateway
					// fallback / no route at all) is unit-testable — it is not
					// reachable from a test while inline in main(). See #5460.
					endpoint, resolvedModel, ok := resolveLiteLLMInferenceRoute(cfg, backend, model)
					if !ok {
						logger.Warn("litellm backend selected but no endpoint configured",
							"agent", agentName, "model", model)
						return
					}
					model = resolvedModel
					// Key source must MATCH the entitlement/probe path (gateways.go,
					// cost.go, openrouter.go), which resolve the key from the gateway
					// via ResolveGateway(backend).ResolveAPIKey(). When an EXPLICIT
					// `gateways:` block names this backend, that gateway carries its
					// own api_key_file (e.g. the key saved from the Model Gateways
					// tab). Reading the legacy Governor.LiteLLM key file here instead
					// would send a DIFFERENT (often stale) key than entitlement
					// validated, causing inference 401s after a key rotation done via
					// the Gateways tab. Resolve from the same gateway so inference and
					// entitlement always agree on one key source.
					//
					// Only explicit gateways override: ResolvedGateways synthesizes an
					// implicit "litellm" gateway from the legacy block when no
					// `gateways:` are set, but that synthetic gateway lacks the
					// multi-location file fallback of LiteLLMConfig.ResolveAPIKey
					// (k8s Secret mount + PVC copy). For no-gateway hives we therefore
					// keep the legacy resolver to preserve today's behavior.
					apiKey := cfg.Governor.ResolveLiteLLMInferenceKey(backend)
					caBundle := lc.CABundle
					if len(cfg.Governor.Gateways) > 0 {
						if gw := cfg.Governor.ResolveGateway(backend); gw != nil {
							caBundle = gw.CABundle
						}
					}
					githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:  backend,
						Endpoint: endpoint,
						Model:    model,
						APIKey:   apiKey,
						CABundle: caBundle,
					})
					return
				}
				if backend == config.GatewayKindWatsonx {
					// Built-in "watsonx" backend: the operator set
					// `backend: watsonx` without a gateway literally NAMED
					// watsonx (a named one is handled by the gateway branch
					// above). Resolve the watsonx gateway by KIND so the
					// endpoint, IBM Cloud key, project id and region all come
					// from the existing `gateways:` plumbing rather than being
					// re-derived here.
					gw := resolveWatsonxGateway(cfg)
					if gw == nil {
						logger.Warn("watsonx backend selected but no watsonx gateway is configured; add one under the Model Gateways tab",
							"agent", agentName, "model", model)
						return
					}
					// Region-only gateways are legal (the guided form can save a
					// region without an endpoint), so fall back to the shared
					// region template — the same helper the dashboard preset uses.
					endpoint := strings.TrimSpace(gw.Endpoint)
					if endpoint == "" {
						endpoint = watsonx.EndpointForRegion(gw.Region)
					}
					if model == "" {
						model = gw.DefaultModel
					}
					apiKey, extraHeaders := resolveGatewayAuth(gw, agentName, backend, logger)
					githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:      backend,
						Endpoint:     endpoint,
						Model:        model,
						APIKey:       apiKey,
						CABundle:     gw.CABundle,
						ExtraHeaders: extraHeaders,
					})
					return
				}
				endpoints := vllmEndpoints
				if backend == "llm-d" {
					endpoints = llmdEndpoints
				}
				// vllm/llm-d endpoints are unauthenticated with a public
				// or in-cluster CA — no bearer key or custom CA bundle.
				endpoint := proxy.FindEndpointForModel(endpoints, model, "", "")
				if endpoint == "" {
					logger.Warn("no endpoint serves model, using first endpoint",
						"agent", agentName, "model", model, "backend", backend)
					endpoint = endpoints[0]
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
		if cfg.Governor.LiteLLM.LocalProxy {
			go superviseLocalLiteLLM(ctx, logger)
		}
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
			AllowedUsers:   cfg.Notifications.Discord.AllowedUsers,
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
	// One visible "hive restarted" marker per boot, so the audit log shows a
	// restart happened (and at what build) instead of only a burst of
	// per-agent agent_start rows. Include the persisted pauses being restored
	// so the operator can confirm pause state survived the restart — broken
	// down by trigger, and EXCLUDING agents that are startup-paused by design
	// (on-demand agents like brainstorm), whose inclusion turned "restoring 9
	// paused agent(s)" into a false systemic-incident signal on every upgrade
	// restart of a deliberately owner-quiesced fleet (#4041).
	dashSrv.AuditLog("system", "hive_restart",
		fmt.Sprintf("build=%s version=%s; %s", gitShort, version,
			pausedRestoreDetail(cfg.EnabledAgents(), onDemandFromPack, agentMgr.AllStatuses())), "")

	// Mark the dashboard READY as soon as the HTTP server can serve requests —
	// which is NOW: config is loaded, GitHub client/App auth are wired, the
	// dashboard deps are set, and the listener (go dashSrv.Start() above) is up.
	// None of /api/*, /sso, /open, /api/livez or /api/health depend on the agent
	// fleet being up; the frontend already handles agents appearing over time.
	//
	// This MUST precede the staggered agent-launch loop below. That loop sleeps
	// ~15s per agent (× the whole fleet = several minutes) and previously ran
	// BEFORE MarkReady, so /api/livez returned 503 "starting" for the entire
	// launch window. The liveness probe (period 30s × failureThreshold 3 ≈ 90s)
	// then SIGKILLed the container (exit 137) before readiness was ever reached
	// on cold start, and rolling upgrades left the Service with no Ready endpoint
	// for minutes → 503s on /open and /sso. Flipping ready here makes the pod
	// Ready in seconds and moves the fleet spin-up entirely off the critical path.
	dashSrv.MarkReady()

	// Launch the persistent (non-on-demand) agents in the BACKGROUND so the
	// staggered start no longer gates pod readiness. The loop honors ctx: on
	// shutdown the ctx-aware stagger returns immediately instead of leaking a
	// goroutine parked in a bare time.Sleep.
	go func() {
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
				select {
				case <-time.After(time.Duration(agentLaunchDelaySec) * time.Second):
				case <-ctx.Done():
					logger.Info("aborting staggered agent launch: shutting down")
					return
				}
			}
			// Bail before starting another agent if we are already shutting down,
			// so a SIGTERM during the launch window doesn't spawn fresh processes.
			if ctx.Err() != nil {
				logger.Info("aborting staggered agent launch: shutting down")
				return
			}
			logger.Info("audit: starting agent", "name", name, "trigger", "startup")
			if err := agentMgr.Start(ctx, name); err != nil {
				logger.Warn("failed to start agent", "name", name, "error", err)
			} else {
				// Surface whether a persisted operator pause was honored on this
				// restart, so the audit log shows pause state survived (or didn't).
				detail := "trigger=startup"
				if ac.Paused {
					detail = "trigger=startup; restored paused (persisted)"
				}
				dashSrv.AuditLog("system", "agent_start", detail, name)
			}
			agentIndex++
		}
	}()

	// Start hub heartbeat push if configured (env var or config)
	hubURL := cfg.Hub.URL
	if envHub := os.Getenv("HIVE_HUB_URL"); envHub != "" {
		hubURL = envHub
		cfg.Hub.Enabled = true
		cfg.Hub.URL = envHub
	}
	if envCluster := os.Getenv("HIVE_CLUSTER_ID"); envCluster != "" {
		cfg.Hub.ClusterID = envCluster
	}
	if cfg.Hub.Enabled && hubURL != "" {
		// Heartbeat cadence is INDEPENDENT of the governor eval interval. It was
		// previously tied to cfg.Governor.EvalIntervalS, so a low-ACMM hive
		// (which evaluates infrequently by design — e.g. ~10 min at L2) beat the
		// hub only every ~10 min. The hub marks a hive stale after
		// heartbeatHealthStaleness (5 min), so such hives showed a gray/stale
		// dot for half of every cycle despite being perfectly healthy. Beat on a
		// fixed interval comfortably under that 5-min threshold so every hive,
		// regardless of ACMM level, stays fresh on the hub.
		const heartbeatSendInterval = 2 * time.Minute
		// Publish the collect-independent identity BEFORE the loop starts, so
		// this spoke can report liveness even if its very first collects time
		// out. collect() below reaches api.github.com (owner-token validation,
		// and it shares the pass that enumerates issues/PRs for MTTR), which on
		// a hive with real repos routinely exceeds the collect budget right
		// after a restart. Without this, such a spoke sent NOTHING and read
		// OFFLINE on the hub while being perfectly healthy.
		hub.PublishHeartbeatIdentity(
			cfg.HiveID,
			cfg.Project.Org,
			cfg.Project.PrimaryRepo,
			cfg.Project.Repos,
			reporterName,
			processStartedAt.UTC().Format(time.RFC3339),
			gitShort,
		)
		go hub.StartHeartbeat(ctx, hubURL, func() *hub.HeartbeatPayload {
			if !cfg.Hub.Enabled {
				return nil
			}
			statuses := agentMgr.AllStatuses()
			govState := gov.GetState()
			currentMode := strings.ToLower(string(govState.Mode))
			agents := make([]hub.AgentSummary, 0, len(statuses))
			for name, proc := range statuses {
				mode := ""
				if ac, ok := cfg.Agents[name]; (ok && ac.OnDemand) || onDemandFromPack[name] {
					mode = "on_demand"
				}
				agents = append(agents, hub.NewAgentSummary(name, string(proc.State), mode,
					hub.AgentActivityFor(agentMgr, cfg, govState, currentMode, name, proc, onDemandFromPack)))
			}
			acmmLvl := 0
			if cfg.ACMMLevel != nil {
				acmmLvl = *cfg.ACMMLevel
			}
			// Attach cached fleet-stat counts only once the collector has done a
			// successful compute — nil pointers until then, so the hub never
			// aggregates a not-yet-computed zero into the public fleet total.
			var prsMerged, prsRejected, cvesClosed *int
			fleetStatsCollectedAt := ""
			if fc, ok := fleetStatsCollector.Snapshot(); ok {
				m, rj, cv := fc.PRsMerged, fc.PRsRejected, fc.CVEsClosed
				prsMerged, prsRejected, cvesClosed = &m, &rj, &cv
				// Report WHEN these were computed, not when this beat was sent.
				// The hub carries counts forward across spoke restarts, so this
				// timestamp is the only way it can tell a fresh contribution
				// from one frozen by a collector that has started failing.
				if t := fleetStatsCollector.CollectedAt(); !t.IsZero() {
					fleetStatsCollectedAt = t.UTC().Format(time.RFC3339)
				}
			}
			// Per-repo output-activity summary (hive-health): map the dashboard
			// collector's snapshot into the plain hub wire structs. Nil when the
			// collector hasn't produced a snapshot yet, so the hub carries the
			// last one forward rather than seeing a fabricated empty summary.
			var repoActivity []hub.RepoActivityWire
			repoActivityCollectedAt := ""
			repoActivityWindowHours := 0
			repoActivityCountWindowHours := 0
			if asnap, ok := activityCollector.Snapshot(); ok {
				repoActivity = buildRepoActivityWire(asnap.Repos)
				repoActivityWindowHours = asnap.WindowHours
				repoActivityCountWindowHours = asnap.CountWindowHours
				if t := activityCollector.CollectedAt(); !t.IsZero() {
					repoActivityCollectedAt = t.UTC().Format(time.RFC3339)
				}
			}
			// Count agents with a method/model assigned for the hub's
			// user-journey stage detection. Always a non-nil pointer from a
			// spoke new enough to compute it, so the hub can distinguish
			// "genuinely zero agents configured" from "old spoke, unknown".
			agentsWithModel := agentMgr.CountAgentsWithModel()

			// --- Quadrant signals ------------------------------------------
			// All read from state this spoke already maintains on an existing
			// timer: ZERO new GitHub API calls, which matters because the whole
			// fleet shares one search quota. Every one stays nil unless its
			// source has actually produced a measurement — the hub's scorer
			// reads nil as absent evidence and a zero as a genuine low score,
			// so emitting a zero for missing data would silently misinform
			// operators rather than merely lose precision.

			// Budget spend is uninterpretable without its window bounds (zero
			// equally means "window just rolled" and "nothing consumed"), so
			// the three travel together or not at all.
			var budgetSpend *int64
			var budgetLimit *int64
			var budgetIgnored *bool
			var budgetWindowStartsAt, budgetWindowEndsAt string
			budget := gov.GetBudget()
			limit := budget.WeeklyLimit
			ignored := budget.IgnoreAll
			budgetLimit = &limit
			budgetIgnored = &ignored
			if start, end, ok := gov.BudgetWindow(); ok {
				spend := budget.CurrentSpend
				budgetSpend = &spend
				budgetWindowStartsAt = start.UTC().Format(time.RFC3339)
				budgetWindowEndsAt = end.UTC().Format(time.RFC3339)
			}
			// BudgetExhausted and SLAViolations are both plain (non-pointer)
			// governor state, so their zero values are indistinguishable from
			// "never evaluated" at the source. LastEval is the only thing that
			// tells the two apart: before the first eval — or before a restart
			// restores one — false/0 are struct defaults, not readings. Gate
			// both on it so a spoke still booting reports nil rather than
			// asserting a healthy budget and a clean SLA it has not checked.
			var budgetExhausted *bool
			var slaViolations *int
			if !govState.LastEval.IsZero() {
				exhausted := govState.BudgetExhausted
				violations := govState.SLAViolations
				budgetExhausted, slaViolations = &exhausted, &violations
			}

			// Hold comes from the cached actionable result rather than
			// govState.QueueHold: both carry the same number, but the cache is
			// a nilable pointer, so a spoke that has not yet completed (or
			// restored) a scan reports nil instead of an int zero that is
			// indistinguishable from "nothing is on hold".
			var holdTotal *int
			if act := lastActionable.Load(); act != nil {
				total := act.Hold.Total
				holdTotal = &total
			}

			// Planning is unavailable below ACMM L5, where AwaitingReview is
			// structurally zero rather than measured — report nil so the hub
			// does not read "no plans are blocked on a human" into a hive that
			// has no planning subsystem at all.
			//
			// architectPaused is passed false rather than resolved from agent
			// statuses: it feeds only FrontendPlanning.ArchitectPaused, which
			// this heartbeat does not send, and the resolver is unexported to
			// pkg/dashboard. Passing false cannot perturb AwaitingReview.
			var awaitingReview *int
			if planning := dashboard.BuildPlanning(beadStores, false, acmmLvl); planning.Available {
				n := planning.AwaitingReview
				awaitingReview = &n
			}

			// Contributor-relay tasks over the trailing 7d, summed from the
			// spoke's own 168 hourly buckets. nil until the store exists; a
			// zero from an existing store is a real "no contributor finished
			// anything" reading.
			var tasksCompleted7d *int
			if n, ok := dashSrv.TasksCompleted7d(); ok {
				tasksCompleted7d = &n
			}

			providerLimitReason, providerLimitRebuffs := hub.ProviderLimitHeartbeatFields(agents, dashboard.InferenceBudgetExceeded)

			// Remediation-hint detectors (#5577). All three read state the
			// spoke already maintains — no new GitHub calls, no new file
			// scans on the beat path. AgentErrorStreaks is nil until the
			// token collector's first bob-recording scan completes ("not
			// measured", hub carries forward); the other two are always live
			// measurements and send [] to clear a stale carry-forward.
			agentErrorStreaks := tokenCollector.AgentErrorStreaks()
			consentWedged := agentMgr.ConsentWedgedAgents()
			noCadenceAgents := gov.NoCadenceAgents()

			return &hub.HeartbeatPayload{
				AgentsWithModel:      &agentsWithModel,
				BudgetCurrentSpend:   budgetSpend,
				BudgetLimit:          budgetLimit,
				BudgetWindowStartsAt: budgetWindowStartsAt,
				BudgetWindowEndsAt:   budgetWindowEndsAt,
				BudgetExhausted:      budgetExhausted,
				BudgetIgnored:        budgetIgnored,
				HoldTotal:            holdTotal,
				AwaitingReview:       awaitingReview,
				SLAViolations:        slaViolations,
				TasksCompleted7d:     tasksCompleted7d,
				AgentErrorStreaks:    agentErrorStreaks,
				ConsentWedged:        consentWedged,
				NoCadenceAgents:      noCadenceAgents,
				// Read-back for hub-funded gateways: the hub clears its pending
				// record only when it sees the gateway named here, so a lost
				// delivery is re-offered rather than dropped. Names only — the
				// key never leaves the spoke.
				GatewayNames: dashSrv.ConfiguredGatewayNames(),
				// Hash only, never the raw token: lets the hub verify this
				// spoke's upgrade-proof credential without reading the
				// hive-secrets secret from a cluster it may not reach
				// (pull-only). Empty when no token is configured.
				DashboardTokenHash: func() string {
					if cfg.Dashboard.AuthToken == "" {
						return ""
					}
					return hub.HashDashboardToken(cfg.Dashboard.AuthToken)
				}(),
				HiveID:            cfg.HiveID,
				Org:               cfg.Project.Org,
				AIAuthor:          cfg.Project.AIAuthor,
				AIAuthorEffective: cfg.EffectiveAIAuthor(),
				StartedAt:         processStartedAt.UTC().Format(time.RFC3339),
				// FD gauge (#3875): a socket leak reached 92,962 FDs and
				// self-DoSed spokes with nothing surfacing it. Report the count
				// and its rlimit every beat so the next leak is a climbing
				// number on the hub, not a manual /proc excavation.
				OpenFDs:     hub.OpenFDCount(),
				FDSoftLimit: hub.FDSoftLimit(),
				// Reporter names THIS process (the pod) so the hub can tell two
				// instances reporting as one hive apart — the pod name is the
				// hostname inside the container.
				Reporter: reporterName,
				// Advisory-staleness signal (mirrors StartedAt/uptime). Report the
				// last successful digest-post time only if the spoke has actually
				// posted one — a zero time is left as an empty string so the hub
				// reads it as UNKNOWN (not-advisory-mode / old spoke), never a
				// false stale alarm. The last post error rides alongside so a
				// working-App-but-failing-post hive can be flagged with its cause.
				AdvisoryLastPostedAt: func() string {
					postedAt, _, _ := dashSrv.AdvisoryState()
					if postedAt.IsZero() {
						return ""
					}
					return postedAt.UTC().Format(time.RFC3339)
				}(),
				AdvisoryError: func() string {
					_, _, errMsg := dashSrv.AdvisoryState()
					return errMsg
				}(),
				// Digest SHAPE: how many findings went out, and how many the
				// top-N cap withheld. The hub renders the pair so a capped
				// digest never reads as the complete picture.
				AdvisoryFindingCount: func() int {
					findings, _ := dashSrv.AdvisoryCounts()
					return findings
				}(),
				AdvisoryOverflowCount: func() int {
					_, overflow := dashSrv.AdvisoryCounts()
					return overflow
				}(),
				// Inference-backend auth-failure signal (repeated 401s from a
				// stale gateway key). Reported as its own field so the hub can
				// raise a dedicated inference-auth alert whose ROOT cause an
				// operator sees directly — distinct from the advisory-staleness
				// pill AdvisoryError also trips. Empty when inference auth is
				// healthy or the hive routes to no inference backend; self-clears
				// on the next successful inference call.
				InferenceAuthError: func() string {
					errMsg, _ := dashSrv.InferenceAuthState()
					return errMsg
				}(),
				ProviderLimitReason:     providerLimitReason,
				ProviderLimitRebuffs:    providerLimitRebuffs,
				RepoTargetMisconfigured: repoTargetMisconfigured(),
				RepoTargetIssue:         repoTargetIssueMessage(),
				Repos:                   cfg.Project.Repos,
				PrimaryRepo:             cfg.Project.PrimaryRepo,
				ACMMLevel:               acmmLvl,
				Agents:                  agents,
				Governor: hub.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs, WorkSource: func() string {
					if t := cfg.Governor.WorkSource.Type; t != "" && t != "github" {
						return t
					}
					return ""
				}()},
				// Tokens carries the spoke's authoritative cumulative token
				// total (same store the dashboard token panel and governor
				// budget read). It flows to the hub's My Hives token column so
				// heartbeat-only hives (reached via heartbeat, not hub-kubectl)
				// display real consumption. Refreshed each heartbeat, so the
				// column is as fresh as the last heartbeat. Despite the
				// "24h"-suffixed field name this is a lifetime/window total,
				// consistent with what the spoke dashboard already shows.
				Tokens24h: func() int64 {
					if tokenCollector == nil {
						return 0
					}
					if summary := tokenCollector.Summary(); summary != nil {
						return summary.TotalTokens
					}
					return 0
				}(),
				Contributors: func() hub.ContributorSummary {
					reg, active := dashSrv.ContributorSummary()
					return hub.ContributorSummary{Registered: reg, Active: active}
				}(),
				Leaderboard: func() []hub.LeaderboardEntry {
					lb := dashSrv.LeaderboardForHub()
					out := make([]hub.LeaderboardEntry, len(lb))
					for i, e := range lb {
						out[i] = hub.LeaderboardEntry{
							GitHubUsername: e.GitHubUsername,
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
				// Report who has a live dashboard session so the hub can accumulate
				// per-user "time in hive". Bare usernames only — never session
				// ids/tokens (ActiveSessionUsernames guarantees this).
				ActiveSessionUsers: dashSrv.ActiveSessionUsernames(),
				// The honest subset of the above: users whose browser reported
				// focused, recent-input presence (see dashboard/presence.go).
				// An idle open tab appears in ActiveSessionUsers but not here.
				EngagedSessionUsers: dashSrv.EngagedSessionUsernames(),
				// Per-user last audit-logged real action, so the hub can tell
				// users who DO things from users who merely stay logged in.
				UserLastActions: dashSrv.UserLastActions(),
				Owner: func() string {
					if td, err := os.ReadFile("/data/gh-user-token"); err == nil {
						tok := strings.TrimSpace(string(td))
						if tok != "" {
							// gh-user-token is a github.com OAuth token — validate its
							// identity against github.com, not the (possibly GHE) repo host.
							if u, err := github.ValidateToken(tok, cfg.GitHub.OAuthAPIURL()); err == nil {
								return u.Login
							}
						}
					}
					return ""
				}(),
				// Report the API URL we are actually running against so the hub
				// can see whether a GitHub Enterprise API URL it delivered has
				// landed. Resolved (never empty) so the hub can distinguish
				// "public github.com" from "spoke too old to report this".
				GitHubAPIURL: cfg.GitHub.ResolvedAPIURL(),
				Health:       dashSrv.HealthSummary(),
				DashboardURL: func() string {
					if cfg.Hub.DashboardURL != "" {
						return cfg.Hub.DashboardURL
					}
					// Prefer the host our OWN Route/Ingress actually serves.
					//
					// The synthesised "<hiveID>.<hub host>" below is only
					// correct when this spoke is fronted by the hub's wildcard
					// domain, i.e. co-located with the hub. On any other
					// cluster that name resolves — via that same wildcard — to
					// the HUB's router, which has no backend for it and returns
					// 503, so the hub linked users at a hostname that could
					// never work while our real Route served fine. Reading the
					// live object is the only source that is right on every
					// cluster, and on a pull-only cluster the hub cannot read
					// it, so the spoke must report it.
					if host := hub.SpokeServedHost(ctx); host != "" {
						return "https://" + host
					}
					if cfg.HiveID != "" && cfg.Hub.URL != "" {
						if u, err := url.Parse(cfg.Hub.URL); err == nil && u.Host != "" {
							return fmt.Sprintf("https://%s.%s", cfg.HiveID, u.Host)
						}
					}
					return fmt.Sprintf("http://localhost:%d", cfg.Dashboard.Port)
				}(),
				SnapshotURL: cfg.Hub.SnapshotURL,
				HiveType:    cfg.Hub.HiveType,
				ClusterID:   cfg.Hub.ClusterID,
				IsPublic:    cfg.Hub.IsPublic,
				Version:     version,
				GitHash:     gitShort,
				GitBranch:   gitBranch,
				// The image ref the Deployment tracks, read in-cluster and
				// cached. The hub cannot see it for firewalled spokes, and it
				// is the only way to distinguish a hive pinned to an immutable
				// SHA tag (which can never receive a rolling upgrade) from one
				// riding <branch>-latest. Empty off-cluster — never guessed.
				ImageRef: hub.SelfDeploymentImage(),
				// The GitHub instance this spoke actually runs against. Only
				// the spoke knows this for certain: a hive's GitHub can differ
				// from its cluster's default, so the hub cannot infer it.
				// Reported as a bare hostname via HostLabel(), which reads BOTH
				// base_url and api_url — a GHE placeholder with base_url:"" but
				// api_url: github.ibm.com must report github.ibm.com, not be
				// silently rendered as github.com in the spokes table.
				GitHubHost:         cfg.GitHub.HostLabel(),
				GitHubAppRequired:  dashSrv.IsGitHubAppRequired(),
				GitHubAppPermIssue: dashSrv.GetGitHubAppPermIssue(),
				GitHubAppState:     dashSrv.GetGitHubAppState(),
				GitHubAppTokenStatus: func() string {
					status, _, _ := githubAppTokenHeartbeatFields(cfg, dashSrv.GetGitHubAppPermIssue())
					return status
				}(),
				GitHubAppTokenLastMintAt: func() string {
					_, lastMintAt, _ := githubAppTokenHeartbeatFields(cfg, dashSrv.GetGitHubAppPermIssue())
					return lastMintAt
				}(),
				GitHubAppTokenError: func() string {
					_, _, errMsg := githubAppTokenHeartbeatFields(cfg, dashSrv.GetGitHubAppPermIssue())
					return errMsg
				}(),
				PendingGitHubAppInstall: dashSrv.IsPendingGitHubAppInstall(),
				AutoUpgrade:             cfg.Hub.AutoUpgrade,
				ClusterHealth: func() *hub.HeartbeatClusterHealthReport {
					if os.Getenv("HIVE_CLUSTER_ID") == "" {
						return nil
					}
					return hub.CollectClusterHealth(logger)
				}(),
				PRsMerged90d:                 prsMerged,
				PRsRejected90d:               prsRejected,
				CVEsClosed:                   cvesClosed,
				FleetStatsCollectedAt:        fleetStatsCollectedAt,
				RepoActivity:                 repoActivity,
				RepoActivityCollectedAt:      repoActivityCollectedAt,
				RepoActivityWindowHours:      repoActivityWindowHours,
				RepoActivityCountWindowHours: repoActivityCountWindowHours,
				// Report WHICH App key we hold, never the key. The hub compares
				// this against its per-cluster key and pushes a correction only
				// on a mismatch, so a spoke already holding the right key costs
				// nothing and a spoke holding the wrong one self-heals.
				GitHubAppKeyFingerprint: appKeys.ReportedFingerprint(cfg.GitHub.KeyFile, cfg.GitHub.AppID),
				GitHubAppKeyPerHive:     appKeys.HasPerHiveKey(cfg.GitHub.KeyFile, cfg.GitHub.AppID),
				// Report the App this hive believes it authenticates as. The hub
				// pairs it with the fingerprint above to tell a per-hive key that
				// is WRONG for this App from one that is deliberately for another.
				GitHubAppID: cfg.GitHub.AppID,
				// Report the REST of the identity set too. app_id alone cannot
				// distinguish a correctly-delivered identity from a
				// half-applied one: a GHE app_id with an empty api_url looks
				// identical to the hub, and 404s on every token request. All
				// four together let the hub see the whole set.
				GitHubAppSlug:        cfg.GitHub.AppSlug,
				GitHubInstallationID: cfg.GitHub.InstallationID,
				GitHubBaseURL:        cfg.GitHub.BaseURL,
				// Report the fingerprint of every ADDITIONAL per-app-id key already
				// on the PVC, so the hub delivers the fleet's other App keys once
				// and then stops re-sending them.
				GitHubAppKeysHeld: appKeys.HeldFingerprints(),
				// Component reach counters (#3993, phase 2a of #3973): per
				// (component, running commit) span counts aggregated in-process,
				// exporter or not (D2) — the heartbeat is the only channel that
				// reaches every spoke, pull-only ones included (D1). nil until
				// the first span, which the hub reads as "no data", never as
				// zero reach. Capped at tracing.MaxReachComponents entries.
				ComponentReach: tracing.ReachSnapshot(),
			}
		}, heartbeatSendInterval, logger, hub.RestartSpokeCallback(func() {
			if up := time.Since(processStartedAt); up < spokeRestartMinUptime {
				logger.Info("hub requested a spoke restart; ignoring — this process just started",
					"uptime", up.Round(time.Second))
				return
			}
			logger.Warn("hub requested a spoke restart — rolling this deployment",
				"reporter", reporterName)
			if err := hub.RolloutRestartSelf(logger); err != nil {
				// Do NOT exit here: without deployment-patch RBAC an exit would
				// restart onto the same state every delivery and look like a
				// crash-loop. The error names the missing Role instead.
				logger.Error("spoke restart failed: could not patch own Deployment",
					"error", err,
					"hint", "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding)")
			}
		}), hub.UpgradeCallback(func(targetSHA string) {
			const upgradeMarkerPath = "/data/upgrade-requested"

			// attemptCount carries the number of PREVIOUS failed attempts for this
			// (current_sha → target_sha) pair, read from the marker below.
			attemptCount := 0

			// Never self-upgrade to the commit we are already running. The hub
			// may instruct an upgrade to a short SHA that is a prefix of our
			// full gitShort (or vice-versa); treating that as "behind" caused a
			// crash-loop (patch → 403 → os.Exit → repeat) on hives sitting
			// exactly at HEAD. Prefix-compare so same-commit is a no-op.
			if sha1, sha2 := targetSHA, gitShort; sha1 != "" && sha2 != "" {
				n := len(sha1)
				if len(sha2) < n {
					n = len(sha2)
				}
				if strings.EqualFold(sha1[:n], sha2[:n]) {
					logger.Info("self-upgrade skipped: target is the running commit",
						"target", targetSHA, "current", gitShort)
					return
				}
			}

			// A previous process attempted an upgrade and we booted with the same
			// git hash, so the image did not actually change: the attempt FAILED.
			// Back off rather than retrying instantly (that was a crash-loop), but
			// do NOT latch forever — "image unchanged" is the signature of a failed
			// upgrade, not a reason to stop trying. The latch is keyed on
			// (current_sha → target_sha) and bounded to selfUpgradeMaxAttempts, so a
			// NEW target always gets a fresh budget and a transient failure (an RBAC
			// Role that showed up late, a registry blip) still converges.
			if markerData, err := os.ReadFile(upgradeMarkerPath); err == nil {
				m := parseUpgradeMarker(markerData)
				if m.CurrentSHA == gitShort && sameUpgradeTarget(m.TargetSHA, targetSHA) {
					if m.Attempts >= selfUpgradeMaxAttempts {
						// Terminal: report it LOUDLY and tell the hub, so the UI stops
						// claiming "Upgrading" forever and a human sees the real cause.
						logger.Error("self-upgrade FAILED: giving up after repeated attempts (image never changed)",
							"target", targetSHA,
							"current", gitShort,
							"attempts", m.Attempts,
							"max_attempts", selfUpgradeMaxAttempts,
							"last_error", m.LastError,
							"hint", "the spoke must be able to get/patch its own Deployment; check the hive-self-upgrade Role/RoleBinding in this namespace",
						)
						hub.ReportUpgradeFailure(hubURL, cfg.HiveID, targetSHA, gitShort,
							fmt.Sprintf("self-upgrade failed after %d attempts: %s", m.Attempts, m.LastError), logger)
						return
					}
					// Exponential backoff between attempts so a hard failure does not
					// spin every heartbeat while a recoverable one still retries.
					backoff := selfUpgradeBaseBackoff << (m.Attempts - 1)
					if backoff > selfUpgradeMaxBackoff {
						backoff = selfUpgradeMaxBackoff
					}
					if since := time.Since(m.RequestedAt); since < backoff {
						logger.Warn("self-upgrade retry deferred: backing off after a failed attempt",
							"target", targetSHA,
							"current", gitShort,
							"attempts", m.Attempts,
							"retry_in", (backoff - since).Round(time.Second),
							"last_error", m.LastError,
						)
						return
					}
					logger.Warn("self-upgrade retrying after a failed attempt (image unchanged)",
						"target", targetSHA,
						"current", gitShort,
						"attempt", m.Attempts+1,
						"max_attempts", selfUpgradeMaxAttempts,
						"last_error", m.LastError,
					)
					attemptCount = m.Attempts
				} else {
					// Different SHA or a different target — the old marker is stale.
					if err := os.Remove(upgradeMarkerPath); err != nil && !os.IsNotExist(err) {
						logger.Warn("failed to clear stale upgrade marker", "path", upgradeMarkerPath, "error", err)
					}
				}
			}

			// Minimum uptime before allowing self-upgrade to avoid restart loops.
			const minUptimeBeforeUpgrade = 5 * time.Minute
			uptime := time.Since(startTime)
			if uptime < minUptimeBeforeUpgrade {
				logger.Warn("self-upgrade deferred: minimum uptime not reached",
					"target", targetSHA,
					"current", gitShort,
					"uptime", uptime.Round(time.Second),
					"min_uptime", minUptimeBeforeUpgrade,
				)
				return
			}

			// Record the attempt BEFORE acting: if the process dies mid-upgrade the
			// next boot must still see an incremented count, otherwise a crash loop
			// would retry without ever exhausting the budget.
			writeUpgradeMarker(upgradeMarkerPath, upgradeMarker{
				TargetSHA:   targetSHA,
				CurrentSHA:  gitShort,
				RequestedAt: time.Now().UTC(),
				Attempts:    attemptCount + 1,
			}, logger)

			logger.Info("self-upgrade triggered: sending upgrading heartbeat then exiting",
				"current", gitShort,
				"latest", targetSHA,
				"uptime", uptime.Round(time.Second),
			)

			hub.SendUpgradingHeartbeat(hubURL, func() *hub.HeartbeatPayload {
				if !cfg.Hub.Enabled {
					return nil
				}
				statuses := agentMgr.AllStatuses()
				govState := gov.GetState()
				currentMode := strings.ToLower(string(govState.Mode))
				agents := make([]hub.AgentSummary, 0, len(statuses))
				for name, proc := range statuses {
					mode := ""
					if ac, ok := cfg.Agents[name]; (ok && ac.OnDemand) || onDemandFromPack[name] {
						mode = "on_demand"
					}
					agents = append(agents, hub.NewAgentSummary(name, string(proc.State), mode,
						hub.AgentActivityFor(agentMgr, cfg, govState, currentMode, name, proc, onDemandFromPack)))
				}
				acmmLvl := 0
				if cfg.ACMMLevel != nil {
					acmmLvl = *cfg.ACMMLevel
				}
				providerLimitReason, providerLimitRebuffs := hub.ProviderLimitHeartbeatFields(agents, dashboard.InferenceBudgetExceeded)
				return &hub.HeartbeatPayload{
					HiveID: cfg.HiveID,
					Org:    cfg.Project.Org,
					// Project identity rides even this minimal beat. The hub
					// rebuilds the registry entry from each payload VERBATIM
					// (no carry-forward for these fields), and this beat is
					// the LAST one the hub holds for the whole restart window
					// that follows — omitting repos/primary_repo here blanked
					// the entry (org set, primaryRepo "", repos []) until the
					// new process's first successful collect, breaking the
					// public-directory row (no repo link) and rendering the
					// hive name as "org/". Both values are plain config reads,
					// exactly as cheap as Org above.
					Repos:                   cfg.Project.Repos,
					PrimaryRepo:             cfg.Project.PrimaryRepo,
					ACMMLevel:               acmmLvl,
					Agents:                  agents,
					GitHash:                 gitShort,
					ClusterID:               cfg.Hub.ClusterID,
					HiveType:                cfg.Hub.HiveType,
					IsPublic:                cfg.Hub.IsPublic,
					Version:                 version,
					RepoTargetMisconfigured: repoTargetMisconfigured(),
					RepoTargetIssue:         repoTargetIssueMessage(),
					ProviderLimitReason:     providerLimitReason,
					ProviderLimitRebuffs:    providerLimitRebuffs,
					// Remediation-hint detectors (#5577): all three are
					// cheap in-memory reads, so even this minimal upgrading
					// beat carries them — the pod is about to restart, and
					// carrying the last real measurement across the roll keeps
					// a live wedge visible instead of blanking it.
					AgentErrorStreaks: tokenCollector.AgentErrorStreaks(),
					ConsentWedged:     agentMgr.ConsentWedgedAgents(),
					NoCadenceAgents:   gov.NoCadenceAgents(),
				}
			}, targetSHA, logger)

			// A plain rollout restart only advances a deployment tracking a
			// MUTABLE tag. On a SHA-pinned deployment it relaunches the very
			// same image, so the hive reports the old hash and the hub re-sends
			// this upgrade every heartbeat — a restart loop that never lands.
			// UpgradeSelfToSHA patches the image instead when we are pinned.
			needsRestart, err := hub.UpgradeSelfToSHA(logger, targetSHA)
			if err != nil {
				logger.Warn("pinned-image upgrade failed, falling back to rolling restart",
					"target", targetSHA, "error", err)
				recordUpgradeError(upgradeMarkerPath, err, logger)
				needsRestart = true
			}
			if needsRestart {
				if err := hub.RolloutRestartSelf(logger); err != nil {
					// This is the wedge. os.Exit here restarts the pod onto the
					// SAME image, so the upgrade silently never lands. It is an
					// ERROR, not a Warn, and the cause (typically a 403 because
					// the spoke lacks patch on its own Deployment) must be both
					// persisted for the next attempt and reported to the hub so
					// the UI stops showing a permanent "Upgrading".
					logger.Error("self-upgrade FAILED: could not patch own Deployment, restarting onto the same image",
						"target", targetSHA,
						"current", gitShort,
						"error", err,
						"hint", "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding)",
					)
					recordUpgradeError(upgradeMarkerPath, err, logger)
					hub.ReportUpgradeFailure(hubURL, cfg.HiveID, targetSHA, gitShort, err.Error(), logger)
					// Exit NON-ZERO. Exiting 0 on a failed upgrade told Kubernetes
					// the process had completed successfully, so the restart looked
					// routine and nothing — not the pod's exit code, not an event,
					// not a probe — recorded that an upgrade had just failed. A
					// non-zero code makes the failure visible in the pod's
					// lastState.terminated and in `kubectl describe`.
					os.Exit(selfUpgradeFailureExitCode)
				}
			}
			// Rolling restart initiated — K8s will start a new pod and
			// send SIGTERM to this one once the replacement is Ready.
			// Block here so the process stays alive until terminated.
			logger.Info("waiting for SIGTERM after rolling restart")
			<-ctx.Done()
		}), hub.GitHubAppConfigCallback(func(ghCfg *hub.HeartbeatGitHubAppConfig) {
			logger.Info("received github app config via heartbeat",
				"app_id", ghCfg.AppID,
				"installation_id", ghCfg.InstallationID,
				"has_key", ghCfg.PrivateKey != "",
			)

			// WRITE-PATH GUARD. Compute the identity this push WOULD produce and
			// refuse the whole delivery if it is internally inconsistent.
			//
			// This is the guard the 2026-07-31 incident needed. That push carried
			// the GHE app_id and slug with no api_url; the adoption below applies
			// each field independently under an "empty means unchanged" contract,
			// so seven public-GitHub hives took the GHE App ID, kept api_url: "",
			// and every token request 404'd. Refusing the whole delivery leaves
			// those hives on their previous, working identity instead of a half of
			// two identities.
			//
			// Rejection is loud and repeats on every beat: the hub keeps pushing
			// until the spoke reports back, so a silent skip would be an invisible
			// permanent stall. There is no auto-repair here — the fix is on the
			// hub, in clusters.json.
			if prospective := prospectiveGitHubIdentity(cfg.GitHub, ghCfg); prospective != nil {
				if err := config.RejectIdentitySet(*prospective); err != nil {
					logger.Error("REFUSING hub github app config: the pushed identity set is inconsistent and would half-apply — nothing was changed",
						"error", err,
						"pushed_app_id", ghCfg.AppID,
						"pushed_app_slug", ghCfg.AppSlug,
						"current_app_id", cfg.GitHub.AppID,
						"current_api_url", cfg.GitHub.APIURL,
						"current_base_url", cfg.GitHub.BaseURL,
						"remedy", "correct github_app_id/github_app_slug/github_api_url/github_base_url for this cluster on the hub",
					)
					return
				}
			}

			// Write the delivered key to the path that NAMES the App it belongs
			// to, not to a generic filename.
			//
			// /data/gh-app-key.pem carries no evidence of which App signed it, so
			// a key delivered for one App silently becomes "the key" for whatever
			// app_id the config later claims. On 2026-07-31 that is exactly what
			// happened: all 33 heartbeat-only-cluster spokes had key_file pinned to the generic
			// path holding the GHE key, so correcting app_id to the public App
			// still produced 404 Integration not found — the right key was
			// already on disk at gh-app-key-3568013.pem and unreachable, because
			// an explicit key_file short-circuits resolveAppKeyFile before the
			// per-app-id lookup runs.
			//
			// Deriving the filename from the app_id makes that mismatch
			// unrepresentable: a key can only be found under the App it was
			// delivered for. Falls back to the generic path when the delivery
			// names no App, so a key is never dropped on the floor.
			keyPath := deliveredKeyPath(ghCfg.AppID)
			// keyChanged gates dropping the cached installation token below: a
			// token minted under the previous key is invalid the moment the key
			// is replaced, but a redelivery of the SAME key must not throw away a
			// perfectly good token every heartbeat.
			keyChanged := false
			if ghCfg.PrivateKey != "" {
				// Fingerprint before and after so the key rotation is auditable
				// from the spoke's own logs. Fingerprints only — the key itself
				// is never logged.
				beforeFP, _ := config.AppKeyFingerprintFromFile(keyPath)
				if err := os.WriteFile(keyPath, []byte(ghCfg.PrivateKey), appKeys.FileMode); err != nil {
					logger.Error("failed to write github app key from heartbeat", "error", err)
					return
				}
				// os.WriteFile does NOT re-apply the mode to a file that already
				// exists, so a key written by an older build (or restored from a
				// looser-moded source) would keep its old permissions forever.
				// Chmod unconditionally so every path converges on 0600.
				if err := os.Chmod(keyPath, appKeys.FileMode); err != nil {
					logger.Warn("could not tighten github app key permissions", "path", keyPath, "error", err)
				}
				afterFP, _ := config.AppKeyFingerprintFromFile(keyPath)
				keyChanged = afterFP != "" && afterFP != beforeFP
				logger.Info("github app private key written via heartbeat",
					"path", keyPath,
					"from_fingerprint", beforeFP,
					"to_fingerprint", afterFP,
					"key_changed", keyChanged,
				)
				if keyChanged {
					// Invalidate before the new client is built, so nothing can
					// read the dead token out of the shared on-disk cache in
					// between. Agents read that file directly.
					appAuth.DropCachedToken()
				}
			}

			// Persist the fleet's ADDITIONAL App keys — every OTHER App's key,
			// keyed by app_id — so this spoke can sign for the App it is actually
			// configured as even when that is NOT its cluster's default. This is
			// the both-keys fix: a github.com hive on a GHE cluster now receives
			// and stores the github.com key here, and resolveAppKeyFile selects it
			// by matching cfg.GitHub.AppID.
			//
			// Written to distinct /data/gh-app-key-<appid>.pem files, so they never
			// collide with the primary /data/gh-app-key.pem above. Writing one that
			// matches our OWN app_id must take effect immediately: flip keyChanged
			// so the client is rebuilt below, exactly as a primary-key change does.
			// applyDeliveredPerAppKey writes ONE (app_id, key) pair to its
			// per-app-id file and reports whether that changed the key we
			// ourselves sign with. Shared by the (now-inert) AdditionalKeys loop
			// and the targeted SecondaryKey delivery below so both write through
			// identical code — the alternative is two copies of an atomic 0600
			// write, one of which eventually loses a guard.
			applyDeliveredPerAppKey := func(kind string, appID int64, privateKey string) {
				if privateKey == "" || appID <= 0 {
					return
				}
				perAppPath := appKeys.PerAppIDKeyPath(appID)
				beforeFP, _ := config.AppKeyFingerprintFromFile(perAppPath)
				fp, err := appKeys.WritePerAppIDKey(appID, privateKey)
				if err != nil {
					logger.Error("failed to write "+kind+" github app key from heartbeat",
						"app_id", appID, "error", err)
					return
				}
				changed := fp != "" && fp != beforeFP
				logger.Info(kind+" github app private key written via heartbeat",
					"app_id", appID,
					"path", perAppPath,
					"from_fingerprint", beforeFP,
					"to_fingerprint", fp,
					"key_changed", changed,
				)
				// If this key is for the App we ourselves authenticate as, it is
				// now the key resolveAppKeyFile will pick — treat it like a
				// primary-key rotation so the client rebuild below uses it.
				if changed && appID == cfg.GitHub.AppID {
					keyChanged = true
					appAuth.DropCachedToken()
				}
			}
			for _, ak := range ghCfg.AdditionalKeys {
				applyDeliveredPerAppKey("additional", ak.AppID, ak.PrivateKey)
			}

			// The OPTIONAL SECOND App key (#4815), delivered targeted at this
			// hive alone rather than broadcast. It lands in the same
			// /data/gh-app-key-<appid>.pem namespace the spoke has always used,
			// so heldPerAppIDKeyFingerprints reports it back on the next beat
			// (which is what stops the hub re-pushing it) and the Forge App tab
			// renders it, both with no further change. nil for every hive with no
			// second App.
			if ghCfg.SecondaryKey != nil {
				applyDeliveredPerAppKey("secondary", ghCfg.SecondaryKey.AppID, ghCfg.SecondaryKey.PrivateKey)
			}

			// Adopt a hub-delivered app_id only when it names a REAL App. Zero
			// means "not speaking to this field"; the placeholder sentinel is
			// what a pre-provisioned hive already carries, so re-adopting it
			// would overwrite a good app_id with a non-App on any heartbeat
			// that echoed the original seed back.
			if ghCfg.AppID != 0 && ghCfg.AppID != config.PlaceholderAppID {
				cfg.GitHub.AppID = ghCfg.AppID
			}
			// A zero installation_id means "the hub is not speaking to this
			// field", not "clear it". The cluster-wide key reconcile repairs the
			// KEY on hives whose installation_id is already correct (and which
			// the hub does not track); assigning zero here would blank a working
			// value and turn a key-only fault into a total auth outage.
			if next, cleared := nextInstallationID(cfg.GitHub.InstallationID, ghCfg); cleared {
				// The banner and the hub must flip to not-installed NOW, not
				// when the cached token dies an hour from now. Same config-truth
				// rule as startup.
				dashSrv.SetGitHubAppRequired(true)
				dashSrv.SetGitHubAppState(github.AppStateNotInstalled.String())
				logger.Info("clearing github app installation_id on operator request",
					"was", cfg.GitHub.InstallationID)
				cfg.GitHub.InstallationID = next
			} else {
				cfg.GitHub.InstallationID = next
			}
			// Deliberately NOT `cfg.GitHub.KeyFile = keyPath`.
			//
			// key_file is DERIVABLE from app_id (resolveAppKeyFile prefers
			// /data/gh-app-key-<app_id>.pem, the only key correct by
			// construction). Persisting the path turns a derived value into a
			// stored one that outlives the App it was derived for: once written,
			// it short-circuits resolveAppKeyFile on every later boot, so a
			// corrected app_id keeps signing with the previous App's key. That is
			// what left all 33 heartbeat-only-cluster spokes pinned to the GHE key.
			//
			// Leaving it empty lets derivation run every time, so the key always
			// tracks the App actually in effect. An operator-set key_file still
			// wins — that override is intentional, for a hive whose App this
			// build does not know (e.g. a hive on a third App ID with a key at a
			// bespoke path).
			// Same "empty means unchanged" contract as installation_id: adopting
			// an empty slug would blank a working install link.
			if ghCfg.AppSlug != "" && cfg.GitHub.AppSlug != ghCfg.AppSlug {
				logger.Info("adopting github app slug from hub",
					"was", cfg.GitHub.AppSlug, "now", ghCfg.AppSlug)
				cfg.GitHub.AppSlug = ghCfg.AppSlug
			}
			// Adopt the forge URLs from the SAME delivery as the App above.
			// prospectiveGitHubIdentity already validated all four fields
			// together, so reaching here means the complete set is coherent —
			// applying the App without its URLs would undo that check by leaving
			// the spoke pointed at the previous forge.
			//
			// Same "empty means unchanged" contract: empty is the correct steady
			// state for a public hive, so it can never be read as "blank this".
			if ghCfg.APIURL != "" && cfg.GitHub.APIURL != ghCfg.APIURL {
				logger.Info("adopting github api url from hub",
					"was", cfg.GitHub.APIURL, "now", ghCfg.APIURL)
				cfg.GitHub.APIURL = ghCfg.APIURL
			}
			if ghCfg.BaseURL != "" && cfg.GitHub.BaseURL != ghCfg.BaseURL {
				logger.Info("adopting github base url from hub",
					"was", cfg.GitHub.BaseURL, "now", ghCfg.BaseURL)
				cfg.GitHub.BaseURL = ghCfg.BaseURL
			}

			// Persist the adopted App IDENTITY to the PVC overlay, exactly as the
			// claimed-project-config callback persists what it adopts.
			//
			// Without this the adoption lived only in memory. The key files are
			// written to /data (durable), but app_id/app_slug/installation_id were
			// not, so on every pod restart the entrypoint re-merged the ConfigMap
			// seed and the spoke reverted to whatever App it was provisioned with —
			// silently undoing a completed repair and making the hub's push look
			// like it had never happened. That is how a GHE hive kept re-appearing
			// with the github.com app_id and an empty slug across restarts.
			//
			// Saved even when the App is not yet usable (installation_id still 0):
			// the corrected app_id and slug are precisely what the owner needs on
			// disk so the dashboard renders a working install link BEFORE they have
			// installed anything.
			if err := cfg.Save(); err != nil {
				logger.Error("failed to persist github app config from heartbeat", "error", err)
			}

			// Resolve the key file the same way startup does, so a hive whose
			// only correct key arrived as an ADDITIONAL per-app-id key (no
			// primary key_file configured — the exact heartbeat-only-cluster state) still finds
			// it: resolveAppKeyFile prefers /data/gh-app-key-<appid>.pem for the
			// app_id we now claim. An explicit key_file still wins outright.
			rebuildKeyFile := appKeys.Resolve(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID)
			if cfg.GitHub.HasUsableApp() && rebuildKeyFile != "" {
				newAppAuth, err := github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, rebuildKeyFile, logger, cfg.GitHub.ResolvedAPIURL())
				if err != nil {
					logger.Error("github app auth init via heartbeat failed", "error", err)
					return
				}
				// Deliberately NOT persisted. rebuildKeyFile is the RESOLVED
				// path, and writing a resolved value back into config is what
				// converts a derivation into a pin: the next boot reads it as an
				// explicit key_file, short-circuits resolveAppKeyFile, and keeps
				// using this App's key even after app_id changes. Re-resolving on
				// every use costs a stat and cannot go stale.
				// Hub-delivered creds can carry a wrong installation_id just as
				// easily as a hand-edited config; correct (and persist) it
				// before building a client that would 403 on every write.
				healGitHubAppInstallation(ctx, newAppAuth, cfg, logger)
				newClient := github.NewClientFromAppWithBotLogin(newAppAuth, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.BotLogin())
				if len(cfg.Governor.Labels.Exempt) > 0 {
					newClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
					newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
				}
				newClient.SetIssueFilter(cfg.Project.IssueFilter)
				ghClient = newClient
				appAuth = newAppAuth
				agentMgr.SetAppAuth(newAppAuth)
				// Immediate per-agent token delivery: hosted spokes get their
				// App creds via this heartbeat path AFTER agents have already
				// launched (with empty 0-byte caches), so waiting for the next
				// 40-minute tick guarantees a window of gh 401s (#4072).
				go agentMgr.RefreshAgentTokens(ctx)
				dashSrv.UpdateGitHubClient(newClient, newAppAuth)
				dashSrv.SetGitHubAppRequired(false)
				dashSrv.ClearPendingGitHubAppInstall()
				logger.Info("github app configured via heartbeat delivery",
					"app_id", cfg.GitHub.AppID,
					"installation_id", cfg.GitHub.InstallationID,
				)
			}
		}), hub.HubBannerCallback(func(banner *hub.HubBanner) {
			if banner == nil {
				dashSrv.ClearHubBanner()
				return
			}
			dashSrv.SetHubBanner(banner.ID, banner.Message, banner.Color)
		}), hub.VisibilityCallback(func(isPublic bool) {
			if cfg.Hub.IsPublic != isPublic {
				logger.Info("hub overrode visibility via heartbeat",
					"was", cfg.Hub.IsPublic, "now", isPublic)
				cfg.Hub.IsPublic = isPublic
			}
		}), hub.SwitchBranchCallback(func(tag string) {
			// Branch switch delivered via heartbeat (the hub couldn't reach
			// this cluster over kubectl). Patch our OWN deployment image via
			// the in-cluster K8s API — the pod has no kubectl binary, but its
			// SA holds the hive-self-upgrade role (patch on deployment/hive).
			// K8s then rolls the pod onto the new tag.
			image := "ghcr.io/hivecommons/hive:" + tag
			if err := hub.SwitchImageSelf(logger, image); err != nil {
				logger.Warn("branch switch via heartbeat failed", "tag", tag, "image", image, "error", err)
				return
			}
		}), hub.AuthorizedUsersCallback(func(users []string, names map[string]string) {
			// The hub delivered its authoritative access list. Reconcile our
			// login allowlist so Manage Access grants take effect on this
			// heartbeat-only spoke without any kubectl push. The dashboard reads
			// cfg.Dashboard.AuthorizedUsers live on each login, so updating it in
			// place is enough. Only log when it actually changes to avoid noise.
			if !sameStringSlice(cfg.Dashboard.AuthorizedUsers, users) {
				logger.Info("authorized users updated from hub heartbeat",
					"was", len(cfg.Dashboard.AuthorizedUsers), "now", len(users))
				cfg.Dashboard.AuthorizedUsers = users
			}
			// AuthorizedUserNames is purely cosmetic (see its doc) — it never
			// gates sign-in, so it's fine to just take whatever the hub sent
			// (including nil, which means "no names known") without the
			// same-value guard above.
			cfg.Dashboard.AuthorizedUserNames = names
		}), hub.ProjectConfigCallback(func(pc *hub.HeartbeatProjectConfig) {
			// The hub assigned this (previously placeholder) hive a real project.
			// Reconcile our running project config so agents work the claimed
			// org/repos at the claimed maturity level. This is the ONLY delivery
			// channel on heartbeat-only clusters (the heartbeat-only cluster) — no kubectl push is
			// possible. The hub keeps sending this every beat until we report the
			// matching project back, so an idempotent no-op when already matched
			// is expected and cheap.
			if pc == nil {
				return
			}
			// A URL-only push (org empty, dashboard_url set) delivers the vanity
			// dashboard URL to an already-claimed hive whose meta project is stale/
			// empty on the hub — we must still adopt+report it, or the hub keeps
			// showing the raw placeholder host forever. Handle it BEFORE the
			// org-empty bail below, which exists so an empty project never blanks a
			// working config: with no org there is nothing to reconcile except the
			// URL, so adopt it, persist, and return without touching the project.
			if pc.Org == "" {
				if pc.DashboardURL != "" && cfg.Hub.DashboardURL != pc.DashboardURL {
					logger.Info("adopting vanity dashboard URL from hub heartbeat (url-only push)",
						"was", cfg.Hub.DashboardURL, "now", pc.DashboardURL)
					cfg.Hub.DashboardURL = pc.DashboardURL
					if err := cfg.Save(); err != nil {
						logger.Error("failed to save adopted vanity dashboard URL", "error", err)
					}
				}
				return
			}
			if issue := config.ValidateProjectRepoTargets(pc.Org, pc.Repos, pc.PrimaryRepo, cfg.GitHub.HostLabel()); issue != nil {
				logger.Error("REFUSING hub project config: repo target is misconfigured — project left unchanged",
					"error", issue.Message,
					"pushed_org", pc.Org,
					"pushed_repos", pc.Repos,
					"pushed_primary_repo", pc.PrimaryRepo,
				)
				return
			}
			curACMM := 0
			if cfg.ACMMLevel != nil {
				curACMM = *cfg.ACMMLevel
			}
			// Adopt the vanity dashboard URL delivered on claim, if any. We
			// report cfg.Hub.DashboardURL in our heartbeats, so once set the hub
			// registry's dashboardUrl becomes the vanity URL (not the placeholder
			// host). Track it in the already-reconciled check so a URL-only change
			// still gets applied and persisted.
			vanityMatched := pc.DashboardURL == "" || cfg.Hub.DashboardURL == pc.DashboardURL
			authorMatched := pc.AIAuthor == "" || cfg.Project.AIAuthor == pc.AIAuthor
			apiURLMatched := pc.GitHubAPIURL == "" || cfg.GitHub.APIURL == pc.GitHubAPIURL
			// Issue filter: nil means "the hub is not speaking to this field"
			// (mirrors AIAuthor's empty-means-keep), so the hub's every-beat
			// echo can never blank a locally configured filter.
			issueFilterMatched := pc.IssueFilter == nil || cfg.Project.IssueFilter.Equal(*pc.IssueFilter)
			if cfg.Project.Org == pc.Org &&
				sameStringSlice(cfg.Project.Repos, pc.Repos) &&
				cfg.Project.PrimaryRepo == pc.PrimaryRepo &&
				curACMM == pc.ACMMLevel &&
				authorMatched &&
				apiURLMatched &&
				issueFilterMatched &&
				vanityMatched {
				return // already reconciled
			}
			if pc.DashboardURL != "" && cfg.Hub.DashboardURL != pc.DashboardURL {
				logger.Info("adopting vanity dashboard URL from hub heartbeat",
					"was", cfg.Hub.DashboardURL, "now", pc.DashboardURL)
				cfg.Hub.DashboardURL = pc.DashboardURL
			}
			logger.Info("project config updated from hub heartbeat (placeholder claimed)",
				"was_org", cfg.Project.Org, "now_org", pc.Org,
				"repos", pc.Repos, "primary_repo", pc.PrimaryRepo,
				"acmm_level", pc.ACMMLevel)
			cfg.Project.Org = pc.Org
			cfg.Project.Repos = pc.Repos
			cfg.Project.PrimaryRepo = pc.PrimaryRepo
			// Only adopt a non-empty author. The hub echoes this struct back on
			// every beat, so assigning unconditionally would reset a locally
			// configured ai_author to "" each time — which is precisely what
			// kept the fleet-stats collector disabled on every hive.
			if pc.AIAuthor != "" {
				cfg.Project.AIAuthor = pc.AIAuthor
			}
			// Adopt a hub-delivered issue filter only when the hub actually
			// sent one (non-nil). A push without the field leaves the spoke's
			// locally configured filter untouched — the org/repos assignments
			// above never wipe it either, so a local filter SURVIVES claim
			// delivery. A non-nil but EMPTY filter is an explicit clear.
			if pc.IssueFilter != nil && !cfg.Project.IssueFilter.Equal(*pc.IssueFilter) {
				logger.Info("adopting issue filter from hub heartbeat",
					"require_labels", pc.IssueFilter.RequireLabels)
				cfg.Project.IssueFilter = *pc.IssueFilter
			}
			// Adopt a GitHub Enterprise API URL when the hub sends one. Empty
			// means "leave mine alone" — the spoke's own default is already
			// api.github.com, so this never clobbers a working config.
			if pc.GitHubAPIURL != "" && cfg.GitHub.APIURL != pc.GitHubAPIURL {
				// WRITE-PATH GUARD, mirroring the App-config callback: an api_url
				// that names a different forge than our app_id is the same
				// half-applied identity arriving from the other direction. Skip
				// only this field — the org/repos/ACMM adoption around it is
				// unrelated and must still land.
				prospective := cfg.GitHub
				prospective.APIURL = pc.GitHubAPIURL
				if err := config.RejectIdentitySet(prospective); err != nil {
					logger.Error("REFUSING hub GitHub API URL: it does not match this hive's app_id and would half-apply an identity — api_url left unchanged",
						"error", err,
						"pushed_api_url", pc.GitHubAPIURL,
						"current_api_url", cfg.GitHub.APIURL,
						"current_app_id", cfg.GitHub.AppID,
						"remedy", "correct github_api_url/github_app_id for this cluster on the hub",
					)
				} else {
					logger.Info("adopting GitHub API URL from hub heartbeat",
						"was", cfg.GitHub.APIURL, "now", pc.GitHubAPIURL)
					cfg.GitHub.APIURL = pc.GitHubAPIURL
				}
			}
			level := pc.ACMMLevel
			cfg.ACMMLevel = &level

			// Re-sync the GitHub client that caches the repo list (mirrors the
			// config-watcher reload path). The issue filter is cached the same
			// way, so re-install it too — a hub-delivered filter must take
			// effect on the next enumeration, not the next restart.
			ghClient.SetRepos(cfg.Project.Repos)
			ghClient.SetIssueFilter(cfg.Project.IssueFilter)

			// Persist to the PVC overlay so the claim survives a pod restart
			// (config save writes the overlay hive.yaml, same as level switches).
			if err := cfg.Save(); err != nil {
				logger.Error("failed to save claimed project config", "error", err)
			}
		}), hub.GatewayConfigCallback(func(gw *hub.HeartbeatGatewayConfig) {
			// The hub funded an OpenRouter gateway on this hive's behalf (scan-to-
			// fund from My Hives) and delivered it over the heartbeat channel — the
			// only path that reaches a firewalled/heartbeat-only spoke (the heartbeat-only cluster). We
			// store the key in our OWN per-gateway secret-file store and create the
			// "openrouter" gateway. The hub drains the delivery after sending, so
			// this fires once per fund; the key value is never logged.
			if gw == nil || gw.Key == "" {
				return
			}
			if err := dashSrv.ApplyDeliveredGateway(gw.Name, gw.Kind, gw.Endpoint, gw.DefaultModel, gw.Key); err != nil {
				logger.Error("failed to apply hub-delivered gateway", "gateway", gw.Name, "error", err)
			}
		}))

		go hub.StartTaskStatusPush(ctx, hubURL, func() *hub.TaskStatusPayload {
			reg, active := dashSrv.ContributorSummary()
			lb := dashSrv.LeaderboardForHub()
			out := make([]hub.LeaderboardEntry, len(lb))
			for i, e := range lb {
				out[i] = hub.LeaderboardEntry{
					GitHubUsername: e.GitHubUsername,
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

	// Trajectory-review lane (opt-in): a second-model check that reads each
	// running agent's recent transcript and pauses on goal drift. Built once;
	// runs off the governor tick, gated by its own cadence. If the reviewer
	// cannot be constructed (no LiteLLM endpoint/model), the lane is disabled
	// with a single warning rather than erroring every tick.
	var trajLane *trajectory.Lane
	if cfg.Governor.Trajectory.IsEnabled() {
		reviewEndpoint, reviewKey, reviewModel := cfg.Governor.ResolveReviewer()
		reviewer, terr := trajectory.NewReviewer(trajectory.Config{
			Endpoint:        reviewEndpoint,
			APIKey:          reviewKey,
			Model:           reviewModel,
			TranscriptLines: cfg.Governor.Trajectory.TranscriptLines,
		})
		if terr != nil {
			// Enabled but not runnable — surface it as a dashboard alert, not
			// just a log line, so a safety control is never silently inert.
			// (Reconciled below so it also clears when the lane is disabled.)
			logger.Warn("trajectory-review lane enabled but not running", "reason", terr.Error())
		} else {
			trajLane = trajectory.NewLane(reviewer, agentMgr,
				dashboard.NewTrajectorySink(dashSrv, notifier),
				trajectory.LaneConfig{
					IntervalS:    cfg.Governor.Trajectory.IntervalS,
					OnDivergence: cfg.Governor.Trajectory.OnDivergence,
					ExemptAgents: cfg.Governor.Trajectory.ExemptAgents,
				}, logger)
			logger.Info("trajectory-review lane enabled",
				"model", reviewModel,
				"interval_s", cfg.Governor.Trajectory.IntervalS,
				"on_divergence", cfg.Governor.Trajectory.OnDivergence)
		}
	}
	// Clear any legacy "not configured" banner alert persisted by an older
	// build. The half-configured state is shown inline in Settings →
	// General, not in the top banner.
	dashSrv.ReconcileTrajectoryAlert(&cfg.Governor)

	// Stall-replan lane (Phase 3 planning intelligence): periodically detects
	// approved plans whose sub-tasks have stopped progressing and re-kicks the
	// architect to revise them, bounded by a per-plan replan cap. It runs off the
	// governor tick, gated by its own Due() cadence (no goroutine of its own), and
	// drives the architect only through SendKick (agentKicker) from this tick —
	// never from the agent-launch path — so it cannot touch the manager lock
	// unsafely. On by default; a no-op when there are no approved plans.
	var replanLane *planning.ReplanLane
	if cfg.Governor.Replan.IsEnabled() {
		rc := cfg.Governor.Replan
		replanLane = planning.NewReplanLane(
			beadStores,
			agentKicker{mgr: agentMgr},
			gov,
			dashboard.NewReplanSink(dashSrv, notifier),
			planning.ReplanLaneConfig{
				IntervalS: rc.IntervalS,
				Stall: planning.StallConfig{
					StallThreshold: time.Duration(rc.StallThresholdS) * time.Second,
					MaxReplans:     rc.MaxReplans,
				},
			}, logger)
		logger.Info("stall-replan lane enabled",
			"interval_s", rc.IntervalS,
			"stall_threshold_s", rc.StallThresholdS,
			"max_replans", rc.MaxReplans)
	}

	var retroLane *retro.Lane
	if cfg.Retro.Enabled {
		retroStore := beadStores[retro.Actor]
		escalationStoreOnce.Do(func() {
			escalationStore = escalation.Load(escalationLedgerPath)
		})
		retroLane = retro.NewLane(beadStores, retroStore, dashSrv.LifecycleTimeline(), escalationStore, retro.Config{
			Enabled:             cfg.Retro.Enabled,
			ScanIntervalS:       cfg.Retro.ScanIntervalS,
			MaxFixAttempts:      cfg.Retro.MaxFixAttempts,
			MaxKicks:            cfg.Retro.MaxKicks,
			LongStallDays:       cfg.Retro.LongStallDays,
			RecentClosedWindowS: cfg.Retro.RecentClosedWindowS,
			AnalysisModel:       cfg.Retro.AnalysisModel,
			AnalysisEndpoint:    cfg.Governor.LiteLLM.ResolveEndpoint(),
			AnalysisAPIKey:      cfg.Governor.LiteLLM.ResolveAPIKey(),
		}, logger)
		if knowledgeAPI != nil {
			retroLane.SetKnowledgeSink(knowledgeAPI)
		}
		logger.Info("retro lane enabled",
			"scan_interval_s", cfg.Retro.ScanIntervalS,
			"max_fix_attempts", cfg.Retro.MaxFixAttempts,
			"max_kicks", cfg.Retro.MaxKicks,
			"long_stall_days", cfg.Retro.LongStallDays,
			"analysis_enabled", cfg.Retro.AnalysisModel != "")
	}

	logger.Info("entering governor loop", "interval_seconds", cfg.Governor.EvalIntervalS)
	lastEvalInterval := cfg.Governor.EvalIntervalS
	ticker := time.NewTicker(time.Duration(cfg.Governor.EvalIntervalS) * time.Second)
	defer ticker.Stop()
	var lastAutoMergeSweep time.Time

	var agentTicker *time.Ticker
	if cfg.Dashboard.AgentPollIntervalS > 0 {
		agentTicker = time.NewTicker(time.Duration(cfg.Dashboard.AgentPollIntervalS) * time.Second)
		defer agentTicker.Stop()
		logger.Info("fast agent status enabled", "interval_seconds", cfg.Dashboard.AgentPollIntervalS)
	}

	// NOTE: dashSrv.MarkReady() was previously HERE, after the staggered agent
	// launch and the heartbeat/trajectory/ticker setup. It has been moved to
	// immediately after the HTTP listener starts (before the agent-launch loop),
	// so the pod becomes Ready in seconds instead of minutes. See the MarkReady
	// call and comment above the agent-launch goroutine.

	const cliStartupDelay = 10 * time.Second
	logger.Info("waiting for CLI startup before first eval", "delay", cliStartupDelay)
	select {
	case <-time.After(cliStartupDelay):
	case <-ctx.Done():
		return
	}

	// #2573: startup must NOT clear persisted last-kick timestamps. It used to
	// (gov.ClearLastKicks) so that every eligible agent was kicked on the first
	// eval — "one kick per agent per pod boot, by design". But on hosted hives
	// the hub rolls the Deployment for every auto-upgrade, and each roll is a
	// brand-new pod with container restart count 0, so agents on 4h/6h cadences
	// were re-kicked at roll frequency — burning backend tokens ("Bob coins")
	// far beyond any configured cadence, while "next run" (recomputed from the
	// wiped timestamps) showed sooner than the cadence implied. LastKick state
	// is persisted to /data (PVC) after every eval and restored above via
	// SeedLastKicks, so this first eval kicks exactly the agents whose cadence
	// has actually elapsed — including everything after downtime longer than an
	// interval. A hive with no persisted state (fresh install) has no LastKick
	// entries, and every cadenced agent is still kicked here, unchanged.
	logger.Info("startup honors persisted cadence state — first eval kicks only agents whose cadence has elapsed")
	runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, nil, approvalDesk, logger)
	runRotationCheck(ctx, cfg, rotationMgr, gov, agentMgr, logger)
	if wd != nil {
		wd.Tick(ctx)
	}
	runAutoMergeSweepIfDue(ctx, ghClient, cfg, dashSrv, &lastAutoMergeSweep, logger)
	persistState(agentMgr, gov, cfg, statePath, logger, dashSrv, wd)

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
			persistState(agentMgr, gov, cfg, statePath, logger, dashSrv, wd)
			return
		case <-ticker.C:
			restarted := agentMgr.CheckAndRestartCrashedAgents(ctx)
			for _, name := range restarted {
				dashSrv.AuditLog("system", "restart", "trigger=crash-recovery", name)
			}
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
							dashSrv.AuditLog("system", "kick", "trigger=inception-crash-recovery", "brainstorm")
						}
						gov.RecordKick("brainstorm")
					}
				}
			}
			// Watchdog sweep (RFC #4665): synchronous but bounded — every
			// probe carries a deadline and restarts run detached under a hard
			// timeout, so a wedged agent can never stall this tick. Tick
			// self-gates to watchdog.probe_interval_s.
			//
			// It runs BEFORE runEvalCycle so agents it revived join this
			// cycle's resume-kick list rather than waiting a full eval
			// interval. Restarts are detached, so a given sweep's completions
			// are usually collected on the next pass — TakeRestarted drains
			// whatever has finished, and the governor gate gets the final say
			// either way.
			if wd != nil {
				// Re-resolve the mode each sweep so a change saved from the
				// dashboard (or the fleet-wide kill switch being engaged)
				// takes effect without a restart — and so dead-session
				// ownership moves with it. Without this, leaving heal via the
				// settings page would stop the watchdog restarting while the
				// manager's crash loop was still standing down: a window in
				// which NEITHER recovers a dead agent.
				if s, errs := watchdog.SettingsFrom(cfg.Governor.Watchdog); s.Mode != wd.Mode() {
					for _, e := range errs {
						logger.Warn("watchdog config problem", "error", e)
					}
					logger.Info("watchdog mode changed", "from", string(wd.Mode()), "to", string(s.Mode))
					dashSrv.AuditLog("system", "watchdog-mode", "from="+string(wd.Mode())+", to="+string(s.Mode), "")
					wd.SetSettings(s)
					agentMgr.SetDeadSessionRecoveryOwner(s.MayAct())
				}
				wd.Tick(ctx)
				for _, name := range wd.TakeRestarted() {
					dashSrv.AuditLog("system", "restart", "trigger=watchdog", name)
					restarted = append(restarted, name)
				}
			}
			runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, restarted, approvalDesk, logger)
			runRotationCheck(ctx, cfg, rotationMgr, gov, agentMgr, logger)
			runAutoMergeSweepIfDue(ctx, ghClient, cfg, dashSrv, &lastAutoMergeSweep, logger)
			// Trajectory review runs after the eval cycle (so kicks/intents are
			// current) on its own cadence, gated by Due().
			if trajLane != nil && trajLane.Due(time.Now()) {
				trajLane.Run(ctx)
			}
			// Stall-replan runs on the same tick, gated by its own Due() cadence.
			// It is synchronous and adds no goroutine; kicks go through the same
			// out-of-band SendKick path as the eval cycle above.
			if replanLane != nil && replanLane.Due(time.Now()) {
				if n := replanLane.Run(ctx); n > 0 {
					logger.Info("stall-replan lane re-kicked stalled plans", "replans", n)
				}
			}
			if retroLane != nil && retroLane.Due(time.Now()) {
				if n := retroLane.Run(ctx); n > 0 {
					logger.Info("retro lane filed advisory beads", "findings", n)
				}
			}
			persistState(agentMgr, gov, cfg, statePath, logger, dashSrv, wd)
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
