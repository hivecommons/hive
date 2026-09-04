package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/github/requestwatch"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/pushbroker"
)

func (w *spokeWire) wireSpokeAgentsAndRequests() {
	// Find or create the pinned advisory issue. Any level can have advisory
	// agents whose findings should be posted to this issue.
	w.advisoryIssues = map[string]int{}
	if w.acmmLevel > 0 && w.ghClient != nil {
		primaryRepo := w.cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(w.cfg.Project.Repos) > 0 {
			primaryRepo = w.cfg.Project.Repos[0]
		}
		if primaryRepo != "" {
			num, err := w.ghClient.EnsureAdvisoryIssue(w.ctx, primaryRepo)
			if err != nil {
				w.logger.Error("failed to ensure advisory issue", "repo", primaryRepo, "error", err)
				// GitHub returns 403 for rate limiting too — a transient
				// condition that must not raise the "App Not Installed"
				// banner (matches the guard on the repo-change path).
				if isGitHubRateLimitText(err) {
					w.logger.Warn("GitHub API rate limit hit during advisory issue ensure", "repo", primaryRepo)
				} else {
					// Do NOT decide from the error string. This call site used
					// to raise the banner on a substring match for "403"/"401"
					// and set w.githubAppRequired=true BEFORE classifying, then
					// never lower it again when classification came back OK or
					// inconclusive — which is why the banner showed on boot and
					// vanished on the first Re-check with nothing fixed.
					// classifyGitHubAppFailure is the same verdict Re-check
					// uses, and it declines to raise on AppStateUnknown.
					raise, diag, state := classifyGitHubAppFailure(w.ctx, w.ghClient.AppAuth(), w.cfg.Project.Org, w.logger)
					if raise {
						w.githubAppRequired = true
						w.githubAppDiag, w.githubAppState = diag, state
						w.logger.Warn("GitHub App authentication failed at startup",
							"state", state.String(),
							"operator_actionable", state.OperatorActionable(),
							"error", err)
					} else {
						w.logger.Warn("advisory issue ensure failed but GitHub App auth verified healthy — not raising the App banner",
							"repo", primaryRepo, "state", state.String(), "error", err)
					}
				}
			} else {
				w.advisoryIssues[primaryRepo] = num
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
				w.logger.Info("advisory issue ready", "repo", primaryRepo, "number", num)
			}
		}
	}

	w.advisoryStore = advisory.NewStore()

	w.policyDir = w.cfg.Policies.LocalDir
	if w.policyDir == "" {
		w.policyDir = "/data/policies"
	}
	if w.cfg.Policies.Path != "" {
		w.policyDir = w.policyDir + "/" + w.cfg.Policies.Path
	}

	// Write brainstorm policy to disk so the agent can find it.
	// The policy is embedded in the binary but the agent searches the filesystem.
	brainstormPolicyDir := w.policyDir
	if brainstormPolicyDir == "" {
		brainstormPolicyDir = "/data/policies/examples/kubestellar/agents"
	}
	if err := os.MkdirAll(brainstormPolicyDir, 0o755); err != nil {
		w.logger.Warn("failed to create brainstorm policy dir", "path", brainstormPolicyDir, "error", err)
	}
	if policyData, err := policies.DefaultPolicies.ReadFile("defaults/brainstorm-advisory.md"); err == nil {
		policyPath := filepath.Join(brainstormPolicyDir, "brainstorm-advisory.md")
		// Always overwrite — the embedded policy may have been updated
		// (e.g., inception reaping guard added in bug #113 fix).
		if err := os.WriteFile(policyPath, policyData, 0o644); err != nil {
			w.logger.Warn("failed to write brainstorm policy", "path", policyPath, "error", err)
		} else {
			w.logger.Info("wrote brainstorm policy to disk", "path", policyPath)
		}
	}

	w.projectCtx = agent.ProjectContext{
		Org:             w.cfg.Project.Org,
		Repos:           w.cfg.Project.Repos,
		PrimaryRepoName: w.cfg.Project.PrimaryRepo,
		ACMMLevel:       w.acmmLevel,
		PRsAllowed:      w.cfg.Project.PRsAllowed(),
		PolicyDir:       w.policyDir,
		AppAuthoredPRs:  w.cfg.GitHub.AppAuthoredPRsEnabled(),
	}
	if w.cfg.GitHub.IsGHE() {
		w.projectCtx.GHHost = w.cfg.GitHub.HostLabel()
	}
	w.agentMgr = agent.NewManager(w.cfg.EnabledAgents(), w.logger, w.projectCtx)
	// SIGTERM (pod roll, hive upgrade) destroys every tmux server and with it
	// the in-flight kick's scrollback; archive it to /data first (#4296).
	archiveOnShutdown := func() { w.agentMgr.ArchiveAllKickLogs("shutdown") }
	w.preShutdownHooks.add("archive-kick-logs", archiveOnShutdown)
	w.agentMgr.SetSandboxConfig(w.cfg.AgentSandbox)

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
	logAgentSandboxPosture(w.logger, w.cfg)
	// Treat any configured gateway name as an inference-routable backend so an
	// agent with backend: <gateway> routes through it. Resolution is live
	// (reads w.cfg on each call) so gateways added from the Model Gateways tab
	// take effect without a restart.
	//
	// Wired HERE — immediately after the manager is constructed — and not in
	// the proxy/dashboard wiring further down, because SetBackendOverride
	// validates backend names against this predicate. The persisted-state
	// replay (restoreAgentRuntimeState, below) re-applies w.saved backend
	// overrides long before the dashboard wiring runs, and with the predicate
	// still unset every gateway-named override was rejected there — silently,
	// while the model override beside it restored fine. That is the #3961
	// asymmetric revert: an agent switched to a gateway backend came back on
	// its config backend but with the switched model still applied, producing
	// launch-dead hybrids like `pi --model gpt-5.6-luna`.
	w.agentMgr.SetGatewayBackendChecker(func(backend string) bool {
		return w.cfg.Governor.ResolveGateway(backend) != nil &&
			!strings.EqualFold(backend, "") // empty is the default, not a named backend
	})
	// Resolve the bob API key at LAUNCH time, not here: w.cfg is the live config
	// pointer (the config watcher swaps its contents in place on reload), so a
	// key added via the Secret mount, the PVC file, or a config edit takes
	// effect on the next agent launch with no hive restart. Only the key's
	// LOCATION is ever in w.cfg; the value is read from file/env on each call and
	// is never logged.
	w.agentMgr.SetBobAPIKeyResolver(func() string {
		return w.cfg.Governor.Bob.ResolveAPIKey()
	})
	// Hive-wide default explain mode, resolved per kick/launch off the live w.cfg
	// pointer for the same reason as the bob key above: an operator debugging a
	// misbehaving fleet turns explanation on from Settings → Governor and needs
	// it on the NEXT kick, not after a restart. Governor config wins over
	// HIVE_EXPLAIN_MODE; the env var stays as the fallback (#4712).
	w.agentMgr.SetExplainModeDefaultResolver(func() string {
		return w.cfg.Governor.ResolveExplainModeDefault()
	})
	// The launch path also needs to know WHICH FILE the key came from, so it can
	// check that file is readable by the agent UID rather than only by the hive
	// process. Returns a loggable source string, never the key value.
	w.agentMgr.SetBobKeySourceResolver(func() string {
		return w.cfg.Governor.Bob.ResolveAPIKeySource()
	})
	// Log only WHERE the key came from (or that none is set) so a misconfigured
	// hive is diagnosable without the value ever reaching the logs.
	if src := w.cfg.Governor.Bob.ResolveAPIKeySource(); src != "" {
		w.logger.Info("bob api key detected", "source", src)
	} else {
		w.logger.Info("no bob api key configured; agents with backend \"bob\" will not launch",
			"remedy", "set governor.bob.api_key_file or the "+config.DefaultBobAPIKeyEnv+" env var")
	}
	if w.appAuth != nil {
		w.agentMgr.SetAppAuth(w.appAuth)
		w.agentMgr.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: w.appAuth})
	}
	// Start the per-agent token refresh loop UNCONDITIONALLY. It no-ops until
	// App auth is wired, and on hosted spokes that wiring happens AFTER boot
	// (heartbeat delivery / config API reinit / config reload). Gating this on
	// w.appAuth != nil at boot meant those hives never refreshed per-agent token
	// caches: agent sessions outlived their scoped token, gh 401'd and printed
	// "gh auth login", and the login-detector auto-paused the agent (#4072).
	go w.agentMgr.StartAgentTokenRefresh(w.ctx)
	// Start the credential watchdog UNCONDITIONALLY. It self-gates per backend
	// on the presence of an agent using that backend each tick, so it is a
	// no-op on gateway/inference-only hives. On Copilot/Claude hives it turns a
	// missing or expired durable credential — the "stuck at login after an
	// upgrade roll" outage — into an immediate Audit Log signal instead of a
	// silent multi-hour stall.
	go w.agentMgr.StartCredentialWatchdog(w.ctx)
	// Keep the Copilot CLI's config.json copilotTokens populated from the
	// durable user token, so agents never sit stuck at "Please use /login"
	// while a valid token exists (CLI 1.0.78 does not re-populate the emptied
	// store from the injected env token on its own). Self-gates on a copilot
	// backend and only writes when the store is empty; never runs a login.
	go w.agentMgr.StartCopilotSessionRefresh(w.ctx)
	if w.ghClient != nil {
		w.agentMgr.SetSandboxPRClient(w.ghClient)
	}

	// PR-open-as-the-App-bot: agents push their branch (App-token credential
	// helper) then drop a request file; the hive opens the PR here with the App
	// token so it is authored by "<slug>[bot]", not the Copilot login user.
	// Gated on a real client + usable App — with no App there is no bot to author
	// as, and requests simply accumulate rather than opening under a wrong
	// identity. w.ghClient uses the App installation token (see w.ghAuth wiring).

	// Approval desk (RFC #4000): the single tool-approval decision point plus
	// its durable operator-lane inbox. Both are nil unless
	// `tool_approval.enabled` is set — the default — so this costs nothing and
	// changes nothing on a hive that has not opted in. Built here, before the
	// auto-merge sweep is started below, because the sweep is the one producer
	// wired in this slice. Also handed to the dashboard for the Approvals panel.
	w.approvalDesk, w.approvalInbox = buildApprovalDesk(w.cfg, w.logger)
	// Create the agent-facing request queues REGARDLESS of App state. The
	// watchers below stay gated (no App, no bot to author as), but the queues
	// must exist either way or the "requests simply accumulate" behavior above
	// is a fiction: hive-open-pr / hive-open-issue run in the AGENT's shell and
	// hard-fail on a missing directory, discarding the finding instead of
	// queueing it. App setup routinely completes after boot (operator saves the
	// installation ID, /gh-setup persists it, auto-discovery finds it later), so
	// this gap silently disarms agent writes on a hive that looks healthy.
	github.PrepareRequestDirs(w.logger)

	if w.ghClient != nil && w.cfg.GitHub.HasUsableApp() {
		// Attribution resolver: effective backend/model from the manager
		// (runtime overrides included), falling back to the configured values
		// for an agent the manager does not know; tool version resolved
		// lazily per backend and cached. Only launch descriptors flow here —
		// never tokens, keys, or prompt content.
		w.ghClient.SetAttributionResolver(func(agentName string) github.InvocationMeta {
			backend, model, effort, known := w.agentMgr.InvocationMetadata(agentName)
			if !known {
				if ac, inCfg := w.cfg.Agents[agentName]; inCfg {
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
			return shouldHoldAgentPR(agentName, w.agentMgr.GetACMMLevel())
		}
		// #5117: tell the client which accounts are ours, so the
		// self-authorization gate recognises an issue filed under
		// project.ai_author's plain user account as hive-filed rather than
		// mistaking it for a human's. The App bot is recognised without this;
		// hiveIdentity() is the same resolver the duplicate-PR guard uses.
		w.ghClient.SetHiveIdentity(hiveIdentity(w.cfg))
		startRequestWatchers(w.ctx, requestwatch.New(w.ghClient, w.agentMgr.AuthorizePROpen, w.agentMgr.AuthorizeIssueOpen, holdLabel, nil), w.logger)
		// Review relay: agents request PR reviews by dropping a file (hive-review)
		// instead of running `gh pr review` in their own shell, which the hive
		// never observes. The watcher submits the review with the App token and
		// records it on the audit/activity trail, gated by the same
		// forge-resistance + push-capability (CanPush) check as opening a PR —
		// reviewing is a PR-write, so AuthorizePROpen is the correct gate.
		w.ghClient.StartReviewRequestWatcher(w.ctx, w.agentMgr.AuthorizePROpen, nil)
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
		w.ghClient.SetMergeReEngageHook(mergeReEngageHook(w.cfg))
		w.ghClient.StartMergeRequestWatcher(w.ctx, bindMergeAuthz(w.agentMgr.AuthorizeMerge), nil)

		// SECURITY (audit F3): re-verify the merger tier inside the sweep. The
		// dashboard's queue endpoint gates on requireMergerOrOwnerRole, but the
		// sweep merges a minute later off the label + App-authored approval body
		// alone, so without this ANY actor who can get the label applied merges
		// anything, and a sockpuppet pair defeats the self-merge ban. Resolved
		// against the SAME allowlist the dashboard uses so there is one notion of
		// trust; read through w.cfg on every call so a config reload takes effect.
		autoMergeOpts := automerge.Options{
			Logger:           w.logger,
			MergerAuthorizer: trustedMergerFunc(w.cfg),
		}

		// commitGreen's required-checks gate (self-merge sweep, see
		// automerge_sweep.go): install the operator-declared
		// auto_merge.required_checks list, if any, so gating does not depend
		// on GitHub's branch-protection API — the Hive App token lacks
		// administration:read, so that API call reliably errors and would
		// otherwise fail closed to the coarser isMetaCheck/isIgnorableCICheck
		// allowlist. Unset/empty leaves the API/allowlist fallback chain
		// intact (SetRequiredChecks(nil) is a safe no-op).
		if set, ok := w.cfg.AutoMerge.RequiredCheckSet(); ok {
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
		if w.approvalDesk != nil && w.approvalInbox != nil {
			autoMergeOpts.ApprovalDesk = newSelfMergeDeskHook(w.approvalDesk, w.approvalInbox, w.cfg, w.logger)
		}
		automerge.StartSelfAuthoredAutoMergeSweep(w.ctx, w.ghClient, w.cfg.AutoMerge.MaxMerges, w.cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(w.cfg.ACMMLevel), w.cfg.ACMMLevel, autoMergeOpts)
	}

}
