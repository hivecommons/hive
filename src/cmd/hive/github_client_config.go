package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/mergelane"
	"github.com/hivecommons/hive/pkg/upstreamwatch"
	"github.com/hivecommons/hive/pkg/writeback"
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
// predicate, the hive identity, signed commits, attribution,
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
	b.applyGitHubClientForgeState(client)
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

// githubHoldLabels is the hold-label set every client carries: the canonical
// per-hive hold label (hive-pause/<id>), the configured sentinel alert label,
// plus the clanker-requested label while reporter_trust.clanker_requested is
// on. The legacy hive/<id> spelling is deliberately NOT part of it: that label
// was the provenance label on every item the hive claimed (retired in
// #12539; the boot sweep strips leftovers), and github.HasHoldLabelWith never
// treats it as a hold (#9371), so carrying it here could hold nothing and
// treating it as a hold would park every claimed item.
func (b *boot) githubHoldLabels() []string {
	labels := []string{github.CanonicalHiveHoldLabel(b.cfg.HiveID), b.cfg.Sentinel.LabelOrDefault()}
	return append(labels, b.cfg.Project.IssueFilter.ReporterTrust.ExtraHoldLabels()...)
}

// openPRLinkGuardTimeout bounds the live open-PR listing behind the
// contribute lane's pre-dispatch guard (hivecommons/hive#14799) so a slow
// forge cannot stall candidate selection; on timeout the guard fails open.
const openPRLinkGuardTimeout = 15 * time.Second

