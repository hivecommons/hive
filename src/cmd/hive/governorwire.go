package main

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strconv"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/proxy"
	"github.com/hivecommons/hive/pkg/pushbroker"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/toolapprove"
)

func (w *spokeWire) wireSpokeAuthGovernor() {
	// w.appAuthFailure, when non-empty, is the operator-facing reason GitHub auth
	// is unavailable. It is surfaced through the existing
	// GitHubAppRequired/PermIssue banner rather than killing the process.
	w.appAuthFailure = w.ghAuth.Failure
	// w.appAuthState classifies that failure so the banner and the hub's journey
	// nudges can tell an operator-side fault (no key was ever delivered) from a
	// user-actionable one (the App is not installed).
	w.appAuthState = w.ghAuth.State
	if w.ghClient != nil && len(w.cfg.Governor.Labels.Exempt) > 0 {
		w.ghClient.SetExemptLabels(w.cfg.Governor.Labels.Exempt)
		w.ghClient.SetAutoMergeLabel(normalizedAutoMergeLabel(w.cfg.Governor.Labels.AutoMerge))
	}
	// Unconditional (nil-safe, zero value = no filtering): the issue filter
	// gates which issues become actionable at all, so it must be installed
	// even when no exempt labels are configured.
	w.ghClient.SetIssueFilter(w.cfg.Project.IssueFilter)
	// The user write-token client (userGHClient) was removed: every GitHub write
	// — issues, PRs, comments, merges, and the advisory digest — now goes through
	// the hive's App installation token (w.ghClient / kubestellar-hive[bot]). The
	// user token only ever served as an advisory-digest fallback writer, which is
	// no longer wanted (and forced the excessive "repo" login scope, issue #1927).
	// Dashboard login now requests no scope and no user write-token is persisted.

	w.gov = governor.New(w.cfg.Governor, w.cfg.EnabledAgents(), w.logger)
	// Default mode thresholds scale with how many repos the hive watches, so
	// the mode ladder means the same thing on a 3-repo hive as on a 39-repo
	// one (#3498). Explicit thresholds are unaffected.
	w.gov.SetRepoCount(w.cfg.Project.RepoCount())
	w.sched = scheduler.New(w.cfg, w.logger)

	// Wire the GitHub prompt-source resolver so agents may source their kick
	// prompt from a repo (agent.prompt_source). Fetching reuses the hive's App
	// token via w.ghClient and is gated to the seed-only allowlist — the closure
	// captures w.cfg so a live config reload updates the allowlist on the next kick.
	// A nil w.ghClient must be passed as a nil Fetcher interface (not a typed-nil
	// *github.Client) so the resolver's nil-fetcher fallback path triggers.
	if w.ghClient != nil {
		w.promptFetcher = w.ghClient
	}
	w.sched.SetGitHubPromptResolver(promptsrc.NewResolver(
		w.promptFetcher,
		func(slug string) bool { return w.cfg.GitHubPromptAllowed(slug) },
		w.logger,
	))

	// Wire the whole-agent definition_source resolver so agents imported with
	// "keep linked" re-fetch their portable AgentDefinition from the repo on
	// reload/kick and re-apply its operator-safe fields (never security/seed-only
	// fields — see pkg/defsrc). Same seed-only allowlist gate and graceful
	// fallback as the prompt resolver.
	if w.ghClient != nil {
		w.defFetcher = w.ghClient
	}
	w.definitionResolver = defsrc.NewResolver(
		w.defFetcher,
		func(slug string) bool { return w.cfg.GitHubDefinitionAllowed(slug) },
		w.logger,
	)
	// Apply live definitions once at startup so a repo edit made while the hive
	// was down is reflected before the first kick.
	defsrc.ApplyToConfig(context.Background(), w.cfg, w.definitionResolver, w.logger)

	// Restore sparkline history from disk so it survives container restarts
	const sparklinePath = "/data/sparkline-history.json"
	if sparkData, err := os.ReadFile(sparklinePath); err == nil {
		var snapshots []governor.EvalSnapshot
		if err := json.Unmarshal(sparkData, &snapshots); err == nil && len(snapshots) > 0 {
			w.gov.SeedEvalHistory(snapshots)
			w.logger.Info("sparkline history restored", "entries", len(snapshots))
		}
	}

	// Restore mode history from disk so the mode timeline survives container restarts
	const modeHistoryPath = "/data/mode-history.json"
	if modeData, err := os.ReadFile(modeHistoryPath); err == nil {
		var changes []governor.ModeChange
		if err := json.Unmarshal(modeData, &changes); err == nil && len(changes) > 0 {
			w.gov.SeedModeHistory(changes)
			w.logger.Info("mode history restored", "entries", len(changes))
		}
	}

	// Restore token sparkline history from disk so token charts survive container restarts
	const tokenSparklinePath = "/data/token-sparkline-history.json"
	if tokenSparkData, err := os.ReadFile(tokenSparklinePath); err == nil {
		if err := json.Unmarshal(tokenSparkData, &w.pendingTokenSeed); err == nil && len(w.pendingTokenSeed) > 0 {
			w.logger.Info("token sparkline history loaded", "entries", len(w.pendingTokenSeed))
		}
	}

	// Restore fact count history from disk so the knowledge sparkline survives restarts
	const factHistoryPath = "/data/fact-history.json"
	if factData, err := os.ReadFile(factHistoryPath); err == nil {
		if err := json.Unmarshal(factData, &w.pendingFactSeed); err == nil && len(w.pendingFactSeed) > 0 {
			w.logger.Info("fact history loaded", "entries", len(w.pendingFactSeed))
		}
	}

	// Restore estimated-cost history from disk so the cost sparkline survives restarts
	const costHistoryPath = "/data/cost-history.json"
	if costData, err := os.ReadFile(costHistoryPath); err == nil {
		if err := json.Unmarshal(costData, &w.pendingCostSeed); err == nil && len(w.pendingCostSeed) > 0 {
			w.logger.Info("cost history loaded", "entries", len(w.pendingCostSeed))
		}
	}

	// #4298: restore per-budget-window history so past resets survive a restart.
	// A missing or unparseable file is ordinary on a hive upgrading into this
	// feature — it simply starts with no history rather than failing to boot.
	const budgetWindowHistoryPath = "/data/budget-window-history.json"
	if budgetData, err := os.ReadFile(budgetWindowHistoryPath); err == nil {
		if err := json.Unmarshal(budgetData, &w.pendingBudgetWindowSeed); err == nil && len(w.pendingBudgetWindowSeed) > 0 {
			w.logger.Info("budget window history loaded", "entries", len(w.pendingBudgetWindowSeed))
		}
	}

	// #4263: restore convergence soak telemetry so a fixed-commit off/shadow/
	// enforce comparison survives restarts. Missing or unparseable is ordinary
	// on a hive that never ran with the toggle on — start empty, never fail.
	const convergenceSoakHistoryPath = "/data/convergence-soak-history.json"
	if soakData, err := os.ReadFile(convergenceSoakHistoryPath); err == nil {
		if err := json.Unmarshal(soakData, &w.pendingConvergenceSoakSeed); err == nil && len(w.pendingConvergenceSoakSeed) > 0 {
			w.logger.Info("convergence soak history loaded", "entries", len(w.pendingConvergenceSoakSeed))
		}
	}

	// Restore governor/repo/beads/system trend history from disk so those
	// sparklines survive restarts and render for any viewer (previously kept
	// only in the browser's localStorage).
	const trendHistoryPath = "/data/trend-history.json"
	if trendData, err := os.ReadFile(trendHistoryPath); err == nil {
		if err := json.Unmarshal(trendData, &w.pendingTrendSeed); err == nil && len(w.pendingTrendSeed) > 0 {
			w.logger.Info("trend history loaded", "entries", len(w.pendingTrendSeed))
		}
	}

	if w.cfg.Knowledge.Enabled {
		layers := convertKnowledgeLayers(w.cfg.Knowledge.Layers)
		primerCfg := knowledge.PrimerConfig{
			MaxFacts:      w.cfg.Knowledge.Primer.MaxFacts,
			Priority:      w.cfg.Knowledge.Primer.Priority,
			MergeStrategy: w.cfg.Knowledge.Primer.MergeStrategy,
		}
		w.primer = knowledge.NewPrimer(layers, primerCfg, w.logger)
		w.sched.SetPrimer(w.primer)
		w.logger.Info("knowledge primer enabled",
			"layers", len(w.cfg.Knowledge.Layers),
			"max_facts", primerCfg.MaxFacts,
		)
	}

	w.notifier = notify.New(w.cfg.Notifications, w.logger)
	w.notifier.SetHiveID(w.cfg.HiveID)
	w.acmmLevel = inferACMMLevel(w.cfg)
	// A hive that booted without usable GitHub credentials raises the banner
	// immediately, seeded with the classification made at startup. Otherwise
	// these stay empty and are filled in later by the live probes below.
	w.githubAppRequired = w.appAuthFailure != ""
	// w.githubAppDiag/w.githubAppState carry the classified reason App auth failed,
	// so the banner can name the true cause (and the hub can avoid escalating
	// against a hive whose credentials the operator never delivered).
	w.githubAppDiag = w.appAuthFailure
	w.githubAppState = w.appAuthState
	// Config truth outranks live probes: an App with no installation cannot
	// mint, period. A cached token can keep clients green for up to an hour
	// after an installation is cleared, and waiting for the first failed mint
	// left the banner down and the hub green exactly when the operator needed
	// the opposite (the fast-model-actuation incident).
	if w.cfg.GitHub.ConfiguredButUninstalled() {
		w.githubAppRequired = true
		w.githubAppState = github.AppStateNotInstalled
		w.githubAppDiag = "GitHub App " + strconv.FormatInt(w.cfg.GitHub.AppID, 10) +
			" has no installation for this org — install it (the spoke adopts the installation automatically)"
	}

	// Invocation-attribution trail (pkg/github/attribution.go): stamp hive-
	// created PRs/issues with what the hive invoked, and audit every such
	// creation. Wired in stages as dependencies come up: the trailer gate now
	// (w.cfg exists, and the advisory-issue ensure just below must respect the
	// toggle), the per-agent resolver after the agent manager exists, and the
	// audit sink after the dashboard server exists. w.cfg is the live pointer
	// (the config watcher swaps contents in place), so the toggle is read
	// fresh per creation — a dashboard flip takes effect immediately.
	if w.ghClient != nil {
		w.ghClient.SetAttributionHooks(github.AttributionHooks{
			TrailerEnabled: func() bool { return w.cfg.Governor.AttributionTrailerEnabled() },
		})
	}

}

