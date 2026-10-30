package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/loginscan"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/spokealerts"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/tracing"
	"github.com/hivecommons/hive/pkg/worksource"
	"go.opentelemetry.io/otel/attribute"
)

// buildRepoActivityWire maps the dashboard activity collector's per-repo
// snapshot into the plain hub wire structs the heartbeat carries. Kept here (in
// the one package that imports both hub and dashboard) so pkg/hub never has to
// import pkg/dashboard back — that would be an import cycle, since dashboard
// already imports hub. A field-by-field copy, mirroring how the fleet-stat
// scalars are lifted out of their snapshot at the beat's build site.
func buildRepoActivityWire(repos []collect.RepoActivity) []spoke.RepoActivityWire {
	if len(repos) == 0 {
		return nil
	}
	stat := func(s collect.ActivityActionStat) spoke.ActivityStatWire {
		return spoke.ActivityStatWire{Count: s.Count, NewestAt: s.NewestAt}
	}
	out := make([]spoke.RepoActivityWire, 0, len(repos))
	for _, r := range repos {
		agents := make([]spoke.AgentRepoActivityWire, 0, len(r.Agents))
		for _, a := range r.Agents {
			agents = append(agents, spoke.AgentRepoActivityWire{
				Agent:      a.Agent,
				Issues:     stat(a.Issues),
				PRs:        stat(a.PRs),
				Comments:   stat(a.Comments),
				Merges:     stat(a.Merges),
				Claims:     stat(a.Claims),
				Reviews:    stat(a.Reviews),
				Advisory:   stat(a.Advisory),
				Reconciled: stat(a.Reconciled),
			})
		}
		out = append(out, spoke.RepoActivityWire{
			Repo:       r.Repo,
			Issues:     stat(r.Issues),
			PRs:        stat(r.PRs),
			Comments:   stat(r.Comments),
			Merges:     stat(r.Merges),
			Claims:     stat(r.Claims),
			Reviews:    stat(r.Reviews),
			Advisory:   stat(r.Advisory),
			Reconciled: stat(r.Reconciled),
			Agents:     agents,
		})
	}
	return out
}

// providerBudgetNotify is the one-shot guard for the provider spend-rebuff
// notification (#4294). Package-level because runEvalCycle is a function called
// once per tick with no state of its own; mutex-guarded inside pkg/governor.
var providerBudgetNotify governor.ProviderBudgetNotifyState

// providerBudgetProbe remembers when the last probe kick was released while a
// provider spend rebuff is latched. Package-level for the same reason as
// providerBudgetNotify: runEvalCycle has no state of its own.
var providerBudgetProbe governor.ProviderBudgetProbeState

// governorKickFairness remembers, per agent, how many cycles in a row the
// governor kick was deferred behind a delivered review-fix kick
// (hivecommons/hive#15166). Package-level for the same reason as
// providerBudgetProbe: runEvalCycle has no state of its own.
var governorKickFairness = newKickLaneFairness()

var evalAPIBudgetCycle atomic.Uint64

// applyBudgetAlerts is a thin wrapper around spokealerts.ApplyBudget, kept so
// call sites in this file do not need the package-qualified name (same
// pattern as applyModeUnscheduledAlert below).
func applyBudgetAlerts(gov *governor.Governor, trans governor.BudgetTransitions, dashSrv *dashboard.Server, notifier *notify.Notifier) {
	spokealerts.ApplyBudget(gov, trans, dashSrv, notifier)
}

// applyNoCadenceAlert is a thin wrapper around spokealerts.ApplyNoCadence
// (see that package for the banner's design: #5577).
func applyNoCadenceAlert(gov *governor.Governor, dashSrv *dashboard.Server) {
	spokealerts.ApplyNoCadence(gov, dashSrv)
}

func applyModeUnscheduledAlert(gov *governor.Governor, dashSrv *dashboard.Server) {
	spokealerts.ApplyModeUnscheduled(gov, dashSrv)
}

func activeIssueRefsFromLastActionable(actionable *github.ActionableResult) (map[string]struct{}, map[string]struct{}) {
	if actionable == nil {
		return nil, nil
	}
	refs := make(map[string]struct{})
	coveredRepos := make(map[string]struct{})
	for repo := range actionable.TotalByRepo {
		coveredRepos[repo] = struct{}{}
	}
	for _, issue := range actionable.Issues.Items {
		if ref := planning.IssueRef(issue); ref != "" {
			refs[ref] = struct{}{}
		}
	}
	for _, held := range actionable.Hold.Items {
		if held.Type != "issue" || held.Repo == "" || held.Number <= 0 {
			continue
		}
		ref := planning.IssueRef(github.Issue{Repo: held.Repo, Number: held.Number, Title: held.Title})
		if ref != "" {
			refs[ref] = struct{}{}
		}
	}
	return refs, coveredRepos
}

// agentKicker adapts crux.Runtime to planning.Kicker for the Phase 3
// stall-replan lane. Kick delegates to SendKick, which takes the manager lock
// ITSELF and is only ever called here from the governor tick (never from the
// agent-launch path), so it cannot re-enter a held manager lock. This is the
// same out-of-band kick path the eval loop already uses for governor kicks.
type agentKicker struct{ mgr crux.Runtime }

func (k agentKicker) Kick(agent, message string) error {
	if k.mgr == nil {
		return fmt.Errorf("agent manager unavailable: cannot kick %s", agent)
	}
	return k.mgr.SendKick(agent, message)
}

// planFromLabeledIssues is Phase 4 Part B: for each actionable issue carrying a
// `plan`/`epic` label, mint an epic (idempotent) and hand it to the architect,
// RESPECTING the architect's pause. It is a plain synchronous call on the eval
// tick — no goroutine — and only ever touches the manager via SendKick/IsPaused,
// exactly like every governor kick, so it never re-enters the launch-path mutex.
// Epics are minted into the architect store (falling back to any store) so the
// dashboard plan-review flow and replan lane find them the same way.
//
// A minted epic is decompose_pending (plan_status=draft). While the architect is
// paused, the request stays queued and visible (the PLANNING tile shows it as
// pending) — we never force-unpause. Once the architect is available, we kick it
// each cycle until it decomposes and clears the pending marker.
func planFromLabeledIssues(
	actionable *github.ActionableResult,
	beadStores map[string]*beads.Store,
	agentMgr crux.Runtime,
	gov *governor.Governor,
	dashSrv *dashboard.Server,
	logger *slog.Logger,
	cfg *config.Config,
	acmmLevel int,
) {
	if actionable == nil || len(beadStores) == 0 {
		return
	}
	store, ok := beadStores[planning.ArchitectAgentName]
	if !ok {
		for name := range beadStores {
			store = beadStores[name]
			break
		}
	}
	if store == nil {
		return
	}

	designCfg := planning.DesignConfig{
		PlanLabels:    cfg.Planning.PlanLabelsOrDefault(),
		DesignLabels:  cfg.Planning.DesignLabelsOrDefault(),
		ApprovedLabel: cfg.Planning.DesignApprovedLabelOrDefault(),
		MaxRevisions:  cfg.Planning.MaxDesignRevisionsOrDefault(),
		MaxConcurrent: cfg.Planning.MaxConcurrentDesignsOrDefault(),
	}
	issues := actionable.Issues.Items
	if cfg == nil || !cfg.Planning.PlanFromLabelEnabled(acmmLevel) {
		issues = nil
		for _, issue := range actionable.Issues.Items {
			if store.FindByExternalRef(planning.IssueRef(issue)) != nil {
				issues = append(issues, issue)
			}
		}
	}
	sink := labelPlanSink{gov: gov, dashSrv: dashSrv, logger: logger}
	planning.PlanIssuesFromLabelsWithConfig(store, agentMgr, issues, designCfg, sink,
		func(ref string, err error) {
			logger.Warn("plan-from-label: minting epic failed", "issue", ref, "error", err)
		}, acmmLevel)
	if config.PlanAutoApproveForLevel(acmmLevel) {
		for _, id := range planning.AutoApproveDrafts(store) {
			if dashSrv != nil {
				dashSrv.AuditLog("planning", "plan_auto_approved", "epic="+id, planning.ArchitectAgentName)
			}
			logger.Info("audit: plan auto-approved by ACMM pack", "epic", id, "acmm_level", acmmLevel)
		}
	}
}

// labelPlanSink adapts the governor/dashboard/logger to planning.LabelPlanSink so
// the label-trigger core lives (and is tested) in pkg/planning.
type labelPlanSink struct {
	gov     *governor.Governor
	dashSrv *dashboard.Server
	logger  *slog.Logger
}

func (s labelPlanSink) KickedPlan(epic *beads.Bead) {
	s.gov.RecordKick(planning.ArchitectAgentName)
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "plan_from_label", "epic="+epic.ID+" ref="+epic.ExternalRef, planning.ArchitectAgentName)
	}
	s.logger.Info("audit: plan requested from labeled issue", "epic", epic.ID, "ref", epic.ExternalRef)
}

func (s labelPlanSink) FailedPlan(epic *beads.Bead) {
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "plan_decompose_failed", "epic="+epic.ID+" ref="+epic.ExternalRef+" attempts="+strconv.Itoa(planning.DecomposeMaxAttempts), planning.ArchitectAgentName)
	}
	s.logger.Warn("plan-from-label: architect produced no plan after max attempts — epic marked stuck; re-request it from the dashboard",
		"epic", epic.ID, "ref", epic.ExternalRef, "attempts", planning.DecomposeMaxAttempts)
}

