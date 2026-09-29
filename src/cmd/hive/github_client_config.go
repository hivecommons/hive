package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
)

// configureGitHubClient installs EVERY hook, policy gate and setting the hive
// puts on a *github.Client (hivecommons/hive#9614). It is the one place a
// client is configured: boot calls it on the client initGitHubAuth built, and
// every rebuild path (the dashboard's ReinitGitHubFunc, the config watcher's
// App identity change, and the heartbeat's App credential delivery) builds
// its replacement through newConfiguredGitHubAppClient, which calls it too.
//
// Before this existed the rebuild paths each carried their own copy of the
// setter list, and every copy was shorter than boot's: after a routine
// credential re-delivery the PR repo policy gate, the self-authorization hold
// predicate, the hive identity, signed commits, PR prechecks, attribution,
// merge re-engage, the canary scanner and the dashboard sinks were silently
// gone until the pod restarted. #6203 and #6204 each patched one line of that
// gap; TestGitHubClientSetHooksAllConfigured now fails CI when a new Set*
// hook on *github.Client is not reachable from here (or explicitly
// allowlisted), so the gap cannot reopen one hook at a time.
//
// Boot brings the hooks' dependencies up in phases (the agent manager in
// bootAgents, the dashboard server in bootDashboard, the mutation boundary in
// bootAdvisory), so each dependency tier below is its own function and skips
// itself while its dependency is still nil. Boot phases call the tier that
// just became ready; a rebuild, which happens after boot has finished, calls
// this and gets all of them. Every hook reads its inputs through b (b.cfg,
// b.agentMgr, b.dashSrv) at invocation time, never through b.ghClient, so a
// hook installed on a rebuilt client never reaches back to the old one.
//
// Idempotent: every setter replaces, none appends, so a second call leaves
// the client in the same state as the first.
func (b *boot) configureGitHubClient(client *github.Client) {
	if b == nil || client == nil || b.cfg == nil {
		return
	}
	b.applyGitHubClientConfigHooks(client)
	b.applyGitHubClientAppPolicyHooks(client)
	b.applyGitHubClientMutationBoundary(client)
	b.applyGitHubClientAgentHooks(client)
	b.applyGitHubClientDashboardHooks(client)
}

// newConfiguredGitHubAppClient is the rebuild paths' constructor: the same
// factory initGitHubAuth uses at boot, followed by configureGitHubClient.
// Routing all three rebuild sites through it (pinned by
// TestGitHubClientRebuildSitesUseSharedConstructor) is what keeps a rebuilt
// client from starting life with fewer policy hooks than the one it replaces.
func (b *boot) newConfiguredGitHubAppClient(auth *github.AppAuth) *github.Client {
	client := github.NewClientFromAppWithBotLogin(auth, b.cfg.Project.Org, b.cfg.Project.Repos, b.logger, b.cfg.GitHub.BotLogin())
	b.configureGitHubClient(client)
	return client
}

// githubHoldLabels is the hold-label set every client carries. Normally the
// canonical per-hive hold label; after a failed hold-label migration (see
// runLoopWith) the legacy provenance spelling is kept as a hold too, fail
// closed, and a client rebuild must keep it rather than silently releasing
// that work.
func (b *boot) githubHoldLabels() []string {
	if fallback := b.holdLabelFallback.Load(); fallback != nil && len(*fallback) > 0 {
		return append([]string(nil), (*fallback)...)
	}
	return []string{github.CanonicalHiveHoldLabel(b.cfg.HiveID)}
}

// applyGitHubClientConfigHooks installs the settings that depend on config
// alone. All of them are applied on every client regardless of auth mode.
func (b *boot) applyGitHubClientConfigHooks(client *github.Client) {
	if client == nil || b.cfg == nil {
		return
	}
	client.SetHoldLabels(b.githubHoldLabels())
	// Per-repo pause (#6203). A live predicate over the shared config, so a
	// pause taken in the dashboard narrows the very next enumeration and
	// automerge sweep without a restart.
	client.SetRepoPausedFunc(b.cfg.IsRepoPaused)
	// Per-repo custom agents (#6204). A live predicate over the shared
	// config, so the hive-open-pr / hive-merge / hive-open-issue relays
	// refuse an out-of-scope request without a restart.
	client.SetAgentRepoScopeFunc(b.cfg.AgentServesRepo)
	if len(b.cfg.Governor.Labels.Exempt) > 0 {
		client.SetExemptLabels(b.cfg.Governor.Labels.Exempt)
		client.SetAutoMergeLabel(normalizedAutoMergeLabel(b.cfg.Governor.Labels.AutoMerge))
	}
	// Unconditional (nil-safe, zero value = no filtering): the issue filter
	// gates which issues become actionable at all, so it must be installed
	// even when no exempt labels are configured.
	client.SetIssueFilter(b.cfg.Project.IssueFilter)
	installReviewBots(client, b.cfg, b.logger)
	installReviewRelaySettings(client, b.cfg, b.logger)
	syncAutoMergePolicyToGitHubClient(b.cfg, client)
	// Invocation-attribution trail (pkg/github/attribution.go): the trailer
	// gate reads the live cfg pointer (the config watcher swaps contents in
	// place), so a dashboard flip takes effect on the next creation. The
	// per-agent resolver and the audit sink are installed by the agent and
	// dashboard tiers below, once those dependencies exist.
	client.SetAttributionHooks(github.AttributionHooks{
		TrailerEnabled: func() bool { return b.cfg.Governor.AttributionTrailerEnabled() },
	})
}