// applyGitHubClientConfigHooks installs the settings that depend on config
// alone. All of them are applied on every client regardless of auth mode.
func (b *boot) applyGitHubClientConfigHooks(client *github.Client) {
	if client == nil || b.cfg == nil {
		return
	}
	// Use the same live author-kind names as the PR stage reconciler, also
	// after an App client rebuild or config reload.
	client.SetAuthorKindLabels(func() authorkind.Names { return b.cfg.Governor.Labels.Author.Names() })
	client.SetLinearScopeTeams(b.cfg.Governor.WorkSource.Linear.Teams)
	client.SetHoldLabels(b.githubHoldLabels())
	// Umbrella issues no automated claim may take and no claim stages
	// (governor.claims.never_claim_labels, default `epic`).
	client.SetNeverClaimLabels(b.cfg.Governor.Claims.EffectiveNeverClaimLabels())
	// Pre-dispatch open-PR guard (hivecommons/hive#14799,
	// scanner.skip_issues_with_open_pr, default on).
	client.SetSkipIssuesWithOpenPR(b.cfg.Scanner.IsSkipIssuesWithOpenPR())
	client.SetAdditionalBotLogins(b.cfg.GitHub.AdditionalBotLogins)
	// Every unsolicited comment/label/review the client writes consults the
	// one process-wide write-back gate (pkg/writeback); a rebuilt client
	// must inherit it or the sweeps regress to ungated.
	client.SetWritebackGate(b.writebackGate())
	client.SetSentinelAlertLabel(b.cfg.Sentinel.LabelOrDefault())
	client.SetAPIBudgetThresholds(b.cfg.GitHub.APIReserve, b.cfg.GitHub.APICritical)
	// Per-repo pause (#6203). A live predicate over the shared config, so a
	// pause taken in the dashboard narrows the very next enumeration and
	// automerge sweep without a restart.
	client.SetRepoPausedFunc(b.cfg.IsRepoPaused)
	// Red-main circuit breaker threshold (#13630), live per repo.
	client.SetRedMainThresholdFunc(b.cfg.RepoRedMainThreshold)
	// Open-held-PR cap (#13645), live per repo.
	client.SetMaxOpenHeldPRsFunc(b.cfg.RepoMaxOpenHeldPRs)
	// Per-repo custom agents (#6204). A live predicate over the shared
	// config, so the hive-open-pr / hive-merge / hive-open-issue relays
	// refuse an out-of-scope request without a restart.
	client.SetAgentRepoScopeFunc(b.cfg.AgentServesRepo)
	// Lane write allowlist (#9587), same live-config contract as the two
	// predicates above.
	client.SetWriteAllowlistFunc(b.cfg.AgentMayWrite)
	// Per-lane mention sanitizing for relay-posted bodies (#9587), same
	// live-config contract.
	client.SetMentionNeutralizeFunc(b.cfg.WriteSurfaceNeutralizesMentions)
	client.SetPRDetailTTLFunc(b.cfg.GitHubPRDetailTTL)
	client.SetGraphQLPRBatchConfig(
		b.cfg.GitHubGraphQLPRBatchEnabled,
		b.cfg.GitHubGraphQLPRBatchPageSize,
	)
	if len(b.cfg.Governor.Labels.Exempt) > 0 {
		client.SetExemptLabels(b.cfg.Governor.Labels.Exempt)
		client.SetAutoMergeLabel(normalizedAutoMergeLabel(b.cfg.Governor.Labels.AutoMerge))
	}
	// Unconditional (nil-safe, zero value = no filtering): the issue filter
	// gates which issues become actionable at all, so it must be installed
	// even when no exempt labels are configured.
	client.SetIssueFilter(b.cfg.Project.IssueFilter)
	b.applyPriorArtFilingGate(client)
	client.SetAgentIssueDecisionPolicy(b.cfg.Project.EffectiveAgentIssueDecisionPolicy())
	client.SetReporterConfirmationEnabledFunc(func() bool {
		return b.cfg.Issues.ReporterConfirmationEnabled()
	})
	// Hive-authored superseded PR auto-close (hivecommons/hive#14965):
	// governor.auto_close_superseded gates it, the repo's ACMM level must
	// allow own-PR closes.
	client.SetSupersededAutoClosePolicy(func(repo string) (bool, int) {
		return b.cfg.Governor.AutoCloseSupersededEnabled(), b.cfg.EffectiveACMMLevelForRepo(repo)
	})
	// Relay provenance for the clanker-requested PR parking (#10781). The
	// ledger is read per call because the client can be configured before
	// it is built.
	client.SetRelayContributor(b.relayContributor)
	// Ledger fallback for the enumeration-time claim read and the
	// in-progress sweep: a ledger claim whose 🔒 comment was budget-suppressed
	// must still read as a claim (claimswire.go, ledgerClaimLookup).
	client.SetIssueClaimLedger(ledgerClaimLookup(func() *claims.Ledger { return b.issueClaims }))
	// Duplicate-PR claim ledger fallback for the out-of-cycle stage facts:
	// a claim release on an issue whose covering PR the ledger already holds
	// re-derives 040, not 010 (stage_facts_sink.go, ledgerCoveringPR).
	client.SetPRClaimLookup(b.prClaimCoverLookup())
	// A marker this hive wrote is live only while its ledger still holds the
	// claim (hivecommons/hive#13579, issue_claims.go). The ledger itself is
	// built after this client (main.go), so the hive is resolved per call
	// and is empty — guard off — until the ledger exists.
	client.SetIssueClaimLedgerHive(func() string {
		if b.issueClaims == nil {
			return ""
		}
		return b.cfg.HiveID
	})
	installReviewBots(client, b.cfg, b.logger)
	installReviewRelaySettings(client, b.cfg, b.logger)
	// The duplicate-fix sweep tells a registered relay's PR from a
	// person's by the same registry the stage engine uses
	// (hivecommons/hive#15469).
	client.SetRelayKnownFunc(contributorRelayKnown)
	syncAutoMergePolicyToGitHubClient(b.cfg, client)
	// Merge strategy (#10889): the self-merge sweep, the lgtm queue sweep and
	// the hive-merge relay read the live per-repo strategy from config alone;
	// only hive-serialized repos reach the lane gate.
	client.SetSerializedLane(b.cfg.RepoMergeStrategy, b.serializedLaneGate)
	// The central stage engine (ADR-0025): built here, once per client, so
	// both sweeps find the same reconciler (stage_engine.go).
	stageEngineReconciler(b.cfg, client, b.logger)
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
// commits. Gated on a usable App exactly as boot always gated
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
	if a := strings.TrimSpace(b.cfg.Project.AIAuthor); a != "" && !strings.HasSuffix(a, "[bot]") && strings.TrimSpace(b.cfg.GitHub.Token) == "" {
		b.logger.Warn("config: project.ai_author names a user account on an App-authenticated hive; /hive commands from that account are ignored. Clear ai_author to author as the App bot (EffectiveAIAuthor).", "ai_author", a)
	}
	client.SetPRRepoPolicyGate(b.prRepoPolicyGate)
	// github.app_signed_commits: re-author each agent branch through
	// createCommitOnBranch before the PR opens, so its commit is
	// GitHub-signed and authored by the App bot. Read through a func so a
	// config reload takes effect on the next request.
	client.SetSignedCommits(b.cfg.GitHubAppSignedCommitsEnabled)
}