func (w *spokeWire) wireSpokeConfigReloadAndHooks() {
	// Watch hive.yaml for external changes and reload config when modified
	w.configWatcher = config.NewWatcher(w.configPath, func(newCfg *config.Config) {
		// Preserve runtime-only fields that are not in the YAML
		newCfg.HiveID = w.cfg.HiveID

		// Preserve ACMM level from the agent manager — it is the
		// authoritative source. The file may have a stale value if
		// a watcher reload races with a level-switch saveConfig().
		if w.cfg.ACMMLevel != nil {
			newCfg.ACMMLevel = w.cfg.ACMMLevel
		}

		// Preserve removed-agent tombstones across the swap as a union of the
		// live w.cfg and the incoming reload. LoadWithDashboardOverlay now carries
		// the overlay's tombstones into newCfg, but a removal that landed in the
		// live w.cfg after this reload's snapshot (or an overlay too short/stale to
		// echo it back yet) must not be lost — otherwise the next persistState
		// saver rewrites every layer tombstone-free and the deleted agents
		// reappear (#2439). Union keeps any tombstone present in either side.
		for _, name := range w.cfg.RemovedAgents {
			newCfg.MarkAgentRemoved(name)
		}
		newCfg.PruneRemovedAgents()

		// Observability (#2439): this is the ~2-min interval reload path, so keep it
		// at DEBUG to avoid spamming a healthy hive. When a removal is not sticking,
		// enabling DEBUG shows the tombstone surviving each swap — an empty count here
		// while the agent keeps reappearing localizes the leak to this union-preserve.
		w.logger.Debug("reload: preserved removed-agents",
			"hive_id", w.cfg.HiveID,
			"count", len(newCfg.RemovedAgents),
			"agents", newCfg.RemovedAgents,
		)

		// Capture the outgoing GitHub App identity before the swap so we can
		// tell whether the reload changed it.
		prevGitHub := w.cfg.GitHub

		// Swap the in-memory config pointer contents
		*w.cfg = *newCfg

		// Re-sync subsystems that cache config values
		w.ghClient.SetRepos(w.cfg.Project.Repos)
		w.gov.UpdateConfig(w.cfg.Governor)
		// A reload can add or archive repos, which moves every scaled default
		// threshold — re-sync it alongside the repo list above.
		w.gov.SetRepoCount(w.cfg.Project.RepoCount())
		w.agentMgr.SetSandboxConfig(w.cfg.AgentSandbox)
		// Re-run the posture check on reload, not only at boot: flipping the
		// Security tab's sandbox toggle writes the config and lands here, which
		// is the exact moment an operator forms the belief that they are now
		// sandboxed. See logAgentSandboxPosture.
		logAgentSandboxPosture(w.logger, w.cfg)

		// Hot-reload the state-triggered hooks (RFC #4001). Recompiles only
		// when the `hooks:` list actually changed, and swaps the registry in
		// place so per-hook rate-limit windows SURVIVE the reload — otherwise
		// a reload loop would be a way to clear the anti-storm ceiling.
		buildHookDispatcher(w.cfg, hookSinks{
			Notifier: w.notifier,
			AgentMgr: w.agentMgr,
			Timeline: w.dashSrv.LifecycleTimeline(),
			Audit:    w.dashSrv.AgentAuditSink(),
		}, w.logger)

		// Hot-reload the state-triggered hooks (RFC #4001). Recompiles only
		// when the `hooks:` list actually changed, and swaps the registry in
		// place so per-hook rate-limit windows SURVIVE the reload — otherwise
		// a reload loop would be a way to clear the anti-storm ceiling.
		buildHookDispatcher(w.cfg, hookSinks{
			Notifier: w.notifier,
			AgentMgr: w.agentMgr,
			Timeline: w.dashSrv.LifecycleTimeline(),
			Audit:    w.dashSrv.AgentAuditSink(),
			// #4000 ↔ #4001 seam: an `enqueue-approval` hook lands in the same
			// durable operator inbox the desk uses. nil when the desk is off,
			// which keeps the dispatcher's "no approval queue wired" error
			// honest rather than failing on every firing.
			Approvals: newHookApprovalAdapter(w.approvalInbox, toolapprove.ACMMLevelOf(w.cfg)),
		}, w.logger)

		// Re-apply live agent definitions (definition_source) on reload so an
		// operator's edit to a linked repo propagates. Merges only operator-safe
		// fields; a fetch failure keeps each agent's baked definition. Runs before
		// initAgentConfigDrivenSystems so downstream systems see the merged config.
		defsrc.ApplyToConfig(context.Background(), w.cfg, w.definitionResolver, w.logger)
		if err := w.cfg.ExpandAgentReplicas(); err != nil {
			w.logger.Warn("failed to expand agent replicas after config reload", "error", err)
		}
		addedAgents := w.agentMgr.ReconcileAgents(w.cfg.EnabledAgents())
		for _, added := range addedAgents {
			if ac, ok := w.cfg.Agents[added]; ok && !ac.OnDemand {
				go func(name string) {
					w.logger.Info("audit: starting reconciled agent", "name", name, "trigger", "config-reload")
					if err := w.agentMgr.Start(w.ctx, name); err != nil {
						w.logger.Warn("failed to start reconciled agent", "name", name, "error", err)
					}
				}(added)
			}
		}
		w.gov.UpdateAgents(w.cfg.EnabledAgents())

		initAgentConfigDrivenSystems(w.cfg)

		// Rebuild GitHub App auth when its identity changed. AppAuth captures
		// app_id/installation_id at construction, so without this a corrected
		// installation_id in hive.yaml keeps minting tokens for the OLD
		// installation until the pod restarts.
		//
		// RESOLVE the key file rather than reading w.cfg.GitHub.KeyFile raw. An
		// unset key_file is the CORRECT steady state on a hosted spoke — the
		// heartbeat apply path deliberately does not persist one, because the
		// path is derivable from app_id and a stored value outlives the App it
		// was derived for. Gating on the raw field therefore skipped the rebuild
		// entirely on exactly the hives that need it: a corrected
		// installation_id w.saved to hive.yaml kept minting tokens for the old
		// installation until the pod restarted. Startup (resolveAppKeyFile
		// above), the heartbeat rebuild, and the dashboard's Set ID handler
		// (#2459) all already resolve here; this was the last raw reader.
		//
		// Comparing RESOLVED paths also catches a change the raw comparison
		// cannot see: a per-app-id key arriving on the PVC changes which key
		// this process should sign with while w.cfg.GitHub.KeyFile stays "".
		prevKeyFile := appKeys.Resolve(prevGitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), prevGitHub.AppID)
		nextKeyFile := appKeys.Resolve(w.cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), w.cfg.GitHub.AppID)
		if prevGitHub.AppID != w.cfg.GitHub.AppID ||
			prevGitHub.InstallationID != w.cfg.GitHub.InstallationID ||
			prevKeyFile != nextKeyFile ||
			prevGitHub.APIURL != w.cfg.GitHub.APIURL {
			if w.cfg.GitHub.HasUsableApp() && nextKeyFile != "" {
				newAppAuth, appErr := github.NewAppAuth(w.cfg.GitHub.AppID, w.cfg.GitHub.InstallationID, nextKeyFile, w.logger, w.cfg.GitHub.ResolvedAPIURL())
				if appErr != nil {
					w.logger.Error("github app auth rebuild after config reload failed", "error", appErr)
				} else {
					newClient := github.NewClientFromAppWithBotLogin(newAppAuth, w.cfg.Project.Org, w.cfg.Project.Repos, w.logger, w.cfg.GitHub.BotLogin())
					if len(w.cfg.Governor.Labels.Exempt) > 0 {
						newClient.SetExemptLabels(w.cfg.Governor.Labels.Exempt)
						newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(w.cfg.Governor.Labels.AutoMerge))
					}
					newClient.SetIssueFilter(w.cfg.Project.IssueFilter)
					w.ghClient = newClient
					w.appAuth = newAppAuth
					w.agentMgr.SetAppAuth(newAppAuth)
					// Immediate per-agent token delivery — see #4072.
					go w.agentMgr.RefreshAgentTokens(w.ctx)
					w.agentMgr.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: newAppAuth})
					w.agentMgr.SetSandboxPRClient(newClient)
					w.dashSrv.UpdateGitHubClient(newClient, newAppAuth)
					w.logger.Info("github app auth rebuilt after config reload",
						"app_id", w.cfg.GitHub.AppID,
						"installation_id", w.cfg.GitHub.InstallationID,
						"key_file", nextKeyFile,
					)
				}
			}
		}

		w.refreshDashboard()
	}, w.logger)
	w.dashSrv.SetSkipReloadFunc(w.configWatcher.SkipNext)
	go w.configWatcher.Start(w.ctx)

	// Persist operator pause/resume into the on-disk config so it survives
	// restarts and upgrades. Without this, a pod restart rebuilt every agent
	// un-paused, silently undoing an operator's pause on the next upgrade.
	// Concurrent pauses (e.g. an ACMM pack pausing several agents in a loop,
	// or login-detector firing while the operator pauses) each did an
	// unsynchronized w.cfg.Agents map write + w.cfg.Save(). The saves clobbered
	// each other (last writer wins), so only some pauses reached the PVC and
	// the rest were silently lost on the next restart. Serialize the
	// read-modify-save under a dedicated mutex so every pause transition is
	// durably persisted.
	w.agentMgr.SetPersistPauseCallback(func(name string, paused bool) {
		w.configWatcher.SkipNext() // don't let our own write trigger a reload
		changed, err := w.cfg.SetAgentPausedAndSave(name, paused)
		if err != nil {
			w.logger.Warn("failed to persist agent pause state", "agent", name, "paused", paused, "error", err)
		}
		_ = changed
	})

	// Persist the fully-expanded prompt text of every kick so owners can review
	// what their agents were actually told, over a day/week window, in the
	// per-agent "Prompts" tab. Redaction and truncation happen inside the
	// store, before anything is written to the PVC.
	w.agentMgr.SetRecordPromptCallback(w.dashSrv.RecordPrompt)

	// Feed agent lifecycle events (start, stop, launch FAILURE, backend/model
	// change) into the durable audit store behind the dashboard's Audit Log.
	// Injected as an interface because pkg/dashboard already imports pkg/agent,
	// so the manager cannot reach the audit store directly without an import
	// cycle. Motivating case: an agent configured with a backend its hive image
	// did not support failed at every launch for a day, visible only as a WARN
	// line inside the pod.
	w.agentMgr.SetAuditSink(w.dashSrv.AgentAuditSink())

	// Compile the operator's state-triggered hooks (RFC #4001). Every sink the
	// vetted actions act through exists by this point: the w.notifier, the agent
	// manager's AUDITED pause, the lifecycle timeline, and the same audit store
	// the dashboard writes. The approvals sink stays nil until #4000's queue
	// lands — an enqueue-approval hook then reports a wiring failure per firing
	// rather than silently dropping the request.
	//
	// Fail-closed: an invalid hooks list logs and leaves the previous set
	// armed; it never crashes the process or silently disarms working hooks.
	buildHookDispatcher(w.cfg, hookSinks{
		Notifier: w.notifier,
		AgentMgr: w.agentMgr,
		Timeline: w.dashSrv.LifecycleTimeline(),
		Audit:    w.dashSrv.AgentAuditSink(),
		// #4000 ↔ #4001 seam: see the reload site above.
		Approvals: newHookApprovalAdapter(w.approvalInbox, toolapprove.ACMMLevelOf(w.cfg)),
	}, w.logger)

	// Emit the governor_mode_change transition post-commit. Installed once:
	// the observer reads the dispatcher through hookDispatcher() on each
	// firing, so a later config reload that arms or disarms hooks is picked up
	// without re-registering.
	installGovernorModeChangeEmitter(w.gov)
	installAgentPauseEmitter(w.agentMgr)

	// Register custom GHE hostnames with the proxy allowlist so mode
	// enforcement applies to GitHub Enterprise API and web requests.
	for _, rawURL := range []string{w.cfg.GitHub.ResolvedAPIURL(), w.cfg.GitHub.ResolvedBaseURL()} {
		if parsed, err := url.Parse(rawURL); err == nil && parsed.Host != "" {
			proxy.RegisterGitHubHost(parsed.Host)
		}
	}

	dashboard.SetBackendAuthProvider(w.agentMgr.BackendAuthAvailable)
}