func (s labelPlanSink) QueuedPlan(epic *beads.Bead, paused bool) {
	if paused {
		// Architect deliberately paused — queue, do not unpause, log once.
		s.logger.Info("plan-from-label: architect paused, plan queued", "epic", epic.ID, "ref", epic.ExternalRef)
		return
	}
	s.logger.Warn("plan-from-label: architect unavailable, plan queued", "epic", epic.ID, "ref", epic.ExternalRef)
}

func (s labelPlanSink) KickedDesign(epic *beads.Bead, revision int) {
	s.gov.RecordKick(planning.ArchitectAgentName)
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "design_kicked", "epic="+epic.ID+" ref="+epic.ExternalRef+" revision="+strconv.Itoa(revision), planning.ArchitectAgentName)
	}
	s.logger.Info("audit: design requested from labeled issue", "epic", epic.ID, "ref", epic.ExternalRef, "revision", revision)
}

func (s labelPlanSink) ApprovedDesign(epic *beads.Bead) {
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "design_approved", "epic="+epic.ID+" ref="+epic.ExternalRef, planning.ArchitectAgentName)
	}
	s.logger.Info("audit: design approved from labeled issue", "epic", epic.ID, "ref", epic.ExternalRef)
}

func (s labelPlanSink) DesignNeedsHuman(epic *beads.Bead, revisions int) {
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "design_needs_human", "epic="+epic.ID+" ref="+epic.ExternalRef+" revisions="+strconv.Itoa(revisions), planning.ArchitectAgentName)
	}
	s.logger.Warn("plan-from-label: design revision cap reached — epic needs a human", "epic", epic.ID, "ref", epic.ExternalRef, "revisions", revisions)
}

// maxTimelineEnumeratePerCycle bounds how many enumerated-issue events a single
// eval cycle records into the lifecycle timeline, keeping the recording loop
// O(1)-bounded so it never slows the eval cycle. Since #5656 the store dedupes
// by (ref, kind) — re-enumeration refreshes the journey instead of appending —
// so the cap no longer protects the ring from eviction floods; it matches the
// endpoint's default journey limit so every renderable journey gets refreshed.
const maxTimelineEnumeratePerCycle = 200

// lifecycleRecorder narrows *dashboard.Server to just the timeline accessor the
// recording helpers need, so they stay trivially testable with a fake and never
// depend on the rest of the Server surface.
type lifecycleRecorder interface {
	LifecycleTimeline() *timeline.Store
}

// recordEnumeratedIssues records a KindEnumerated event for each enumerated
// actionable issue, bounded by maxTimelineEnumeratePerCycle. The store dedupes
// by (ref, kind), so each cycle refreshes the journeys' enumerated stage
// rather than appending a flood (#5656). It is fully guarded: a nil recorder,
// nil store, or nil actionable set is a no-op, and Record itself is nil-safe.
// This must never slow or break the eval loop, so it does no blocking I/O
// (journey persistence is throttled and atomic inside the store).
//
// PR-open/merge lifecycle spans are emitted by the same tracing mapper when
// callers record those timeline events; this helper only has enumerated issues.
func recordEnumeratedIssues(ctx context.Context, rec lifecycleRecorder, actionable *github.ActionableResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rec == nil || actionable == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	limit := maxTimelineEnumeratePerCycle
	if len(actionable.Issues.Items) < limit {
		limit = len(actionable.Issues.Items)
	}
	for i := 0; i < limit; i++ {
		issue := actionable.Issues.Items[i]
		event := timeline.Event{
			IssueRef: issueRef(issue.Repo, issue.Number),
			Kind:     timeline.KindEnumerated,
		}
		// The GitHub created_at is the open→triage / open→merge SLA origin;
		// retained here so the SLA never re-fetches the issue.
		if !issue.CreatedAt.IsZero() {
			event.Attrs = map[string]string{timeline.AttrIssueCreatedAt: issue.CreatedAt.UTC().Format(time.RFC3339)}
		}
		_, span := tracing.StartTimelineSpan(ctx, event)
		store.Record(event)
		span.End()
	}
}

// recordKick records KindKicked events for the given agent. When issue refs are
// supplied it records one issue-scoped event per ref so post-completion lanes
// can reconstruct per-bead kick counts. With no refs, it records one
// agent-scoped event. Guarded and nil-safe; no I/O.
func recordKick(ctx context.Context, rec lifecycleRecorder, agent string, issueRefs ...string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rec == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	if len(issueRefs) > 0 {
		for _, ref := range issueRefs {
			if ref == "" {
				continue
			}
			event := timeline.Event{
				IssueRef: ref,
				Kind:     timeline.KindKicked,
				Agent:    agent,
			}
			_, span := tracing.StartTimelineSpan(ctx, event)
			store.Record(event)
			span.End()
		}
		return
	}
	event := timeline.Event{
		Kind:  timeline.KindKicked,
		Agent: agent,
	}
	_, span := tracing.StartTimelineSpan(ctx, event)
	store.Record(event)
	span.End()
}

// issueRef renders the canonical "repo#number" reference the timeline uses to
// group events by issue. An empty repo yields an empty ref (the store tolerates
// it).
func issueRef(repo string, number int) string {
	if repo == "" {
		return ""
	}
	return fmt.Sprintf("%s#%d", repo, number)
}

func actionableIssueRef(issue github.Issue) string {
	ref := worksource.Ref{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: issue.ExternalID,
		Number:     issue.Number,
		URL:        issue.URL,
	}
	if key := ref.Key(); key != "" {
		return key
	}
	if issue.Repo != "" {
		return issue.Repo
	}
	return issue.ExternalID
}

// actionableAfterGitHubEnumerate decides whether an eval cycle survives a
// failed GitHub enumeration. On the default (GitHub) work source the answer
// is no: an all-repos failure usually means a rate limit or outage, and a
// zero-count result would idle the agents, so the cycle keeps prior state.
//
// On a non-default work source (e.g. Linear), or a default GitHub source with
// additive work enabled (run stages / Wavefront), the GitHub issue call is not
// the only backlog authority. Aborting here meant a Linear-sourced hive whose
// GitHub App could not list issues (403 "Resource not accessible by
// integration", an Issues permission a Linear hive should not need) never
// enumerated its Linear backlog at all and sat at queue 0; the same rule keeps
// local additive sources reachable during a transient GitHub issue outage. Such
// a hive continues with whatever partial result GitHub returned (nil becomes
// an empty result; PRs are kept when obtainable) and lets the overlay populate
// issues.
func actionableAfterGitHubEnumerate(cfg *config.Config, actionable *github.ActionableResult, err error, logger *slog.Logger) (*github.ActionableResult, bool) {
	if err == nil {
		return actionable, true
	}
	if !workSourceOverlayEnabled(cfg.Governor.WorkSource) {
		logger.Error("failed to enumerate actionable items", "error", err)
		return nil, false
	}
	logger.Warn("GitHub enumeration failed; continuing so the configured work source can still populate issues",
		"work_source", cfg.Governor.WorkSource.Type, "run_stages", cfg.Governor.WorkSource.RunStages,
		"wavefront", cfg.Governor.WorkSource.Wavefront.Enabled, "error", err)
	if actionable == nil {
		actionable = &github.ActionableResult{GeneratedAt: time.Now()}
	}
	// The GitHub share of this result is missing, not zero: every configured
	// repo is marked stale so the dashboard shows the gap as stale rather
	// than as an empty hive (a cold cache has no last-good to fall back on).
	markConfiguredReposStale(actionable, cfg.Project.Repos)
	return actionable, true
}

// markConfiguredReposStale flags every configured repo the result does not
// already cover as stale, with a zero count entry so the repo still appears.
func markConfiguredReposStale(actionable *github.ActionableResult, repos []string) {
	if actionable == nil {
		return
	}
	if actionable.TotalByRepo == nil {
		actionable.TotalByRepo = map[string]github.RepoCounts{}
	}
	for _, repo := range repos {
		if _, ok := actionable.TotalByRepo[repo]; !ok {
			actionable.TotalByRepo[repo] = github.RepoCounts{}
		}
	}
	actionable.MarkAllStale()
}

func tokensForBackend(summary *tokens.AggregateSummary, backend string) int64 {
	if summary == nil {
		return 0
	}
	var total int64
	for _, sess := range summary.Sessions {
		if strings.EqualFold(sess.Backend, backend) {
			total += sess.TotalTokens
		}
	}
	return total
}