// prRepoPolicyGate refuses a PR request when the repo's effective ACMM level
// does not let the requesting agent push. Read through b.cfg on every call so
// a per-repo level change takes effect without a restart.
func (b *boot) prRepoPolicyGate(agentName, repo string) error {
	level := b.cfg.EffectiveACMMLevelForRepo(repo)
	if level <= 0 {
		level = b.cfg.ACMMLevelOrZero()
	}
	if !crux.DefaultAgentMode(agentName, level).CanPush() {
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

// reporterTrustHoldEnabled is the live per-repo switch for the #9665
// reporter-trust hold. Deliberately level-independent: see
// GitHubConfig.ReporterTrustHoldEnabled.
func (b *boot) reporterTrustHoldEnabled(repo string) bool {
	return b.cfg.ReporterTrustHoldEnabledForRepo(repo)
}

// reporterTrusted is the live reporter-trust predicate shared by admission and
// the PR-side hold.
func (b *boot) reporterTrusted(login, association string) bool {
	return b.cfg.Project.IssueFilter.ReporterTrust.Trusted(login, association)
}

// relayContributor reports whether login holds a live contributor (relay)
// claim, so its PRs count as relay-originated and are never parked as
// clanker-requested.
func (b *boot) relayContributor(login string) bool {
	return ledgerHasContributor(b.issueClaims, login)
}

func ledgerHasContributor(ledger *claims.Ledger, login string) bool {
	login = strings.TrimSpace(login)
	if ledger == nil || login == "" {
		return false
	}
	for _, c := range ledger.List() {
		if c.Kind == claims.KindContributor && strings.EqualFold(strings.TrimSpace(c.Holder), login) {
			return true
		}
	}
	return false
}

// serializedLaneGate is the lane gate installed on every client (#10889).
// The lane is one per process over the durable front record and is built on
// the first request from a hive-serialized repository, so a hive with every
// repo on direct never opens it. A lane that cannot be built refuses every
// merge into hive-serialized repos rather than merging directly.
func (b *boot) serializedLaneGate(ctx context.Context, req github.LaneMergeRequest) (github.LaneMergeResult, error) {
	b.mergeLaneOnce.Do(func() {
		store, err := mergelane.OpenStore(mergelane.DefaultStorePath)
		if err != nil {
			b.mergeLaneErr = err
			return
		}
		b.mergeLane, b.mergeLaneErr = mergelane.New(mergelane.Options{
			Store: store,
			GitHub: &mergelane.RESTGitHub{
				Client:               b.currentGitHubClient,
				ConfigRequiredChecks: func() (map[string]bool, bool) { return b.cfg.AutoMerge.RequiredCheckSet() },
			},
			TimeoutFor:                 b.cfg.RepoMergeFrontTimeout,
			Strategy:                   b.cfg.RepoMergeStrategy,
			AutoMergeAllowed:           b.cfg.RepoAutoMergeEnabled,
			Paused:                     b.cfg.IsRepoPaused,
			ContributorBaseSyncAllowed: func(string) bool { return b.cfg.Review.ContributorPRs.BaseSync },
			Logger:                     b.logger,
		})
		if b.mergeLaneErr != nil && b.logger != nil {
			b.logger.Error("serialized lane unavailable: hive-serialized repos get no merge", "error", b.mergeLaneErr)
		}
	})
	if b.mergeLane == nil {
		reason := github.ReasonLaneUnavailable
		if b.mergeLaneErr != nil {
			reason += ": " + b.mergeLaneErr.Error()
		}
		return github.LaneMergeResult{Outcome: github.LaneOutcomeRefused, Reason: reason}, nil
	}
	return b.mergeLane.Merge(ctx, req)
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
				effort = cruxconfig.ResolveReasoningEffort(backend, model, ac.ReasoningEffort)
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
	// #11313 label-driven approval: the same live resolver the un-park sweep
	// uses, so a trusted acceptance label counts as sign-off for #5117 on
	// exactly the repos where it releases parked issues.
	client.SetLabelApprovalPolicy(labelApprovalPolicyResolver(b.cfg))
	// #9665 reporter trust: both seams read live config, so a dashboard edit
	// takes effect on the next PR request without a client rebuild. The
	// trust predicate is the SAME one enumeration uses for admission
	// (project.issue_filter.reporter_trust), so the two halves cannot drift.
	client.SetReporterTrustHoldEnabled(b.reporterTrustHoldEnabled)
	client.SetReporterTrusted(b.reporterTrusted)
	client.SetReporterTrustUpstream(func(ctx context.Context, repo string) (string, error) {
		if !b.cfg.UpstreamWatch.Enabled {
			return "", nil
		}
		owner, name, ok := upstreamWatchFork(b.cfg.Project.Org, repo)
		if !ok {
			return "", nil
		}
		for key, rc := range b.cfg.UpstreamWatch.Repos {
			forkOwner, forkName, valid := upstreamWatchFork(b.cfg.Project.Org, key)
			if valid && strings.EqualFold(owner+"/"+name, forkOwner+"/"+forkName) {
				return upstreamwatch.NewGitHubSource(client.GoGitHub(), owner, name, rc).Upstream(ctx)
			}
		}
		return "", nil
	})
	client.SetOtherNeedsHumanReason(func(repo string, number int) bool {
		return getEscalationStore().IsEscalated(repo, number)
	})
	client.SetReporterTrustEscalation(getEscalationStore().SetReporterTrustReason)
	// Fix #2: on a terminal merge failure caused by a failing REQUIRED check,
	// re-engage the fix loop instead of abandoning the PR. The hook records a
	// re-engagement under the escalation store's per-red-SHA cap (shared with
	// the reaper so a PR is never double-dispatched beyond its budget) and
	// returns whether the cap still allowed a dispatch. The PR is already
	// surfaced into CI_FAILING by writeMergeEligible each eval tick; the hook
	// is the loop-safety authority that decides when to STOP nudging.
	client.SetMergeReEngageHook(mergeReEngageHook(b.cfg, agentKicker{mgr: b.agentMgr},
		agentAvailability(b.cfg, b.agentMgr), b.logger))
	// #13085: the merge-request relay honours the review swarm's verdict hold
	// the self-merge sweep applies (#12374), released only by a trusted
	// merger's approving review or `/hive approve` reply.
	client.SetMergeReviewVerdictGate(reviewVerdictHoldFunc(b.logger), trustedMergerFunc(b.cfg))
	// Merge-request CIWaits/attempts, poll backoff and "exhausted on this
	// SHA" records live on the PVC so a restart cannot reset them (#11894).
	client.SetMergeRequestStatePath(github.MergeRequestStatePath)
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
	// Typed audit sink (#9587) for the attribution trail: repo and target land
	// as first-class audit fields alongside the legacy repo=/number= detail
	// pairs. Every hive-created PR/issue is recorded here whether or not the
	// trailer toggle is on. The same stream feeds the lifecycle timeline:
	// agent_pr_created → pr_opened and pr_merged → merged, see
	// recordLifecycleFromAudit.
	client.SetAttributionAuditRecord(func(rec github.AuditRecord) {
		b.dashSrv.AuditLogRecord("system", rec.Action, rec.Detail, rec.Agent, rec.Repo, rec.Target)
		recordLifecycleFromAudit(b.dashSrv, b.cfg.Project.Org, rec)
		b.dashSrv.ApplyPRTerminalAudit(rec)
		// #10927/#10933: the agent card's "what is it on" and stale warning
		// read each agent's last hive-mediated write.
		dashboard.RecordAgentLastAction(rec, time.Now())
		// A PR the hive merged or closed itself leaves the tracker and the
		// governor's cached snapshot now, instead of waiting for GitHub's
		// `closed` delivery to come back round through the hub.
		evictHiveClosedPR(b.dashSrv, rec, time.Now())
	})
	client.SetPRTerminalObservedHook(func(obs github.PRTerminalObservation) {
		recordPRTerminalObservedAudit(b.dashSrv, b.cfg.Project.Org, obs)
		if obs.State != github.PRStateMerged {
			return
		}
		path := obs.Attribution
		if path == "" {
			path = "unknown"
		}
		// #12007: link the PR's Fixes issues first (real created_at /
		// merged_at), so issues started before issue-side recording
		// existed close instead of lingering as stale in-progress.
		backfillMergedPROnIssues(b.dashSrv, b.cfg.Project.Org, obs)
		reworkCommits, reworkObserved := 0, false
		// Only PRs the hive already journeys are measured, which keeps
		// the extra GitHub reads off merges the hive never worked.
		if store := b.dashSrv.LifecycleTimeline(); store != nil && hasJourney(store, timelineRef(b.cfg.Project.Org, obs.Repo, obs.Number)) {
			reworkCommits, reworkObserved = client.MergedPRReworkCommits(b.ctx, obs.Repo, obs.Number, obs.ObservedAt)
		}
		recordObservedMerge(b.dashSrv, b.cfg.Project.Org, obs.Repo, obs.Number, obs.ObservedAt, obs.Actor, path, reworkCommits, reworkObserved)
	})
	client.SetPROpenedHook(func(agentName, repo string, number int, url string) {
		b.dashSrv.LinearAgentPROpened(agentName, repo, number, url)
		// Same typed hook feeds the lifecycle timeline: the watcher fires
		// it on the exact path that opened the PR, with the agent name the
		// audit stream attributes to the governor flow (#5656). The store
		// dedupes with the audit-sink bridge by (ref, kind).
		recordPROpened(b.dashSrv, b.cfg.Project.Org, agentName, repo, number, url)
	})
	// #10527: an agent's own claim request, explicit start comment or PR
	// request on an issue a kick listed to it is its start signal; that is
	// when the agent claim (and its 🔒 comment) is recorded. The ledger is read per call because the
	// client can be configured before it is built.
	gfiRelease := gfiReleaseOnHiveStart(b.ctx, b.cfg, b.gfiState(), func() *github.Client { return b.ghClient }, b.dashSrv, b.logger)
	client.SetAgentStartHook(func(agentName, repo string, issue int, signal string) {
		// A hive agent starting on an advertised good-first-issue withdraws
		// the advertisement (label off, welcome comment superseded).
		gfiRelease(agentName, repo, issue, signal)
		recordAgentStart(b.issueClaims, b.cfg.Project.Org, agentName, repo, issue, signal, b.logger)
		// The same signal is the journey's "started" stage: the instant the
		// hive/in-progress label states, and the kick→start / start→PR
		// boundary the per-repo SLA metrics read. Recorded whether or not the
		// claims ledger is on.
		recordStarted(b.dashSrv, b.cfg.Project.Org, agentName, repo, issue, signal)
	})
	// hivecommons/hive#14705: a recorded dispatched verdict frees its head,
	// so the governor re-plans now instead of at the next eval cycle.
	client.SetReviewVerdictHook(b.wakeForReviewVerdict)
	// PR follow-up session resume (#9583, default off): remember which live
	// CLI session authored this PR, and the PR's handoff note, so its
	// follow-ups can resume that conversation or brief a fresh one. The
	// manager may be nil before bootAgents; the pointer is then session-less.
	client.SetPROpenedDetailHook(func(d github.PROpenedDetail) {
		var sessions prFollowUpSessions
		if b.agentMgr != nil {
			sessions = b.agentMgr
		}
		recordPRFollowUpPointer(b.cfg, sessions, d, time.Now(), b.logger)
		// The issues the PR fixes get their own pr_opened stage: the
		// start→PR SLA boundary, and the link a later merge folds back on.
		recordPROpenedForIssues(b.dashSrv, b.cfg.Project.Org, d.Agent, d.Repo, d.Number, d.URL, d.Issues)
		// The duplicate-PR claim ledger learns about the PR now, not at the
		// next scan, so a contributor cannot be admitted to an issue this
		// PR already covers in the gap between (hivecommons/hive#14472).
		client.RecordOpenedPRClaims(getClaimLedger(b.logger), d, hiveIdentity(b.cfg), time.Now())
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

// holdMigrationFailedAlertID is the dashboard system alert raised when the
// startup hive hold-label migration fails (see runLoopWith).
const holdMigrationFailedAlertID = "hive-hold-migration-failed"

// holdMigrationFailedAlertMessage explains a failed hold-label migration to the
// operator: items still carrying only the legacy hive/<id> label are not held,
// because that label is provenance and is never matched as a hold (#9371).
func holdMigrationFailedAlertMessage(hiveID, reportPath string) string {
	msg := "Hive hold-label migration failed: items still carrying only the legacy " +
		github.HiveProvenanceLabel(hiveID) + " label are NOT treated as held (it is the provenance label, #9371). " +
		"Re-apply " + github.CanonicalHiveHoldLabel(hiveID) + " to anything that must stay held, or restart to retry the migration."
	if reportPath != "" {
		msg += " Migration report: " + reportPath
	}
	return msg
}

// prTerminalAuditRecorder is the dashboard audit write the PR poller's
// terminal observations go through.
type prTerminalAuditRecorder interface {
	AuditLogRecordAt(timestamp, user, action, detail, agent, repo string, target int)
}

// recordPRTerminalObservedAudit records a merge or close the PR poller saw.
// The repo is qualified with the org, the form the hive's own merge paths
// record (RecordPRMergedAudit), so the PR throughput card counts a merge the
// hive made and then observed as one PR, not two.
func recordPRTerminalObservedAudit(audit prTerminalAuditRecorder, org string, obs github.PRTerminalObservation) {
	ts := obs.ObservedAt.UTC().Format(time.RFC3339)
	repo := cruxconfig.QualifyRepo(org, obs.Repo)
	path := obs.Attribution
	if path == "" {
		path = "unknown"
	}
	switch obs.State {
	case github.PRStateMerged:
		audit.AuditLogRecordAt(ts, "system", github.AuditActionPRMerged,
			fmt.Sprintf("repo=%s, number=%d, path=%s, actor=%s", repo, obs.Number, path, obs.Actor),
			github.AttributionAgentGovernor, repo, obs.Number)
	case github.PRStateClosed:
		audit.AuditLogRecordAt(ts, "system", github.AuditActionPRClosed,
			fmt.Sprintf("repo=%s, number=%d, path=%s, reason=observed_closed, actor=%s", repo, obs.Number, path, obs.Actor),
			github.AttributionAgentGovernor, repo, obs.Number)
	}
}

// evictHiveClosedPR removes a pull request from the tracker and the cached
// actionable snapshot the moment the hive records having merged or closed it
// (AuditActionPRMerged / AuditActionPRClosed). The audit record is the one
// point every merge path (sweep, queue, relay, forge) passes through, so it
// is where the hive's own write stops depending on the webhook round-trip.
func evictHiveClosedPR(srv *dashboard.Server, rec github.AuditRecord, now time.Time) {
	if srv == nil || rec.Target <= 0 || rec.Repo == "" {
		return
	}
	switch rec.Action {
	case github.AuditActionPRMerged:
		srv.EvictHiveClosedItem(rec.Repo, rec.Target, true, true, rec.Action, now)
	case github.AuditActionPRClosed:
		srv.EvictHiveClosedItem(rec.Repo, rec.Target, true, false, rec.Action, now)
	}
}

// writebackGate returns the process-wide unsolicited write-back gate,
// building it on first use. The gate reads the live config on every
// decision, so dashboard edits of the writeback block apply without a
// rebuild; its first_seen ledger persists under /data so a repo that gains
// write mid-life never back-fills onto PRs that predate the install.
func (b *boot) writebackGate() *writeback.Gate {
	b.writebackOnce.Do(func() {
		deps := b.cfg.WritebackGateDeps()
		deps.Config = func() writeback.Config { return b.cfg.Writeback }
		deps.Logger = b.logger
		b.writeback = writeback.New(b.cfg.Writeback, deps, writebackStatePath())
		for _, repo := range b.cfg.Project.Repos {
			b.writeback.ObserveRepo(repo)
		}
	})
	return b.writeback
}

// writebackStatePath is the gate's persisted first_seen ledger, beside the
// hold-guard ledger so a PVC restore brings both back together.
func writebackStatePath() string {
	if v := strings.TrimSpace(os.Getenv("HIVE_WRITEBACK_STATE_PATH")); v != "" {
		return v
	}
	return "/data/metrics/writeback.json"
}

// observeWritebackRepos records first_seen for every repo now in
// project.repos, so a repo added at runtime starts its backfill clock the
// moment the hive learns of it, not when it first tries to write.
func (b *boot) observeWritebackRepos() {
	g := b.writebackGate()
	for _, repo := range b.cfg.Project.Repos {
		g.ObserveRepo(repo)
	}
}
