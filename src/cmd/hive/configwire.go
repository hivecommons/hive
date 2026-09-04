package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/tracing"
)

func (w *spokeWire) wireSpokeConfigAndSignals() {
	// Use LoadWithDashboardOverlay (not plain Load) so the dashboard overlay's
	// removed_agents tombstones are populated into w.cfg.RemovedAgents at boot —
	// BEFORE the startup ApplyPack below reconciles the ACMM roster. Plain Load
	// never reads the overlay, so on restart the tombstone was invisible and
	// ApplyPack re-added deleted pack agents (brainstorm/guide) every time
	// (#2439). Same return signature as Load; falls back to the seed when no
	// overlay exists or the pod is not in Kubernetes.
	var err error
	w.cfg, err = config.LoadWithDashboardOverlay(w.configPath)
	if err != nil {
		w.logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Reconfigure w.logger with rolling file output
	w.logger = setupLogger(w.cfg.Governor.Logging.Dir, w.cfg.Governor.Logging.MaxSizeMB,
		w.cfg.Governor.Logging.MaxAgeDays, w.cfg.Governor.Logging.MaxBackups,
		w.cfg.Governor.Logging.Compress, w.cfg.Governor.Logging.Level)
	slog.SetDefault(w.logger)

	// Load or generate a unique Hive ID for this instance
	w.cfg.HiveID = loadOrGenerateHiveID(w.logger)
	_ = os.Setenv("HIVE_ID", w.cfg.HiveID) // valid key/value; Setenv cannot fail on Unix

	// Observability (#2439): report the removed-agents tombstone LoadWithDashboardOverlay
	// adopted from the dashboard overlay at boot, BEFORE the startup ApplyPack below. On
	// a non-sticking-removal report this line is the first check — an empty set here on a
	// hive that removed an agent means the tombstone did not persist across the restart.
	w.logger.Info("boot: loaded removed-agents tombstone",
		"hive_id", w.cfg.HiveID,
		"count", len(w.cfg.RemovedAgents),
		"agents", w.cfg.RemovedAgents,
	)

	// Surface config provenance: when the persisted runtime config exists, init
	// containers restore it over the ConfigMap seed on restart, so edits made
	// only to the seed (or only to the live file) silently lose to it.
	//
	// Checks the legacy name too: during the migration a hive may still carry
	// only /data/hive.yaml.bak, and the whole point of this log line is to warn
	// that such a file is shadowing the seed. Note this path was previously
	// built as w.configPath + ".bak", which only ever resolved to the real
	// location when HIVE_CONFIG happened to live under /data — a literal grep
	// for "hive.yaml.bak" could not find it either.
	for _, runtimePath := range []string{config.RuntimeConfigFile, config.RuntimeConfigFileLegacy} {
		if _, statErr := os.Stat(runtimePath); statErr == nil {
			w.logger.Info("persisted runtime config present — restored over the seed on pod restart; fixes must land in the live config so the next save refreshes it",
				"path", runtimePath,
				"github_installation_id", w.cfg.GitHub.InstallationID,
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
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" && envCfg != w.configPath {
		w.logger.Warn("config path disagreement: HIVE_CONFIG names a different file than the one loaded — an explicit -config (the image CMD) outranked the entrypoint's redirect; persisted state in HIVE_CONFIG may be overwritten by the next save",
			"hive_config_env", envCfg,
			"loaded_config", w.configPath,
		)
	}

	w.logger.Info("hive starting",
		"org", w.cfg.Project.Org,
		"repos", w.cfg.Project.Repos,
		"agents", len(w.cfg.Agents),
		"hive_id", w.cfg.HiveID,
	)
	startupRepoTargetIssue := config.ValidateRepoTargets(w.cfg)
	if startupRepoTargetIssue != nil {
		w.logger.Warn("repo target misconfigured — owner action required",
			"issue", startupRepoTargetIssue.Message,
			"hive_id", w.cfg.HiveID,
			"org", w.cfg.Project.Org,
			"repos", w.cfg.Project.Repos,
			"primary_repo", w.cfg.Project.PrimaryRepo,
		)
	}
	w.repoTargetMisconfigured = func() bool {
		return config.ValidateRepoTargets(w.cfg) != nil
	}
	w.repoTargetIssueMessage = func() string {
		if issue := config.ValidateRepoTargets(w.cfg); issue != nil {
			return issue.Message
		}
		return ""
	}

	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.addCleanup(w.cancel)

	// Initialize OpenTelemetry tracing. Off by default: with no otel block
	// (or otel.enabled=false) this installs a no-op provider with zero export
	// overhead. Never fatal — a tracing setup error must not stop hive.
	otelCfg := w.cfg.EffectiveOTel()
	traceShutdown, traceErr := tracing.Init(w.ctx, tracing.Config{
		Enabled:     otelCfg.Enabled,
		Endpoint:    otelCfg.Endpoint,
		Headers:     otelCfg.Headers,
		ServiceName: otelCfg.ServiceNameOrDefault(),
		Insecure:    otelCfg.Insecure,
		SampleRatio: otelCfg.SampleRatio,
		HiveID:      w.cfg.HiveID,
		Branch:      w.cfg.Policies.Branch,
		// Reach anchors (#3973): gitShort is the ldflags-baked commit of THIS
		// binary (already canonicalized to 7 chars above), and the image ref is
		// the Deployment-declared image (cached — warmed by the release-channel
		// read at startup; "" outside a cluster). Spans attribute to the code
		// that actually runs, not to the merge/publish event (#3816).
		Commit: gitShort,
		Image:  hub.SelfDeploymentImage(),
	})
	if traceErr != nil {
		w.logger.Warn("tracing init failed; continuing without tracing", "error", traceErr)
	} else if otelCfg.Enabled {
		w.logger.Info("otel tracing enabled", "endpoint", otelCfg.Endpoint, "service_name", otelCfg.ServiceNameOrDefault())
	}
	// Component reach counters (#3993): resume this commit's counters from the
	// PVC before any span can start. Independent of the otel block above —
	// counters increment with or without an exporter (design D2 of #3973), so
	// this runs unconditionally and a load failure only costs history, never
	// counting. Counters persisted by a DIFFERENT commit are dropped inside
	// LoadReachState: a new binary starts fresh keys naturally.
	if err := tracing.LoadReachState(reachStatePath, gitShort, w.logger); err != nil {
		w.logger.Warn("reach state load failed; starting with fresh counters", "error", err)
	}
	w.addCleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), traceShutdownTimeout)
		defer shutdownCancel()
		if err := traceShutdown(shutdownCtx); err != nil {
			w.logger.Warn("tracing shutdown error", "error", err)
		}
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	// w.preShutdownHooks run in the signal handler before the context is canceled,
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
	go func() {
		sig := <-sigCh
		w.logger.Info("received signal, shutting down", "signal", sig)
		w.preShutdownHooks.run()
		w.cancel()
	}()

	w.ghAuth = initGitHubAuth(w.ctx, w.cfg, w.logger)
	w.ghClient, w.appAuth = w.ghAuth.Client, w.ghAuth.AppAuth
}