// applyGitHubClientAppPolicyHooks installs the App-only write policy: who
// counts as the hive, whether PRs may be opened on a repo at all, signed
// commits and PR prechecks. Gated on a usable App exactly as boot always gated
// them: these govern the App-authored relay paths, which never run without
// one.
func (b *boot) applyGitHubClientAppPolicyHooks(client *github.Client) {
	if client == nil || b.cfg == nil || !b.cfg.GitHub.HasUsableApp() {
		return
	}
	// #5117: tell the client which accounts are ours, so the
	// self-authorization gate recognises an issue filed under
	// project.ai_author's plain user account as hive-filed rather than
	// mistaking it for a human's. The App bot is recognised without this;
	// hiveIdentity() is the same resolver the duplicate-PR guard uses.
	client.SetHiveIdentity(hiveIdentity(b.cfg))
	client.SetPRRepoPolicyGate(b.prRepoPolicyGate)
	// github.app_signed_commits: re-author each agent branch through
	// createCommitOnBranch before the PR opens, so its commit is
	// GitHub-signed and authored by the App bot. Read through a func so a
	// config reload takes effect on the next request.
	client.SetSignedCommits(func() bool { return b.cfg.GitHub.AppSignedCommitsEnabled() })
	prPrecheckDataRoot := filepath.Dir(b.cfg.Data.MetricsDir)
	client.SetPRPrecheckOptions(&github.PRPrecheckOptions{
		DocsEnabled:    func() bool { return b.cfg.GitHub.PRPrecheck.DocsEnabled() },
		GoTestsEnabled: func() bool { return b.cfg.GitHub.PRPrecheck.GoTestsEnabled() },
		Timeout:        func() time.Duration { return b.cfg.GitHub.PRPrecheck.EffectiveTimeout() },
		MaxConcurrent:  func() int { return b.cfg.GitHub.PRPrecheck.EffectiveMaxConcurrent() },
		CacheDir:       b.cfg.GitHub.PRPrecheck.EffectiveCacheDir(prPrecheckDataRoot),
		WorkRoot:       filepath.Join(prPrecheckDataRoot, "pr-precheck", "checkouts"),
		CloneBaseURL:   b.cfg.GitHub.ResolvedBaseURL(),
	})
}

// prRepoPolicyGate refuses a PR request when the repo's effective ACMM level
// does not let the requesting agent push. Read through b.cfg on every call so
// a per-repo level change takes effect without a restart.
func (b *boot) prRepoPolicyGate(agentName, repo string) error {
	level := b.cfg.EffectiveACMMLevelForRepo(repo)
	if level <= 0 {
		level = b.cfg.ACMMLevelOrZero()
	}
	if !agent.DefaultAgentMode(agentName, level).CanPush() {
		return fmt.Errorf("repo %s effective ACMM L%d does not allow %s to open PRs", repo, level, agentName)
	}
	return nil
}

// selfAuthorizationHoldEnabled reports whether the self-authorization hold
// applies to repo at the hive's current ACMM level. Shared by the client hook
// and the self-authored auto-merge sweep so the two cannot disagree.
func (b *boot) selfAuthorizationHoldEnabled(repo string) bool {
	level := b.agentMgr.GetACMMLevel()
	return b.cfg.SelfAuthorizationHoldEnabledForRepoAtLevel(repo, level)
}

// applyGitHubClientMutationBoundary installs the external-mutation fencing
// boundary once bootAdvisory has opened it. A hive whose ledger failed to
// open runs without one, as it always has.
func (b *boot) applyGitHubClientMutationBoundary(client *github.Client) {
	if client == nil || b.mutationBoundary == nil {
		return
	}
	client.SetMutationBoundary(b.mutationBoundary)
}