func runEvalCycle(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	gov *governor.Governor,
	sched *scheduler.Scheduler,
	agentMgr crux.Runtime,
	dashSrv *dashboard.Server,
	notifier *notify.Notifier,
	beadStores map[string]*beads.Store,
	tokenCollector *tokens.Collector,
	metricsCollector *dashboard.MetricsCollector,
	nousState *dashboard.NousState,
	lastActionable *atomic.Pointer[github.ActionableResult],
	advisoryStore *advisory.Store,
	advisoryIssues map[string]int,
	restartedAgents []string,
	approvalDesk *toolapprove.Desk,
	source evalCycleSource,
	logger *slog.Logger,
) {
	// Governor eval-cycle span. When tracing is disabled (the default) this is
	// a no-op span with no allocation of note and no export — see pkg/tracing.
	ctx, span := tracing.StartSpan(ctx, "governor.eval_cycle",
		attribute.String("hive.id", cfg.HiveID),
		attribute.Int(tracing.AttrHiveACMMLevel, inferACMMLevel(cfg)))
	defer span.End()

	// A hive running without GitHub credentials (placeholder app_id, or a real
	// App whose key could not be read) has nothing to enumerate. Return before
	// the first API call rather than logging a misleading enumeration failure
	// once per eval interval — the dashboard banner already states the cause.
	if ghClient == nil {
		logger.Debug("skipping eval cycle: hive is running without GitHub credentials")
		return
	}

	budgetMode, _ := ghClient.APIBudgetMode()
	budgetDecision := decideEvalBudgetWork(budgetMode, cfg.Governor.OptionalSweepEveryNCycles, evalAPIBudgetCycle.Add(1))

	// A cached cycle (webhook or re-kick wake) plans from the stored
	// snapshot: no listing, and none of the enrichment or unstick sweeps
	// that walk every item — those belong to the reconcile pass. The first
	// cycle has no snapshot yet and enumerates regardless of source.
	cachedSnapshot := cachedSnapshotFor(source, lastActionable)
	if cachedSnapshot != nil {
		budgetDecision.SkipOptional = true
		budgetDecision.SkipUnstick = true
		budgetDecision.SkippedSteps = append(budgetDecision.SkippedSteps, cachedSnapshotSkippedStep)
	} else if source == evalSourceCached {
		logger.Info("eval cycle: no cached snapshot yet; enumerating")
	}
	ghClient.RecordAPIBudgetSkippedSteps(budgetDecision.SkippedSteps)

	// Re-ensure the pinned advisory issue whenever it is still unresolved, not
	// only while the App banner is up (#4167). The startup ensure can fail for
	// reasons that deliberately do NOT raise that banner — a rate limit, a 5xx,
	// a search-API blip — and the old gate meant such a hive kept an empty
	// advisoryIssues map for the rest of the process lifetime: every later
	// digest silently found no issue to post to, so the pinned comment froze at
	// whatever it last said. Retrying here is cheap (one search per eval cycle
	// only while unresolved, nothing once resolved) and is the difference
	// between a transient boot error and a permanently wedged digest.
	//
	// advisoryEnsureErr keeps this cycle's ensure failure so the post-path
	// error recorded below can name the CAUSE (e.g. Issues disabled on a fork,
	// #4329) instead of only the symptom.
	primaryRepoAtCycleStart := primaryAdvisoryRepo(cfg)
	_, hadPinnedAdvisoryIssueAtCycleStart := advisoryIssueNumber(advisoryIssues, primaryRepoAtCycleStart)
	advisoryEnsureDepsForCycle := advisoryEnsureDeps{setenv: os.Setenv}
	if ghClient != nil {
		advisoryEnsureDepsForCycle.ensure = ghClient.EnsureAdvisoryIssue
	}
	advisoryEnsureErr := error(nil)
	if !budgetDecision.SkipOptional {
		advisoryEnsureErr = ensurePinnedAdvisoryIssue(
			ctx, advisoryIssues, primaryRepoAtCycleStart, advisoryEnsureDepsForCycle, logger)
	}

	var actionable *github.ActionableResult
	if cachedSnapshot != nil {
		actionable = cachedSnapshot
		logger.Info("eval cycle planning from cached snapshot",
			"snapshot_age", time.Since(actionable.GeneratedAt).Round(time.Second).String(),
			"issues", actionable.Issues.Count, "prs", actionable.PRs.Count, "hold", actionable.Hold.Total,
			"stale", actionable.Stale())
	} else {
		enumCtx, enumSpan := tracing.StartSpan(ctx, "governor.enumerate_actionable")
		fresh, err := ghClient.EnumerateActionable(enumCtx)
		enumSpan.End()
		var ok bool
		actionable, ok = actionableAfterGitHubEnumerate(cfg, fresh, err, logger)
		if !ok {
			if kept := keepLastGoodAfterFailedEnumerate(lastActionable, logger); kept != nil && dashSrv != nil {
				dashSrv.MarkStatusStale(kept.StaleSince)
			}
			return
		}

		// If a non-default or additive work source is configured, overlay its
		// issues onto the actionable result. PRs always come from GitHub. A
		// cached cycle keeps the overlay the reconcile pass already applied.
		if workSourceOverlayEnabled(cfg.Governor.WorkSource) {
			actionable.Issues = workSourceIssuesForConfiguredCycle(ctx, cfg, ghClient, actionable.Issues, logger)
		}
	}
	if swarmRepo := dashSrv.ActiveSwarmRepo(); swarmRepo != "" {
		github.BoostActionableRepoPriority(actionable.Issues.Items, swarmRepo)
		logger.Info("swarm priority boost applied", "repo", swarmRepo)
	}

	ghClient.EnrichCIStatus(ctx, actionable.PRs.Items)
	// Held PRs need CI status too, or a red held PR can never be repaired:
	// the hold label kept it out of PRs.Items, so nothing ever learned it was
	// red and no agent was ever told to fix it (hivecommons/hive#7438). This
	// enriches the held list ONLY for the repair path — held PRs still never
	// reach the merge sweep, escalation or the queue counts.
	if budgetMode != github.APIBudgetCritical {
		ghClient.EnrichCIStatus(ctx, actionable.PRs.Held)
	}
	// Stale drafts sit in the same dashboard PR column; they get the
	// review/link signals only — no mergeability or check-run fetches, a
	// draft is not a merge candidate (hivecommons/hive#8968).
	if !budgetDecision.SkipOptional {
		// Sidelined PRs (drafts, exempt labels) sit in the same column and
		// need their closing references to pair with the issue they fix
		// (hivecommons/hive#13421); one shared collector keeps the budget at
		// one query per repo for both.
		ghClient.EnrichReviewSignalsAndWorkPRs(ctx, actionable.PRs.StaleDrafts, actionable.WorkDetailsByRepo)
		// A closing reference whose issue is already closed (a superseded
		// PR's `Fixes #N`) has no open row to borrow a title from; fill it
		// once per issue, from a long-lived memo, so the activity monitor
		// never shows a bare "#N".
		ghClient.EnrichLinkedIssueTitles(ctx, actionable.WorkDetailsByRepo, actionable.PRs.Items, actionable.PRs.Held, actionable.PRs.StaleDrafts)
	}

	// Publish the human-facing "what should I merge next?" digest. This reads
	// the PR set enumerated and CI-enriched immediately above, so it must stay
	// after those two calls: Mergeable and the failing-check names it sorts on
	// are populated by EnrichCIStatus, not by EnumerateActionable.
	if !budgetDecision.SkipOptional {
		postRecommendationsForCycle(ctx, cfg, ghClient, actionable, logger)
	}

	// Fold this pass's CI state into the fix-loop staleness clock BEFORE any
	// consumer reads it, so the claim-suppression guard (#3), the merge watcher
	// (#2), and the stuck-PR reaper (#4) all key off a consistent, current
	// signal within the same tick. This only records first-seen-red times; the
	// distinct-SHA attempt counting still happens in runEscalationSweep below.
	recordRedStaleness(cfg, actionable)

	// Duplicate-PR guard: drop issues an open hive-authored PR already claims,
	// before the governor counts the queue or the scheduler builds kicks. A
	// restart storm otherwise re-offers the same issue on every fresh agent
	// start, and the agent — having no memory of the PR it just filed — files
	// another. Backed by a PVC ledger so it survives those restarts, and fails
	// closed (keeps the last known claims) when the GitHub API is unavailable.
	if !budgetDecision.SkipOptional {
		applyDuplicatePRGuard(ctx, cfg, ghClient, actionable, logger)
	}
	// Every cycle, cached ones included: heal issue stage records the
	// claim ledger says an open PR covers but the store still holds below
	// 040 (a claim release written over the cover between reconcile passes).
	reconcileLedgerCoverRecords(ctx, cfg, ghClient, actionable, logger)

	// PR review queue (#9590): stamp each PR's rank, priority and reasons
	// onto the snapshot so last-actionable.json carries them next to
	// review_class. Additive only: Items and Held keep their order, and
	// nothing that gates work reads the stamp.
	reviewQueue := stampReviewQueue(cfg, actionable, logger)

	// Only the reconcile pass publishes the snapshot: a cached cycle works
	// on a copy, and storing it back would overwrite a webhook patch that
	// landed while the cycle ran.
	if cachedSnapshot == nil {
		publishEnumeratedSnapshot(lastActionable, actionable, logger)
		if data, err := json.Marshal(actionable); err == nil {
			atomicWrite(lastActionablePath, data)
		}
	}
	publishStageSharedCIIncidents(actionable)

	// Record enumerated issues into the lifecycle timeline so the dashboard's
	// lifecycle view has real data. Cheap and fully guarded: a nil dashboard or
	// nil store is a no-op (timeline.Store.Record is nil-safe), the loop is
	// bounded by maxTimelineEnumeratePerCycle, and the store dedupes by
	// (ref, kind) so this per-cycle sweep refreshes journeys instead of
	// flooding them (#5656).
	recordEnumeratedIssues(ctx, dashSrv, actionable)

	escalatedPRs := map[string]bool{}
	if !budgetDecision.SkipUnstick {
		escalatedPRs = runEscalationSweep(ctx, cfg, governorForge(cfg, ghClient, logger), actionable, notifier, dashSrv, logger)
	}

	intentVerdicts := writeIntentVerdicts(ctx, cfg, ghClient, actionable, beadStores, logger)
	refreshReviewVerdicts(cfg, logger)
	recordReviewOutcomes(ctx, cfg, ghClient, actionable, logger)
	requiredCheckSet, _ := cfg.AutoMerge.RequiredCheckSetForRepo("")

	// Hold guard (#5589): snapshot hold-gated PR heads, and when a hold lifts
	// on a branch that moved, block the merge lanes and force a fresh review.
	// Runs before writeMergeEligible so drifted PRs are excluded from the very
	// tick their hold lifted — no window for the sweep to race the re-hold.
	selfAuthorizationHoldEnabled := func(repo string) bool {
		level := cfg.ACMMLevelOrZero()
		if agentMgr != nil {
			level = agentMgr.GetACMMLevel()
		}
		return cfg.SelfAuthorizationHoldEnabledForRepoAtLevel(repo, level)
	}
	holdDriftPRs := enforceHoldGuard(ctx, cfg, ghClient, governorForge(cfg, ghClient, logger), actionable, logger, selfAuthorizationHoldEnabled)

	// The per-PR verdicts come back so the dashboard's PR pills can be
	// painted from the sweep's own classification rather than a looser
	// reading of GitHub's mergeable flag (hivecommons/hive#7478).
	mergeVerdicts := writeMergeEligible(actionable, actionable.Hold, cfg.Project.Org, escalatedPRs, cfg.Intent.Enforce, intentVerdicts, cfg.Review.RequireApproval, requiredCheckSet, holdDriftPRs, cfg, logger)

	// Review-bot threads (hivecommons/hive#7360): list every unresolved
	// external-review-bot thread on an open hive-authored PR into
	// review-threads.json, attributed to the agent that opened the PR the
	// same way ci-failing.json is, so the scheduler can route each PR back
	// to its author for a fix + in-thread replies before any new work.
	if !budgetDecision.SkipOptional {
		writeReviewThreads(ctx, ghClient, actionable, cfg.Project.Org, escalatedPRs, logger)
	}

	// PR follow-up session resume (hivecommons/hive#9583, default off): feed
	// CI failures, changes-requested reviews and new review-bot threads on a
	// PR this hive opened back into the CLI session that authored it, while
	// that session is still live. Anything it cannot resume stays on the
	// fix-before-new path above, unchanged.
	routePRFollowUps(ctx, cfg, ghClient, actionable, escalatedPRs, agentMgr, logger)

	// Stuck-PR reaper (backstop): DELIVER a targeted FIX-BEFORE-NEW kick for any
	// hive-authored PR that is red on a required check AND stale (its red head
	// SHA unchanged past RedPRStaleAfter). writeMergeEligible already surfaces
	// every red PR into ci-failing.json (the CI_FAILING kick block), but relying
	// on the owner's next cadence kick to carry it meant a red PR could be
	// escalated as "6 automated fix re-dispatches" having been mentioned to
	// nobody (hivecommons/hive#9472); the reaper now kicks the owner — or the
	// fallback fixer when the owner is paused or unreachable — and charges the
	// re-engagement budget only for kicks that were accepted. Composes with the
	// merge watcher (#2): both go through the same per-red-SHA cap, so the same
	// PR is never double-dispatched within a red-SHA's budget.
	reapStuckRedPRs(cfg, actionable, escalatedPRs, agentKicker{mgr: agentMgr},
		agentAvailability(cfg, agentMgr), logger)

	// Release sentinel (hivecommons/hive#9585), opt-in and default OFF: when
	// the current v<version> tag's CI fails, dispatch a bounded repair round
	// through the same kick path as the reaper above, or escalate to a human
	// when the failure is a setting no commit can fix. Never pushes a branch;
	// moves the tag to a merged fix PR only with retag_enabled (also opt-in).
	runReleaseSentinel(ctx, cfg, ghClient, agentKicker{mgr: agentMgr},
		agentAvailability(cfg, agentMgr), notifier, logger)

	// Upstream watch (hivecommons/hive#9967), opt-in and default OFF: at most
	// once per upstream_watch.interval, file a labelled fork issue for each
	// upstream merged PR or release that still applies to the fork. Opens
	// issues only, never PRs; the watermark and dedupe index live on the PVC.
	runUpstreamWatch(ctx, cfg, ghClient, logger)

	// Merge-hotspot advisor: at most hourly, score the files concurrent
	// PRs keep colliding on (from the merge train's own records) and file
	// one modularize issue per hotspot for the ci-maintainer lane through
	// the writeback gate. Skipped with the other optional sweeps when the
	// GitHub budget is tight.
	if !budgetDecision.SkipOptional {
		runHotspotAdvisor(ctx, cfg, ghClient, actionable, logger)
	}

	if !budgetDecision.SkipUnstick {
		shaResult, shaErr := ghClient.EnforceSHAHold(ctx, github.SHAHoldConfig{
			PrimaryRepo:     cfg.Project.PrimaryRepo,
			AIAuthor:        cfg.Project.AIAuthor,
			InternalAuthors: []string{"hivecommons-hive[bot]", "kubestellar-hive[bot]", "hivecommons-hive-ghe[bot]", "kubestellar-hive-ghe[bot]", "github-actions[bot]", "dependabot[bot]", "copilot-swe-agent[bot]"},
			// Trusted reporters get the soft needs-sha label, not the hard
			// hold (hivecommons/hive#12002).
			TrustedAssociations: cfg.Project.IssueFilter.ReporterTrust.EffectiveTrustedAssociations(),
			TrustedLabel:        cfg.Issues.EffectiveSHAHoldTrustedLabel(),
		})
		if shaErr != nil {
			logger.Warn("SHA hold enforcement failed", "error", shaErr)
		} else {
			logger.Info("SHA hold enforcement complete",
				"held", shaResult.Held,
				"unheld", shaResult.Unheld,
				"softened", shaResult.Softened,
				"skipped", shaResult.Skipped,
			)
		}
		runHardSuppressReconcile(ctx, cfg, ghClient, logger)
	}

	// Refresh budget spend from lifetime token totals before Evaluate so
	// the kick gate sees current-window numbers. The banner itself is
	// level-triggered every cycle (#9612 follow-up): a collector summary may
	// not be available yet (right after a restart, before the first session
	// scan completes), and the one-shot Crossed edge that used to gate the
	// raise is lost on restart too, so gate ONLY the spend refresh on the
	// summary and re-derive the banner from whatever budget state the
	// governor currently holds (persisted spend if the summary is not in
	// yet, refreshed spend once it is) on every cycle.
	trans := gov.BudgetLevel()
	if tokenCollector != nil {
		if summary := tokenCollector.Summary(); summary != nil {
			var bobCoins, bobUSD float64
			if coinCfg, ok := cfg.Governor.Budget.CoinConfig(tokens.BackendBob); ok {
				bobCoins = coinCfg.TokensToCoins(tokensForBackend(summary, tokens.BackendBob))
				bobUSD = coinCfg.CoinsToUSD(bobCoins)
			}
			trans = gov.UpdateBudgetFromTotalsCoinsAndUSD(summary.TotalTokens, summary.ByAgent, summary.ByModel, bobCoins, bobUSD)
		}
	}
	applyBudgetAlerts(gov, trans, dashSrv, notifier)

	// Cause+fix banner for the never-kicked class (#5577): the dashboard's
	// not-producing warnings name the SYMPTOM (agent idle, zero tokens); this
	// names the cause — enabled agent, no cadence in any mode, never kicked —
	// and the fix. Computed from the spoke's own governor config, no hub
	// round-trip; self-clears the moment a cadence is set or any kick lands.
	applyNoCadenceAlert(gov, dashSrv)

	agentsDue := gov.EvaluateWithRepoDepths(
		actionable.Issues.Count,
		actionable.PRs.Count,
		actionable.Hold.Total,
		actionable.Issues.SLAViolations,
		governor.RepoDepthsFromActionable(actionable),
	)

	// The weaker sibling of the banner above (#7474): an agent SOME mode
	// schedules but the mode the fleet is now in does not — a reviewer with a
	// cadence only in surge goes silent the moment its own work drives the
	// backlog below the surge threshold, and every other signal calls it
	// healthy. Applied after Evaluate so it reads the mode this tick settled
	// on; self-clears when the mode changes back or the operator fills the
	// gap.
	applyModeUnscheduledAlert(gov, dashSrv)

	// Crash-restarted agents may get a "resume" kick ahead of their cadence
	// slot so work interrupted mid-task resumes promptly — but ONLY through
	// the governor's gate (#2573). Unconditionally kicking every restarted
	// agent meant a crash-looping CLI was kicked on every eval cycle,
	// burning backend tokens far faster than any configured cadence and
	// bypassing the budget gate; AllowResumeKick bounds resume kicks to one
	// per cadence interval and respects mode pauses and the budget.
	// Refused restarts are not silent: an interval-throttled agent sits at
	// an empty prompt until its slot, so raise a per-agent alert (with an OOM
	// hint when the cgroup killed the CLI). Every cycle, reconcile clears it
	// once it no longer describes a problem (#9612): kicked, paused/removed,
	// no longer expected to run, working again, or past its max age.
	var resumeHeld []spokealerts.ResumeKickHeldAgent
	agentsDue, resumeHeld = mergeResumeKicks(agentsDue, restartedAgents, gov.AllowResumeKickReason, logger)
	resumeKickHeldAlerts.Apply(dashSrv, resumeHeld, agentMgr.CrashOOMSuspected)
	resumeKickHeldAlerts.Reconcile(dashSrv, resumeKickHeldFacts(agentMgr, gov.ResumeKickVerdict))

	govState := gov.GetState()
	span.SetAttributes(
		attribute.String(tracing.AttrHiveGovernorMode, string(govState.Mode)),
		attribute.Int("hive.queue.issues", govState.QueueIssues),
		attribute.Int("hive.queue.prs", govState.QueuePRs),
		attribute.Int("hive.queue.hold", govState.QueueHold),
	)
	// cadence.Paused (cadence: "pause" in config) means "don't kick this agent
	// in this mode" — it does NOT force-pause the agent. Manual pause/resume
	// via the dashboard is always respected; the governor only controls kicks.

	// Filter out on-demand agents — they are only triggered explicitly
	// Operator-paused agents must consume NOTHING (#2573); see
	// filterKickableAgents for the full gate. This runs BEFORE the eval-cycle
	// log so "agents_due" names the agents that are actually about to be
	// kicked; anything gated out is reported in "agents_skipped" with its
	// reason rather than vanishing silently.
	agentsDue, agentsSkipped := partitionKickableAgents(agentsDue, cfg.Agents, config.OnDemandAgentsFromPacks(), agentMgr.IsPaused, func(name string) string {
		if st, err := agentMgr.GetStatusFast(name); err == nil && st != nil {
			return st.PausedTrigger
		}
		return ""
	})
	agentsDue, agentsSkipped = reviewerAutoscaler.withholdDrainingReviewers(agentsDue, agentsSkipped)

	// PROVIDER SPEND REBUFF (#4294). When the inference gateway is refusing on a
	// money limit, every kick launched this cycle is a run that cannot buy a
	// single token — precisely the failure this addresses: a hive that kept
	// firing its cadence into a gateway rejecting 100% of requests all day,
	// silently, until the provider's spend window happened to roll over.
	//
	// The alert is raised HERE, before kick assembly, so an operator is told
	// even on a cycle where nothing happened to be due. The actual suppression
	// happens after every kick source has contributed — see below.
	providerBudgetCause, providerBudgetSince, providerBudgetLastRebuff, providerBudgetRebuffs := dashboard.InferenceBudgetExceeded()
	// Suppress only while the latch is FRESH. Withholding kicks also withholds
	// the inference calls that clear the latch, so a hive whose only kick source
	// is the governor cadence would never learn the provider's window reset and
	// would stay muted forever — the exact topology in the field report. Once
	// the last rebuff is older than the probe interval, this cycle's kicks go
	// through as a probe: still clipped re-freshens the stamp and suppression
	// resumes, served clears the latch outright.
	providerBudgetProbeInterval := cfg.Governor.ProviderBudget.EffectiveProbeInterval()
	providerBudgetLatched := providerBudgetCause != ""
	suppressKicks := governor.ProviderBudgetSuppresses(providerBudgetLatched,
		providerBudgetProbe.Freshest(providerBudgetLastRebuff), time.Now(), providerBudgetProbeInterval)
	budgetAlert := decideProviderBudgetAlert(providerBudgetLatched, suppressKicks,
		providerBudgetCause, providerBudgetSince, providerBudgetRebuffs,
		func() string {
			return hub.QuotaExhaustedAgentReason(hub.QuotaExhaustedProcessCount(agentMgr.AllStatuses()))
		})
	if budgetAlert.Message != "" {
		dashSrv.AddSystemAlert(providerBudgetAlertID, "error", budgetAlert.Message)
	}
	if budgetAlert.Clear {
		dashSrv.ClearSystemAlert(providerBudgetAlertID)
	}
	if budgetAlert.Cause != "" {
		providerBudgetCause = budgetAlert.Cause
	}
	// Notify ONCE per latch, not once per cycle. The deduped banner above
	// already carries the ongoing state; a high-priority notification repeated
	// every eval cycle for as long as the provider stays clipped is a day of
	// pages saying the same thing. Keyed on the latch time, which recordRebuff
	// deliberately does not move forward, so a genuinely new clip after a
	// recovery notifies again. Matches applyBudgetAlerts, which notifies on the
	// crossing rather than on the condition.
	notifyProviderBudget := providerBudgetLatched && providerBudgetNotify.ShouldSend(providerBudgetSince)
	if !providerBudgetLatched {
		providerBudgetProbe.Reset()
		// The RECOVERY crossing: the latch a notification went out for has
		// cleared (a probe's inference call succeeded), so tell the operator
		// once that the hive resumed — the counterpart of the entering page,
		// without which the only signal of recovery is a banner quietly
		// vanishing. Every later healthy cycle is silent.
		if providerBudgetNotify.Reset() {
			logger.Info("provider spending limit lifted: agent kicks resumed")
			notifier.Send("Provider spending limit lifted",
				"the inference provider is serving again — agent kicks have resumed",
				notify.PriorityDefault)
		}
	}

	// ADDITIVE CEL routing: evaluate operator-defined CEL trigger rules
	// (cfg.Triggers, pkg/celtrigger) against the items enumerated this cycle and
	// UNION any matched, gated agents into agentsDue. This runs alongside — never
	// instead of — the label/governor selection above: unionAgents only ever adds
	// names, and celMatchedAgents enforces the same pause/budget/on-demand gates,
	// so a CEL match can neither remove a governor-selected agent nor bypass a
	// paused agent or exhausted budget. Fully guarded/cheap: with no `triggers:`
	// configured, celEngineFor returns nil and celMatchedAgents does zero work.
	if celEngine := celEngineFor(cfg, logger); celEngine != nil {
		celAgents := celMatchedAgents(celEngine, actionable, cfg, gov, agentMgr.IsPaused, logger)
		if len(celAgents) > 0 {
			before := len(agentsDue)
			agentsDue = unionAgents(agentsDue, celAgents)
			if len(agentsDue) > before {
				logger.Info("celtrigger: unioned CEL-matched agents into due set",
					"added", agentsDue[before:], "cel_matched", celAgents)
			}
		}
	}

	// #4247/#4263 (parent #3845): apply the shared convergence admission at the
	// internal-kick dispatch boundary — AFTER governor policy evaluated the raw
	// queue, BEFORE the scheduler caches and renders issues. Gated by the
	// runtime convergence rollout mode, captured once per pass: with mode "off"
	// (the DEFAULT) this is entirely inert and kickActionable IS actionable;
	// "shadow" logs and records what would be withheld but still dispatches the
	// raw population; only "enforce" gates the scheduled/cached issue payloads
	// below. The raw actionable population stays authoritative for governor
	// policy, dashboard status, PR/review dispatch, escalation, and every path
	// not explicitly enrolled.
	kickActionable := applyConvergenceKickAdmission(cfg, dashSrv, actionable, notifier, logger)

	sched.SetLastActionable(kickActionable)
	// The filter compacts agentsDue in place, so snapshot it first to report
	// the targets it withholds alongside the other skips below.
	dueBeforeDeferral := append([]string(nil), agentsDue...)
	agentsDue = sched.FilterFirstScanDeferredAgents(agentsDue)
	agentsSkipped = append(agentsSkipped, firstScanDeferredSkips(dueBeforeDeferral, agentsDue)...)
	reviewPlan := planReviewDispatchWith(ctx, cfg, actionable, agentMgr, beadStores, reviewPlannerGitHubFor(ghClient), logger)
	// Size the reviewer lane to what the planner just left waiting
	// (hivecommons/hive#14685). A replica added here takes kicks from the
	// next cycle on; a retire only lands once the reviewer is idle.
	reviewerAutoscaler.Run(ctx, cfg, reviewPlan, agentMgr, gov, logger, time.Now())
	auditWithheldReviewFixes(reviewPlan, dashSrv, logger)
	if ghClient != nil {
		postReviewAcknowledgements(ctx, ghClient, reviewPlan, logger)
	}
	applyHumanDecisionLabels(ctx, cfg, ghClient, actionable, reviewPlan, logger)
	if ghClient != nil {
		applyReviewPriorityLabels(ctx, cfg, ghClient, reviewQueue, logger)
		applyReviewVerdictLabels(ctx, cfg, ghClient, actionable, reviewPlan, logger)
		sweepClosedPRVerdictLabels(ctx, cfg, ghClient, time.Now(), logger)
	}
	messages, messageSkips := sched.BuildKickMessagesWithSkips(kickActionable, agentsDue)
	agentsSkipped = append(agentsSkipped, messageSkips...)
	// Message construction can withhold a due kick (notably an idle reviewer
	// lane). Report only assembled targets as due. Skipped targets dispatch
	// nothing, so (as before) no LastKick is recorded for them and new lane
	// work stays eligible on the next evaluation.
	assembledTargets := make([]string, 0, len(messages))
	for _, msg := range messages {
		assembledTargets = append(assembledTargets, config.CadenceTargetKey(msg.Agent, msg.Repo))
	}
	logger.Info("governor eval complete",
		"mode", govState.Mode,
		"issues", govState.QueueIssues,
		"prs", govState.QueuePRs,
		"agents_due", assembledTargets,
		"agents_skipped", agentsSkipped,
	)

	reviewKickByMessage := map[string]review.DispatchKick{}
	for _, k := range append(reviewPlan.ReviewKicks, reviewPlan.FixKicks...) {
		if !gov.AgentEligibleForCELKick(k.Agent) || agentMgr.IsPaused(k.Agent) {
			logger.Info("review swarm kick suppressed by governor gate", "agent", k.Agent, "pr", k.PRRef)
			continue
		}
		messages = append(messages, scheduler.KickMessage{Agent: k.Agent, Message: k.Message, IssueRefs: []string{k.PRRef}})
		reviewKickByMessage[k.Agent+"\x00"+k.Message] = k
	}
	// A fix kick (a maintainer's changes_requested / `/hive decision`, or
	// the swarm's) goes ahead of the governor kick for the same agent, or
	// the governor kick owns the agent and the fix kick times out every
	// cycle (hivecommons/hive#13100).
	for _, agent := range pendingKickAgentsFromMessages(messages) {
		if governorKickFairness.GovernorFirst(agent) {
			logger.Warn("governor kick displaced by fix kicks on consecutive cycles; governor kick goes first this cycle",
				"agent", agent, "displaced_cycles", governorKickFairness.Displaced(agent), "limit", governorKickDisplacementLimit)
		}
	}
	messages = orderReviewFixKicksFirst(messages, reviewKickByMessage, governorKickFairness.GovernorFirst)
	// Freshness bound before a kick (#13375): a ref older than
	// github.DecisionFreshnessBound in a cache-served or last-good snapshot
	// is confirmed with one targeted GET; a stale repo's refs are withheld.
	messages = freshenKickMessages(ctx, ghClient.SnapshotFreshnessGuard(actionable, time.Now), actionable, messages, logger)
	// #4294: drop EVERY assembled kick while the provider is refusing on a
	// spending limit. Placed after all three sources have contributed —
	// governor-due agents, the CEL union, and the review swarm — because gating
	// only `agentsDue` earlier would still let a CEL match or a review kick fire
	// into the same clipped key.
	//
	if len(messages) > 0 {
		filtered := messages[:0]
		for _, msg := range messages {
			if remaining, class, line, ok := agentMgr.ProviderErrorBackoffRemaining(msg.Agent); ok {
				logger.Warn("provider inference error: withholding agent kick during backoff",
					"agent", msg.Agent,
					"class", class,
					"retry_in", remaining.Round(time.Second),
					"error", line)
				gov.RecordKickFailure(msg.Agent, fmt.Errorf("provider inference backoff (%s): %s", class, line), time.Now())
				continue
			}
			filtered = append(filtered, msg)
		}
		messages = filtered
	}

	// Suppression is total rather than per-agent because the limit is on the
	// KEY: no agent can succeed while it is clipped. It self-heals — the first
	// inference call that succeeds after the provider's window resets clears the
	// signal — so there is no timer to tune and no operator action required.
	//
	// Deliberately does NOT force-pause agents. Operator pause state is a human
	// decision (#2573) and must not be forged by an automatic signal that will
	// clear itself; withholding kicks achieves the saving without leaving paused
	// agents behind for a human to un-pause by hand.
	//
	// Probe cycle: when the last rebuff has gone stale, ONE kick is
	// deliberately allowed through to find out whether the provider is
	// serving again. Its inference calls are what clear the latch (on a
	// 2xx) or re-freshen it (on another rebuff) — nothing else can. Only
	// one: the question is "is the window still clipped", and every kick
	// beyond the first spends a run to learn the same answer. Releasing it
	// re-arms suppression immediately, so the cycles while the probe's run
	// is still in flight withhold again rather than leaking more kicks.
	kickGate := gateKickMessagesForProviderBudget(messages, suppressKicks, providerBudgetLatched)
	releaseProviderBudgetProbe := kickGate.ReleaseProbe
	if suppressKicks && len(messages) > 0 {
		logger.Warn("provider spending limit: withholding agent kicks",
			"withheld", kickGate.Withheld, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince,
			"next_probe_in", (providerBudgetProbeInterval - time.Since(providerBudgetProbe.Freshest(providerBudgetLastRebuff))).Truncate(time.Second))
	} else if releaseProviderBudgetProbe {
		if len(kickGate.Withheld) > 0 {
			logger.Warn("provider spending limit: withholding all but the probe kick",
				"withheld", kickGate.Withheld, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince)
		}
		logger.Info("provider spending limit: releasing a single probe kick",
			"probe_agent", kickGate.Kept[0].Agent, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince,
			"last_rebuff", providerBudgetLastRebuff, "probe_interval", providerBudgetProbeInterval)
	}
	messages = kickGate.Kept
	// Self-upgrade drain (#11876): a hub-instructed upgrade is waiting for
	// in-flight kicks to end, so starting new turns would only feed the
	// restart. Withheld kicks stay due and go out from the new process.
	if target, draining := selfUpgradeDrain.Active(selfUpgradeDrainNow()); draining && len(messages) > 0 {
		logger.Info("self-upgrade drain: withholding new agent kicks",
			"target", target, "withheld", pendingKickAgentsFromMessages(messages))
		messages = nil
	}
	govState.PendingKicks = pendingKickAgentsFromMessages(messages)
	gov.SetPendingKicks(govState.PendingKicks)
	if notifyProviderBudget {
		notifier.Send("Provider spending limit reached", providerBudgetCause, notify.PriorityHigh)
	}

	// buildStatusSnapshot assembles the full dashboard snapshot from this
	// cycle's evaluation. It is called twice: once BEFORE the kick dispatch
	// below and once at the end of the cycle. Each kick can wait up to the
	// input-prompt timeout for a busy pane; before dispatch ran one agent at
	// a time, a dozen due agents took 10-15 minutes on hosted hives — during
	// which /api/status (and everything derived from it: stage tracker, agent
	// states, governor.nextKickAt) kept showing the previous cycle's data, and
	// a rolling upgrade landing mid-dispatch meant the evaluation never
	// reached the dashboard at all. Dispatch is now concurrent across agents
	// (dispatchAgentKicks), so the wait is one agent's, but the early publish
	// stays: the evaluation should be visible before any kick is typed.
	buildStatusSnapshot := func() *dashboard.StatusPayload {
		payload := dashboard.BuildFrontendStatus(
			govState,
			actionable,
			agentMgr.AllStatuses(),
			cfg,
			tokenCollector,
			gov,
			beadStores,
			ghClient,
			ctx,
			metricsCollector,
		)
		// Green on a PR pill means "the sweep would merge this now" — the
		// verdict writeMergeEligible reached for this same actionable set,
		// not a re-derivation from mergeable_state (#7478).
		dashboard.AttachMergeVerdicts(payload, mergeVerdicts)
		// A PR pill also carries the hive's OWN review on that PR, so the
		// queue view shows where the hive has already spoken. Read from the
		// durable ledger, so this costs no GitHub call per PR; a missing or
		// unreadable ledger simply means no review pills this cycle.
		attachReviewLinksForDashboard(payload, logger)
		return payload
	}
	publishStatusBeforeKicks(dashSrv, len(messages), buildStatusSnapshot, logger)

	// Kick dispatch lives behind a seam (#7232): the skip rules and the
	// single-probe rule are the decisions worth testing, and they were
	// previously unreachable without a tmux session and a live governor.
	var deliveredReviewKicks []review.DispatchKick
	var deliveredKicks []scheduler.KickMessage
	if len(messages) > 0 {
		// governor.kick wraps the whole dispatch pass; each delivered kick
		// still gets its own agent.kick child via startKickSpan below.
		kickCtx, kickPassSpan := tracing.StartSpan(ctx, "governor.kick",
			attribute.Int("hive.kicks.assembled", len(messages)))
		ctx := kickCtx
		dispatchAgentKicks(messages, releaseProviderBudgetProbe, kickDispatchDeps{
			backoffRemaining: agentMgr.ProviderErrorBackoffRemaining,
			sendKick:         agentMgr.SendKick,
			startStoppedAgent: func(agentName string) error {
				if !agentMgr.GovernorMayStart(agentName) {
					return nil
				}
				agentMgr.MarkStartupLaunchQueued([]string{agentName})
				if err := agentMgr.Start(ctx, agentName); err != nil {
					if stoppedAgentStartErrorIsBenign(err) {
						logger.Info("stopped agent start raced with existing launch before governor kick", "agent", agentName, "error", err)
						return nil
					}
					return fmt.Errorf("start stopped agent: %w", err)
				}
				logger.Info("started stopped agent before governor kick", "agent", agentName)
				return nil
			},
			startKickSpan: func(agentName string) func(error) {
				agentCfg := cfg.Agents[agentName]
				_, kickSpan := tracing.StartSpan(ctx, "agent.kick", tracing.AgentKickAttributes(
					agentName,
					agentCfg.Backend,
					agentCfg.Model,
					agentCfg.Role,
					string(govState.Mode),
					inferACMMLevel(cfg),
				)...)
				return func(err error) {
					if err != nil {
						kickSpan.RecordError(err)
					}
					kickSpan.End()
				}
			},
			onReviewDelivered: func(msg scheduler.KickMessage) {
				if k, ok := reviewKickByMessage[msg.Agent+"\x00"+msg.Message]; ok {
					deliveredReviewKicks = append(deliveredReviewKicks, k)
					persistReviewDispatchStateInProgress(reviewPlan, deliveredReviewKicks, logger)
				}
			},
			onFailed: func(msg scheduler.KickMessage, err error) {
				gov.RecordKickFailure(msg.Agent, err, time.Now())
				if k, ok := reviewKickByMessage[reviewKickKey(msg)]; ok {
					logUndeliveredReviewKick(logger, dashSrv, k, err)
				}
			},
			onDelivered: func(msg scheduler.KickMessage, deliveredAt time.Time) {
				deliveredKicks = append(deliveredKicks, msg)
				gov.RecordKickForRepoAt(msg.Agent, msg.Repo, deliveredAt)
				dashSrv.AuditLog("governor", "kick", "trigger=governor-eval", msg.Agent)

				// Record issue-scoped kicks into the lifecycle timeline. Cheap,
				// guarded, and nil-safe (Record no-ops on a nil dashboard/store).
				recordKick(ctx, dashSrv, msg.Agent, msg.IssueRefs...)
				// A review-swarm kick (review or fix) names a pull request,
				// not an issue: its IssueRefs is the PR ref. Listing it in
				// the issue-claims ledger made the author's next comment on
				// its own PR an issue "start signal", so the PR was claimed
				// as an issue and staged stage/030-pr-in-progress — an
				// issue-only stage — until the PR sweep healed it.
				reviewKick, isReview := reviewKickByMessage[msg.Agent+"\x00"+msg.Message]
				if !isReview {
					// #8380/#10527: list these issues for the kicked agent so
					// contributors and other agents back off while the kick is
					// live; the claim itself is recorded on the agent's first
					// start signal (recordAgentStart), not here.
					recordAgentKickListings(dashSrv, cfg.Project.Org, msg.Agent, msg.IssueRefs, logger)
				}

				// #12128: record which issue/PR/head SHA this kick was launched
				// for, so /api/launch-cost can place the turn's spend. Review
				// kicks carry the PR head; governor kicks carry the issue refs.
				if tokenCollector != nil {
					tokenCollector.RecordLaunch(launchRecordForKick(msg, reviewKick, isReview, time.Now()))
				}

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
			},
			markProbeReleased: providerBudgetProbe.MarkReleased,
			now:               time.Now,
		}, logger)
		kickPassSpan.SetAttributes(attribute.Int("hive.kicks.review_delivered", len(deliveredReviewKicks)))
		kickPassSpan.End()
	}
	governorKickFairness.ObserveCycle(messages, deliveredKicks, reviewKickByMessage)
	persistReviewDispatchState(reviewPlan, deliveredReviewKicks, logger)

	if actionable.Issues.SLAViolations > 0 {
		toNotify, capped := selectSLABreachNotifications(actionable.Issues.Items)
		for _, issue := range toNotify {
			notifier.Send(
				"SLA 2x breach",
				fmt.Sprintf("%s age %dm: %s\n%s", actionableIssueRef(issue), issue.AgeMinutes, issue.Title, issue.URL),
				notify.PriorityHigh,
			)
		}
		if capped {
			logger.Info("SLA notification cap reached, skipping remaining", "remaining", actionable.Issues.SLAViolations-len(toNotify))
		}
	}

	// Scan agent panes for login-required patterns and pause + notify if detected
	loginscan.Scan(ctx, cfg, agentMgr, notifier, dashSrv, logger, loginSightings)

	// Epoch captured before reading agent/governor state so a mutation that
	// lands mid-build (restart-count/budget reset) drops this snapshot instead
	// of letting it revert the mutation on the dashboard (#4348).
	buildEpoch := dashSrv.BeginStatusSnapshot()
	statusPayload := buildStatusSnapshot()
	statusPublished := false
	// Ingest any JSONL findings agents wrote and persist them as beads.
	if advisoryStore != nil {
		findings, err := advisoryStore.ReadNewFindings()
		if err != nil {
			logger.Warn("failed to read advisory findings", "error", err)
		} else if len(findings) > 0 {
			// The canary gate's decisions live behind a seam (#7232); only the
			// effects — audit entry, critical bead — are supplied here.
			var scanCanary func(agent, reportText, source string) (ioscan.CanaryLeak, bool)
			if cfg.Ioscan.IsEnabled() && cfg.Ioscan.CanariesEnabled() {
				scanCanary = ioscan.DefaultCanaries.Scan
			}
			safeFindings := gateAdvisoryFindings(findings, advisoryIngestDeps{
				scanCanary: scanCanary,
				failClosed: cfg.Ioscan.FailClosed(),
				auditLog:   dashSrv.AuditLog,
				recordLeakBead: func(leak ioscan.CanaryLeak) {
					if store, ok := beadStores[leak.Agent]; ok && store != nil {
						if b, berr := store.Create("Canary token leaked via "+leak.Source, beads.TypeAdvisory, beads.PriorityCritical, leak.Agent, ""); berr == nil {
							_ = store.SetMetadata(b.ID, "rule", ioscan.CanaryLeakRule)
							_ = store.SetMetadata(b.ID, "source", leak.Source)
						}
					}
				},
			}, logger)
			if persisted := advisory.PersistAsBeads(safeFindings, beadStores); persisted > 0 {
				logger.Info("advisory findings persisted as beads", "count", persisted)
			}
		}
	}

	// Reload bead stores from disk before building the digest. Agents write
	// beads via the bd CLI which persists directly to disk, so the in-memory
	// stores can become stale between eval cycles. Reload failures are deduped
	// (WARN once per distinct error, then DEBUG) — see beads_reload.go (#5505).
	reloadBeadStores(beadStores, logger)

	// Phase 4 Part B: `plan`/`epic` label trigger. An actionable issue carrying a
	// plan label auto-mints an epic and requests decomposition — the same flow as
	// the dashboard "Plan this issue" click, but triggered by the label. It runs
	// AFTER the store reload so FindByExternalRef sees current state (idempotent:
	// no duplicate epic if one already exists). Cheap, synchronous, adds NO
	// goroutine, and drives the architect only via SendKick (never the launch
	// path). Gated by config/ACMM so low-maturity hives stay advisory-only.
	if acmmLvl := inferACMMLevel(cfg); planning.PlanningAllowedAtLevel(acmmLvl) &&
		approvalDeskAllowsLegacyOperation(ctx, approvalDesk, cfg, toolapprove.KindPlanFromLabel, "plan-from-label", planning.ArchitectAgentName, logger) {
		planFromLabeledIssues(actionable, beadStores, agentMgr, gov, dashSrv, logger, cfg, acmmLvl)
	}

	// Advisory digest: build from beads (the source of truth) before status broadcast.
	primaryRepo := primaryAdvisoryRepo(cfg)
	issueNum, hasPinnedAdvisoryIssue := advisoryIssueNumber(advisoryIssues, primaryRepo)
	hasExistingPinnedIssueForEmptyDigest := hasPinnedAdvisoryIssue &&
		primaryRepo == primaryRepoAtCycleStart &&
		hadPinnedAdvisoryIssueAtCycleStart
	if shouldBuildAdvisoryDigest(beadStores, ghClient, hasExistingPinnedIssueForEmptyDigest) {
		// Mark findings no agent has re-reported inside the staleness window
		// BEFORE the digest is built, so stale evidence is captioned. Silence is
		// not proof a finding healed: an agent run can be partial, truncated, or
		// non-deterministic, so absence cannot move an item to Recently Resolved.
		advCfg := cfg.Governor.Advisory
		if advCfg.StalenessDays > 0 {
			if marked := advisory.MarkStaleAdvisoryBeads(beadStores, time.Duration(advCfg.StalenessDays)*24*time.Hour); len(marked) > 0 {
				logger.Info("marked advisory findings not re-reported within the staleness window",
					"count", len(marked), "staleness_days", advCfg.StalenessDays, "titles", strings.Join(marked, "; "))
			}
		}
		// Repo entries may be org-qualified ("org/repo"); the digest linkifier
		// needs the bare repo name alongside the org.
		org, repoName := cfg.Project.Org, primaryRepo
		if parts := strings.SplitN(primaryRepo, "/", 2); len(parts) == 2 {
			org, repoName = parts[0], parts[1]
		}

		// #3704: pin the digest to ONE repo commit. Resolve the target repo's
		// latest commit ONCE here (invariants 1 & 3), cite it in the rendered
		// comment (invariant 2, via the footer FormatDigestMarkdown emits when
		// AnalyzedSnapshot is set), and verify each finding's file path against
		// that exact commit so a since-removed path (e.g. "docs/install.md") is
		// flagged as outdated rather than cited as live. Best-effort: if the SHA
		// cannot be resolved, fall back to the previous unpinned behavior rather
		// than skip the digest.
		//
		// This is resolved BEFORE the digest is built because the top-N cap
		// consumes it: ranking cannot prefer a live finding over a since-removed
		// one unless it knows which is which at ranking time (#2364).
		digestOpts := advisory.DigestOptions{
			MaxFindings: advCfg.MaxFindings,
			ShowAll:     advCfg.ShowAll,
		}
		if ghClient != nil {
			pinDigestSnapshot(ctx, &digestOpts, org, repoName, primaryRepo, cfg.Policies.Branch, digestSnapshotDeps{
				defaultBranch: func(ctx context.Context, owner, repo string) (string, error) {
					r, _, err := ghClient.GetRepo(ctx, owner, repo)
					if err != nil {
						return "", err
					}
					return r.GetDefaultBranch(), nil
				},
				latestCommit:  ghClient.LatestCommitHash,
				pathExists:    ghClient.PathExistsAtRef,
				issueClosedAt: ghClient.IssueClosedAt,
			}, logger)
		}
		digest := advisory.BuildDigestFromBeads(beadStores, string(govState.Mode), digestOpts)
		enrichAdvisoryLinkedWork(ctx, ghClient, digest, org, repoName, logger)
		if advisoryStore != nil {
			advisoryStore.SetLatestDigest(digest)
		}
		dashSrv.SetAdvisoryDigest(digest)
		statusPayload.AdvisoryDigest = digest
		statusPublished = dashSrv.UpdateStatusIfFresh(statusPayload, buildEpoch)
		if !statusPublished {
			return
		}
		hiveAdvice := statusPayload.HiveAdvice

		// Post whenever there is something CURRENT to say: open findings,
		// recently resolved ones, or an empty evaluation for a hive that already
		// has a pinned advisory issue. The resolved and empty cases matter for
		// freshness: otherwise the pinned comment and AdvisoryLastPostedAt
		// freeze after the last finding disappears, and the hub reports a stale
		// advisory loop even though the agents are running cleanly.
		//
		// advisoryPostDue additionally paces the GitHub write to the
		// operator's governor.advisory.update_interval_s (#4820); 0/unset
		// keeps this exact per-cycle cadence. cfg is read live each cycle —
		// the same pattern as the staleness/max-findings knobs above — so a
		// dashboard edit applies from the next cycle without a restart. The
		// digest itself and the dashboard state above still refresh every
		// cycle; only the comment write is throttled. Note the #4821
		// write-through counts consecutive unchanged post ATTEMPTS, so its
		// forced full rewrite stretches with this interval (60 attempts ×
		// interval) — acceptable, since it only heals out-of-band comment
		// edits, and documented in the settings tooltip.
		// governor.advisory.target routes the comment write: GitHub (default,
		// the unchanged path below) or a designated Linear issue. For the
		// Linear route the configured issue plays the pinned issue's role in
		// the empty-digest freshness rule, so a clean Linear-sourced hive
		// keeps refreshing its comment exactly as a GitHub one does.
		advisoryTarget, advisoryLinearIssue, advisoryRouteErr := resolveAdvisoryDigestRoute(cfg)
		hasDigestHome := hasExistingPinnedIssueForEmptyDigest ||
			(advisoryTarget == config.AdvisoryTargetLinear && advisoryRouteErr == nil)
		if shouldPostAdvisoryDigest(digest, ghClient, hasDigestHome) &&
			advisoryPostDue(advCfg, primaryRepo, time.Now(), logger) {
			bySeverity, agentNames := summarizeDigestForLog(digest)
			logger.Info("advisory digest built",
				"total_findings", digest.TotalCount,
				"critical", bySeverity["critical"],
				"high", bySeverity["high"],
				"medium", bySeverity["medium"],
				"low", bySeverity["low"],
				"agents", agentNames,
				"resolved_count", len(digest.RecentlyResolved),
			)
			if digest.TotalCount == 0 && len(digest.RecentlyResolved) == 0 {
				logger.Info("advisory digest empty — posting freshness marker",
					"repo", primaryRepo, "issue", issueNum)
			}

			md := advisory.FormatDigestMarkdown(digest, advisory.DigestOptions{
				MaxFindings:  digestOpts.MaxFindings,
				ShowAll:      digestOpts.ShowAll,
				Org:          org,
				ShowEmpty:    digest.TotalCount == 0 && len(digest.RecentlyResolved) == 0,
				PrimaryRepo:  repoName,
				Advice:       hiveAdvice,
				DashboardURL: advisoryDashboardOrigin(cfg),
			})
			// The routing/classification decisions live in
			// publishAdvisoryDigest (#7232); only the effects are wired here.
			// The App is the sole advisory-digest writer (#1927): a user-token
			// failure must never drive the App banner, so every banner hook
			// below is fed exclusively by the App's own error.
			publishAdvisoryDigest(ctx, md, digest, advisoryPublishRoute{
				target:         advisoryTarget,
				linearIssue:    advisoryLinearIssue,
				routeErr:       advisoryRouteErr,
				primaryRepo:    primaryRepo,
				issueNum:       issueNum,
				hasPinnedIssue: hasPinnedAdvisoryIssue,
				ensureErr:      advisoryEnsureErr,
			}, advisoryPublishDeps{
				postGitHub: ghClient.PostAdvisoryDigest,
				postLinear: func(ctx context.Context, linearIssue, md string) error {
					return postAdvisoryDigestToLinear(ctx, cfg, linearIssue, md)
				},
				recordError: dashSrv.RecordAdvisoryError,
				recordPosted: func(findings, overflow int) {
					dashSrv.RecordAdvisoryPost(findings)
					recordAdvisoryPostSuccess(primaryRepo, time.Now())
					dashSrv.RecordAdvisoryOverflow(overflow)
				},
				onWriteForbidden: func(ctx context.Context) {
					// App is installed (we found the issue) but a real WRITE
					// was forbidden. #2353: attribute this honestly — surface
					// a DISTINCT write-forbidden state naming the likeliest
					// cause instead of faking a permission gap the diagnosis
					// just disproved.
					msg, state := classifyGitHubAppWriteForbidden(ctx, ghClient.AppAuth(), cfg.Project.Org, primaryRepo)
					dashSrv.SetGitHubAppRequired(true)
					dashSrv.SetGitHubAppPermIssue(msg)
					dashSrv.SetGitHubAppState(state.String())
					logger.Warn("GitHub App write failed — cannot write issue comments",
						"repo", primaryRepo, "state", state.String(),
						"operator_actionable", state.OperatorActionable(), "detail", msg)
				},
				onRepoMoved: func(ctx context.Context, moved *github.RepoMovedError) {
					// #11791: the write was refused because the repository
					// now lives under another owner. The installation is
					// healthy, so the generic write-forbidden remedy would
					// misdirect the operator; name the move instead.
					dashSrv.SetGitHubAppRequired(true)
					dashSrv.SetGitHubAppPermIssue(moved.Error())
					dashSrv.SetGitHubAppState(github.AppStateRepoMoved.String())
					logger.Warn("GitHub App write failed — repository transferred to another owner",
						"configured", moved.Configured, "current", moved.Current)
				},
				recordRepoPaused: func(repo string) {
					recordAdvisoryPostSuccess(repo, time.Now())
				},
				recordWriteForbidden: func(repo string) {
					backoff := advCfg.PostRetryBackoff()
					recordAdvisoryPostForbidden(repo, time.Now(), backoff)
					if backoff > 0 {
						logger.Warn("advisory digest write forbidden — pausing post attempts",
							"repo", repo, "retry_after", backoff.String(),
							"knob", "governor.advisory.post_retry_backoff_s")
					}
				},
				onAuthFailure: func(ctx context.Context) {
					// Same verdict function as boot and Re-check, so a healthy
					// or unclassifiable probe cannot raise the banner here.
					raise, msg, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger)
					if !raise {
						return
					}
					dashSrv.SetGitHubAppRequired(true)
					if msg != "" {
						dashSrv.SetGitHubAppPermIssue(msg)
					}
					dashSrv.SetGitHubAppState(state.String())
					logger.Warn("GitHub App authentication failed posting advisory digest",
						"repo", primaryRepo, "state", state.String(),
						"operator_actionable", state.OperatorActionable())
				},
				onWriteProven: func(ctx context.Context) {
					// A successful write proves the app is installed AND has
					// write access — clear BOTH the perm issue and the
					// app-required flag, or the "Not Installed" banner sticks
					// forever despite tokens working.
					dashSrv.SetGitHubAppPermIssue("")
					dashSrv.SetGitHubAppRequired(false)
					dashSrv.ClearPendingGitHubAppInstall()
					// The same proof retires stale ACCESS findings (#2575).
					if healed := advisory.CloseHealedAppAuthFindings(beadStores); len(healed) > 0 {
						logger.Info("closed healed GitHub App access findings after successful App digest post",
							"count", len(healed), "titles", strings.Join(healed, "; "))
					}
					// Repo-ACCESS findings need their own proof: a digest post
					// only proves issues:WRITE, so verify with a real
					// advisor-scoped Contents read, memoized per repo.
					readVerified := map[string]bool{}
					canRead := func(ownerRepo string) bool {
						target := ownerRepo
						if target == "" {
							target = primaryRepo
						}
						owner, name := cfg.Project.Org, target
						if i := strings.LastIndex(target, "/"); i > 0 {
							owner, name = target[:i], target[i+1:]
						}
						if owner == "" || name == "" {
							return false
						}
						key := owner + "/" + name
						if v, ok := readVerified[key]; ok {
							return v
						}
						appAuth := ghClient.AppAuth()
						if appAuth == nil {
							// Static-token client: no advisor-tier token can be
							// minted, so leave the finding open.
							return false
						}
						err := appAuth.VerifyRepoRead(ctx, owner, name)
						if err != nil {
							logger.Info("repo-access finding left open: advisor read probe failed",
								"repo", key, "error", err)
						}
						readVerified[key] = err == nil
						return readVerified[key]
					}
					if healed := advisory.CloseHealedRepoAccessFindings(beadStores, canRead); len(healed) > 0 {
						logger.Info("closed healed repo-access findings after verified advisory read path",
							"count", len(healed), "titles", strings.Join(healed, "; "))
					}
				},
			}, logger)
		}
	} else if d := dashSrv.GetAdvisoryDigest(); d != nil {
		statusPayload.AdvisoryDigest = d
	}

	if !statusPublished {
		dashSrv.UpdateStatusIfFresh(statusPayload, buildEpoch)
	}

	publishFleetReportsWith(ctx, logger, dashSrv, statusPayload.FleetReport, newSelfReportPublishDeps(cfg, ghClient, dashSrv))

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
		if err := nousState.RecordSnapshot(govState, actionable, agentsDue, agentMgr.AllStatuses(), tokenSummary); err != nil {
			logger.Warn("failed to record nous snapshot", "error", err)
		}
	}
}
