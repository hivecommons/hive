package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/watchdog"
)

func (w *spokeWire) wireSpokeManagersAndLinear() {
	// Brainstorm is on-demand only. Only restart with bootstrap during
	// capture phase — structure/scaffold phases don't need a fresh kick
	// and restarting would revert the phase back to capture.
	// Skip stale inceptions (> 10 min old) — these are leftovers from
	// previous runs that would interfere with new inceptions.
	const staleInceptionThreshold = 10 * time.Minute
	if state := w.inceptionEngine.GetState(); state != nil &&
		state.Phase != knowledge.PhaseComplete &&
		state.Phase != knowledge.PhaseScaffold {
		if time.Since(state.StartedAt) < staleInceptionThreshold {
			msg := w.sched.BuildAgentMessage("brainstorm", nil, nil)
			if err := w.agentMgr.RestartWithBootstrap(w.ctx, "brainstorm", msg); err != nil {
				w.logger.Warn("failed to resume brainstorm for active inception", "error", err)
			} else {
				w.logger.Info("brainstorm resumed for active inception", "phase", state.Phase)
			}
		} else {
			w.logger.Info("skipping stale inception resume — resetting",
				"phase", state.Phase,
				"age", time.Since(state.StartedAt).Round(time.Second),
			)
			_ = w.inceptionEngine.Reset()
			if err := w.agentMgr.Pause("brainstorm", "startup", "stale inception cleared — on-demand only"); err != nil {
				w.logger.Debug("brainstorm pause on startup", "error", err)
			}
		}
	} else {
		if err := w.agentMgr.Pause("brainstorm", "startup", "on-demand agent — triggered by inception only"); err != nil {
			w.logger.Debug("brainstorm pause on startup", "error", err)
		}
	}

	// Provider rotation (RFC #3958): opt-in automatic failover when a
	// provider's subscription/credit is exhausted. Nil when disabled.
	if w.cfg.Governor.Rotation.Enabled {
		w.rotationMgr = rotation.NewManager(w.cfg.Governor.Rotation)
		w.rotationMgr.Start(w.ctx)
		w.logger.Info("provider rotation enabled",
			"threshold_pct", w.cfg.Governor.Rotation.EffectiveThreshold(),
			"providers", len(w.cfg.Governor.Rotation.Providers))
	}

	// Agent self-healing watchdog (RFC #4665): liveness/readiness
	// reconciliation on the governor tick. Config problems fall back to the
	// RFC defaults loudly — a typo must not disable self-healing silently.
	wdSettings, wdCfgErrs := watchdog.SettingsFrom(w.cfg.Governor.Watchdog)
	for _, e := range wdCfgErrs {
		w.logger.Warn("watchdog config problem", "error", e)
	}
	if wdSettings.Enabled() {
		wdFleet := agent.WatchdogFleet{
			M: w.agentMgr,
			// Queue depth for the readiness gate: an agent producing nothing
			// while nothing is queued is correct, not unhealthy. Read live
			// from the governor so it reflects the current sweep.
			Queued: func() (int, bool) {
				st := w.gov.GetState()
				return st.QueueIssues + st.QueuePRs, true
			},
		}
		w.wd = watchdog.New(wdSettings, wdFleet, w.dashSrv, w.logger,
			watchdog.WithAuthProbes(watchdogAuthProbes(w.cfg)))
		if w.saved != nil && len(w.saved.Watchdog) > 0 {
			w.wd.Restore(w.saved.Watchdog)
		}
		// Dead-session recovery moves under the watchdog's bounded ladder ONLY
		// when the watchdog may actually act. In observe mode the manager's
		// crash loop keeps its existing job, so there is never a window in
		// which neither component restarts a dead agent.
		w.agentMgr.SetDeadSessionRecoveryOwner(wdSettings.MayAct())
		w.logger.Info("agent watchdog enabled (RFC #4665)",
			"mode", string(wdSettings.Mode),
			"probe_interval", wdSettings.ProbeInterval,
			"crash_loop_after", wdSettings.CrashLoopAfter,
			"auth_probe", wdSettings.AuthProbe,
			"dead_session_recovery", map[bool]string{true: "watchdog", false: "crash-loop"}[wdSettings.MayAct()])
		if wdSettings.Mode == watchdog.ModeObserve {
			w.logger.Info("agent watchdog is in OBSERVE mode: it will classify agents, publish conditions and record what it WOULD have done, but will not restart or pause anything. Set governor.watchdog.mode: heal to enable healing.")
		}
	} else {
		w.logger.Info("agent watchdog disabled by config", "mode", string(wdSettings.Mode))
	}

	// Linear write credential for ISSUES_ONLY+ agents (GitHub-issue parity):
	// prefer the connected Linear agent app's OAuth token, so agent writes are
	// authored by the same "Hive" app identity that acknowledges sessions —
	// the analogue of App-bot authorship on GitHub — and fall back to the
	// work-source API key from hive.yaml. Resolved live off the dashboard's
	// install store and the w.cfg pointer so a workspace connected after boot
	// reaches agents on their next launch / hourly token refresh. Values are
	// never logged. Wired before RegisterAPI so the resolver is in place
	// before any agent launches.
	w.agentMgr.SetLinearCredentialResolver(func() agent.LinearCredential {
		if tok := w.dashSrv.LinearAgentAccessToken(); tok != "" {
			return agent.LinearCredential{AccessToken: tok}
		}
		if w.cfg.Governor.WorkSource.Type == "linear" {
			return agent.LinearCredential{APIKey: strings.TrimSpace(w.cfg.Governor.WorkSource.Linear.APIKey)}
		}
		return agent.LinearCredential{}
	})

	// In-flight ledger + session PR link (Linear GitHub-parity follow-ups):
	// the scheduler withholds work a Linear session is already working, and
	// the pr-request watcher narrates opened PRs into the session.
	w.sched.SetInflightLookup(w.dashSrv.LinearSessionHolder)
	if w.ghClient != nil {
		w.ghClient.SetPROpenedHook(func(agentName, repo string, number int, url string) {
			w.dashSrv.LinearAgentPROpened(agentName, repo, number, url)
			// Same typed hook feeds the lifecycle timeline: the watcher fires
			// it on the exact path that opened the PR, with the agent name the
			// audit stream attributes to the governor flow (#5656). The store
			// dedupes with the audit-sink bridge by (ref, kind).
			recordPROpened(w.dashSrv, w.cfg.Project.Org, agentName, repo, number, url)
		})
	}

	w.dashSrv.RegisterAPI(&dashboard.Dependencies{
		Config:   w.cfg,
		AgentMgr: w.agentMgr,
		// Provider gateways (#5565 slice 3): concrete openrouter/watsonx/
		// linearagent adapters behind the dashboard's consumer-defined
		// interfaces — this composition root is the only non-test place that
		// names the concrete types.
		Watsonx:              watsonxGateway{},
		OpenRouter:           openRouterGateway{},
		NewLinearAgent:       newLinearAgentGateway(w.logger),
		LinearStoredViewerID: linearStoredViewerID,
		Governor:             w.gov,
		GHClient:             w.ghClient,
		GHAppAuth:            w.appAuth,
		GHTokenScopes:        w.ghAuth.TokenScopes,
		Tokens:               w.tokenCollector,
		Knowledge:            w.knowledgeAPI,
		Inception:            w.inceptionEngine,
		Nous:                 w.nousState,
		Scheduler:            w.sched,
		MetricsCollector:     w.metricsCollector,
		RotationMgr:          w.rotationMgr,
		// #3972: hand the ACMM advisor the SAME cached fleet-stats collector
		// the heartbeat reads, so its merge-success signal reuses the existing
		// 30-minute collect loop instead of issuing a second GitHub fetch.
		FleetStats:            w.fleetStatsCollector,
		Activity:              w.activityCollector,
		RepoCost:              w.repoCostCollector,
		BeadSynthesizer:       w.beadSynth,
		BeadStores:            w.beadStores,
		BeadStoreLoadFailures: w.beadStoreLoadFailures,
		// RFC #4000 approval desk. Nil unless `tool_approval.enabled`, in which
		// case the Approvals panel renders as "not enabled".
		ApprovalDesk:  w.approvalDesk,
		ApprovalInbox: w.approvalInbox,
		Logger:        w.logger,
		Ctx:           w.ctx,
		RefreshFunc:   w.refreshDashboard,
		// #3768: give the contribute queue read access to the duplicate-PR
		// claim ledger, so an issue any open PR (hive-authored or a human
		// contributor's) already claims to fix is never offered to another
		// contributor. Lazy: the ledger loads on first use, same as the
		// eval-cycle guard.
		IssueClaimed: func(repo string, number int) (github.IssueClaim, bool) {
			return getClaimLedger(w.logger).Lookup(repo, number)
		},
		HookFire: func(ctx context.Context, p hooks.Payload) {
			hookDispatcher().Fire(ctx, p)
		},
		PersistFunc: func() {
			persistState(w.agentMgr, w.gov, w.cfg, spokeStatePath, w.logger, w.dashSrv, w.wd)
		},
		ReInitFunc: func() {
			initAgentConfigDrivenSystems(w.cfg)
		},
		EnumerateFunc: func() {
			runEvalCycle(w.ctx, w.cfg, w.ghClient, w.gov, w.sched, w.agentMgr, w.dashSrv, w.notifier, w.beadStores, w.tokenCollector, w.metricsCollector, w.nousState, &w.lastActionable, w.advisoryStore, w.advisoryIssues, nil, w.approvalDesk, w.logger)
		},
		AdvisoryResetFunc: func(newPrimaryRepo string) {
			w.logger.Info("advisory reset: primary repo changed, creating new advisory issue", "repo", newPrimaryRepo)
			if w.ghClient != nil {
				num, err := w.ghClient.EnsureAdvisoryIssue(w.ctx, newPrimaryRepo)
				if err != nil {
					w.logger.Error("failed to create advisory issue on new primary repo", "repo", newPrimaryRepo, "error", err)
					if isGitHubRateLimitText(err) {
						w.logger.Warn("GitHub API rate limit hit during advisory issue creation", "repo", newPrimaryRepo)
					} else {
						// #2224 replaced error-string classification everywhere
						// else but missed this site, which raised the banner on
						// a bare "403"/"401" substring and recorded no state at
						// all — so the UI fell back to "App Not Installed" even
						// for an operator-side key fault. Classify properly.
						raise, diag, state := classifyGitHubAppFailure(w.ctx, w.ghClient.AppAuth(), w.cfg.Project.Org, w.logger)
						if raise {
							w.dashSrv.SetGitHubAppRequired(true)
							w.dashSrv.SetGitHubAppState(state.String())
							if diag != "" {
								w.dashSrv.SetGitHubAppPermIssue(diag)
							}
							w.logger.Warn("GitHub App authentication failed creating advisory issue",
								"repo", newPrimaryRepo, "state", state.String(),
								"operator_actionable", state.OperatorActionable())
						}
					}
				} else {
					w.advisoryIssues[newPrimaryRepo] = num
					_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
					w.dashSrv.SetGitHubAppRequired(false)
					w.dashSrv.ClearPendingGitHubAppInstall()
					w.logger.Info("advisory issue ready on new primary repo", "repo", newPrimaryRepo, "number", num)
				}
			}
		},
		ReinitGitHubFunc: func(newAppID, newInstallationID int64, keyFile string) error {
			newAppAuth, err := github.NewAppAuth(newAppID, newInstallationID, keyFile, w.logger, w.cfg.GitHub.ResolvedAPIURL())
			if err != nil {
				return fmt.Errorf("initializing app auth: %w", err)
			}
			newClient := github.NewClientFromAppWithBotLogin(newAppAuth, w.cfg.Project.Org, w.cfg.Project.Repos, w.logger, w.cfg.GitHub.BotLogin())
			if len(w.cfg.Governor.Labels.Exempt) > 0 {
				newClient.SetExemptLabels(w.cfg.Governor.Labels.Exempt)
				newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(w.cfg.Governor.Labels.AutoMerge))
			}
			newClient.SetIssueFilter(w.cfg.Project.IssueFilter)

			w.ghClient = newClient
			w.appAuth = newAppAuth
			w.agentMgr.SetAppAuth(newAppAuth)
			// Deliver fresh per-agent scoped tokens to already-running agents
			// immediately — the periodic refresh loop only ticks every 40m,
			// far too long for agents whose caches are empty or stale (#4072).
			go w.agentMgr.RefreshAgentTokens(w.ctx)
			w.dashSrv.UpdateGitHubClient(newClient, newAppAuth)
			w.logger.Info("github client reinitialized via config API", "app_id", newAppID, "installation_id", newInstallationID)

			primaryRepo := w.cfg.Project.PrimaryRepo
			if primaryRepo == "" && len(w.cfg.Project.Repos) > 0 {
				primaryRepo = w.cfg.Project.Repos[0]
			}
			if primaryRepo != "" {
				num, advErr := w.ghClient.EnsureAdvisoryIssue(w.ctx, primaryRepo)
				if advErr != nil {
					w.logger.Warn("advisory issue creation failed after reinit", "repo", primaryRepo, "error", advErr)
				} else {
					w.advisoryIssues[primaryRepo] = num
					_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
					w.logger.Info("advisory issue ready after reinit", "repo", primaryRepo, "number", num)
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
	w.dashSrv.SetForgeAppInventoryFn(func() dashboard.ForgeAppInventory {
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
			ActiveKeyFile: appKeys.Resolve(w.cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), w.cfg.GitHub.AppID),
			HeldKeys:      keys,
		}
	})

	w.dashSrv.SetGitHubAppRequired(w.githubAppRequired)
	// Order matters: SetGitHubAppRequired(false) clears both fields, so the
	// classified state is applied only after it, and only when a failure was
	// actually detected.
	if w.githubAppRequired {
		w.dashSrv.SetGitHubAppState(w.githubAppState.String())
		if w.githubAppDiag != "" {
			w.dashSrv.SetGitHubAppPermIssue(w.githubAppDiag)
		}
	}

}

func (w *spokeWire) wireSpokeAppCallbacks() {
	// Wire up the manual re-check callback for the dashboard button.
	{
		recheckRepo := w.cfg.Project.PrimaryRepo
		if recheckRepo == "" && len(w.cfg.Project.Repos) > 0 {
			recheckRepo = w.cfg.Project.Repos[0]
		}
		if recheckRepo != "" {
			w.dashSrv.SetGitHubAppRecheckFn(func() bool {
				// The Re-check button is the first thing an owner clicks on a
				// degraded hive. Report the real cause instead of the generic
				// "not accessible" — there is no client to check WITH, so the
				// credentials themselves are what must be fixed.
				if w.ghClient == nil {
					w.logger.Warn("github app recheck: hive is running without GitHub credentials", "detail", w.appAuthFailure)
					w.dashSrv.AuditLog("system", "github_app_check", "result=no GitHub client: "+w.appAuthFailure, "")
					return false
				}
				// #4360: ask about repo COVERAGE before attempting a read.
				// A repo the installation does not cover answers 404, which is
				// indistinguishable from "no such repo" and used to be reported
				// as "app not installed / no read" — sending the operator after
				// credentials that were never broken. Checking first means the
				// specific, correct message wins over the generic one.
				if raise, diag, state := classifyGitHubAppRepoCoverage(w.ctx, w.ghClient.AppAuth(), w.cfg.Project.Org, w.cfg.Project.Repos, w.logger); raise {
					w.dashSrv.SetGitHubAppPermIssue(diag)
					w.dashSrv.SetGitHubAppState(state.String())
					w.logger.Warn("github app recheck: installation does not cover every configured repo",
						"org", w.cfg.Project.Org, "state", state.String(), "detail", diag)
					w.dashSrv.AuditLog("system", "github_app_check", "result=repos not in installation: "+diag, "")
					return false
				}
				num, err := w.ghClient.EnsureAdvisoryIssue(w.ctx, recheckRepo)
				if err != nil {
					w.logger.Debug("github app recheck: not accessible", "repo", recheckRepo, "error", err)
					w.dashSrv.AuditLog("system", "github_app_check", "result=not accessible (app not installed / no read)", "")
					return false
				}
				w.advisoryIssues[recheckRepo] = num
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
				// Finding the advisory issue only proves the app is installed
				// (reads succeed on public repos even with a token from the
				// wrong installation). Verify write capability before letting
				// the handler clear the banner, so Re-check can't produce a
				// clears-then-returns flip-flop.
				// Before reporting a wrong-account installation, try to fix it:
				// this is the exact case rediscovery exists for. Cached by TTL,
				// so a repeated re-check does not re-hit the API.
				healGitHubAppInstallation(w.ctx, w.ghClient.AppAuth(), w.cfg, w.logger)
				// Shared verdict with the boot and advisory-digest paths. Re-check
				// previously branched on `diag != ""` while boot branched on the
				// error string, which is how the two came to disagree about the
				// same hive; routing both through classifyGitHubAppFailure means
				// they cannot drift again.
				if raise, diag, state := classifyGitHubAppFailure(w.ctx, w.ghClient.AppAuth(), w.cfg.Project.Org, w.logger); raise {
					w.dashSrv.SetGitHubAppPermIssue(diag)
					w.dashSrv.SetGitHubAppState(state.String())
					w.logger.Warn("github app recheck: app detected but write not verified",
						"repo", recheckRepo, "state", state.String(),
						"operator_actionable", state.OperatorActionable(), "detail", diag)
					w.dashSrv.AuditLog("system", "github_app_check", "result=installed but write NOT verified: "+diag, "")
					return false
				}
				// #2353: the classifier above only proves the installation
				// authenticates and grants issues:write — NOT that this repo can
				// actually be written. Finding the advisory issue is a READ, which
				// succeeds even when the repo is not in the App installation's
				// selected repos. Perform a REAL write probe before clearing the
				// banner, so re-check cannot falsely "verify write" for a repo the
				// App can only read (the recheck false-positive).
				if werr := w.ghClient.ProbeIssueWrite(w.ctx, recheckRepo, num); werr != nil {
					if strings.Contains(werr.Error(), "403") && strings.Contains(werr.Error(), "Resource not accessible by integration") {
						msg, state := classifyGitHubAppWriteForbidden(w.ctx, w.ghClient.AppAuth(), w.cfg.Project.Org, recheckRepo)
						w.dashSrv.SetGitHubAppPermIssue(msg)
						w.dashSrv.SetGitHubAppState(state.String())
						w.logger.Warn("github app recheck: write probe returned 403 — not clearing the banner",
							"repo", recheckRepo, "state", state.String(), "detail", msg)
						w.dashSrv.AuditLog("system", "github_app_check", "result=write probe FORBIDDEN: "+msg, "")
						return false
					}
					// A non-403 probe failure is inconclusive (rate limit,
					// transient network). Do NOT clear the banner on a write we
					// could not confirm, but also do NOT accuse anyone.
					w.logger.Warn("github app recheck: write probe inconclusive — leaving banner as-is",
						"repo", recheckRepo, "error", werr)
					w.dashSrv.AuditLog("system", "github_app_check", "result=write probe inconclusive", "")
					return false
				}
				w.logger.Info("github app recheck: app detected, write verified", "repo", recheckRepo, "number", num)
				w.dashSrv.AuditLog("system", "github_app_check", "result=OK (installed, write verified)", "")
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
			if w.cfg.GitHub.InstallationID != 0 {
				return
			}
			_, _ = w.dashSrv.AutoDiscoverGitHubInstallationID(w.ctx, false)
		}
		go func() {
			tryDiscover()
			ticker := time.NewTicker(githubAppDiscoveryInterval)
			defer ticker.Stop()
			for {
				select {
				case <-w.ctx.Done():
					return
				case <-ticker.C:
					tryDiscover()
				}
			}
		}()
	}

	// Self-heal the "GitHub App not installed" banner. This handles:
	//  1. GitHub App credentials arrived after startup (via heartbeat/webhook)
	//  2. ReinitGitHubFunc succeeded but cleared w.githubAppRequired before
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
		primaryRepo := w.cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(w.cfg.Project.Repos) > 0 {
			primaryRepo = w.cfg.Project.Repos[0]
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
					case <-w.ctx.Done():
						return
					case <-ticker.C:
						// Nothing to heal unless the banner is showing.
						if !w.dashSrv.IsGitHubAppRequired() {
							continue
						}
						_, _ = w.dashSrv.AutoDiscoverGitHubInstallationID(w.ctx, false)
						// Re-run the same read+write verification the manual
						// Re-check button uses. It clears the flag on success
						// (installed AND write-verified) and leaves it set on a
						// genuine failure (not installed / insufficient perms).
						if w.dashSrv.RecheckGitHubApp() {
							if num, exists := w.advisoryIssues[primaryRepo]; exists {
								_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
							}
							w.logger.Info("github app self-heal: banner cleared, app installed and write verified", "repo", primaryRepo)
						} else {
							w.logger.Debug("github app self-heal: still not verified, banner remains", "repo", primaryRepo)
						}
					}
				}
			}()
		}
	}

	if brainstormBeads, ok := w.beadStores["brainstorm"]; ok {
		inceptionWatcher := dashboard.NewInceptionWatcher(brainstormBeads, w.inceptionEngine, w.sched, w.agentMgr, w.gov, w.logger)
		go inceptionWatcher.Run(w.ctx)
	}

	if w.saved == nil {
		if levelStr := os.Getenv("HIVE_LEVEL"); levelStr != "" {
			const maxACMMLevel = 6
			level, err := strconv.Atoi(levelStr)
			if err != nil || level < 1 || level > maxACMMLevel {
				w.logger.Warn("invalid HIVE_LEVEL, skipping auto-apply", "value", levelStr)
			} else {
				w.logger.Info("first start detected, auto-applying ACMM pack", "level", level)
				result, err := w.dashSrv.ApplyPack(level)
				if err != nil {
					w.logger.Error("failed to auto-apply ACMM pack", "level", level, "error", err)
				} else {
					w.logger.Info("ACMM pack auto-applied",
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
		if w.cfg.ACMMLevel != nil && *w.cfg.ACMMLevel >= 1 && *w.cfg.ACMMLevel <= maxACMMLevel {
			level = *w.cfg.ACMMLevel
		} else if w.saved.ACMMLevel != nil && *w.saved.ACMMLevel >= 1 && *w.saved.ACMMLevel <= maxACMMLevel {
			level = *w.saved.ACMMLevel
		} else if levelStr := os.Getenv("HIVE_LEVEL"); levelStr != "" {
			if parsed, err := strconv.Atoi(levelStr); err == nil && parsed >= 1 && parsed <= maxACMMLevel {
				level = parsed
			} else {
				w.logger.Warn("invalid HIVE_LEVEL, skipping auto-apply", "value", levelStr)
			}
		}
		if level > 0 {
			action := "merging pack updates"
			if w.saved.ACMMLevel == nil || *w.saved.ACMMLevel != level {
				action = "re-applying pack (level changed)"
			}
			w.logger.Info("audit: "+action, "level", level, "saved_level", w.saved.ACMMLevel, "trigger", "startup")
			result, err := w.dashSrv.ApplyPack(level)
			if err != nil {
				w.logger.Error("failed to apply ACMM pack", "level", level, "error", err)
			} else {
				w.logger.Info("ACMM pack applied on startup",
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

	if w.cfg.Policies.Repo != "" {
		localDir := w.cfg.Policies.LocalDir
		if localDir == "" {
			localDir = "/data/policies"
		}
		watcher := policies.NewWatcher(
			w.cfg.Policies.Repo,
			w.cfg.Policies.Branch,
			w.cfg.Policies.Path,
			localDir,
			w.cfg.Policies.PollInterval,
			w.logger,
		)
		if err := watcher.Start(w.ctx); err != nil {
			w.logger.Warn("policy watcher failed to start", "error", err)
		}
	}

}