// applyGitHubClientAgentHooks installs the hooks that consult the agent
// manager. App-only for the same reason as applyGitHubClientAppPolicyHooks.
func (b *boot) applyGitHubClientAgentHooks(client *github.Client) {
	if client == nil || b.cfg == nil || b.agentMgr == nil || !b.cfg.GitHub.HasUsableApp() {
		return
	}
	// Attribution resolver: effective backend/model from the manager
	// (runtime overrides included), falling back to the configured values
	// for an agent the manager does not know; tool version resolved
	// lazily per backend and cached. Only launch descriptors flow here —
	// never tokens, keys, or prompt content.
	client.SetAttributionResolver(func(agentName string) github.InvocationMeta {
		backend, model, effort, known := b.agentMgr.InvocationMetadata(agentName)
		if !known {
			if ac, inCfg := b.cfg.Agents[agentName]; inCfg {
				backend, model = ac.Backend, ac.Model
				// Same resolver the Manager uses, not a second copy of the
				// rule: a hardcoded default here would drift silently the
				// moment agy's default effort changed.
				effort = agent.ResolveReasoningEffort(backend, model, ac.ReasoningEffort)
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
	client.SetSelfAuthorizationHoldEnabled(b.selfAuthorizationHoldEnabled)
	// Fix #2: on a terminal merge failure caused by a failing REQUIRED check,
	// re-engage the fix loop instead of abandoning the PR. The hook records a
	// re-engagement under the escalation store's per-red-SHA cap (shared with
	// the reaper so a PR is never double-dispatched beyond its budget) and
	// returns whether the cap still allowed a dispatch. The PR is already
	// surfaced into CI_FAILING by writeMergeEligible each eval tick; the hook
	// is the loop-safety authority that decides when to STOP nudging.
	client.SetMergeReEngageHook(mergeReEngageHook(b.cfg, agentKicker{mgr: b.agentMgr},
		agentAvailability(b.cfg, b.agentMgr), b.logger))
}

// applyGitHubClientDashboardHooks installs the sinks that report into the
// dashboard server: merge-failure alerts, the creation audit trail, the
// PR-opened notification and the ioscan canary scanner (whose leak handler
// records to the audit log).
func (b *boot) applyGitHubClientDashboardHooks(client *github.Client) {
	if client == nil || b.cfg == nil || b.dashSrv == nil {
		return
	}
	client.SetMergeFailureAlertSink(b.dashSrv)
	// Audit sink for the attribution trail. Every hive-created PR/issue is
	// recorded here whether or not the trailer toggle is on. The same stream
	// feeds the lifecycle timeline: agent_pr_created → pr_opened and
	// pr_merged → merged, see recordLifecycleFromAudit.
	client.SetAttributionAudit(func(action, detail, agentName string) {
		b.dashSrv.AuditLog("system", action, detail, agentName)
		recordLifecycleFromAudit(b.dashSrv, b.cfg.Project.Org, action, detail, agentName)
	})
	client.SetPROpenedHook(func(agentName, repo string, number int, url string) {
		b.dashSrv.LinearAgentPROpened(agentName, repo, number, url)
		// Same typed hook feeds the lifecycle timeline: the watcher fires
		// it on the exact path that opened the PR, with the agent name the
		// audit stream attributes to the governor flow (#5656). The store
		// dedupes with the audit-sink bridge by (ref, kind).
		recordPROpened(b.dashSrv, b.cfg.Project.Org, agentName, repo, number, url)
	})
	client.SetCanaryScanner(b.cfg.Ioscan.IsEnabled() && b.cfg.Ioscan.CanariesEnabled(), b.cfg.Ioscan.FailClosedAtLevel(b.cfg.ACMMLevelOrZero()), ioscan.DefaultCanaries, b.handleCanaryLeak)
}

// handleCanaryLeak records an ioscan canary leak on the audit log and as a
// critical advisory bead for the leaking agent. Shared by the GitHub client
// and the GitHub proxy scanners.
func (b *boot) handleCanaryLeak(leak ioscan.CanaryLeak) {
	detail := fmt.Sprintf("rule=%s, agent=%s, source=%s", ioscan.CanaryLeakRule, leak.Agent, leak.Source)
	b.dashSrv.AuditLog(leak.Agent, "ioscan_canary_leak", detail, leak.Agent)
	if store, ok := b.beadStores[leak.Agent]; ok && store != nil {
		if bead, berr := store.Create("Canary token leaked via "+leak.Source, beads.TypeAdvisory, beads.PriorityCritical, leak.Agent, ""); berr == nil {
			_ = store.SetMetadata(bead.ID, "rule", ioscan.CanaryLeakRule)
			_ = store.SetMetadata(bead.ID, "source", leak.Source)
		}
	}
}
