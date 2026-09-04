package main

import (
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/snapshot"
)

func (w *spokeWire) wireSpokeStateDashboard() {
	// Opt-in mint credential: when mint.enabled, build a Minter from the config
	// (signing key + issuer + TTL) and attach it so each per-agent token refresh
	// ALSO issues a scoped short-lived OIDC token alongside the GitHub App token.
	// Default off — an absent/disabled `mint:` block leaves the credential path
	// byte-identical. Fail-safe: a mint setup error is logged, never fatal.
	if w.cfg.Mint.Enabled {
		var err error
		if w.agentMinter, err = buildAgentMinter(w.cfg, w.logger); err != nil {
			w.logger.Warn("mint enabled but minter setup failed; agents keep App token only", "error", err)
		} else {
			w.agentMgr.SetAgentMint(w.agentMinter)
			w.logger.Info("mint credential enabled", "issuer", w.cfg.Mint.Issuer, "hive_id", w.cfg.HiveID)
		}
	}

	go agent.StartPermissionsWatcher(w.logger)

	const spokeStatePath = "/data/hive-state.json"
	var stateErr error
	w.saved, stateErr = snapshot.LoadState(spokeStatePath, w.logger)
	if stateErr != nil {
		w.logger.Warn("failed to load persisted state", "error", stateErr)
	} else if w.saved != nil {
		restoreAgentRuntimeState(w.saved, w.cfg, w.agentMgr, w.logger)
		// Re-establish the fleet breaker AFTER per-agent pauses are restored
		// above: the agents it held are already back in the paused state (with
		// PausedTrigger == fleet-breaker from their persisted pause), so this
		// only re-attaches the breaker so a later release resumes exactly them.
		// An engaged breaker must REMAIN engaged across restart — it does not
		// auto-release, and it does not re-pause or resume anything here.
		if w.saved.Breaker != nil && w.saved.Breaker.Engaged {
			w.agentMgr.RestoreBreaker(true, w.saved.Breaker.Paused)
			w.logger.Info("fleet breaker restored from state", "held", len(w.saved.Breaker.Paused))
		}
		if w.saved.BudgetLimit > 0 {
			w.gov.SetBudgetLimit(w.saved.BudgetLimit)
		}
		if w.saved.BudgetIgnoreAll {
			w.gov.SetBudgetIgnoreAll(true)
		}
		if len(w.saved.BudgetIgnored) > 0 {
			w.gov.SetBudgetIgnored(w.saved.BudgetIgnored)
		}
		if len(w.saved.CadenceOverrides) > 0 {
			for modeName, agentCadences := range w.saved.CadenceOverrides {
				mode, ok := w.cfg.Governor.Modes[modeName]
				if !ok {
					continue
				}
				if mode.Cadences == nil {
					mode.Cadences = make(map[string]config.Cadence)
				}
				for agentName, cadence := range agentCadences {
					mode.Cadences[agentName] = cadence
				}
				w.cfg.Governor.Modes[modeName] = mode
			}
			w.logger.Info("cadence overrides restored", "modes", len(w.saved.CadenceOverrides))
		}
		if w.saved.GovernorMode != "" {
			w.gov.SetMode(governor.Mode(w.saved.GovernorMode))
			w.logger.Info("governor mode restored", "mode", w.saved.GovernorMode)
		}
		if len(w.saved.LastKicks) > 0 {
			w.gov.SeedLastKicks(w.saved.LastKicks)
			w.logger.Info("governor last kicks restored", "agents", len(w.saved.LastKicks))
		}
		if w.saved.BudgetSpend > 0 || !w.saved.BudgetResetAt.IsZero() || len(w.saved.BudgetByAgent) > 0 {
			w.gov.SeedBudget(w.saved.BudgetSpend, w.saved.BudgetByAgent, w.saved.BudgetByModel, w.saved.BudgetResetAt)
			w.gov.SeedBudgetWindowBaseline(w.saved.BudgetWindowBaseline)
			w.logger.Info("budget state restored", "spend", w.saved.BudgetSpend, "reset_at", w.saved.BudgetResetAt, "window_baseline", w.saved.BudgetWindowBaseline)
		}
		if len(w.saved.KickHistory) > 0 {
			records := make([]governor.KickRecord, len(w.saved.KickHistory))
			for i, ke := range w.saved.KickHistory {
				records[i] = governor.KickRecord{Timestamp: ke.Timestamp, Agent: ke.Agent}
			}
			w.gov.SeedKickHistory(records)
			w.logger.Info("kick history restored", "entries", len(records))
		}
		if !w.saved.LastEval.IsZero() {
			w.gov.SeedLastEval(w.saved.LastEval)
		}
		if w.saved.ACMMLevel != nil && w.cfg.ACMMLevel == nil {
			w.cfg.ACMMLevel = w.saved.ACMMLevel
			w.logger.Info("ACMM level restored", "level", *w.saved.ACMMLevel)
		}
		if w.saved.ConfigOverrides != nil {
			applyConfigOverrides(w.cfg, w.saved.ConfigOverrides)
			w.ghClient.SetRepos(w.cfg.Project.Repos)
			if len(w.cfg.Governor.Labels.Exempt) > 0 {
				w.ghClient.SetExemptLabels(w.cfg.Governor.Labels.Exempt)
				w.ghClient.SetAutoMergeLabel(normalizedAutoMergeLabel(w.cfg.Governor.Labels.AutoMerge))
			}
			w.ghClient.SetIssueFilter(w.cfg.Project.IssueFilter)
			w.logger.Info("migrated config overrides from state to hive.yaml",
				"repos", w.cfg.Project.Repos)

			// Write merged config to hive.yaml so overrides become the base config
			if err := w.cfg.Save(); err != nil {
				w.logger.Error("failed to save migrated config", "error", err)
			}

			// Strip config_overrides from state and re-save
			w.saved.ConfigOverrides = nil
			if err := snapshot.SaveState(spokeStatePath, w.saved, w.logger); err != nil {
				w.logger.Error("failed to re-save state after migration", "error", err)
			}
		}
	}

	if w.gov.GetBudget().WeeklyLimit == 0 && w.cfg.Governor.Budget.TotalTokens > 0 {
		w.gov.SetBudgetLimit(w.cfg.Governor.Budget.TotalTokens)
	}

	w.dashSrv = dashboard.NewServerWithAuth(w.cfg.Dashboard.Port, w.cfg.Dashboard.AuthToken, w.logger)
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
	w.preShutdownHooks.addUrgent("drain-contributor-websockets", func() {
		w.dashSrv.DrainContributorsForShutdown()
	})

	// Wire ioscan input enforcement (opt-in via ioscan.enabled) to the dashboard
	// audit log so a blocked/redacted issue title surfaces in the existing
	// audit-trail UI with no new sink. The closure keeps pkg/scheduler decoupled
	// from pkg/dashboard — the scheduler only knows a func(action, detail, agent).
	w.sched.SetAuditFunc(func(action, detail, agent string) {
		w.dashSrv.AuditLog(agent, action, detail, agent)
	})
	w.sched.SetAdvisoryFunc(func(title, detail, agentName string) {
		store := w.beadStores[agentName]
		if store == nil {
			store = w.beadStores["scanner"]
		}
		if store == nil {
			store = w.beadStores["supervisor"]
		}
		if store == nil {
			for _, candidate := range w.beadStores {
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
	if w.cfg.Ioscan.IsEnabled() && w.cfg.Ioscan.Classifier.Enabled {
		endpoint, apiKey, model := w.cfg.Governor.ResolveReviewer()
		if w.cfg.Ioscan.Classifier.Model != "" {
			model = w.cfg.Ioscan.Classifier.Model
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
			w.logger.Warn("ioscan classifier enabled but not running", "reason", cerr.Error())
		} else {
			w.sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), ioscan.Thresholds{
				Warn:  w.cfg.Ioscan.Classifier.WarnThreshold,
				Block: w.cfg.Ioscan.Classifier.BlockThreshold,
			})
			w.logger.Info("ioscan semantic classifier enabled", "model", model)
		}
	}
	w.agentMgr.SetSandboxAuditCallback(func(agentName, action, detail string) {
		w.dashSrv.AuditLog(agentName, action, detail, agentName)
		if action == "sandbox_broker_rejected" {
			if store, ok := w.beadStores[agentName]; ok && store != nil {
				if b, err := store.Create("Sandbox push broker rejected changes", beads.TypeAdvisory, beads.PriorityHigh, agentName, ""); err == nil {
					_ = store.SetMetadata(b.ID, "sandbox_broker_rejection", detail)
				}
			}
		}
	})

	// Persist per-user dashboard sessions on the PVC (/data) so direct-route
	// users aren't logged out by pod restarts. NOTE: use /data explicitly, NOT
	// filepath.Dir(w.configPath) — the config lives at /etc/hive/hive.yaml, which
	// is an ephemeral emptyDir (the ConfigMap seed mount), so a sessions file
	// there is wiped on every pod roll. That was the "re-login on every visit"
	// bug on direct-route spokes. /data is the CephFS PVC (same place cost/fact
	// history persist).
	w.dashSrv.EnableSessionPersistence("/data/dashboard-sessions.json")

	// Lifecycle timeline journeys persist on the PVC too (#5656): the ring is
	// the panel's only memory of merged/blocked outcomes, so a pod roll must
	// not zero the fleet counters. Enabled before any producer records.
	w.dashSrv.EnableLifecyclePersistence("/data/lifecycle-timeline.json")

	// The scheduler's classifier pass records KindClassified journeys the
	// moment lane routing decides an issue's lane — same store, no extra work.
	w.sched.SetLifecycleRecorder(w.dashSrv.LifecycleTimeline())

	// Attribution audit sink: every hive-mediated PR/issue creation lands in
	// the dashboard audit log (audit.jsonl + ring) UNCONDITIONALLY — the
	// trailer toggle never gates this. Creations before this point (the
	// startup advisory-issue ensure) fall back to the hive log inside
	// recordCreationAudit, so no creation goes unrecorded. The same stream
	// feeds the lifecycle timeline: agent_pr_created → pr_opened and
	// pr_merged → merged (both automerge sweep paths, MergePR from the
	// dashboard queue and the merge watcher), see recordLifecycleFromAudit.
	if w.ghClient != nil {
		w.ghClient.SetAttributionAudit(func(action, detail, agent string) {
			w.dashSrv.AuditLog("system", action, detail, agent)
			recordLifecycleFromAudit(w.dashSrv, w.cfg.Project.Org, action, detail, agent)
		})
	}

	// Seed token sparkline history now that the dashboard server exists
	if len(w.pendingTokenSeed) > 0 {
		w.dashSrv.SeedTokenSparklineHistory(w.pendingTokenSeed)
		w.logger.Info("token sparkline history restored", "entries", len(w.pendingTokenSeed))
	}

	if len(w.pendingFactSeed) > 0 {
		w.dashSrv.SeedFactHistory(w.pendingFactSeed)
		w.logger.Info("fact history restored", "entries", len(w.pendingFactSeed))
	}

	if len(w.pendingCostSeed) > 0 {
		w.dashSrv.SeedCostHistory(w.pendingCostSeed)
		w.logger.Info("cost history restored", "entries", len(w.pendingCostSeed))
	}

	if len(w.pendingBudgetWindowSeed) > 0 {
		w.dashSrv.SeedBudgetWindowHistory(w.pendingBudgetWindowSeed)
		w.logger.Info("budget window history restored", "entries", len(w.pendingBudgetWindowSeed))
	}
	if len(w.pendingConvergenceSoakSeed) > 0 {
		w.dashSrv.SeedConvergenceSoak(w.pendingConvergenceSoakSeed)
		w.logger.Info("convergence soak history restored", "entries", len(w.pendingConvergenceSoakSeed))
	}

	if len(w.pendingTrendSeed) > 0 {
		w.dashSrv.SeedTrendHistory(w.pendingTrendSeed)
		w.logger.Info("trend history restored", "entries", len(w.pendingTrendSeed))
	}

	w.beadStores = make(map[string]*beads.Store)
}
