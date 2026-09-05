package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	"gopkg.in/natefinch/lumberjack.v2"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/discord"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/holdguard"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/hub"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/toolapprove"
	"github.com/hivecommons/hive/pkg/tracing"
	"github.com/hivecommons/hive/pkg/watchdog"
	"github.com/hivecommons/hive/pkg/watsonx"
	"github.com/hivecommons/hive/pkg/worksource"
	"go.opentelemetry.io/otel/attribute"
)

// Dashboard system-alert IDs for the budget thresholds.
const (
	budgetWarnAlertID      = "budget-warn"
	budgetExhaustedAlertID = "budget-exhausted"
	// noCadenceAlertID is the never-kicked cause+fix banner (#5577): enabled
	// agents with no cadence in any mode and no kick ever.
	noCadenceAlertID = "agent-no-cadence"
	// providerBudgetAlertID is the PROVIDER spend rebuff (#4294), kept distinct
	// from the two token-budget alerts above so an operator can tell "we used
	// our token allowance" from "the gateway will not spend more money".
	providerBudgetAlertID = "provider-budget-exceeded"
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

// applyBudgetAlerts turns budget threshold crossings into dashboard system
// alerts and notifications. Crossings fire once per window (governor tracks
// the one-shot flags); alerts are cleared when the threshold no longer
// applies (window rolled, limit raised, or budgeting disabled).
func applyBudgetAlerts(gov *governor.Governor, trans governor.BudgetTransitions, dashSrv *dashboard.Server, notifier *notify.Notifier) {
	if !trans.WarnActive {
		dashSrv.ClearSystemAlert(budgetWarnAlertID)
	}
	if !trans.ExhaustedActive {
		dashSrv.ClearSystemAlert(budgetExhaustedAlertID)
	}

	budget := gov.GetBudget()
	if trans.WarnCrossed {
		msg := fmt.Sprintf("token budget at %d%%+ of weekly limit: %d of %d tokens used",
			governor.BudgetWarnPct, budget.CurrentSpend, budget.WeeklyLimit)
		dashSrv.AddSystemAlert(budgetWarnAlertID, "warning", msg)
		notifier.Send("Budget warning", msg, notify.PriorityDefault)
	}
	if trans.ExhaustedCrossed {
		windowEnd := budget.ResetAt.Add(governor.BudgetWindowDuration)
		msg := fmt.Sprintf("token budget exhausted: %d of %d tokens used — agent kicks suspended until %s (exempt agents keep running)",
			budget.CurrentSpend, budget.WeeklyLimit, windowEnd.Format(time.RFC1123))
		dashSrv.AddSystemAlert(budgetExhaustedAlertID, "error", msg)
		notifier.Send("Budget exhausted", msg, notify.PriorityHigh)
	}
}

// applyNoCadenceAlert keeps the never-kicked cause+fix banner (#5577) in sync
// with the governor's view: raised (warning, not error — the hive is not
// broken, it is unconfigured) while any enabled, governor-kickable agent has
// no cadence in any mode and has never been kicked; cleared the moment the
// operator sets a cadence or any kick path reaches the agent. This is the
// spoke-side parity for the hub verdict's no-cadence amber: the same
// governor-derived signal, rendered where the operator can act on it, with no
// hub round-trip.
func applyNoCadenceAlert(gov *governor.Governor, dashSrv *dashboard.Server) {
	agents := gov.NoCadenceAgents()
	if len(agents) == 0 {
		dashSrv.ClearSystemAlert(noCadenceAlertID)
		return
	}
	dashSrv.AddSystemAlert(noCadenceAlertID, "warning", noCadenceAlertMessage(agents))
}

// noCadenceAlertMessage renders the banner line: symptom, cause AND fix — the
// exact gap the RFC calls out in the dashboard's not-producing warnings,
// which name only the symptom.
func noCadenceAlertMessage(agents []string) string {
	return fmt.Sprintf("agent(s) %s enabled but never kicked — no cadence configured; set cadences on the agent card",
		strings.Join(agents, ", "))
}

// agentKicker adapts *agent.Manager to planning.Kicker for the Phase 3
// stall-replan lane. Kick delegates to SendKick, which takes the manager lock
// ITSELF and is only ever called here from the governor tick (never from the
// agent-launch path), so it cannot re-enter a held manager lock. This is the
// same out-of-band kick path the eval loop already uses for governor kicks.
type agentKicker struct{ mgr *agent.Manager }

func (k agentKicker) Kick(agent, message string) error {
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
	agentMgr *agent.Manager,
	gov *governor.Governor,
	dashSrv *dashboard.Server,
	logger *slog.Logger,
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

	sink := labelPlanSink{gov: gov, dashSrv: dashSrv, logger: logger}
	planning.PlanIssuesFromLabels(store, agentMgr, actionable.Issues.Items, sink,
		func(ref string, err error) {
			logger.Warn("plan-from-label: minting epic failed", "issue", ref, "error", err)
		}, acmmLevel)
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

func (s labelPlanSink) QueuedPlan(epic *beads.Bead, paused bool) {
	if paused {
		// Architect deliberately paused — queue, do not unpause, log once.
		s.logger.Info("plan-from-label: architect paused, plan queued", "epic", epic.ID, "ref", epic.ExternalRef)
		return
	}
	s.logger.Warn("plan-from-label: architect unavailable, plan queued", "epic", epic.ID, "ref", epic.ExternalRef)
}

// healGitHubAppInstallation self-heals a hive whose github.installation_id
// points at the WRONG account — the failure mode diagnoseGitHubApp
// already detects and reports ("installation N belongs to 'X', not 'Y'"). It
// asks pkg/github to rediscover the installation covering cfg.Project.Org via
// the App JWT and, only on an unambiguous match, adopts it in place and
// persists it so the fix survives a pod restart.
//
// Every failure path is soft and silent-ish: a hive with no App key is not
// App-authenticated (skip), an API error or an ambiguous/absent discovery
// result leaves installation_id exactly as configured so the existing
// "check github.installation_id" banner still stands. It never returns an
// error to the caller and never blocks startup or a heartbeat.
//
// Rediscovery is rate-limited by pkg/github's discovery cache
// (github.InstallationDiscoveryTTL), so calling this from the self-heal tick
// is cheap even when the App is genuinely not installed on the org.
func healGitHubAppInstallation(ctx context.Context, appAuth *github.AppAuth, cfg *config.Config, logger *slog.Logger) {
	if appAuth == nil || !appAuth.HasKey() || cfg == nil {
		return
	}
	org := cfg.Project.Org
	if org == "" {
		return
	}
	newID, err := appAuth.RediscoverAndAdopt(ctx, org, logger)
	if err != nil {
		logger.Debug("github app installation rediscovery did not adopt a new id",
			"org", org, "error", err)
		return
	}
	if newID == 0 {
		return // already correct, or nothing safe to adopt
	}
	cfg.GitHub.InstallationID = newID
	if err := cfg.Save(); err != nil {
		logger.Error("adopted rediscovered installation_id but failed to persist it — "+
			"it will revert on the next pod restart",
			"installation_id", newID, "error", err)
		return
	}
	logger.Info("persisted rediscovered github app installation_id",
		"installation_id", newID, "org", org)
}

// diagnoseGitHubApp classifies this hive's GitHub App credential state and
// returns both the machine-readable state and banner-ready copy.
//
// It supersedes a substring match on the formatted error ("403"/"401"), which
// could not tell a user-side failure from an operator-side one and so showed
// every hive the same "GitHub App Not Installed" banner. The most damaging
// case that fixes: a spoke holding the WRONG private key (the hub's key push
// has not landed, or delivered a public github.com key to a GitHub Enterprise
// hive) gets `401 A JSON web token could not be decoded`. The user cannot see,
// supply, or correct that key — telling them to install the App or check
// github.installation_id sends them to redo work they already did correctly.
//
// The candidate key paths are passed so a MISSING key is detected without any
// API round-trip at all.
//
// Returns ("", AppStateOK) when App auth is healthy, and ("", state) for a nil
// appAuth (a token-authenticated hive has nothing to check).
func diagnoseGitHubApp(ctx context.Context, appAuth *github.AppAuth, expectedOwner string) (string, github.AppAuthState) {
	d := diagnoseGitHubAppFull(ctx, appAuth, expectedOwner)
	return d.Message(), d.State
}

// diagnoseGitHubAppFull is diagnoseGitHubApp without the lossy projection to
// (message, state). Callers that only need the banner should keep using the
// wrapper above; this exists for the one caller that also reports the granted
// Actions and Commit-statuses permissions (#4030), which the projection drops.
func diagnoseGitHubAppFull(ctx context.Context, appAuth *github.AppAuth, expectedOwner string) github.AppAuthDiagnosis {
	if appAuth == nil {
		return github.AppAuthDiagnosis{State: github.AppStateOK, ExpectedAccount: expectedOwner}
	}
	return appAuth.DiagnoseAppAuth(ctx, expectedOwner, appKeys.DataKeyPath, appKeys.ProvisionedKeyPath)
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

// githubRateLimitErrText is the substring GitHub's client surfaces on a rate or
// abuse limit. Matching text is acceptable ONLY here: a rate limit is a reason
// to skip classification entirely, never a reason to accuse anyone of anything,
// so a false negative costs one extra (correct) classification round-trip.
const githubRateLimitErrText = "rate limit"

// isGitHubRateLimitText reports whether an error looks like a GitHub rate limit.
func isGitHubRateLimitText(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), githubRateLimitErrText)
}

// githubAppBannerAttempts is how many times classifyGitHubAppFailure probes
// before accepting an unclassifiable (AppStateUnknown) verdict. A cold start
// races the cluster's DNS/egress-proxy readiness, so the FIRST App call a pod
// makes is the one most likely to fail for reasons that have nothing to do
// with the App. One retry converts that transient into a correct verdict.
const githubAppBannerAttempts = 2

// githubAppBannerRetryDelay spaces those attempts. Short enough not to stall
// boot, long enough for an egress proxy or DNS cache to come up.
const githubAppBannerRetryDelay = 3 * time.Second

// classifyGitHubAppFailure is the SINGLE decision point for "should the GitHub
// App banner be raised?" — used by the boot path, the advisory-digest path and
// the manual Re-check button alike, so those three can never again disagree
// about the same hive.
//
// It exists because they DID disagree. The boot path used to raise the banner
// on a substring match for "403"/"401" in a formatted error string (the exact
// pattern #2224 replaced), set githubAppRequired=true UNCONDITIONALLY, and
// then classify — never lowering the flag again when classification came back
// healthy or inconclusive. Re-check ran the very same diagnoseGitHubApp probe
// but treated an empty diagnosis as success and cleared the banner. Same
// evidence, opposite conclusion: the banner appeared on every cold start whose
// first advisory-issue call blipped, and vanished the moment the user clicked
// Re-check without anything having been fixed.
//
// Two rules make the verdict trustworthy:
//
//  1. Only a state that is genuinely actionable raises the banner. AppStateOK
//     obviously does not, and neither does AppStateUnknown — #2224 defines it
//     as "we could not reach a conclusion", and escalating on it is precisely
//     the false accusation that design was meant to prevent.
//  2. An unknown verdict is retried before it is accepted, so a transient
//     startup network failure is not mistaken for a definitive one.
//
// Returns raise=false with an empty message when the App is fine or when we
// simply cannot tell.
func classifyGitHubAppFailure(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, logger *slog.Logger) (raise bool, msg string, state github.AppAuthState) {
	var d github.AppAuthDiagnosis
	for attempt := 1; attempt <= githubAppBannerAttempts; attempt++ {
		d = diagnoseGitHubAppFull(ctx, appAuth, expectedOwner)
		msg, state = d.Message(), d.State
		if state != github.AppStateUnknown {
			break
		}
		if attempt < githubAppBannerAttempts {
			logger.Debug("github app classification inconclusive — retrying before accepting a verdict",
				"attempt", attempt, "owner", expectedOwner)
			select {
			case <-ctx.Done():
				return false, "", github.AppStateUnknown
			case <-time.After(githubAppBannerRetryDelay):
			}
		}
	}

	// AppStateUnknown must never raise the banner: we did not get an answer
	// from GitHub, and a hive whose App is perfectly healthy would otherwise
	// be told to reinstall it because of a momentary network fault. The
	// self-heal loop and the next eval cycle both re-probe, so deferring the
	// verdict costs nothing but a delay on a hive that IS genuinely broken.
	if state == github.AppStateUnknown {
		logger.Warn("github app state could not be determined — leaving the banner down rather than guessing",
			"owner", expectedOwner)
		return false, "", github.AppStateUnknown
	}

	// #4030: record the Actions and Commit-statuses grants alongside the
	// verdict. These are NOT required of the Hive App and their absence is not
	// a fault — the optional Visual Hive App exists so they never have to be.
	// But an installation that has not approved them is otherwise
	// indistinguishable from one that has, both reporting "ok", which is what
	// would make a half-approved fleet invisible during any later
	// consolidation.
	//
	// It is deliberately emitted for EVERY verdict, including AppStateOK.
	// Gating it on a fault would defeat the purpose: the half-approved
	// installation is the one that looks healthy. It costs no extra API call —
	// the diagnosis above already fetched the installation.
	//
	// Note this runs where verdicts are computed, not on every eval cycle:
	// every caller reaches here from a failed GitHub call or from the
	// dashboard's Re-check. Re-check is therefore the operator-invokable way to
	// read a specific installation's grants.
	// #5774: record the write-path grants on the SAME line, for the same
	// reason and with the same posture. The App migration that blocked every
	// agent PR flow was invisible here because this verdict read Issues and
	// nothing else: a hive that could file issues and could not push a branch
	// reported "ok", and so did a healthy one. Contents/Pull-requests/Workflows
	// are recorded, never enforced — see GrantsAgentPushFlow for why requiring
	// them would misreport the read-only advisory tier — and, like the grants
	// above, they are emitted for EVERY verdict including AppStateOK, because
	// the installation that looks healthy is precisely the one worth counting.
	logger.Info("github app credential verdict",
		"owner", expectedOwner, "state", state.String(),
		"grants", d.ExecutionGrants(),
		"visual_hive_execution_grants", d.GrantsVisualHiveExecution(),
		"push_flow_grants", d.PushFlowGrants(),
		"agent_push_flow_grants", d.GrantsAgentPushFlow())
	if state == github.AppStateOK {
		return false, "", github.AppStateOK
	}
	return true, msg, state
}

// classifyGitHubAppWriteForbidden (#2353) is the verdict for a REAL write that
// returned 403 "Resource not accessible by integration". Authentication-only
// health checks (classifyGitHubAppFailure / diagnoseGitHubApp) inspect the
// installation's granted PERMISSIONS but never whether the target repo is in
// the installation's `selected` repositories — so they return AppStateOK for a
// repo the App cannot write, and the write failure stayed invisible to health
// (githubAppState=None) or, worse, got hard-overridden into a false "lacks
// Issues: Read & Write" banner.
//
// The rule here keeps attribution honest:
//
//   - If diagnoseGitHubApp finds a genuine, classifiable App-auth problem
//     (wrong installation, missing key, insufficient PERMISSION, etc.), report
//     THAT — it is the real cause and its copy is already accurate.
//   - If diagnoseGitHubApp reports the installation is healthy (AppStateOK:
//     right owner, issues:write granted) OR could not reach a verdict
//     (AppStateUnknown), the write 403 is still real and must NOT be silently
//     healthy. Report AppStateWriteForbidden with copy that names the repo and
//     the likeliest cause (repo not in the installation's selected repos),
//     WITHOUT inventing a permission gap that the diagnosis just proved absent.
//
// It always raises: a write that returned 403 is a genuine, standing failure to
// surface, distinct from the transient/unknown probe failures
// classifyGitHubAppFailure guards against.
func classifyGitHubAppWriteForbidden(ctx context.Context, appAuth *github.AppAuth, expectedOwner, repo string) (msg string, state github.AppAuthState) {
	diagMsg, diagState := diagnoseGitHubApp(ctx, appAuth, expectedOwner)
	if diagState != github.AppStateOK && diagState != github.AppStateUnknown {
		// A real, classifiable App-auth problem — its message is the accurate
		// one (e.g. wrong-installation, key-missing, or a genuine permission
		// gap). Use it as-is rather than masking it with the repo-scope story.
		return diagMsg, diagState
	}
	// Installation authenticates and (per the diagnosis) holds issues:write, yet
	// the write was forbidden. Attribute it to the one thing the permission
	// check cannot see — repo scope — and name the repo so the fix is concrete.
	d := github.AppAuthDiagnosis{
		State:           github.AppStateWriteForbidden,
		ExpectedAccount: expectedOwner,
		Repo:            repo,
	}
	return d.Message(), github.AppStateWriteForbidden
}

// classifyGitHubAppRepoCoverage (#4360) asks the deterministic question the
// other classifiers cannot: does this installation actually COVER the repos
// this hive is configured to work on?
//
// Everything else here reasons from a failed call. diagnoseGitHubApp inspects
// installation-level PERMISSIONS and never repo scope, and
// classifyGitHubAppWriteForbidden infers scope from a 403 after a write has
// already failed. Neither can see the case that prompted this: a hive pointed
// at a second repo in the right org, on the right installation, simply not
// ticked in the App's selected repos. GitHub answers 404 for that — the same
// answer it gives for a repo that does not exist — so the read path reported
// "app not installed / no read" and the dashboard blamed an undelivered
// private key that had in fact arrived. The operator was sent to re-upload a
// key, which could not possibly help.
//
// An error here is NOT a verdict. If the listing cannot be fetched the
// credentials themselves are the more likely story, and the existing checks
// tell it better; this returns raise=false and lets them run.
func classifyGitHubAppRepoCoverage(ctx context.Context, appAuth *github.AppAuth, org string, repos []string, logger *slog.Logger) (raise bool, msg string, state github.AppAuthState) {
	if appAuth == nil || len(repos) == 0 {
		return false, "", github.AppStateUnknown
	}

	cov, err := appAuth.InstallationCoverage(ctx)
	if err != nil {
		logger.Debug("github app repo coverage: could not list installation repositories — deferring to the credential checks",
			"org", org, "error", err)
		return false, "", github.AppStateUnknown
	}

	missing := cov.Missing(org, repos)
	if len(missing) == 0 {
		return false, "", github.AppStateOK
	}

	// #5774: a coverage miss whose shape is an org TRANSFER gets its own
	// verdict, checked first because the not-covered copy is actively wrong for
	// it. "Tick this repo in the installation's repository access" cannot be
	// followed when the repository has left that account — there is nothing
	// there to tick — and this classifier exists in the first place because
	// sending an operator to a fix that cannot work costs them real debugging
	// time. MovedTo returns nothing unless the shape is unambiguous (see its
	// three clauses), so the not-covered verdict below remains the default.
	if moves := cov.MovedTo(org, repos); len(moves) > 0 {
		d := github.AppAuthDiagnosis{
			State:           github.AppStateRepoMoved,
			ExpectedAccount: org,
			InstallationID:  appAuth.InstallationID(),
			APIURL:          appAuth.APIURL(),
			RepoMoves:       moves,
		}
		logger.Warn("github app repo coverage: configured repositories were transferred to another account",
			"configured_org", org, "now_under", github.MovedOwner(moves), "repos", len(moves))
		return true, d.Message(), github.AppStateRepoMoved
	}

	d := github.AppAuthDiagnosis{
		State:           github.AppStateRepoNotCovered,
		ExpectedAccount: org,
		InstallationID:  appAuth.InstallationID(),
		APIURL:          appAuth.APIURL(),
		Repos:           missing,
	}
	return true, d.Message(), github.AppStateRepoNotCovered
}

// primaryAdvisoryRepo returns the repo the advisory digest is posted to: the
// configured primary repo, falling back to the first listed repo. Shared by the
// boot ensure, the per-cycle re-ensure, and the post path so all three can never
// disagree about WHICH repo's pinned issue the digest belongs to.
func primaryAdvisoryRepo(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.Project.PrimaryRepo != "" {
		return cfg.Project.PrimaryRepo
	}
	if len(cfg.Project.Repos) > 0 {
		return cfg.Project.Repos[0]
	}
	return ""
}

// advisoryIssueUnresolved reports whether the pinned advisory issue for repo is
// still unknown, i.e. the digest has nowhere to go. A recorded 0 counts as
// unresolved: it is the zero value a failed ensure would leave behind, and
// posting to issue 0 is not a thing.
func advisoryIssueUnresolved(advisoryIssues map[string]int, repo string) bool {
	num, ok := advisoryIssues[repo]
	return !ok || num <= 0
}

func advisoryIssueNumber(advisoryIssues map[string]int, repo string) (int, bool) {
	num, ok := advisoryIssues[repo]
	return num, ok && num > 0
}

func shouldBuildAdvisoryDigest(beadStores map[string]*beads.Store, ghClient *github.Client, hasExistingPinnedIssue bool) bool {
	if len(beadStores) > 0 {
		return true
	}
	return ghClient != nil && hasExistingPinnedIssue
}

func shouldPostAdvisoryDigest(digest *advisory.Digest, ghClient *github.Client, hasPinnedIssue bool) bool {
	if digest == nil {
		return false
	}
	if digest.TotalCount > 0 || len(digest.RecentlyResolved) > 0 {
		return true
	}
	return ghClient != nil && hasPinnedIssue
}

// advisoryPostGate tracks, per repo, when the digest was last SUCCESSFULLY
// posted, so governor.advisory.update_interval_s (#4820) can throttle the
// GitHub round-trip. Package-level because runEvalCycle carries no state of
// its own, and mutex-guarded because startup/restart call sites exist besides
// the ticker. clampLogged makes the "interval clamped" warning a one-shot
// instead of a per-cycle drone.
var advisoryPostGate = struct {
	mu          sync.Mutex
	lastSuccess map[string]time.Time
	clampLogged bool
}{lastSuccess: map[string]time.Time{}}

// advisoryPostDue reports whether the update-interval gate is open for a post
// attempt to repo, logging (once) if the configured value was clamped. An
// interval of 0 (unset knob) means the gate is ALWAYS open — the digest posts
// every eval cycle, exactly the pre-#4820 cadence — and a repo with no
// successful post since process start is open too, so the first post is never
// delayed. The gate advances only on SUCCESS (recordAdvisoryPostSuccess,
// mirroring how the #4818 skip-guard records its hash): a failed attempt is
// retried on the very next cycle instead of waiting out the interval, keeping
// error recovery — and the hub's staleness signal — as prompt as today.
func advisoryPostDue(advCfg config.AdvisoryConfig, repo string, now time.Time, logger *slog.Logger) bool {
	interval := advCfg.UpdateInterval()
	advisoryPostGate.mu.Lock()
	defer advisoryPostGate.mu.Unlock()
	if raw := advCfg.UpdateIntervalS; raw > 0 && time.Duration(raw)*time.Second != interval && !advisoryPostGate.clampLogged {
		advisoryPostGate.clampLogged = true
		logger.Warn("advisory update_interval_s outside allowed bounds — clamped",
			"configured_s", raw, "effective_s", int(interval.Seconds()),
			"min_s", config.MinAdvisoryUpdateIntervalS, "max_s", config.MaxAdvisoryUpdateIntervalS)
	}
	if interval <= 0 {
		return true
	}
	last, ok := advisoryPostGate.lastSuccess[repo]
	return !ok || now.Sub(last) >= interval
}

// recordAdvisoryPostSuccess advances the update-interval gate for repo after a
// successful digest write. Skip-if-unchanged cycles count too: pkg/github
// returns nil for them by design so freshness advances (#4818/#4821), and an
// unchanged digest is exactly the case the throttle exists to quiet.
func recordAdvisoryPostSuccess(repo string, now time.Time) {
	advisoryPostGate.mu.Lock()
	defer advisoryPostGate.mu.Unlock()
	advisoryPostGate.lastSuccess[repo] = now
}

// advisoryIssueMissingError is the error recorded (and reported to the hub) when
// a hive has findings to publish but no advisory issue to publish them to. It is
// deliberately an ERROR rather than a silent skip: the hub's staleness gate
// treats a hive reporting neither a post time nor an error as "not an advisory
// participant" and never alarms, which is how a wedged digest went unnoticed for
// six days in #4167.
func advisoryIssueMissingError(repo string, cause error) string {
	base := fmt.Sprintf("no advisory issue resolved for %s — digest not posted", repo)
	// Issues-disabled is the one ensure failure with a remedy the OPERATOR of
	// the target repo owns (#4329): flipping a repo setting, not fixing App
	// auth. Fold its actionable message into the alert text so the fleet
	// stale-advisory pill says so instead of reading like an auth failure.
	var disabled *github.IssuesDisabledError
	if errors.As(cause, &disabled) {
		return base + ": " + disabled.Error()
	}
	return base
}

// actionableAfterGitHubEnumerate decides whether an eval cycle survives a
// failed GitHub enumeration. On the default (GitHub) work source the answer
// is no: an all-repos failure usually means a rate limit or outage, and a
// zero-count result would idle the agents, so the cycle keeps prior state.
//
// On a non-default work source (e.g. Linear) the GitHub call is only there
// for PR maintenance; the backlog comes from the work-source overlay that
// runs next. Aborting here meant a Linear-sourced hive whose GitHub App could
// not list issues (403 "Resource not accessible by integration", an Issues
// permission a Linear hive should not need) never enumerated its Linear
// backlog at all and sat at queue 0. Such a hive continues with whatever
// partial result GitHub returned (nil becomes an empty result; PRs are kept
// when obtainable) and lets the overlay populate issues.
func actionableAfterGitHubEnumerate(cfg *config.Config, actionable *github.ActionableResult, err error, logger *slog.Logger) (*github.ActionableResult, bool) {
	if err == nil {
		return actionable, true
	}
	wsType := cfg.Governor.WorkSource.Type
	if wsType == "" || wsType == "github" {
		logger.Error("failed to enumerate actionable items", "error", err)
		return nil, false
	}
	logger.Warn("GitHub enumeration failed; continuing so the configured work source can still populate issues",
		"work_source", wsType, "error", err)
	if actionable == nil {
		actionable = &github.ActionableResult{GeneratedAt: time.Now()}
	}
	return actionable, true
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
	restartedAgents []string,
	approvalDesk *toolapprove.Desk,
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
	var advisoryEnsureErr error
	primaryRepoAtCycleStart := primaryAdvisoryRepo(cfg)
	_, hadPinnedAdvisoryIssueAtCycleStart := advisoryIssueNumber(advisoryIssues, primaryRepoAtCycleStart)
	if primaryRepoAtCycleStart != "" && ghClient != nil {
		if advisoryIssueUnresolved(advisoryIssues, primaryRepoAtCycleStart) {
			num, retryErr := ghClient.EnsureAdvisoryIssue(ctx, primaryRepoAtCycleStart)
			if retryErr == nil {
				advisoryIssues[primaryRepoAtCycleStart] = num
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
				logger.Info("advisory issue resolved on retry", "repo", primaryRepoAtCycleStart, "number", num)
			} else {
				advisoryEnsureErr = retryErr
				logger.Warn("advisory issue still unresolved — digest cannot be posted this cycle",
					"repo", primaryRepoAtCycleStart, "error", retryErr)
			}
		}
	}

	enumCtx, enumSpan := tracing.StartSpan(ctx, "governor.enumerate_actionable")
	actionable, err := ghClient.EnumerateActionable(enumCtx)
	enumSpan.End()
	actionable, ok := actionableAfterGitHubEnumerate(cfg, actionable, err, logger)
	if !ok {
		return
	}

	// If a non-default work source is configured, overlay its issues onto
	// the actionable result. PRs always come from GitHub.
	if wsType := cfg.Governor.WorkSource.Type; wsType != "" && wsType != "github" {
		ghToken := cfg.GitHub.Token
		if ghToken == "" {
			ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
		}
		ws, wsErr := worksource.FromConfig(cfg.Governor.WorkSource, ghClient, ghToken, cfg.Project.Org, logger)
		actionable.Issues = workSourceIssuesForCycle(ctx, ws, wsErr, cfg.Governor.Labels.Exempt, cfg.Project.IssueFilter, logger)
	}

	ghClient.EnrichCIStatus(ctx, actionable.PRs.Items)

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
	applyDuplicatePRGuard(ctx, cfg, ghClient, actionable, logger)

	lastActionable.Store(actionable)
	if data, err := json.Marshal(actionable); err == nil {
		atomicWrite("/data/last-actionable.json", data)
	}

	// Record enumerated issues into the lifecycle timeline so the dashboard's
	// lifecycle view has real data. Cheap and fully guarded: a nil dashboard or
	// nil store is a no-op (timeline.Store.Record is nil-safe), the loop is
	// bounded by maxTimelineEnumeratePerCycle, and the store dedupes by
	// (ref, kind) so this per-cycle sweep refreshes journeys instead of
	// flooding them (#5656).
	recordEnumeratedIssues(ctx, dashSrv, actionable)

	escalatedPRs := runEscalationSweep(ctx, cfg, governorForge(cfg, ghClient, logger), actionable, notifier, dashSrv, logger)

	intentVerdicts := writeIntentVerdicts(ctx, cfg, ghClient, actionable, beadStores, logger)
	refreshReviewVerdicts(cfg, logger)
	requiredCheckSet, _ := cfg.AutoMerge.RequiredCheckSet()

	// Hold guard (#5589): snapshot hold-gated PR heads, and when a hold lifts
	// on a branch that moved, block the merge lanes and force a fresh review.
	// Runs before writeMergeEligible so drifted PRs are excluded from the very
	// tick their hold lifted — no window for the sweep to race the re-hold.
	holdDriftPRs := enforceHoldGuard(ctx, cfg, ghClient, governorForge(cfg, ghClient, logger), actionable, logger)

	writeMergeEligible(actionable, actionable.Hold, cfg.Project.Org, escalatedPRs, cfg.Intent.Enforce, intentVerdicts, cfg.Review.RequireApproval, requiredCheckSet, holdDriftPRs, logger)

	// Stuck-PR reaper (backstop): re-dispatch a fix for any hive-authored PR
	// that is red on a required check AND stale (its red head SHA unchanged past
	// RedPRStaleAfter). writeMergeEligible already surfaces every red PR into
	// ci-failing.json (the CI_FAILING kick block), so the PR is already in the
	// work list; the reaper's job is to guarantee a STALE one is not silently
	// abandoned, to dedup the dispatch via the escalation store's re-engagement
	// cap (so a permanently-red PR is not re-nudged every tick forever), and to
	// stand down for PRs already escalated to a human. Composes with the merge
	// watcher (#2): both go through the same TryReEngage cap, so the same PR is
	// never double-dispatched within a red-SHA's budget.
	reapStuckRedPRs(cfg, actionable, escalatedPRs, logger)

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

	// Refresh budget spend from lifetime token totals before Evaluate so
	// the kick gate sees current-window numbers.
	if tokenCollector != nil {
		if summary := tokenCollector.Summary(); summary != nil {
			trans := gov.UpdateBudgetFromTotals(summary.TotalTokens, summary.ByAgent, summary.ByModel)
			applyBudgetAlerts(gov, trans, dashSrv, notifier)
		}
	}

	// Cause+fix banner for the never-kicked class (#5577): the dashboard's
	// not-producing warnings name the SYMPTOM (agent idle, zero tokens); this
	// names the cause — enabled agent, no cadence in any mode, never kicked —
	// and the fix. Computed from the spoke's own governor config, no hub
	// round-trip; self-clears the moment a cadence is set or any kick lands.
	applyNoCadenceAlert(gov, dashSrv)

	agentsDue := gov.Evaluate(
		actionable.Issues.Count,
		actionable.PRs.Count,
		actionable.Hold.Total,
		actionable.Issues.SLAViolations,
	)

	// Crash-restarted agents may get a "resume" kick ahead of their cadence
	// slot so work interrupted mid-task resumes promptly — but ONLY through
	// the governor's gate (#2573). Unconditionally kicking every restarted
	// agent meant a crash-looping CLI was kicked on every eval cycle,
	// burning backend tokens far faster than any configured cadence and
	// bypassing the budget gate; AllowResumeKick bounds resume kicks to one
	// per cadence interval and respects mode pauses and the budget.
	agentsDue = mergeResumeKicks(agentsDue, restartedAgents, gov.AllowResumeKick, logger)

	govState := gov.GetState()
	span.SetAttributes(
		attribute.String(tracing.AttrHiveGovernorMode, string(govState.Mode)),
		attribute.Int("hive.queue.issues", govState.QueueIssues),
		attribute.Int("hive.queue.prs", govState.QueuePRs),
		attribute.Int("hive.queue.hold", govState.QueueHold),
	)
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
	// Operator-paused agents must consume NOTHING (#2573); see
	// filterKickableAgents for the full gate.
	agentsDue = filterKickableAgents(agentsDue, cfg.Agents, config.OnDemandAgentsFromPacks(), agentMgr.IsPaused)

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
	if providerBudgetLatched {
		state := "agent kicks suspended"
		if !suppressKicks {
			state = "probing with a single agent kick to test whether the provider window has reset"
		}
		msg := fmt.Sprintf("provider spending limit reached — %s: %s", state, providerBudgetCause)
		if providerBudgetRebuffs > 1 {
			msg = fmt.Sprintf("provider spending limit reached (%d refused calls since %s) — %s: %s",
				providerBudgetRebuffs, providerBudgetSince.Format(time.RFC1123), state, providerBudgetCause)
		}
		dashSrv.AddSystemAlert(providerBudgetAlertID, "error", msg)
		providerBudgetCause = msg
	} else {
		if reason := hub.QuotaExhaustedAgentReason(hub.QuotaExhaustedProcessCount(agentMgr.AllStatuses())); reason != "" {
			dashSrv.AddSystemAlert(providerBudgetAlertID, "error", "provider quota exhausted — "+reason)
		} else {
			dashSrv.ClearSystemAlert(providerBudgetAlertID)
		}
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
	reviewPlan := planReviewDispatch(cfg, actionable, agentMgr, logger)
	messages := sched.BuildKickMessages(kickActionable, agentsDue)
	reviewKickByMessage := map[string]review.DispatchKick{}
	for _, k := range append(reviewPlan.ReviewKicks, reviewPlan.FixKicks...) {
		if !gov.AgentEligibleForCELKick(k.Agent) || agentMgr.IsPaused(k.Agent) {
			logger.Info("review swarm kick suppressed by governor gate", "agent", k.Agent, "pr", k.PRRef)
			continue
		}
		messages = append(messages, scheduler.KickMessage{Agent: k.Agent, Message: k.Message, IssueRefs: []string{k.PRRef}})
		reviewKickByMessage[k.Agent+"\x00"+k.Message] = k
	}
	// #4294: drop EVERY assembled kick while the provider is refusing on a
	// spending limit. Placed after all three sources have contributed —
	// governor-due agents, the CEL union, and the review swarm — because gating
	// only `agentsDue` earlier would still let a CEL match or a review kick fire
	// into the same clipped key.
	//
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
	if suppressKicks && len(messages) > 0 {
		logger.Warn("provider spending limit: withholding agent kicks",
			"withheld", kickGate.Withheld, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince,
			"next_probe_in", (providerBudgetProbeInterval - time.Since(providerBudgetProbe.Freshest(providerBudgetLastRebuff))).Truncate(time.Second))
	} else if kickGate.ReleaseProbe {
		if len(kickGate.Withheld) > 0 {
			logger.Warn("provider spending limit: withholding all but the probe kick",
				"withheld", kickGate.Withheld, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince)
		}
		providerBudgetProbe.MarkReleased(time.Now())
		logger.Info("provider spending limit: releasing a single probe kick",
			"probe_agent", kickGate.Kept[0].Agent, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince,
			"last_rebuff", providerBudgetLastRebuff, "probe_interval", providerBudgetProbeInterval)
	}
	messages = kickGate.Kept
	if notifyProviderBudget {
		notifier.Send("Provider spending limit reached", providerBudgetCause, notify.PriorityHigh)
	}

	var deliveredReviewKicks []review.DispatchKick
	if len(messages) > 0 {
		for _, msg := range messages {
			agentCfg := cfg.Agents[msg.Agent]
			_, kickSpan := tracing.StartSpan(ctx, "agent.kick", tracing.AgentKickAttributes(
				msg.Agent,
				agentCfg.Backend,
				agentCfg.Model,
				agentCfg.Role,
				string(govState.Mode),
				inferACMMLevel(cfg),
			)...)
			logger.Info("audit: governor kicking agent", "agent", msg.Agent, "trigger", "governor-eval")
			if err := agentMgr.SendKick(msg.Agent, msg.Message); err != nil {
				kickSpan.RecordError(err)
				kickSpan.End()
				logger.Warn("failed to send kick", "agent", msg.Agent, "error", err)
				continue
			}
			if k, ok := reviewKickByMessage[msg.Agent+"\x00"+msg.Message]; ok {
				deliveredReviewKicks = append(deliveredReviewKicks, k)
				persistReviewDispatchState(reviewPlan, deliveredReviewKicks, logger)
			}
			kickSpan.End()
			gov.RecordKick(msg.Agent)
			dashSrv.AuditLog("governor", "kick", "trigger=governor-eval", msg.Agent)

			// Record issue-scoped kicks into the lifecycle timeline. Cheap,
			// guarded, and nil-safe (Record no-ops on a nil dashboard/store).
			recordKick(ctx, dashSrv, msg.Agent, msg.IssueRefs...)

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
	scanForLoginRequired(ctx, cfg, agentMgr, notifier, dashSrv, logger, loginSightings)

	// Epoch captured before reading agent/governor state so a mutation that
	// lands mid-build (restart-count/budget reset) drops this snapshot instead
	// of letting it revert the mutation on the dashboard (#4348).
	buildEpoch := dashSrv.BeginStatusSnapshot()
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
			safeFindings := make([]advisory.Finding, 0, len(findings))
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
				blockFinding := false
				if cfg.Ioscan.IsEnabled() && cfg.Ioscan.Canaries {
					reportText := strings.Join([]string{f.Title, f.Detail, f.File, f.Type, f.Severity}, "\n")
					if leak, ok := ioscan.DefaultCanaries.Scan(f.Agent, reportText, "advisory-finding"); ok {
						detail := fmt.Sprintf("rule=%s, agent=%s, source=%s", ioscan.CanaryLeakRule, leak.Agent, leak.Source)
						dashSrv.AuditLog(leak.Agent, "ioscan_canary_leak", detail, leak.Agent)
						if store, ok := beadStores[leak.Agent]; ok && store != nil {
							if b, berr := store.Create("Canary token leaked via "+leak.Source, beads.TypeAdvisory, beads.PriorityCritical, leak.Agent, ""); berr == nil {
								_ = store.SetMetadata(b.ID, "rule", ioscan.CanaryLeakRule)
								_ = store.SetMetadata(b.ID, "source", leak.Source)
							}
						}
						blockFinding = cfg.Ioscan.FailClosed()
					}
				}
				if blockFinding {
					logger.Warn("ioscan fail-closed blocked advisory finding with canary leak", "agent", f.Agent)
					continue
				}
				safeFindings = append(safeFindings, f)
			}
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
	if acmmLvl := inferACMMLevel(cfg); cfg.Planning.PlanFromLabelEnabled(acmmLvl) &&
		approvalDeskAllowsLegacyOperation(ctx, approvalDesk, cfg, toolapprove.KindPlanFromLabel, "plan-from-label", planning.ArchitectAgentName, logger) {
		planFromLabeledIssues(actionable, beadStores, agentMgr, gov, dashSrv, logger, acmmLvl)
	}

	// Advisory digest: build from beads (the source of truth) before status broadcast.
	primaryRepo := primaryAdvisoryRepo(cfg)
	issueNum, hasPinnedAdvisoryIssue := advisoryIssueNumber(advisoryIssues, primaryRepo)
	hasExistingPinnedIssueForEmptyDigest := hasPinnedAdvisoryIssue &&
		primaryRepo == primaryRepoAtCycleStart &&
		hadPinnedAdvisoryIssueAtCycleStart
	if shouldBuildAdvisoryDigest(beadStores, ghClient, hasExistingPinnedIssueForEmptyDigest) {
		// Retire findings no agent has re-reported inside the staleness window
		// BEFORE the digest is built, so a stale finding never appears in the
		// comment one last time after it has been proven gone. Agents re-file a
		// finding for as long as its condition holds (beads.Store.Upsert), so
		// silence is the evidence here.
		advCfg := cfg.Governor.Advisory
		if advCfg.StalenessDays > 0 {
			if pruned := advisory.PruneStaleAdvisoryBeads(beadStores, time.Duration(advCfg.StalenessDays)*24*time.Hour); len(pruned) > 0 {
				logger.Info("closed stale advisory findings not re-reported within the staleness window",
					"count", len(pruned), "staleness_days", advCfg.StalenessDays, "titles", strings.Join(pruned, "; "))
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
		if ghClient != nil && org != "" && repoName != "" {
			branch := cfg.Policies.Branch
			if branch == "" {
				if r, _, rerr := ghClient.GetRepo(ctx, org, repoName); rerr == nil {
					branch = r.GetDefaultBranch()
				} else {
					logger.Warn("advisory: could not resolve default branch for snapshot", "repo", primaryRepo, "error", rerr)
				}
			}
			if branch != "" {
				if sha, serr := ghClient.LatestCommitHash(ctx, org, repoName, branch); serr == nil && sha != "" {
					digestOpts.Snapshot = &advisory.Snapshot{
						Owner:  org,
						Repo:   repoName,
						Branch: branch,
						SHA:    sha,
					}
					digestOpts.VerifyPath = func(path string) bool {
						exists, verr := ghClient.PathExistsAtRef(ctx, org, repoName, path, sha)
						if verr != nil {
							// Inconclusive check (network/rate-limit, not a 404):
							// treat as existing so a transient error never
							// mislabels a real path as outdated — and never
							// costs a real finding its top-N slot.
							logger.Warn("advisory: path existence check failed", "path", path, "repo", primaryRepo, "sha", sha, "error", verr)
							return true
						}
						return exists
					}
					logger.Info("advisory digest pinned to commit", "repo", primaryRepo, "branch", branch, "sha", sha)
				} else if serr != nil {
					logger.Warn("advisory: could not resolve latest commit for snapshot", "repo", primaryRepo, "branch", branch, "error", serr)
				}
			}
		}
		digest := advisory.BuildDigestFromBeads(beadStores, string(govState.Mode), digestOpts)
		if advisoryStore != nil {
			advisoryStore.SetLatestDigest(digest)
		}
		dashSrv.SetAdvisoryDigest(digest)
		statusPayload.AdvisoryDigest = digest

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
			if digest.TotalCount == 0 && len(digest.RecentlyResolved) == 0 {
				logger.Info("advisory digest empty — posting freshness marker",
					"repo", primaryRepo, "issue", issueNum)
			}

			md := advisory.FormatDigestMarkdown(digest, advisory.DigestOptions{
				MaxFindings: digestOpts.MaxFindings,
				ShowAll:     digestOpts.ShowAll,
				Org:         org,
				ShowEmpty:   digest.TotalCount == 0 && len(digest.RecentlyResolved) == 0,
				PrimaryRepo: repoName,
			})
			if md != "" {
				if advisoryTarget != config.AdvisoryTargetGitHub {
					// Non-GitHub route. A misconfiguration (Linear chosen with
					// no linear_issue, or an unknown target) is recorded as a
					// post FAILURE, never redirected to the GitHub issue: the
					// operator opted out of it, and the hub's staleness pill is
					// how they learn the digest has nowhere to go.
					if advisoryRouteErr != nil {
						dashSrv.RecordAdvisoryError(advisoryRouteErr.Error())
						logger.Error("advisory digest not posted: target misconfigured",
							"target", advisoryTarget, "error", advisoryRouteErr)
					} else if err := postAdvisoryDigestToLinear(ctx, cfg, advisoryLinearIssue, md); err != nil {
						dashSrv.RecordAdvisoryError(err.Error())
						logger.Warn("failed to post advisory digest to linear", "issue", advisoryLinearIssue, "error", err)
					} else {
						logger.Info("posted advisory digest", "linear_issue", advisoryLinearIssue, "findings", digest.TotalCount, "via", "linear")
						dashSrv.RecordAdvisoryPost(digest.TotalCount)
						recordAdvisoryPostSuccess(primaryRepo, time.Now())
						dashSrv.RecordAdvisoryOverflow(digest.OverflowCount)
					}
				} else if hasPinnedAdvisoryIssue {
					// Prefer the App client as the PRIMARY poster. The App
					// authored the advisory-digest comment and always holds
					// issues:write, so it is the correct identity to edit it.
					// The App banner must be driven ONLY by the App's own
					// error — never by a user-token failure. Otherwise a
					// user-token problem (kellyaa: expired token → 401;
					// kalantar: valid token but not repo-admin → 403 editing
					// the bot's own comment) would false-flag the App as "Not
					// Installed" even though the App itself works fine.
					if err := ghClient.PostAdvisoryDigest(ctx, primaryRepo, issueNum, md); err != nil {
						// The App is the sole advisory-digest writer. The former
						// user-token fallback was removed (issue #1927): it only
						// existed to post the digest under the logged-in user's
						// identity when the App failed, which is exactly the
						// owner-attributed write path we no longer want — and it
						// forced every dashboard login through the excessive "repo"
						// scope. Record the App error so the hub flags the digest
						// as stale with its specific cause. err.Error() is the same
						// string logged just below — log-safe, never key material.
						dashSrv.RecordAdvisoryError(err.Error())
						logger.Warn("failed to post advisory digest via app", "repo", primaryRepo, "issue", issueNum, "error", err)
						switch classifyAdvisoryPostError(err) {
						case advisoryPostWriteForbidden:
							// App is installed (we found the issue) but a real
							// WRITE was forbidden. #2353: attribute this honestly.
							// diagnoseGitHubApp only inspects installation-level
							// PERMISSIONS, so when it comes back healthy (issues:write
							// granted, right owner) the previous code hard-overrode
							// that "OK" into a false "lacks Issues: Read & Write"
							// banner — the exact misattribution #2353 reports. When
							// the diagnosis is genuinely a permission/installation
							// problem, use it; otherwise surface a DISTINCT
							// write-forbidden state naming the likeliest real cause
							// (the repo is not in the App installation's selected
							// repos), instead of leaving health at None or faking a
							// permission gap the diagnosis just disproved.
							msg, state := classifyGitHubAppWriteForbidden(ctx, ghClient.AppAuth(), cfg.Project.Org, primaryRepo)
							dashSrv.SetGitHubAppRequired(true)
							dashSrv.SetGitHubAppPermIssue(msg)
							dashSrv.SetGitHubAppState(state.String())
							logger.Warn("GitHub App write failed — cannot write issue comments",
								"repo", primaryRepo, "state", state.String(),
								"operator_actionable", state.OperatorActionable(), "detail", msg)
						case advisoryPostRateLimited:
							logger.Warn("GitHub API rate limit hit, skipping advisory digest post", "repo", primaryRepo)
						default:
							// Same verdict function as boot and Re-check, so a
							// healthy or unclassifiable probe cannot raise the
							// banner here either.
							raise, msg, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger)
							if raise {
								dashSrv.SetGitHubAppRequired(true)
								if msg != "" {
									dashSrv.SetGitHubAppPermIssue(msg)
								}
								dashSrv.SetGitHubAppState(state.String())
								logger.Warn("GitHub App authentication failed posting advisory digest",
									"repo", primaryRepo, "state", state.String(),
									"operator_actionable", state.OperatorActionable())
							}
						}
					} else {
						logger.Info("posted advisory digest", "repo", primaryRepo, "issue", issueNum, "findings", digest.TotalCount, "via", "app")
						// Record the fresh, successful digest post so the hub's
						// advisory-staleness gate stays satisfied for this hive.
						dashSrv.RecordAdvisoryPost(digest.TotalCount)
						recordAdvisoryPostSuccess(primaryRepo, time.Now())
						dashSrv.RecordAdvisoryOverflow(digest.OverflowCount)
						// A successful write proves the app is installed AND has
						// write access — clear BOTH the perm issue and the
						// app-required banner flag. Previously only the perm
						// issue was cleared, so githubAppRequired (set true at
						// startup or on an early transient failure) stuck on
						// forever and the "GitHub App Not Installed" banner
						// never went away despite tokens working.
						dashSrv.SetGitHubAppPermIssue("")
						dashSrv.SetGitHubAppRequired(false)
						dashSrv.ClearPendingGitHubAppInstall()
						// The same proof retires stale ACCESS findings (#2575):
						// an advisory bead like "Insufficient repo permissions"
						// created while the App genuinely could not write was
						// never re-validated, so it stayed in the digest forever
						// after the App was correctly installed. A successful
						// App-authenticated digest post is the strongest
						// possible evidence the condition has healed, so close
						// those beads now; the next cycle's digest moves them to
						// "Recently Resolved" and rewrites the pinned comment.
						if healed := advisory.CloseHealedAppAuthFindings(beadStores); len(healed) > 0 {
							logger.Info("closed healed GitHub App access findings after successful App digest post",
								"count", len(healed), "titles", strings.Join(healed, "; "))
						}
						// Repo-ACCESS findings ("no clone mechanism", "no
						// repository access mechanism in L2 advisory mode",
						// …) are the second #2575 family: true before #4291
						// gave advisory tiers Contents:read and a working
						// credential-helper fetch, but a digest post only
						// proves issues:WRITE, so they need their own proof.
						// Verify with a real advisor-scoped Contents read of
						// the repo each finding names (or the primary repo
						// when it names none), memoized per repo — a finding
						// about a repo the hive genuinely cannot read stays
						// open.
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
								// Static-token client: no advisor-tier token
								// can be minted, so the read path cannot be
								// verified — leave the finding open.
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
					}
				} else {
					// No pinned advisory issue for this repo, yet there IS
					// something to publish. This used to be a completely silent
					// skip (#4167): the digest stopped updating, the spoke
					// reported neither a post time nor an error, and the hub's
					// staleness gate therefore read the hive as "not an advisory
					// participant" and never raised the pill — a wedged digest
					// that looked exactly like a healthy PR-only hive. Record it
					// as a post FAILURE so the hub flags the hive stale with the
					// real cause, and log it once per cycle for the operator.
					msg := advisoryIssueMissingError(primaryRepo, advisoryEnsureErr)
					dashSrv.RecordAdvisoryError(msg)
					logger.Warn("advisory digest not posted: no pinned advisory issue",
						"repo", primaryRepo, "findings", digest.TotalCount)
				}
			}
		}
	} else if d := dashSrv.GetAdvisoryDigest(); d != nil {
		statusPayload.AdvisoryDigest = d
	}

	dashSrv.UpdateStatusIfFresh(statusPayload, buildEpoch)

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

// loginScanAction is what the detector should do about one agent this cycle.
type loginScanAction int

const (
	// loginScanIgnore: nothing that looks like a login problem, or a startup
	// modal is on screen. Any sighting streak is cleared.
	loginScanIgnore loginScanAction = iota
	// loginScanDeferAuthenticated: the pane matched, but the backend credential
	// is demonstrably valid, so this is residue or a stuck CLI — the manager's
	// token-restart heal's case, not an operator's (kubestellar/hive#5291).
	loginScanDeferAuthenticated
	// loginScanDeferStreak: the pane matched and the credential is not provably
	// good, but this is the first consecutive cycle to see it.
	loginScanDeferStreak
	// loginScanPause: pause the agent and page the operator.
	loginScanPause
)

// loginPauseMinSightings is how many CONSECUTIVE governor cycles must see a
// login pattern before the detector pauses (kubestellar/hive#5291).
//
// The manager's own pane poller learned this at its ~3s cadence, where a single
// sighting restarted healthy agents; it now requires loginStreakRestartMin = 3.
// The detector had no equivalent, and a pause is far more expensive than a
// restart — it is sticky, it needs a human to undo, and it cancels the agent
// context that hosts the heal. Two is deliberate rather than three: a governor
// cycle is minutes, not seconds, so each extra cycle is real delay for a
// genuine logout, and the credential gate above already covers the case this
// backstops. It matters most for backends with no credential file this process
// can check, where it is the only new protection.
const loginPauseMinSightings = 2

// loginSightingTracker counts CONSECUTIVE cycles in which each agent's pane
// matched a login pattern. A clean cycle resets the count to zero, so a match
// has to persist to accumulate — a single flicker never reaches the threshold.
type loginSightingTracker struct {
	mu     sync.Mutex
	streak map[string]int
}

func newLoginSightingTracker() *loginSightingTracker {
	return &loginSightingTracker{streak: map[string]int{}}
}

// loginSightings is the detector's process-scoped state. The governor cycle is
// a function rather than an object, so the consecutive-sighting counts have to
// outlive a single call; tests build their own tracker and pass it explicitly.
var loginSightings = newLoginSightingTracker()

// observe records this cycle's reading for one agent and returns the resulting
// consecutive-sighting count (1 on the first sighting).
func (t *loginSightingTracker) observe(agent string, matched bool) int {
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
	}
	if t == nil {
		// No tracker wired: behave as if every sighting is its own streak, which
		// is exactly the pre-#5291 single-observation behaviour.
		if matched {
			return loginPauseMinSightings
		}
		return 0
	}
	if !matched {
		delete(t.streak, agent)
		return 0
	}
	t.streak[agent]++
	return t.streak[agent]
}

// forget drops an agent's streak — on pause (it stops being scanned) and for
// agents that are no longer present, so the map cannot grow without bound
// across a long-lived process.
func (t *loginSightingTracker) forget(agent string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.streak, agent)
}

// retain drops every agent not in the given set.
func (t *loginSightingTracker) retain(present map[string]bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for name := range t.streak {
		if !present[name] {
			delete(t.streak, name)
		}
	}
}

// loginScanDecision is the detector's whole judgement about one agent, as a
// pure function of what was observed. It exists apart from scanForLoginRequired
// so the decision can be tested against real pane text without a tmux session,
// a manager, or a governor cycle.
//
// sightings is the consecutive-cycle count INCLUDING this one.
//
// The credential gate is the fix for kubestellar/hive#5291: the detector used
// to pause on pane text alone, and the pane during and just after an
// interactive /login necessarily contains login-screen chrome — so it fired on
// the evidence the operator's own fix had just produced, seven minutes after
// the credential was already valid. Worse, Pause() cancels the agent context
// and tears down the poller that hosts the token-restart heal (#4606), which is
// the mechanism built for exactly "login prompt on screen, credential valid".
// Pausing first therefore disabled the machinery that would have fixed the pane
// it misread.
//
// Text matching cannot be narrowed out of this: two earlier fixes tried
// (tail-only matching, then a tighter copilot pattern) and this incident is the
// third false positive. The pane legitimately contains login text at the moment
// the credential is freshest, so the credential has to be consulted.
func loginScanDecision(
	backend, paneText string,
	compiled []*regexp.Regexp,
	credentialValid bool,
	sightings int,
) (loginScanAction, *regexp.Regexp) {
	matched := loginScanMatch(backend, paneText, compiled)
	return loginScanVerdict(matched != nil, credentialValid, sightings), matched
}

// loginScanMatch reports which login pattern this pane trips, or nil for none.
// Separate from the verdict so the scan loop can match ONCE and use the answer
// both to advance the sighting streak and to decide.
func loginScanMatch(backend, paneText string, compiled []*regexp.Regexp) *regexp.Regexp {
	// Stand down while a startup-blocking modal (folder trust, codex update, …)
	// is on screen: that is not a login problem, and pausing the agent for it
	// cancels the trust-prompt watcher that would answer it — the deadlock that
	// kept copilot agents "sitting at login prompt" through every operator
	// re-login (hivecommons/hive, 2026-08-22). The watcher answers the modal
	// within seconds; if a REAL login prompt follows, the next detector tick
	// sees it on a clean pane.
	if agent.PaneShowsBlockingPrompt(backend, paneText) {
		return nil
	}
	for _, re := range compiled {
		if re.MatchString(paneText) {
			return re
		}
	}
	return nil
}

// loginScanVerdict turns "what the pane showed" into "what to do". It returns
// loginScanIgnore whenever matched is false, which is what lets the scan loop
// rely on a non-Ignore verdict implying a non-nil pattern to log.
func loginScanVerdict(matched, credentialValid bool, sightings int) loginScanAction {
	if !matched {
		return loginScanIgnore
	}
	if credentialValid {
		return loginScanDeferAuthenticated
	}
	if sightings < loginPauseMinSightings {
		return loginScanDeferStreak
	}
	return loginScanPause
}

// scanForLoginRequired checks each running agent's tmux pane output for login-required
// patterns. When a match is found, the agent is paused and a notification is sent.
func scanForLoginRequired(
	ctx context.Context,
	cfg *config.Config,
	agentMgr *agent.Manager,
	notifier *notify.Notifier,
	dashSrv *dashboard.Server,
	logger *slog.Logger,
	sightings *loginSightingTracker,
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

	// Scan the pane TAIL only. A login prompt the CLI is genuinely stuck at
	// sits at the BOTTOM of the pane; the 50-line window this used to read
	// reached deep into scrollback, where agent WORK OUTPUT that merely
	// mentions a pattern phrase lives — quality's scan findings quoting
	// "gh auth login" from auth documentation got the agent paused mid-kick
	// (hivecommons/hive, 2026-08-22 08:27, on a fully-authenticated CLI).
	// Same discipline as the poller's tail-only match (#4577).
	const paneLines = 12
	statuses := agentMgr.AllStatuses()
	scanned := make(map[string]bool, len(statuses))
	for name, proc := range statuses {
		if proc.State != "running" {
			continue
		}
		scanned[name] = true

		output, err := agentMgr.GetOutput(name, paneLines)
		if err != nil || len(output) == 0 {
			continue
		}

		joined := strings.Join(output, "\n")
		backend := cfg.Agents[name].Backend

		// #5291: ask the CREDENTIAL, not just the pane. A valid credential plus
		// a login prompt is the token-restart heal's case; only an invalid one
		// needs a human.
		credentialValid := agentMgr.AgentHasValidCredential(name)

		// Match once. The streak has to reflect what the pane SHOWED, including
		// on the cycles where a gate below declines to act on it, so the
		// sighting is recorded before the verdict is taken.
		re := loginScanMatch(backend, joined, compiled)
		streak := sightings.observe(name, re != nil)

		switch loginScanVerdict(re != nil, credentialValid, streak) {
		case loginScanIgnore:
			continue
		case loginScanDeferAuthenticated:
			// Logged at Info, not Warn: this is the detector working correctly,
			// and it is the line that explains an agent staying up with login
			// text on its pane.
			logger.Info("login pattern matched but the backend credential is valid — leaving it to the token-restart heal",
				"agent", name, "backend", backend, "pattern", re.String())
			continue
		case loginScanDeferStreak:
			logger.Info("login pattern matched but not yet on enough consecutive cycles — deferring",
				"agent", name, "backend", backend, "pattern", re.String(),
				"sightings", streak, "required", loginPauseMinSightings)
			continue
		case loginScanPause:
			logger.Warn("login required detected",
				"agent", name,
				"pattern", re.String(),
				"sightings", streak,
			)
			sightings.forget(name)

			// Attempt a per-agent token re-cache BEFORE pausing. On an
			// App-authenticated hive the likeliest cause of a "gh auth
			// login" prompt is an expired scoped-token cache (#4072);
			// re-minting it now means the operator's Resume immediately
			// works instead of 401ing straight back into this pause.
			// Best-effort: hives without App auth (or agents without a
			// dedicated UID) simply skip it.
			if refreshErr := agentMgr.RefreshAgentTokenFor(ctx, name); refreshErr == nil {
				logger.Info("re-cached per-agent scoped token before login-detector pause", "agent", name)
			}

			// Pause the agent instead of restarting
			if pauseErr := agentMgr.Pause(name, "login-detector", "login required detected"); pauseErr != nil {
				logger.Warn("failed to pause agent after login detection",
					"agent", name, "error", pauseErr)
			} else {
				dashSrv.AuditLog("system", "pause", "trigger=login-detector", name)
			}

			// Determine the login instruction based on the agent's backend
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
		}
	}
	// Agents that vanished (removed from config, stopped) must not keep a
	// streak alive in the map for the life of the process.
	sightings.retain(scanned)
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

// curatorConfigFromHive maps the hive.yaml curator block onto the knowledge
// package's own config. Enabled is carried across as a pointer so "absent"
// stays distinguishable from "explicitly false" — the scheduled promotion loop
// treats absent as OFF, and flattening it to a bool here would quietly turn
// unreviewed promotion on fleet-wide (#5430).
func curatorConfigFromHive(c config.KnowledgeCurator) knowledge.CuratorConfig {
	return knowledge.CuratorConfig{
		Enabled:              c.Enabled,
		Schedule:             c.Schedule,
		ExtractFrom:          c.ExtractFrom,
		AutoPromoteThreshold: c.AutoPromoteThreshold,
		PromoteFrom:          c.PromoteFrom,
		PromoteTo:            c.PromoteTo,
	}
}

// hiveIDFilePath is the persistent file where the Hive ID is stored across restarts.
const hiveIDFilePath = "/data/hive-id"

// loadOrGenerateHiveID reads the Hive ID from disk, or generates and persists a new one.
const (
	// selfUpgradeMaxAttempts bounds how many times a spoke retries an upgrade
	// that keeps leaving the image unchanged. Bounded rather than unlimited so a
	// genuinely broken hive (e.g. missing RBAC) stops thrashing its pod, and
	// bounded rather than "never again" so a transient failure still converges.
	selfUpgradeMaxAttempts = 5
	// selfUpgradeBaseBackoff is the delay before retry #2; it doubles per
	// attempt up to selfUpgradeMaxBackoff.
	selfUpgradeBaseBackoff = 2 * time.Minute
	// selfUpgradeMaxBackoff caps the exponential backoff between retries.
	selfUpgradeMaxBackoff = 30 * time.Minute
	// selfUpgradeFailureExitCode marks a process exit caused by a FAILED
	// self-upgrade. Distinct from 0 so the failure is visible in the container's
	// termination state instead of looking like a clean shutdown.
	selfUpgradeFailureExitCode = 17
)

// upgradeMarker is the on-PVC record at /data/upgrade-requested. It survives
// pod restarts (that is the whole point: the process exits as part of an
// upgrade), so it is the only place attempt bookkeeping can live.
type upgradeMarker struct {
	TargetSHA   string    `json:"target_sha"`
	CurrentSHA  string    `json:"current_sha"`
	RequestedAt time.Time `json:"requested_at"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error,omitempty"`
}

// parseUpgradeMarker decodes a marker, tolerating the legacy format that had no
// attempts/last_error fields. A legacy marker counts as one prior attempt so an
// already-wedged hive gets retries under the new budget instead of being
// treated as fresh.
func parseUpgradeMarker(data []byte) upgradeMarker {
	var m upgradeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return upgradeMarker{}
	}
	if m.Attempts < 1 {
		m.Attempts = 1
	}
	return m
}

// sameUpgradeTarget reports whether two target SHAs refer to the same commit,
// tolerating short/full SHA length mismatch the way the hub's sameCommit does.
// A DIFFERENT target must reset the attempt budget, so this comparison is what
// keeps the latch from outliving the upgrade it was created for.
func sameUpgradeTarget(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	return strings.EqualFold(a[:n], b[:n])
}

func writeUpgradeMarker(path string, m upgradeMarker, logger *slog.Logger) {
	data, err := json.Marshal(m)
	if err != nil {
		logger.Warn("failed to encode upgrade marker", "error", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		logger.Warn("failed to write upgrade marker", "path", path, "error", err)
	}
}

// recordUpgradeError annotates the existing marker with the cause of the failed
// attempt so the NEXT boot can log why the previous one did not land — without
// it the reason dies with the process and the failure is invisible.
// upgradeFailureSummary renders what the hub shows an operator. An empty
// LastError must never render as a dangling "attempts: " - a colon promising a
// reason and delivering none is worse than saying the reason was not captured,
// because it reads as truncation and sends the reader looking for the rest.
func upgradeFailureSummary(attempts int, lastError string) string {
	if strings.TrimSpace(lastError) == "" {
		return fmt.Sprintf("self-upgrade failed after %d attempts (no error recorded; the image never changed - check that the deployment tracks a tag carrying the target SHA)", attempts)
	}
	return fmt.Sprintf("self-upgrade failed after %d attempts: %s", attempts, lastError)
}

func recordUpgradeError(path string, upgradeErr error, logger *slog.Logger) {
	if upgradeErr == nil {
		return
	}
	var m upgradeMarker
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return
	case err != nil:
		logger.Warn("upgrade marker unreadable; recording the error against a fresh marker",
			"path", path, "error", err)
	default:
		m = parseUpgradeMarker(data)
	}
	m.LastError = upgradeErr.Error()
	writeUpgradeMarker(path, m, logger)
}

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

// watchdogAuthProbes builds the per-provider credential probes for the
// watchdog by adapting the rotation package's provider probers (#4608) —
// the same machinery the #4645 probe rewrite targets, so that rewrite reaches
// the watchdog automatically.
func watchdogAuthProbes(cfg *config.Config) map[string]watchdog.AuthProbe {
	threshold := cfg.Governor.Rotation.EffectiveThreshold()
	probers := []rotation.Prober{
		rotation.ClaudeProber{ThresholdPct: threshold},
		rotation.CodexProber{ThresholdPct: threshold},
		rotation.AgyProber{ThresholdPct: threshold},
		rotation.DeepSeekProber{},
	}
	out := make(map[string]watchdog.AuthProbe, len(probers))
	for _, p := range probers {
		out[p.Provider()] = watchdog.RotationAuthProbe{Prober: p}
	}
	return out
}

// turnLossToSnapshot converts the manager's in-memory turn-loss accumulation
// into its persisted form, or nil when nothing has been recorded.
//
// Nil rather than a zero struct on purpose: `turn_loss` is omitempty, so an
// agent that has never been interrupted adds nothing to /data/hive-state.json.
// The overwhelming majority of agents are in that state, and a measurement that
// bloated every hive's state file with empty records would be its own argument
// for removing it.
func turnLossToSnapshot(loss agent.TurnLoss) *snapshot.AgentTurnLoss {
	if loss.Interruptions == 0 && len(loss.Recent) == 0 {
		return nil
	}
	out := &snapshot.AgentTurnLoss{
		Interruptions: loss.Interruptions,
		Producing:     loss.Producing,
		UpperBoundS:   loss.UpperBound.Seconds(),
		Bytes:         loss.Bytes,
	}
	for _, r := range loss.Recent {
		rec := snapshot.AgentTurnInterruption{
			At:         r.At,
			Reason:     r.Reason,
			SinceKickS: r.SinceKick.Seconds(),
			Producing:  r.Producing,
			Bytes:      r.Bytes,
		}
		if r.SinceOutput != nil {
			s := r.SinceOutput.Seconds()
			rec.SinceOutputS = &s
		}
		out.Recent = append(out.Recent, rec)
	}
	return out
}

func restartEventsToSnapshot(events []agent.RestartEvent) []snapshot.AgentRestartEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]snapshot.AgentRestartEvent, 0, len(events))
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, ev := range events {
		if ev.At.IsZero() || ev.At.Before(cutoff) {
			continue
		}
		out = append(out, snapshot.AgentRestartEvent{At: ev.At, Reason: ev.Reason})
	}
	return out
}

func restartEventsFromSnapshot(events []snapshot.AgentRestartEvent) []agent.RestartEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]agent.RestartEvent, 0, len(events))
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, ev := range events {
		if ev.At.IsZero() || ev.At.Before(cutoff) {
			continue
		}
		out = append(out, agent.RestartEvent{At: ev.At, Reason: ev.Reason})
	}
	return out
}

func persistState(agentMgr *agent.Manager, gov *governor.Governor, cfg *config.Config, path string, logger *slog.Logger, dashSrv *dashboard.Server, wd *watchdog.Reconciler) {
	statuses := agentMgr.AllStatuses()
	agents := make(map[string]snapshot.AgentState, len(statuses))
	for name, proc := range statuses {
		as := snapshot.AgentState{
			Paused:            proc.Paused,
			PinnedCLI:         proc.PinnedCLI,
			PinnedModel:       proc.PinnedModel,
			ModelOverride:     proc.ModelOverride,
			BackendOverride:   proc.BackendOverride,
			RestartCount:      proc.RestartCount,
			RestartEvents:     restartEventsToSnapshot(proc.RestartEvents),
			LastRestartReason: proc.LastRestartReason,
			LastKick:          proc.LastKick,
			PausedReason:      proc.PausedReason,
			PausedTrigger:     proc.PausedTrigger,
			PausedBy:          proc.PausedBy,
			TurnLoss:          turnLossToSnapshot(proc.TurnLoss),
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

	cadenceOverrides := make(map[string]map[string]config.Cadence)
	for modeName, mode := range cfg.Governor.Modes {
		if len(mode.Cadences) > 0 {
			cadenceOverrides[modeName] = make(map[string]config.Cadence, len(mode.Cadences))
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

	state := &snapshot.PersistedState{
		Agents:               agents,
		GovernorMode:         string(govState.Mode),
		BudgetLimit:          budget.WeeklyLimit,
		BudgetIgnored:        budget.IgnoredAgents,
		BudgetIgnoreAll:      budget.IgnoreAll,
		CadenceOverrides:     cadenceOverrides,
		LastKicks:            govState.LastKick,
		BudgetSpend:          budget.CurrentSpend,
		BudgetResetAt:        budget.ResetAt,
		BudgetByAgent:        budget.ByAgent,
		BudgetByModel:        budget.ByModel,
		BudgetWindowBaseline: budget.WindowBaseline,
		KickHistory:          kickEntries,
		LastEval:             govState.LastEval,
		ACMMLevel:            cfg.ACMMLevel,
	}

	// Persist the fleet breaker so an engaged kill-switch survives a restart.
	// Only written when engaged — a never-thrown breaker adds nothing.
	if engaged, breakerPaused := agentMgr.BreakerState(); engaged {
		state.Breaker = &snapshot.BreakerState{Engaged: true, Paused: breakerPaused}
	}

	// Persist the watchdog's backoff/crash-loop/condition state (RFC #4665
	// open question 2: it rides the existing state file).
	if wd != nil {
		if wdState := wd.Snapshot(); len(wdState) > 0 {
			state.Watchdog = wdState
		}
	}

	if err := snapshot.SaveState(path, state, logger); err != nil {
		logger.Error("failed to persist state", "error", err)
	}

	// Component reach counters (#3993) ride the SAME save cadence as the main
	// state file but live in their own file (reachStatePath — resolved OQ-2 of
	// #3973), so a reach write failure never corrupts agent/governor state.
	if err := tracing.SaveReachState(reachStatePath); err != nil {
		logger.Error("failed to persist reach state", "error", err)
	}

	// Reconcile the persisted pause field from the authoritative live manager
	// state and save, atomically under the config save mutex. persistState runs
	// async (go PersistFunc()) on every pause/resume; doing the c.Agents update
	// and Save under saveMu (via ReconcilePausedAndSave) means it can neither
	// race the pause callback's map write nor clobber its file write with a
	// stale paused=false. livePaused is built from AllStatuses(), read above.
	livePaused := make(map[string]bool, len(agents))
	for name, as := range agents {
		livePaused[name] = as.Paused
	}
	if err := cfg.ReconcilePausedAndSave(livePaused); err != nil {
		logger.Error("failed to persist config to yaml", "error", err)
		if dashSrv != nil {
			dashSrv.AddSystemAlert("config-save-failed", "error",
				"Config save failed — runtime state (ACMM level, agent config) will be lost on restart: "+err.Error())
		}
	} else if dashSrv != nil {
		dashSrv.ClearSystemAlert("config-save-failed")
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

		factHist := dashSrv.FactHistory()
		if len(factHist) > 0 {
			factData, err := json.Marshal(factHist)
			if err == nil {
				atomicWrite("/data/fact-history.json", factData)
			}
		}

		costHist := dashSrv.CostHistory()
		if len(costHist) > 0 {
			costData, err := json.Marshal(costHist)
			if err == nil {
				atomicWrite("/data/cost-history.json", costData)
			}
		}

		trendHist := dashSrv.TrendHistory()
		if len(trendHist) > 0 {
			trendData, err := json.Marshal(trendHist)
			if err == nil {
				atomicWrite("/data/trend-history.json", trendData)
			}
		}

		// #4298: per-budget-window history. Written on the same cadence as the
		// other series so a pod roll cannot lose more of one than the others.
		budgetHist := dashSrv.BudgetWindowHistory()
		if len(budgetHist) > 0 {
			budgetData, err := json.Marshal(budgetHist)
			if err == nil {
				atomicWrite("/data/budget-window-history.json", budgetData)
			}
		}

		// #4263: convergence soak telemetry, written atomically on the same
		// cadence as the other series so a pod roll cannot lose more of one
		// than the others.
		soakHist := dashSrv.ConvergenceSoakHistory()
		if len(soakHist) > 0 {
			soakData, err := json.Marshal(soakHist)
			if err == nil {
				atomicWrite("/data/convergence-soak-history.json", soakData)
			}
		}
	}
}

var (
	escalationStoreOnce sync.Once
	escalationStore     *escalation.Store
)

const escalationLedgerPath = "/data/metrics/fix-streaks.json"

// getEscalationStore lazily loads the shared fix-loop ledger (staleness clock +
// distinct-SHA attempt counts + re-engagement caps). It is the SINGLE
// loop-safety/dedup authority shared by the re-engagement paths (#2 merge
// watcher, #3 claim release, #4 reaper) and the human-escalation sweep, so they
// all agree on which red PRs are stale, how many times each has been
// re-nudged, and which have crossed the human-escalation threshold.
func getEscalationStore() *escalation.Store {
	escalationStoreOnce.Do(func() {
		escalationStore = escalation.Load(escalationLedgerPath)
	})
	return escalationStore
}

var (
	holdGuardStoreOnce sync.Once
	holdGuardStore     *holdguard.Store
)

// holdGuardLedgerPath sits beside the fix-loop ledger on the PVC: same
// lifetime, same operator expectations, same backup story.
const holdGuardLedgerPath = "/data/metrics/hold-guard.json"

// getHoldGuardStore lazily loads the hold-gate snapshot ledger (#5589) — a
// sidecar to (never a tenant of) the escalation store, so the formally
// verified escalation Entry lifecycle is untouched by this concern.
func getHoldGuardStore() *holdguard.Store {
	holdGuardStoreOnce.Do(func() {
		holdGuardStore = holdguard.Load(holdGuardLedgerPath)
	})
	return holdGuardStore
}

// enforceHoldGuard closes #5589: a hold-gated PR that accumulates commits —
// from ANY author, but especially from other agents via contaminated worktree
// bases — must not sail into the merge lanes when the hold lifts, because the
// diff the human approved under the hold is no longer the diff that merges.
//
// Per eval tick it (1) snapshots the head SHA + commit/author sets of every
// newly-held PR (first-held snapshot wins), (2) on lift compares the current
// head against the snapshot — an unchanged head pins the entire history, so
// the entry simply clears — and (3) on drift posts a one-time evidence
// comment naming the unreviewed commits and authors (plain text, no
// @-mentions), re-applies the hold label so every merge lane re-gates, and
// re-arms the snapshot at the drifted head so the NEXT human lift is the
// fresh approval. Side-effect ordering fails safe: the comment must land
// before the label (a bare re-hold with no explanation is exactly the silent
// state this guard exists to prevent), and the snapshot only re-arms after
// the label sticks (re-arming first would make the next tick read the drifted
// head as clean and merge it).
//
// Returns the drifted PR keys ("repo/number", matching writeMergeEligible's
// hold-set keying) so the SAME tick's merge-eligible artifact already
// excludes them — no one-tick window between detection and the re-applied
// label reaching enumeration.
func enforceHoldGuard(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	writer forge.IssueWriter,
	actionable *github.ActionableResult,
	logger *slog.Logger,
) map[string]bool {
	reReview := map[string]bool{}
	if actionable == nil {
		return reReview
	}
	store := getHoldGuardStore()
	org := ""
	if cfg != nil {
		org = cfg.Project.Org
	}

	fetchCommits := func(repo string, number int) []holdguard.Commit {
		if ghClient == nil {
			return nil
		}
		got, err := ghClient.ListPRCommits(ctx, repo, number)
		if err != nil {
			// The head SHA alone still pins the tree; a missing commit list
			// only degrades the drift comment's evidence, never the gate.
			logger.Warn("hold guard: commit list unavailable", "repo", repo, "pr", number, "error", err)
			return nil
		}
		commits := make([]holdguard.Commit, 0, len(got))
		for _, c := range got {
			commits = append(commits, holdguard.Commit{SHA: c.SHA, Author: c.Author, Title: c.Title})
		}
		return commits
	}

	// (1) Snapshot newly-held PRs; keep long-standing holds out of retention's
	// reach. The FIRST held observation is the baseline — later pushes while
	// still held must show up as drift at lift time, not become the baseline.
	for _, h := range actionable.Hold.Items {
		if h.Type != "pr" {
			continue
		}
		repo := fullRepoName(h.Repo, org)
		if _, ok := store.Recorded(repo, h.Number); ok {
			store.Touch(repo, h.Number)
			continue
		}
		if h.HeadSHA == "" {
			// Enumeration carried no head for this hold (sparse response);
			// nothing to pin yet — retry next tick.
			continue
		}
		if store.Snapshot(repo, h.Number, h.HeadSHA, fetchCommits(h.Repo, h.Number)) {
			logger.Info("hold guard: snapshot recorded for hold-gated PR",
				"repo", repo, "pr", h.Number, "head_sha", h.HeadSHA)
		}
	}

	// (2) Check every open PR that WAS hold-gated (has a snapshot) and no
	// longer is (it enumerated as actionable): the hold lifted this window.
	for _, pr := range actionable.PRs.Items {
		repo := fullRepoName(pr.Repo, org)
		rec, ok := store.Recorded(repo, pr.Number)
		if !ok {
			continue
		}
		if pr.HeadSHA != "" && pr.HeadSHA == rec.HeadSHA {
			// Head unchanged ⇒ identical history ⇒ the tree the human
			// approved under the hold is the tree that merges. Clear and go.
			store.Clear(repo, pr.Number)
			logger.Info("hold guard: hold lifted with head unchanged; merge lanes reopened",
				"repo", repo, "pr", pr.Number, "head_sha", pr.HeadSHA)
			continue
		}

		// Drift: keep it out of this tick's merge-eligible artifact
		// unconditionally, keyed the way writeMergeEligible keys its hold set.
		key := fmt.Sprintf("%s/%d", pr.Repo, pr.Number)
		reReview[key] = true

		current := fetchCommits(pr.Repo, pr.Number)
		drift := holdguard.Diff(rec, pr.HeadSHA, current)
		logger.Warn("hold guard: branch moved while hold-gated — blocking auto-merge, requiring fresh review",
			"repo", repo, "pr", pr.Number,
			"recorded_head", rec.HeadSHA, "current_head", pr.HeadSHA,
			"new_commits", len(drift.NewCommits), "new_authors", strings.Join(drift.NewAuthors, ","))

		if writer == nil {
			// No forge writer (booted without credentials): the reReview
			// exclusion above still holds every tick; side effects wait.
			continue
		}
		if !rec.Commented {
			if err := writer.CreateIssueComment(ctx, pr.Repo, pr.Number, holdguard.CommentBody(drift)); err != nil {
				// Retry the whole episode next tick rather than re-holding
				// with no explanation — the evidence reaching a human is the
				// point, and the exclusion above already blocks the merge.
				logger.Warn("hold guard: drift comment failed; will retry next pass",
					"repo", repo, "pr", pr.Number, "error", err)
				continue
			}
			store.MarkCommented(repo, pr.Number)
		}
		if err := writer.AddLabels(ctx, pr.Repo, pr.Number, []string{holdguard.ReHoldLabel}); err != nil {
			// Commented but not re-held: the snapshot stays on the OLD head,
			// so next tick re-detects the drift (comment deduped) and retries
			// the label. Never re-arm before the label sticks.
			logger.Warn("hold guard: re-applying hold label failed; will retry next pass",
				"repo", repo, "pr", pr.Number, "error", err)
			continue
		}
		store.ReArm(repo, pr.Number, pr.HeadSHA, current)
	}

	// (3) Age out entries with no clearing event (PR merged or closed while
	// held). See holdguard.Retention for why absence-pruning would be unsafe.
	store.Prune(holdguard.Retention)
	return reReview
}

// hivePRObservations projects the enumerated hive-authored PRs into escalation
// observations (repo fully-qualified, Red == a required check failed). Shared by
// recordRedStaleness and the reaper so both classify PRs identically. A PR is
// "red" here strictly per HasFailingRequiredCheck — GENERIC check state, never a
// specific linter or language.
func hivePRObservations(cfg *config.Config, actionable *github.ActionableResult) []escalation.Observation {
	if actionable == nil {
		return nil
	}
	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	isAgentAuthor := func(author string) bool {
		return author == cfg.Project.AIAuthor || strings.HasSuffix(author, "[bot]")
	}
	var obs []escalation.Observation
	for _, pr := range actionable.PRs.Items {
		if !isAgentAuthor(pr.Author) {
			continue
		}
		obs = append(obs, escalation.Observation{
			Repo:    fullRepo(pr.Repo),
			Number:  pr.Number,
			HeadSHA: pr.HeadSHA,
			Red:     pr.HasFailingRequiredCheck(),
			// Pending marks an unresolved CI window as a no-op observation so
			// it does not clear the staleness clock (#5617, gap G2).
			Pending: pr.CIStatus == "pending",
			Excerpt: pr.CIFailureExcerpt,
			Labels:  pr.Labels,
		})
	}
	return obs
}

// recordRedStaleness updates the shared staleness clock (first-seen-red per red
// head SHA) for every hive-authored PR in this pass. It must run before the
// claim guard and the reaper so their StaleRed() reads reflect the current tick.
// A disabled escalation subsystem skips it (the whole fix-loop machinery is off).
func recordRedStaleness(cfg *config.Config, actionable *github.ActionableResult) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	obs := hivePRObservations(cfg, actionable)
	getEscalationStore().ObserveRed(obs)
	for _, ob := range obs {
		if !ob.Red {
			continue
		}
		hookDispatcher().Fire(context.Background(), hooks.Payload{
			Transition: hooks.TransitionEscalationRed,
			Repo:       ob.Repo,
			Reason:     "required CI check red",
			Attrs: map[string]string{
				hooks.AttrPR: strconv.Itoa(ob.Number),
				"head_sha":   ob.HeadSHA,
				"excerpt":    ob.Excerpt,
			},
		})
	}
}

// mergeReEngageHook builds the Fix #2 re-engagement callback for the merge
// watcher. It normalizes the repo, then records a re-engagement under the shared
// escalation store's per-red-SHA cap. Passing an empty head SHA tells the store
// to reuse the red head SHA it last observed for this PR (the eval cycle's
// ObserveRed keeps it current), so the merge watcher does not need to re-fetch
// the head. Returns false when the cap is exhausted, so the watcher can log that
// the escalation path now owns the PR. A disabled escalation subsystem yields a
// nil hook (watcher falls back to quarantine-only).
func mergeReEngageHook(cfg *config.Config) github.MergeReEngageFunc {
	if cfg.Escalation.Disabled {
		return nil
	}
	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	return func(repo string, number int) bool {
		// Empty head SHA: reuse the store's tracked current red SHA for this PR
		// (do not reset the cap counter). The eval cycle records it via
		// ObserveRed; if the store has never seen this PR red, TryReEngage still
		// allows the first MaxReEngagements nudges.
		return getEscalationStore().TryReEngage(fullRepo(repo), number, "")
	}
}

// reapStuckRedPRs is Fix #4: the governor's backstop sweep. For each
// hive-authored PR that is red on a required check AND stale (StaleRed) AND not
// already escalated to a human, it records a re-engagement (deduped + capped via
// the escalation store) and logs the fix dispatch. The PR is already present in
// ci-failing.json via writeMergeEligible, so recording the re-engagement is what
// guarantees a stale PR is treated as actionable rather than abandoned, while
// the cap prevents re-firing every tick on a permanently-red, never-moving head.
// Entirely generic: it keys only off check state + staleness, never a linter.
func reapStuckRedPRs(cfg *config.Config, actionable *github.ActionableResult, escalatedPRs map[string]bool, logger *slog.Logger) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	store := getEscalationStore()
	for _, o := range hivePRObservations(cfg, actionable) {
		if !o.Red {
			continue
		}
		key := escalation.Key(o.Repo, o.Number)
		if escalatedPRs[key] {
			// Already handed to a human (needs-human label); kick builders skip
			// it and we must not re-dispatch automated fixes.
			continue
		}
		if !store.StaleRed(o.Repo, o.Number, o.HeadSHA) {
			continue // still churning (fresh red SHA) — leave it to the fix agent
		}
		if !store.TryReEngage(o.Repo, o.Number, o.HeadSHA) {
			// Re-engagement cap reached for this red SHA: stop nudging. The
			// distinct-SHA escalation path owns it from here.
			continue
		}
		logger.Info("reaper: re-dispatching fix for stuck red PR",
			"repo", o.Repo, "pr", o.Number, "head_sha", o.HeadSHA,
			"re_engagements", store.ReEngagements(o.Repo, o.Number))
	}
}

// runEscalationSweep folds this enumeration pass into the fix-loop breaker
// ledger and fires the one-time escalation actions (evidence comment +
// needs-human label + ntfy) for any agent-authored PR that just crossed the
// threshold of distinct failed fix attempts. Returns the full set of
// escalated PR keys so the work-list writers can flag them. Deterministic by
// design: no agent judgment is involved in counting, evidence, or the
// stop-order. Human-authored PRs are never escalated.
//
// The two forge writes go through forge.IssueWriter rather than *github.Client
// so the evidence lands on whichever forge the hive is actually configured for
// (see governorForge in forgewire.go). On a GitHub hive the writer IS the
// *github.Client this used to take, so nothing about that path changed.
//
// rec receives a KindBlocked lifecycle event for each newly-escalated PR
// (#5656); a nil rec is a no-op, matching the other timeline producers.
func runEscalationSweep(
	ctx context.Context,
	cfg *config.Config,
	writer forge.IssueWriter,
	actionable *github.ActionableResult,
	notifier *notify.Notifier,
	rec lifecycleRecorder,
	logger *slog.Logger,
) map[string]bool {
	escalated := map[string]bool{}
	if cfg.Escalation.Disabled || writer == nil || actionable == nil {
		return escalated
	}
	getEscalationStore()

	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	isAgentAuthor := func(author string) bool {
		return author == cfg.Project.AIAuthor || strings.HasSuffix(author, "[bot]")
	}

	var obs []escalation.Observation
	type prMeta struct{ checks []string }
	meta := map[string]prMeta{}
	for _, pr := range actionable.PRs.Items {
		if !isAgentAuthor(pr.Author) {
			continue
		}
		repo := fullRepo(pr.Repo)
		obs = append(obs, escalation.Observation{
			Repo:    repo,
			Number:  pr.Number,
			HeadSHA: pr.HeadSHA,
			Red:     pr.CIStatus == "failure",
			// Pending marks an unresolved CI window as a no-op observation:
			// without it every fresh push's pending window wiped the
			// distinct-SHA attempt ledger, making the escalation breaker
			// probabilistic (#5617, gap G2 — Spin witness w_pending_wipe).
			Pending: pr.CIStatus == "pending",
			Excerpt: pr.CIFailureExcerpt,
			// Labels let Sweep reconcile reviewer-lane verdicts (label-only
			// edits: needs-human removed, reviewer-passed added) back into the
			// ledger so a reviewer-repaired PR that goes red again re-enters
			// the fix lifecycle instead of being orphaned (#5511, gap G1).
			Labels: pr.Labels,
		})
		meta[escalation.Key(repo, pr.Number)] = prMeta{checks: pr.FailingChecks}
	}
	results := escalationStore.Sweep(obs, cfg.Escalation.EffectiveThreshold())

	for _, o := range obs {
		key := escalation.Key(o.Repo, o.Number)
		r, ok := results[key]
		if !ok {
			continue
		}
		if r.Escalated {
			escalated[key] = true
		}
		if !r.NewlyEscala {
			continue
		}
		escalated[key] = true
		excerpt := o.Excerpt
		if excerpt == "" {
			excerpt = escalationStore.Excerpt(o.Repo, o.Number)
		}
		// A PR that re-escalates AFTER a reviewer-lane pass reaches its human
		// with a structured hand-off note (#5617 item 3): what the reviewer
		// left on the branch, when, and that no second automated pass is
		// coming. Before this, the only thing distinguishing that hand-off
		// from a first escalation was the label set.
		body := escalation.CommentBody(r.Attempts, meta[key].checks, excerpt)
		afterReviewerPass := false
		if sha, at, ok := escalationStore.ReviewerPass(o.Repo, o.Number); ok {
			body = escalation.HandoffCommentBody(r.Attempts, meta[key].checks, excerpt,
				escalation.ReviewerHandoff{SHA: sha, At: at})
			afterReviewerPass = true
		}
		if err := writer.CreateIssueComment(ctx, o.Repo, o.Number, body); err != nil {
			// Retry next pass rather than marking escalated with no comment:
			// the whole point is that the evidence reaches a human.
			logger.Warn("escalation comment failed; will retry next pass",
				"repo", o.Repo, "pr", o.Number, "error", err)
			continue
		}
		if err := writer.AddLabels(ctx, o.Repo, o.Number, []string{escalation.NeedsHumanLabel}); err != nil {
			logger.Warn("escalation label failed", "repo", o.Repo, "pr", o.Number, "error", err)
		}
		escalationStore.MarkEscalated(o.Repo, o.Number)
		// The escalation IS the real "blocked" lifecycle signal (#5656): a PR
		// out of automated fix attempts, handed to a human. Record it on the
		// item's journey so the panel's Blocked counter reflects reality, not
		// just hook annotations.
		recordBlocked(ctx, rec, cfg.Project.Org, o.Repo, o.Number, r.Attempts, meta[key].checks)
		logger.Info("fix loop escalated to human",
			"repo", o.Repo, "pr", o.Number, "attempts", r.Attempts,
			"failing_checks", strings.Join(meta[key].checks, ","))
		if notifier != nil {
			title := "Fix loop escalated"
			detail := fmt.Sprintf("%s#%d red on %d fix attempts — needs a human (see PR comment for the raw error)", o.Repo, o.Number, r.Attempts)
			if afterReviewerPass {
				// Materially more urgent than a first escalation: a reviewer
				// has already had its one pass, so nothing automated remains
				// behind this page.
				title = "Fix loop escalated after reviewer pass"
				detail = fmt.Sprintf("%s#%d red again on %d fix attempts since the reviewer's pass — no further automated pass will run (see PR comment)", o.Repo, o.Number, r.Attempts)
			}
			notifier.Send(title, detail, notify.PriorityHigh)
		}
	}
	return escalated
}

// autoMergeSweepInterval is the minimum spacing between label-queued
// auto-merge sweeps. The sweep piggybacks on the governor eval tick, which can
// fire much more often than once a minute; this floor keeps the sweep from
// hammering the GitHub API on short eval intervals.
const autoMergeSweepInterval = time.Minute

// trustedMergerFunc resolves a GitHub login against the hive's authorized-users
// allowlist and reports whether it holds at least config.RoleMerger — the same
// bar requireMergerOrOwnerRole enforces on the dashboard queue endpoint (audit
// F3).
//
// Fails CLOSED: a nil config or a login absent from the allowlist is NOT
// trusted, so an unclassifiable actor can never merge. cfg is read on every
// call so a config reload that grants or revokes the merger tier takes effect
// without a restart.
func trustedMergerFunc(cfg *config.Config) automerge.MergerAuthorizer {
	return func(login string) bool {
		if cfg == nil || strings.TrimSpace(login) == "" {
			return false
		}
		role, ok := cfg.Dashboard.AuthorizedRole(login)
		if !ok {
			return false
		}
		return config.RoleAtLeast(role, config.RoleMerger)
	}
}

// runAutoMergeSweepIfDue drains the label-queued auto-merge queue (the human
// "Approved ... for Hive auto-merge" path) at most once per
// autoMergeSweepInterval. All merge-eligibility decisions — queue-approval
// trust, the trusted-merger tier gate (SetMergerAuthorizer), check
// verification — live inside SweepQueuedAutoMerges; this function is only the
// scheduler and the dashboard audit sink.
// rotationTrigger is the PausedTrigger stamped on strand-pauses so rotation's
// auto-resume never resumes a pause it did not create.
const rotationTrigger = "provider-rotation"

// runRotationCheck applies RFC #3958 provider rotation after an eval cycle:
// for each agent not mid-task whose provider was positively measured as
// exhausted, move it to a backend with headroom at the same tier; when
// nothing has headroom, pause it loudly (strand). Stranded agents are
// auto-resumed when their provider recovers headroom. Never runs mid-task:
// only idle agents are candidates.
func runRotationCheck(ctx context.Context, cfg *config.Config, rotMgr *rotation.Manager, gov *governor.Governor, agentMgr *agent.Manager, logger *slog.Logger) {
	if rotMgr == nil || !cfg.Governor.Rotation.Enabled {
		return
	}
	govState := gov.GetState()
	for name, proc := range agentMgr.AllStatuses() {
		backend := proc.Config.Backend
		if proc.BackendOverride != "" {
			backend = proc.BackendOverride
		}

		// Auto-resume: a stranded agent whose provider recovered.
		if proc.Paused && proc.PausedTrigger == rotationTrigger {
			if rotMgr.StrandRecovered(backend) {
				if err := agentMgr.Resume(ctx, name, rotationTrigger, "provider headroom recovered"); err != nil {
					logger.Warn("rotation: auto-resume failed", "agent", name, "error", err)
				} else {
					logger.Info("rotation: auto-resumed stranded agent", "agent", name, "backend", backend)
				}
			}
			continue
		}
		if proc.Paused {
			continue // never touch an operator pause
		}
		// Never rotate mid-task: only idle agents are candidates.
		if proc.State == agent.StateRunning {
			continue
		}

		cadenceS := 0
		if c, ok := govState.Cadences[name]; ok && c.Interval > 0 {
			cadenceS = int(c.Interval / time.Second)
		}

		if !rotMgr.Exhausted(backend) {
			continue
		}
		next := rotMgr.NextBackendForCadence(name, backend, cadenceS)
		if next == "" {
			// Strand loudly: pause so the agent burns nothing until a
			// provider recovers; the loop above auto-resumes it.
			if err := agentMgr.Pause(name, rotationTrigger, "no provider has headroom (RFC #3958)"); err != nil {
				logger.Warn("rotation: strand-pause failed", "agent", name, "error", err)
			} else {
				logger.Info("rotation: stranding agent, no headroom anywhere", "agent", name, "backend", backend)
			}
			continue
		}
		if err := agentMgr.SetBackendOverride(name, next); err != nil {
			logger.Warn("rotation: backend override failed", "agent", name, "to", next, "error", err)
			continue
		}
		logger.Info("rotation: moved agent to new backend", "agent", name, "from", backend, "to", next)
	}
}

func runAutoMergeSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < autoMergeSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	opts := automerge.Options{Logger: logger, MergerAuthorizer: trustedMergerFunc(cfg)}
	if cfg != nil {
		if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
			opts.RequiredChecks = set
		}
	}
	result, err := automerge.SweepQueuedAutoMerges(ctx, ghClient, opts, automerge.AutoMergeSweepOptions{
		MaxMerges: automerge.DefaultAutoMergeSweepMaxMerges,
		Audit: func(event automerge.AutoMergeSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, author=%s, queued_by=%s, label=%s, head_sha=%s, merge_sha=%s",
				event.Repo, event.Number, event.Author, event.QueuedBy, event.Label, event.HeadSHA, event.MergeSHA)
			dashSrv.AuditLog("system", "automerge-sweep-merged", detail, "")
		},
	})
	if err != nil {
		logger.Warn("automerge sweep failed", "error", err)
		return
	}
	if len(result.Merged) > 0 || result.Seen > 0 {
		logger.Info("automerge sweep complete", "seen", result.Seen, "merged", len(result.Merged), "skipped", result.Skipped)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "queued automerge sweep complete",
		Attrs: map[string]string{
			"seen":    strconv.Itoa(result.Seen),
			"merged":  strconv.Itoa(len(result.Merged)),
			"skipped": strconv.Itoa(result.Skipped),
		},
	})
}

// mergeEligiblePath is a var (not a const) only so tests can point
// mergeTargetEligible at a temp file; production never reassigns it.
var mergeEligiblePath = "/var/run/hive-metrics/merge-eligible.json"

var (
	ciFailingPath      = "/var/run/hive-metrics/ci-failing.json"
	intentVerdictsPath = "/var/run/hive-metrics/intent-verdicts.json"
)

// acmmHoldGatedMinLevel / acmmHoldGatedMaxLevel bracket the ACMM levels whose
// merge policy is "hold-gated" — every agent-opened PR gets a "hold" label and
// no agent merges (see src/pkg/config/packs/level-{3,4,5}.yaml). L1/L2 are
// "manual" (agents open no PRs) and L6 is "auto-merge on green CI, no hold
// label", so both fall outside this range. Used by the F6 hold-label decider.
const (
	acmmHoldGatedMinLevel = 3
	acmmHoldGatedMaxLevel = 5
)

// shouldHoldAgentPR keeps public outreach claims human-reviewed even at L6,
// where ordinary agent PRs may auto-merge. The general ACMM hold gate remains
// unchanged for all roles at L3-L5.
func shouldHoldAgentPR(agentName string, level int) bool {
	if strings.EqualFold(strings.TrimSpace(agentName), "outreach") {
		return true
	}
	return level >= acmmHoldGatedMinLevel && level <= acmmHoldGatedMaxLevel
}

// mergeableJSONUnknown is the explicit wire value for "mergeability was never
// determined". It is spelled out rather than left as "" so a consumer reading
// merge-eligible.json cannot mistake an unpopulated field for a definitive
// "no" — the failure mode that made every PR read as unmergeable.
const mergeableJSONUnknown = "unknown"

// mergeTargetEligible reports whether (repo, number) currently appears in the
// governor's merge-eligible.json AT the expected head SHA. It reads the file
// FRESH on every call (never caches) because eligibility is recomputed each
// governor cycle — a stale cache could authorize a PR that has since fallen out
// of the list. On any read/parse error it returns false (FAIL CLOSED): if we
// cannot prove the target is eligible, we must not authorize the merge.
//
// M4 (CWE-367, TOCTOU): the governor records the head SHA it observed when it
// deemed the PR eligible (eligiblePR.HeadSHA). A branch can move between that
// review and the merge relay firing, so matching (repo, number) alone would let
// a moved head merge at a commit the governor never vetted. We therefore also
// require the entry's stored head_sha to equal expectSHA. A mismatch — or a
// stored SHA that is empty (governor could not observe it) — fails closed; the
// relay's SHA pin then fails the merge cleanly if a stale request slips through.
//
// merge-eligible.json stores repos as "owner/repo"; a MergeRequest.Repo may be
// bare ("repo") or fully qualified ("owner/repo"). We match on the bare repo
// name (the segment after the last "/") plus the number, so both request forms
// resolve to the same eligible entry without depending on the org prefix.
func mergeTargetEligible(repo string, number int, expectSHA string) bool {
	data, err := os.ReadFile(mergeEligiblePath)
	if err != nil {
		return false // fail closed: no list ⇒ nothing is eligible
	}
	var payload struct {
		Items []struct {
			Number  int    `json:"number"`
			Repo    string `json:"repo"`
			HeadSHA string `json:"head_sha"`
		} `json:"merge_eligible"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false // fail closed: unparseable list ⇒ deny
	}
	want := bareRepoName(repo)
	wantSHA := strings.TrimSpace(expectSHA)
	for _, it := range payload.Items {
		if it.Number == number && bareRepoName(it.Repo) == want {
			// M4: bind authorization to the governor-observed head. An empty
			// stored SHA cannot be proven to match, so it fails closed rather
			// than authorizing an unpinned head.
			return strings.TrimSpace(it.HeadSHA) != "" && strings.TrimSpace(it.HeadSHA) == wantSHA
		}
	}
	return false
}

// bareRepoName returns the repo segment after the last "/", so "owner/repo" and
// "repo" compare equal. Used to match a MergeRequest.Repo against the
// "owner/repo" entries in merge-eligible.json regardless of prefix.
func bareRepoName(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// bindMergeAuthz wraps the manager's agent/UID/CanMerge authorizer with the
// F4 target-binding checks (CWE-863). The inner authz owns the "may this agent
// merge at all" decision; this wrapper owns "is THIS specific target one the
// governor deemed eligible, at a pinned SHA". Both must pass before MergePR is
// reached. Ordering: run the agent/UID/CanMerge check first (cheapest, and it
// gives the clearest denial reason), then the SHA + eligible-list binding.
func bindMergeAuthz(inner func(agent string, fileUID int) error) github.MergeRequestAuthorizer {
	return func(agent string, fileUID int, repo string, number int, expectSHA string) error {
		if err := inner(agent, fileUID); err != nil {
			return err
		}
		// (a) Require a pinned head SHA. An empty expectSHA means "merge whatever
		// HEAD is now", which is the TOCTOU hole: a PR that was eligible when the
		// governor last looked could have had a malicious commit pushed since.
		// MergePR passes expectSHA as the required head SHA, so a moved head fails
		// cleanly — but only if we insist it is set.
		if strings.TrimSpace(expectSHA) == "" {
			return fmt.Errorf("merge target %s#%d has no expected head SHA — refusing to merge an unpinned head (TOCTOU guard)", repo, number)
		}
		// (b) Require the target to be in the governor's current merge-eligible
		// list AT the expected head SHA. This binds authorization to a PR the
		// hive actually deemed landable this cycle, at the exact commit it
		// reviewed, so an injected agent cannot request landing an arbitrary
		// reachable PR (e.g. its own) whose required checks happen to pass, nor
		// land an eligible PR at a head that moved after review (M4, CWE-367).
		// Read fresh + fail closed (see mergeTargetEligible).
		if !mergeTargetEligible(repo, number, expectSHA) {
			return fmt.Errorf("merge target %s#%d is not in the current merge-eligible list at head %s — only governor-approved PRs may be landed via the merge relay, and only at the reviewed head SHA", repo, number, expectSHA)
		}
		return nil
	}
}

// mergeableJSON renders a tri-state mergeability verdict for the
// merge-eligible.json marker, mapping the unknown zero value to an explicit
// "unknown" rather than an empty string.
func mergeableJSON(m github.Mergeable) string {
	if m == github.MergeableUnknown {
		return mergeableJSONUnknown
	}
	return string(m)
}

// claimLedger holds the duplicate-PR guard's persisted issue→PR claim mapping
// across eval cycles. It is loaded lazily on first use (and retried on a load
// failure) rather than at startup, so a missing or corrupt /data ledger can
// never block the hive from booting.
var (
	claimLedgerOnce   sync.Once
	claimLedger       *github.ClaimLedger
	claimLedgerPath   = github.ClaimLedgerPath
	claimLedgerLoader = github.LoadClaimLedger
)

// hiveIdentity determines which PR authors count as "this hive", so only our
// own PRs suppress work. Two accounts can open PRs on our behalf:
//   - project.ai_author — the account agents push and open PRs as
//   - the GitHub App bot login ("<app-slug>[bot]") when the hive authenticates
//     as an installation, which is what actually authors PRs in that mode
func hiveIdentity(cfg *config.Config) github.HiveIdentity {
	id := github.HiveIdentity{AIAuthor: cfg.Project.AIAuthor}
	if slug := cfg.GitHub.ResolvedAppSlug(); slug != "" {
		id.AppLogin = slug + "[bot]"
	}
	return id
}

// applyDuplicatePRGuard filters issues already claimed by an open hive-authored
// PR out of the actionable set. Failures are logged, never fatal: the guard is
// a safety net, and a broken net must not take the hive down with it.
func applyDuplicatePRGuard(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	actionable *github.ActionableResult,
	logger *slog.Logger,
) {
	ledger := getClaimLedger(logger)
	if ledger == nil {
		return
	}
	github.ApplyDuplicatePRGuard(ctx, ghClient, ledger, hiveIdentity(cfg), actionable, claimingPRRedStale(cfg, actionable), logger)
}

// getClaimLedger lazily loads the persisted claim ledger on first use (and
// keeps a usable empty ledger on a load failure), so a missing or corrupt
// /data ledger can never block the hive from booting. It is shared by the
// eval-cycle guard above and by the dashboard's IssueClaimed hook (#3768); the
// sync.Once publication makes the pointer safe to read from either goroutine,
// and the ledger itself is internally locked.
func getClaimLedger(logger *slog.Logger) *github.ClaimLedger {
	claimLedgerOnce.Do(func() {
		ledger, err := claimLedgerLoader(claimLedgerPath, logger)
		if err != nil {
			// LoadClaimLedger always returns a usable (possibly empty) ledger
			// alongside the error, so we keep it and just report the problem.
			logger.Warn("duplicate-PR guard: could not load persisted claim ledger, starting empty",
				"path", claimLedgerPath, "error", err)
		}
		claimLedger = ledger
	})
	return claimLedger
}

// claimingPRRedStale builds the Fix #3 release predicate: given a claiming PR
// (prRepo, prNumber), report whether it is red on a required check AND stale.
// It looks up the PR's live CI state + head SHA from this pass's enumeration and
// consults the shared staleness clock. A PR not found in the enumeration, or one
// that is green/pending, or one whose red head only just appeared, returns false
// — so a HEALTHY claiming PR still suppresses its issue. Returns a nil func when
// escalation is disabled, preserving the original unconditional-suppress
// behavior. GENERIC: keys only off check state + staleness.
func claimingPRRedStale(cfg *config.Config, actionable *github.ActionableResult) github.RedStaleFunc {
	if cfg.Escalation.Disabled || actionable == nil {
		return nil
	}
	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	// Index this pass's PRs by bare-repo#number so a claim's PRRepo (which may
	// be bare or "owner/repo") resolves regardless of prefix.
	type prState struct {
		red     bool
		headSHA string
		repo    string
	}
	index := map[string]prState{}
	for _, pr := range actionable.PRs.Items {
		index[fmt.Sprintf("%s#%d", bareRepoName(pr.Repo), pr.Number)] = prState{
			red:     pr.HasFailingRequiredCheck(),
			headSHA: pr.HeadSHA,
			repo:    fullRepo(pr.Repo),
		}
	}
	store := getEscalationStore()
	return func(prRepo string, prNumber int) bool {
		st, ok := index[fmt.Sprintf("%s#%d", bareRepoName(prRepo), prNumber)]
		if !ok || !st.red {
			return false // not enumerated, or healthy → keep suppressing
		}
		return store.StaleRed(st.repo, prNumber, st.headSHA)
	}
}

func writeIntentVerdicts(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	actionable *github.ActionableResult,
	beadStores map[string]*beads.Store,
	logger *slog.Logger,
) map[string]intent.Verdict {
	verdicts := make(map[string]intent.Verdict)
	if cfg == nil || actionable == nil {
		return verdicts
	}
	_ = os.MkdirAll("/var/run/hive-metrics", 0o755)
	aiAuthor := strings.TrimSpace(cfg.EffectiveAIAuthor())
	intentCfg := intent.Config{
		TestPathPatterns:      cfg.Intent.TestPathPatterns,
		DocsPathPatterns:      cfg.Intent.DocsPathPatterns,
		GuardrailPathPatterns: cfg.Intent.GuardrailPathPatterns,
		FeatureSignals:        cfg.Intent.FeatureSignals,
	}
	var alignmentReviewer *intent.AlignmentReviewer
	if strings.TrimSpace(cfg.Intent.AlignmentModel) != "" {
		endpoint, apiKey, _ := cfg.Governor.ResolveReviewer()
		var err error
		alignmentReviewer, err = intent.NewAlignmentReviewer(intent.AlignmentReviewerConfig{
			Endpoint: endpoint,
			APIKey:   apiKey,
			Model:    cfg.Intent.AlignmentModel,
		})
		if err != nil {
			logger.Warn("intent alignment reviewer disabled", "error", err)
		}
	}
	type verdictRecord struct {
		Repo       string         `json:"repo"`
		Number     int            `json:"number"`
		Title      string         `json:"title"`
		Author     string         `json:"author"`
		Enforced   bool           `json:"enforced"`
		Verdict    intent.Verdict `json:"verdict"`
		Classify   string         `json:"classification_reason"`
		FetchError string         `json:"fetch_error,omitempty"`
	}
	records := make([]verdictRecord, 0, len(actionable.PRs.Items))
	for _, pr := range actionable.PRs.Items {
		fullRepo := fullRepoName(pr.Repo, cfg.Project.Org)
		key := fmt.Sprintf("%s/%d", fullRepo, pr.Number)
		agentPR := aiAuthor != "" && strings.EqualFold(pr.Author, aiAuthor)
		record := verdictRecord{
			Repo:     fullRepo,
			Number:   pr.Number,
			Title:    pr.Title,
			Author:   pr.Author,
			Enforced: cfg.Intent.Enforce,
		}
		if !agentPR {
			class := intent.Classify(intent.PR{Title: pr.Title, Labels: pr.Labels, Author: pr.Author, AgentAuthor: false}, intentCfg)
			verdict := intent.Evaluate(class, intent.Evidence{})
			verdicts[key] = verdict
			record.Verdict = verdict
			record.Classify = class.Reason
			records = append(records, record)
			continue
		}
		body, files, approved, err := fetchIntentPREvidence(ctx, ghClient, fullRepo, pr.Number)
		if err != nil {
			verdict := intent.Verdict{
				Tier:       intent.Tier1,
				Authorized: false,
				Reason:     "intent evidence unavailable: " + err.Error(),
				AgentPR:    true,
			}
			verdicts[key] = verdict
			record.Verdict = verdict
			record.FetchError = err.Error()
			records = append(records, record)
			logger.Warn("intent verification evidence fetch failed", "repo", fullRepo, "number", pr.Number, "error", err)
			continue
		}
		class := intent.Classify(intent.PR{
			Title:       pr.Title,
			Body:        body,
			Labels:      pr.Labels,
			Files:       files,
			Author:      pr.Author,
			AgentAuthor: true,
		}, intentCfg)
		evidence := intent.BuildEvidenceForRepo(body, fullRepo, beadStores, approved)
		verdict := intent.Evaluate(class, evidence)
		issueTexts, issueErr := fetchIntentIssueTexts(ctx, ghClient, fullRepo, body)
		if issueErr != nil {
			logger.Warn("intent alignment issue evidence fetch failed", "repo", fullRepo, "number", pr.Number, "error", issueErr)
		}
		refs := intent.LinkedIssueRefs(body, fullRepo)
		alignCtx := intent.BuildAlignmentContext(intent.PR{
			Title:       pr.Title,
			Body:        body,
			Labels:      pr.Labels,
			Files:       files,
			Author:      pr.Author,
			AgentAuthor: true,
		}, issueTexts, beadStores, refs)
		alignment := intent.EvaluateAlignment(alignCtx, class.Tier, intentCfg)
		if alignmentReviewer != nil {
			modelVerdict, err := alignmentReviewer.Review(ctx, alignCtx)
			if err != nil {
				logger.Warn("intent alignment model review failed open", "repo", fullRepo, "number", pr.Number, "error", err)
				alignment = intent.MergeAlignment(alignment, nil, err)
			} else {
				alignment = intent.MergeAlignment(alignment, &modelVerdict, nil)
			}
		}
		verdict.Alignment = &alignment
		verdicts[key] = verdict
		record.Verdict = verdict
		record.Classify = class.Reason
		records = append(records, record)
		if !verdict.Authorized {
			logger.Info("intent authorization denied", "repo", fullRepo, "number", pr.Number, "tier", verdict.Tier, "reason", verdict.Reason, "enforce", cfg.Intent.Enforce)
		}
		if alignment.Misaligned() {
			logger.Info("intent alignment denied", "repo", fullRepo, "number", pr.Number, "reason", alignment.Rationale, "enforce", cfg.Intent.Enforce)
			recordIntentAlignmentAdvisory(beadStores, fullRepo, pr.Number, alignment, logger)
		}
	}
	payload := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"enforced":     cfg.Intent.Enforce,
		"verdicts":     records,
	}
	if data, err := json.Marshal(payload); err == nil {
		atomicWrite(intentVerdictsPath, data)
	} else {
		logger.Warn("failed to marshal intent verdicts", "error", err)
	}
	return verdicts
}

func fetchIntentPREvidence(ctx context.Context, ghClient *github.Client, repo string, number int) (string, []intent.ChangedFile, bool, error) {
	if ghClient == nil || ghClient.GoGitHub() == nil {
		return "", nil, false, github.ErrNoGitHubClient
	}
	owner, repoName, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || repoName == "" {
		return "", nil, false, fmt.Errorf("invalid repo %q", repo)
	}
	client := ghClient.GoGitHub()
	pr, _, err := client.PullRequests.Get(ctx, owner, repoName, number)
	if err != nil {
		return "", nil, false, fmt.Errorf("getting PR: %w", err)
	}
	var files []intent.ChangedFile
	fileOpts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.PullRequests.ListFiles(ctx, owner, repoName, number, fileOpts)
		if err != nil {
			return "", nil, false, fmt.Errorf("listing PR files: %w", err)
		}
		for _, f := range page {
			files = append(files, intent.ChangedFile{
				Filename:  f.GetFilename(),
				Status:    f.GetStatus(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		fileOpts.Page = resp.NextPage
	}
	if reported := pr.GetChangedFiles(); reported > len(files) {
		return "", nil, false, fmt.Errorf("incomplete PR file list: GitHub reported %d changed files but API returned %d; intent alignment requires the complete changed-file list", reported, len(files))
	}
	approved, err := hasMaintainerApproval(ctx, client, owner, repoName, number)
	if err != nil {
		return "", nil, false, err
	}
	return pr.GetBody(), files, approved, nil
}

func fetchIntentIssueTexts(ctx context.Context, ghClient *github.Client, defaultRepo, body string) ([]intent.TextEvidence, error) {
	if ghClient == nil || ghClient.GoGitHub() == nil {
		return nil, github.ErrNoGitHubClient
	}
	client := ghClient.GoGitHub()
	refs := intent.LinkedIssueRefs(body, defaultRepo)
	out := make([]intent.TextEvidence, 0, len(refs))
	for _, ref := range refs {
		repo := ref.Repo
		if repo == "" {
			repo = defaultRepo
		}
		owner, repoName, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || repoName == "" {
			continue
		}
		issue, _, err := client.Issues.Get(ctx, owner, repoName, ref.Number)
		if err != nil {
			return out, fmt.Errorf("getting linked issue %s#%d: %w", repo, ref.Number, err)
		}
		out = append(out, intent.TextEvidence{
			Source: fmt.Sprintf("issue %s#%d", repo, ref.Number),
			Title:  issue.GetTitle(),
			Body:   issue.GetBody(),
		})
	}
	return out, nil
}

func recordIntentAlignmentAdvisory(stores map[string]*beads.Store, repo string, number int, alignment intent.AlignmentVerdict, logger *slog.Logger) {
	store := stores["intent"]
	if store == nil {
		store = stores["quality"]
	}
	if store == nil {
		for _, candidate := range stores {
			if candidate != nil {
				store = candidate
				break
			}
		}
	}
	if store == nil {
		return
	}
	title := fmt.Sprintf("Intent alignment drift in %s#%d", repo, number)
	ref := fmt.Sprintf("gh-%s#%d", repo, number)
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type == beads.TypeAdvisory && b.Title == title && b.ExternalRef == ref && b.Status != beads.StatusClosed && b.Status != beads.StatusDone {
			return
		}
	}
	b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, "intent", ref)
	if err != nil {
		logger.Warn("failed to record intent alignment advisory", "repo", repo, "number", number, "error", err)
		return
	}
	_ = store.Update(b.ID, func(bead *beads.Bead) {
		bead.Notes = alignmentSummary(alignment)
	})
}

func alignmentSummary(alignment intent.AlignmentVerdict) string {
	var parts []string
	if alignment.Rationale != "" {
		parts = append(parts, alignment.Rationale)
	}
	for _, f := range alignment.DeterministicFindings {
		if f.Status == intent.AlignmentStatusMisaligned {
			parts = append(parts, f.Code+": "+f.Reason+" ("+strings.Join(f.Files, ", ")+")")
		}
	}
	if alignment.Model != nil && alignment.Model.Status == intent.AlignmentStatusMisaligned {
		parts = append(parts, "model: "+alignment.Model.Rationale)
	}
	if len(parts) == 0 {
		return "intent alignment check reported misalignment"
	}
	return strings.Join(parts, "\n")
}

func hasMaintainerApproval(ctx context.Context, client *gh.Client, owner, repo string, number int) (bool, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := make(map[string]string)
	maintainer := make(map[string]bool)
	for {
		reviews, resp, err := client.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing PR reviews: %w", err)
		}
		for _, review := range reviews {
			login := review.GetUser().GetLogin()
			if login == "" {
				continue
			}
			if maintainerAssociation(review.GetAuthorAssociation()) {
				switch review.GetState() {
				case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
					latest[login] = review.GetState()
				}
				maintainer[login] = true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	approved := false
	for login, state := range latest {
		if !maintainer[login] {
			continue
		}
		switch state {
		case "CHANGES_REQUESTED":
			return false, nil
		case "APPROVED":
			approved = true
		}
	}
	return approved, nil
}

func maintainerAssociation(association string) bool {
	switch strings.ToUpper(strings.TrimSpace(association)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}

func fullRepoName(repo, org string) string {
	if strings.Contains(repo, "/") || org == "" {
		return repo
	}
	return org + "/" + repo
}

// auditPRAttributionWindow bounds how far back the audit trail is scanned to
// map open PRs to the agent that opened them. Red PRs older than this fall
// back to scanner ownership in the kick builders — acceptable: 14d exceeds any
// PR the fleet should still be iterating on.
const auditPRAttributionWindow = 14 * 24 * time.Hour

// auditPRAgents maps "org/repo#number" → agent name from the audit trail's
// agent_pr_created entries (attribution.go records one per relay-opened PR,
// reuses included). Reading the on-disk log per eval tick keeps this
// stateless; OutputActionsSince touches no receiver state, so a zero-value
// AuditLog is safe here.
func auditPRAgents(org string, since time.Time, auditPath string) map[string]string {
	entries := (&dashboard.AuditLog{}).OutputActionsSince(since,
		map[string]bool{github.AuditActionAgentPRCreated: true}, auditPath)
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Agent == "" {
			continue
		}
		var repo, number string
		for _, part := range strings.Split(e.Detail, ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
				switch k {
				case "repo":
					repo = v
				case "number":
					number = v
				}
			}
		}
		if repo == "" || number == "" {
			continue
		}
		if !strings.Contains(repo, "/") && org != "" {
			repo = org + "/" + repo
		}
		out[repo+"#"+number] = e.Agent
	}
	return out
}

// anyRequiredCheckFailing reports whether any of a PR's failing check names
// is in the operator-declared required set.
func anyRequiredCheckFailing(failing []string, required map[string]bool) bool {
	for _, name := range failing {
		if required[name] {
			return true
		}
	}
	return false
}

func writeMergeEligible(actionable *github.ActionableResult, hold github.HoldResult, org string, escalatedPRs map[string]bool, enforceIntent bool, intentVerdicts map[string]intent.Verdict, requireReviewApproval bool, requiredChecks map[string]bool, holdDriftPRs map[string]bool, logger *slog.Logger) {
	// holdDriftPRs ("repo/number", same keying as holdSet) are PRs whose hold
	// just lifted on a branch that MOVED while hold-gated (#5589). They are
	// treated exactly like held PRs — invisible to both the eligible and the
	// ci-failing buckets — because neither the merge sweep nor a fix agent
	// should touch a branch whose unreviewed drift is awaiting a human.
	holdSet := make(map[string]bool)
	for _, h := range hold.Items {
		key := fmt.Sprintf("%s/%d", h.Repo, h.Number)
		holdSet[key] = true
	}
	for key := range holdDriftPRs {
		holdSet[key] = true
	}

	type eligiblePR struct {
		Number int      `json:"number"`
		Repo   string   `json:"repo"`
		Title  string   `json:"title"`
		Author string   `json:"author"`
		Labels []string `json:"labels,omitempty"`
		// Mergeable is a tri-state string ("yes"/"no"/"unknown"), not a bool.
		// A bool here defaulted to false for every PR, because the value was
		// read from a list endpoint that never returns it.
		Mergeable string `json:"mergeable"`
		DCO       string `json:"dco"`
		// HeadSHA is the governor-observed head commit at the moment eligibility
		// was decided. mergeTargetEligible compares the relay's expected SHA
		// against this value (M4, CWE-367): a branch that moved after review
		// no longer matches and fails closed.
		HeadSHA string `json:"head_sha,omitempty"`
	}

	type failingPR struct {
		Number  int    `json:"number"`
		Repo    string `json:"repo"`
		Title   string `json:"title"`
		Author  string `json:"author"`
		HeadSHA string `json:"head_sha,omitempty"`
		// FailingChecks + Excerpt carry the raw CI evidence into the kick
		// work list so fix agents see the actual error, not just "red".
		FailingChecks []string `json:"failing_checks,omitempty"`
		Excerpt       string   `json:"excerpt,omitempty"`
		// Escalated marks PRs past the fix-loop breaker threshold: kick
		// builders list them separately and agents must NOT dispatch more
		// fix work for them.
		Escalated bool `json:"escalated,omitempty"`
		// Agent is the hive agent whose relay request opened this PR (from the
		// audit trail's agent_pr_created entries). The scheduler's
		// fix-before-new section routes each red PR back to its author; empty
		// means unattributed (kick builders default it to scanner).
		Agent string `json:"agent,omitempty"`
		// Labels carries the PR's current labels into the kick builders. The
		// reviewer lane (#5480) reads them to exclude PRs already carrying
		// reviewer-passed — a PR that re-escalates after a reviewer pass
		// belongs to a true human, never to another automated pass.
		Labels []string `json:"labels,omitempty"`
		// CreatedAt is the PR's forge creation time — the reviewer lane's
		// ordering key (#5617 item 4). Its work list is capped at a few PRs
		// per kick and documented "oldest first", but until this field the
		// rows carried no age signal at all and were ordered by (repo name, PR
		// number). Numbers are monotonic only WITHIN a repo, so that proxy
		// sorted by repo NAME first and could starve an old escalated PR in a
		// late-alphabet repo behind newer ones, on every kick, forever.
		CreatedAt time.Time `json:"created_at"`
	}

	prAgents := auditPRAgents(org, time.Now().Add(-auditPRAttributionWindow), "")

	var eligible []eligiblePR
	var failing []failingPR
	var reviewArtifact review.Artifact
	reviewLoaded := false
	if requireReviewApproval {
		var err error
		reviewArtifact, err = review.LoadArtifact("")
		if err != nil {
			logger.Warn("review approval required but review-verdicts.json is unavailable; merge eligibility will fail closed", "error", err)
		} else {
			reviewLoaded = true
		}
	}
	for _, pr := range actionable.PRs.Items {
		if pr.Draft {
			continue
		}
		key := fmt.Sprintf("%s/%d", pr.Repo, pr.Number)
		if holdSet[key] {
			continue
		}
		fullRepo := fullRepoName(pr.Repo, org)
		if enforceIntent {
			if verdict, ok := intentVerdicts[fmt.Sprintf("%s/%d", fullRepo, pr.Number)]; ok && verdict.AgentPR && !verdict.MergeAllowed() {
				reason := verdict.Reason
				if verdict.Authorized && verdict.Alignment != nil && verdict.Alignment.Misaligned() {
					reason = intent.ReasonAlignmentMisaligned + ": " + verdict.Alignment.Rationale
				}
				logger.Info("excluding PR from merge-eligible due to intent verification", "repo", fullRepo, "number", pr.Number, "tier", verdict.Tier, "reason", reason)
				continue
			}
		}

		if pr.CIStatus == "failure" {
			// A PR red ONLY on non-required checks (perma-red Playwright
			// shards, coverage) that GitHub itself reports mergeable is NOT a
			// failing PR — it is merge-eligible, mirroring the
			// pending-but-mergeable rule below. Without this, every dependabot
			// PR on a repo with permanently-red optional checks classified as
			// "failure", landed in ci-failing.json where no sweep or agent
			// would ever merge it, and accumulated indefinitely (observed on
			// kubestellar/console 2026-08-28: 16 dependabot PRs, oldest 11
			// days). Gated on an operator-declared required-check set: with no
			// set configured we cannot distinguish required from optional and
			// keep the old fail-closed behavior. The merge step re-enforces
			// branch protection, so this cannot merge anything GitHub blocks.
			onlyOptionalRed := len(requiredChecks) > 0 &&
				!anyRequiredCheckFailing(pr.FailingChecks, requiredChecks) &&
				pr.Mergeable == github.MergeableYes
			if !onlyOptionalRed {
				failing = append(failing, failingPR{
					Number:        pr.Number,
					Repo:          fullRepo,
					Title:         pr.Title,
					Author:        pr.Author,
					HeadSHA:       pr.HeadSHA,
					FailingChecks: pr.FailingChecks,
					Excerpt:       pr.CIFailureExcerpt,
					Escalated:     escalatedPRs[escalation.Key(fullRepo, pr.Number)],
					Agent:         prAgents[fmt.Sprintf("%s#%d", fullRepo, pr.Number)],
					Labels:        pr.Labels,
					CreatedAt:     pr.CreatedAt,
				})
				continue
			}
		}

		// A PR whose CI is still "pending" is nonetheless merge-eligible when
		// GitHub itself reports it as mergeable (mergeStateStatus=unstable):
		// that state means every REQUIRED check has passed and only
		// non-required checks remain outstanding. Those non-required checks —
		// a cancelled Mobile Browser Tests, a still-running coverage-report,
		// perpetually-pending tide — can never complete on their own, so
		// waiting for CIStatus=="success" (all checks done) leaves cleanly
		// mergeable PRs frozen out of the sweep indefinitely (observed
		// 2026-08-04: three green console PRs stuck for hours). The merge step
		// re-enforces branch protection, so trusting the mergeable verdict here
		// cannot merge anything GitHub would actually block.
		if pr.CIStatus == "pending" && pr.Mergeable != github.MergeableYes {
			// Genuinely not ready: a required check is still running (or
			// mergeability is unknown/no). Leave it out of both buckets, as
			// before — it neither merges nor gets a fix dispatched.
			continue
		}

		if pr.Mergeable == github.MergeableNo {
			// A conflicting PR cannot merge no matter how green its checks
			// are. Listing it as merge-eligible left the eligible count stuck
			// at N forever while nothing could actually merge (console
			// #23002/#23003, 2026-08-31: the only two build-gate-green PRs
			// were DIRTY go.mod dependabot bumps). Conflicts are the
			// rebase/needs-human path's job, not the sweep's — keep them out
			// of the eligible bucket.
			continue
		}

		dco := "unknown"
		for _, l := range pr.Labels {
			switch l {
			case "dco-signoff: yes":
				dco = "yes"
			case "dco-signoff: no":
				dco = "no"
			}
		}
		if requireReviewApproval && (!reviewLoaded || !reviewArtifact.HasAggregateApproval(fullRepo, pr.Number, pr.HeadSHA)) {
			continue
		}
		eligible = append(eligible, eligiblePR{
			Number:    pr.Number,
			Repo:      fullRepo,
			Title:     pr.Title,
			Author:    pr.Author,
			Labels:    pr.Labels,
			Mergeable: mergeableJSON(pr.Mergeable),
			DCO:       dco,
			HeadSHA:   pr.HeadSHA,
		})
	}

	_ = os.MkdirAll("/var/run/hive-metrics", 0o755)

	payload := map[string]any{
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"merge_eligible": eligible,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		logger.Warn("failed to marshal merge-eligible", "error", err)
		return
	}
	atomicWrite(mergeEligiblePath, data)
	logger.Info("merge-eligible.json updated", "eligible", len(eligible), "ci_failing", len(failing), "total_prs", len(actionable.PRs.Items))

	failPayload := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"ci_failing":   failing,
	}
	failData, err := json.Marshal(failPayload)
	if err != nil {
		logger.Warn("failed to marshal ci-failing", "error", err)
		return
	}
	atomicWrite(ciFailingPath, failData)
}

func planReviewDispatch(cfg *config.Config, actionable *github.ActionableResult, agentMgr *agent.Manager, logger *slog.Logger) review.DispatchPlan {
	if cfg == nil || actionable == nil || !cfg.Review.RequireApproval || !cfg.Review.FanOut {
		return review.DispatchPlan{}
	}
	state, err := review.LoadDispatchState("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review dispatch state unavailable; starting fresh", "error", err)
	}
	artifact, err := review.LoadArtifact("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review verdict artifact unavailable for dispatch planning", "error", err)
	}
	prs := make([]review.PullRequest, 0, len(actionable.PRs.Items))
	for _, pr := range actionable.PRs.Items {
		lane := classify.Classify(github.Issue{Title: pr.Title, Labels: pr.Labels}).Lane
		prs = append(prs, review.PullRequest{
			Repo:    pr.Repo,
			Number:  pr.Number,
			Title:   pr.Title,
			Author:  pr.Author,
			HeadSHA: pr.HeadSHA,
			URL:     pr.URL,
			Lane:    string(lane),
			// Grounding anchor for the review prompt. Repo access is the
			// measured active ingredient in review quality (17%→67% hit rate,
			// 61% fewer false positives), so the reviewer is told which commit
			// to read rather than being left to infer from the diff.
			MergeBase: pr.BaseSHA,
		})
	}
	agents := make([]review.AgentCapability, 0, len(cfg.Agents))
	for name, ac := range cfg.EnabledAgents() {
		agents = append(agents, review.AgentCapability{
			Name:           name,
			Enabled:        true,
			Paused:         ac.Paused || (agentMgr != nil && agentMgr.IsPaused(name)),
			OnDemand:       ac.OnDemand,
			UsesKick:       ac.UsesGovernorKick(),
			Role:           ac.Role,
			LaneKeywords:   ac.LaneKeywords,
			DetectKeywords: ac.DetectKeywords,
			Aliases:        ac.Aliases,
		})
	}
	plan := review.PlanDispatch(prs, artifact, state, review.DispatchOptions{
		RequireApproval:    cfg.Review.RequireApproval,
		FanOut:             cfg.Review.FanOut,
		MaxParallelReviews: cfg.Review.EffectiveMaxParallelReviews(),
		ReviewerAgents:     cfg.Review.ReviewerAgents,
		FixerAgent:         cfg.Review.FixerAgent,
		ProjectOrg:         cfg.Project.Org,
		AIAuthor:           cfg.EffectiveAIAuthor(),
		Agents:             agents,
	})
	if len(plan.ReviewKicks)+len(plan.FixKicks) > 0 {
		logger.Info("review swarm dispatch planned", "review_kicks", len(plan.ReviewKicks), "fix_kicks", len(plan.FixKicks))
	}
	return plan
}

func refreshReviewVerdicts(cfg *config.Config, logger *slog.Logger) {
	if cfg == nil || !cfg.Review.RequireApproval {
		return
	}
	artifact, err := review.CollectAndWrite("", "", review.AggregateOptions{})
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("failed to refresh review verdicts", "error", err)
		}
		return
	}
	logger.Info("review verdict artifact refreshed", "aggregates", len(artifact.Items))
}

func persistReviewDispatchState(plan review.DispatchPlan, delivered []review.DispatchKick, logger *slog.Logger) {
	planned := append(append([]review.DispatchKick(nil), plan.ReviewKicks...), plan.FixKicks...)
	if plan.State.GeneratedAt.IsZero() && len(planned) == 0 {
		return
	}
	state := review.ConfirmDelivered(plan.State, planned, delivered)
	if err := review.WriteDispatchState("", state); err != nil {
		logger.Warn("failed to persist review dispatch state", "error", err)
	}
}

// normalizedAutoMergeLabel resolves the configured queue label, falling back
// to the shared default when the value is blank. Client.SetAutoMergeLabel
// ignores blank input (keeping whatever was set before) and
// Client.AutoMergeLabel falls back on read, but the cmd layer normalizes
// eagerly too so a partially-populated config can never propagate an unnamed
// label to a fresh client.
func normalizedAutoMergeLabel(label string) string {
	if label = strings.TrimSpace(label); label != "" {
		return label
	}
	return github.AutoMergeQueuedLabel
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
	// #4041: a persisted sensing_login that is byte-identical to the
	// pre-#3959 default set carries no operator intent — it is the old
	// defaults materialized by an earlier save. Replaying it here would
	// re-pin the false-positive-prone generic patterns over the corrected
	// code defaults the config layer just applied. Skip it; a genuinely
	// customized list still replays verbatim.
	if len(o.SensingLogin) > 0 && !config.IsLegacyDefaultLoginPatterns(o.SensingLogin) {
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
	return loadNousStateFromPaths(logger, nousGovernorDir, nousSnapshotDir)
}

func loadNousStateFromPaths(logger *slog.Logger, governorDir, snapshotDir string) *dashboard.NousState {
	state := &dashboard.NousState{
		Mode:   "observe",
		Scope:  "governor",
		Phase:  "collecting",
		Status: make(map[string]interface{}),
		Config: make(map[string]interface{}),
	}

	if ledgerData, err := os.ReadFile(filepath.Join(governorDir, "ledger.json")); err == nil {
		var ledger struct {
			Iterations []map[string]interface{} `json:"iterations"`
		}
		if err := json.Unmarshal(ledgerData, &ledger); err == nil {
			state.Ledger = ledger.Iterations
			logger.Info("nous ledger loaded", "iterations", len(state.Ledger))
		}
	}

	if principlesData, err := os.ReadFile(filepath.Join(governorDir, "principles.json")); err == nil {
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
	if entries, err := os.ReadDir(snapshotDir); err == nil {
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
		sort.Slice(lanes, func(i, j int) bool { return lanes[i].Name < lanes[j].Name })
		for _, alias := range agent.Aliases {
			discordAliases[alias] = name
		}
	}

	if len(lanes) > 0 {
		classify.SetLanes(lanes)
	}
	// Tier-classification keywords (config-driven, mirroring SetLanes). Empty
	// lists leave the built-in defaults in force, so an absent classifier block
	// keeps behavior unchanged. Always call so a reload that CLEARS the block
	// restores defaults.
	classify.SetTierKeywords(cfg.Classifier.SimpleKeywords, cfg.Classifier.ComplexSignals)
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
// sameStringSlice reports whether two string slices have identical contents in
// the same order. Used to skip no-op authorized-users updates from heartbeats.
func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

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
	if _, err := fmt.Sscanf(color, "%x", &result); err != nil {
		return 0x95a5a6 // malformed hex: fall back to the same default as an empty string
	}
	return result
}

// logAgentSandboxPosture emits the sandbox gate diagnostics from
// config.AgentSandboxGateWarnings at WARN.
//
// Split out so boot and the config-watcher reload report identically — an
// operator who flips the Security tab's sandbox toggle never restarts, so a
// boot-only check would never reach the person who most needs it.
func logAgentSandboxPosture(logger *slog.Logger, cfg *config.Config) {
	for _, warning := range config.AgentSandboxGateWarnings(cfg) {
		logger.Warn("agent sandbox posture", "warning", warning)
	}
}

func runHub(logger *slog.Logger, configPath string) {
	port := 3001
	if p := os.Getenv("HIVE_HUB_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	logger.Info("starting in HUB mode", "port", port)

	hubSrv := hub.NewHubServer(port, logger, gitShort, gitBranch)
	if cfg, err := config.LoadWithDashboardOverlay(configPath); err == nil {
		notifier := notify.New(cfg.Notifications, logger)
		notifier.SetHiveID(cfg.HiveID)
		buildHookDispatcher(cfg, hookSinks{Notifier: notifier}, logger)
	} else if !errors.Is(err, os.ErrNotExist) {
		logger.Warn("hub hooks disabled: failed to load config", "path", configPath, "error", err)
	}
	installUpgradePauseEmitter(hubSrv)

	// /api/reach (#3994) needs merged-PR metadata (merge SHA, changed
	// files). The hub mode has no ambient GitHub client, so reuse the
	// standard token client when credentials exist; without a token the
	// endpoint reports 503 rather than serving fabricated data. The base
	// branch is the hub's own running branch — the lineage its fleet runs.
	if ghToken := os.Getenv("HIVE_GITHUB_TOKEN"); ghToken != "" {
		reachGH := github.NewClient(ghToken, "kubestellar", []string{"hive"}, logger, "")
		hubSrv.SetReachPRSource(hub.NewGitHubPRSource(reachGH, gitBranch))
	}
	// Wire 2a's heartbeat-fed registry store into the /api/reach endpoint
	// (#3973 epic: producer #3993 → consumer #3994). Unconditional — the
	// registry-backed reporter has no external dependencies, and without it
	// the endpoint would keep answering from the empty stub forever.
	hubSrv.SetReachReporter(hubSrv.RegistryReachReporter())

	// Long-lived SaaS pollers (provision watcher, SHA poller, auth audit,
	// advisory diagnostics) are started here — at the composition root — not
	// inside route registration, so constructing a HubServer stays free of
	// background goroutines.
	hubSrv.StartBackgroundPollers(context.Background())

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

// resolveLiteLLMInferenceRoute resolves the endpoint and model an agent's
// inference route should use for the built-in "litellm" backend. It is the
// whole route-install decision tree for that backend, lifted out of main() so
// it can be unit-tested (#5460); main() calls it and keeps ownership of key,
// CA bundle and logging.
//
// requestedModel is the model the agent asked for ("" when it named none). The
// returned model is that request when non-empty, otherwise the default
// inherited from whichever source supplied the endpoint.
//
// Resolution order — each step matches the behavior shipped in 231ca4b:
//
//  1. local_proxy: the Go translator forwards to the bundled litellm proxy on
//     loopback, overriding any configured remote endpoint.
//  2. the legacy governor.litellm block (HIVE_LITELLM_ENDPOINT or yaml), whose
//     default_model supplies the model.
//  3. the EXPLICIT gateway named by this backend. A hive configured only
//     through the Model Gateways tab leaves the legacy block empty; the key
//     and CA bundle already resolve from that gateway, so the endpoint must
//     too, or NO route is installed and every agent call dies "502 no
//     inference route" while the Gateways tab Test button happily passes
//     (ains-validation/pocketmini, 2026-08-31 — #5393).
//
// ok is false when no source yields an endpoint: the caller must warn and
// install NO route. It never invents an endpoint, and never returns a route
// with an empty endpoint — a silently empty endpoint is the 502 this whole
// path exists to prevent.
func resolveLiteLLMInferenceRoute(cfg *config.Config, backend, requestedModel string) (endpoint, model string, ok bool) {
	lc := cfg.Governor.LiteLLM
	model = requestedModel
	endpoint = lc.ResolveEndpoint()
	if lc.LocalProxy {
		endpoint = litellmLocalProxyURL()
	}
	if endpoint == "" {
		if gw := cfg.Governor.ResolveGateway(backend); gw != nil && gw.Endpoint != "" {
			endpoint = gw.Endpoint
			if model == "" {
				model = gw.DefaultModel
			}
		}
	}
	if endpoint == "" {
		return "", requestedModel, false
	}
	if model == "" {
		model = lc.DefaultModel
	}
	return endpoint, model, true
}

// resolveWatsonxGateway finds the gateway backing the built-in "watsonx" agent
// backend. It prefers a gateway explicitly NAMED watsonx, then falls back to
// the first gateway of KIND watsonx — so `backend: watsonx` works whether the
// operator named their gateway "watsonx" or something descriptive like
// "ibm-granite-prod". Returns nil when no watsonx gateway is configured.
func resolveWatsonxGateway(cfg *config.Config) *config.GatewayConfig {
	gws := cfg.Governor.ResolvedGateways()
	for i := range gws {
		if strings.EqualFold(gws[i].Name, config.GatewayKindWatsonx) &&
			strings.EqualFold(gws[i].Kind, config.GatewayKindWatsonx) {
			gw := gws[i]
			return &gw
		}
	}
	for i := range gws {
		if strings.EqualFold(gws[i].Kind, config.GatewayKindWatsonx) {
			gw := gws[i]
			return &gw
		}
	}
	return nil
}

// resolveGatewayAuth resolves the bearer token and non-secret extra headers an
// agent's inference route should present for a gateway.
//
// For every kind except watsonx this is the resolved API key verbatim and no
// extra headers. watsonx authenticates its OpenAI-compatible model gateway with
// a SHORT-LIVED IAM bearer minted from the IBM Cloud API key (not the raw key)
// and scopes billing/limits by a project id sent as X-IBM-Project-ID, so both
// are set here via the shared process-wide minter (pkg/watsonx.DefaultMinter),
// whose cache means one token is reused across inference, probes and discovery.
//
// Shared by the named-gateway branch and the built-in "watsonx" backend branch
// so the two cannot authenticate differently. Never logs the key or the token.
func resolveGatewayAuth(gw *config.GatewayConfig, agentName, backend string, logger *slog.Logger) (string, map[string]string) {
	apiKey := gw.ResolveAPIKey()
	if !strings.EqualFold(gw.Kind, config.GatewayKindWatsonx) {
		return apiKey, nil
	}
	if token, err := watsonx.DefaultMinter.Token(context.Background(), apiKey); err != nil {
		logger.Warn("watsonx IAM token mint failed; agent inference will fail until the key/project are valid",
			"agent", agentName, "gateway", backend, "error", err.Error())
		// Leave apiKey as-is (the raw key). watsonx will reject it, surfacing a
		// clear upstream 401 rather than a silent success — better than dropping
		// the route entirely.
	} else {
		apiKey = token
	}
	var extraHeaders map[string]string
	if gw.ProjectID != "" {
		extraHeaders = map[string]string{watsonx.ProjectIDHeader: gw.ProjectID}
	}
	return apiKey, extraHeaders
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseEndpointList splits a comma-separated list of URLs into a slice.
// A single URL is returned as a one-element slice.
const (
	// litellmLocalProxyPort is the loopback port the bundled litellm proxy
	// listens on when governor.litellm.local_proxy is enabled. Distinct from
	// proxy.InferenceTranslatePort (18444): agents always talk to the Go
	// translator, which forwards to this local litellm instance.
	litellmLocalProxyPort = 18445
	// litellmLocalConfigPath is the user-provided litellm proxy config
	// (model list, upstream keys) on the /data volume.
	litellmLocalConfigPath = "/data/litellm/config.yaml"
	// litellmRestartDelay is the pause before restarting a crashed local
	// litellm proxy, to avoid a tight crash loop.
	litellmRestartDelay = 5 * time.Second
)

// litellmLocalProxyURL is the endpoint the Go inference translator forwards
// to when the local litellm proxy fallback is enabled.
func litellmLocalProxyURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", litellmLocalProxyPort)
}

// superviseLocalLiteLLM runs the bundled litellm binary as a local
// Anthropic-compat translator fallback (governor.litellm.local_proxy: true),
// restarting it on exit like StartInferenceTranslator's supervision.
// Agents never talk to it directly — the Go translator stays in front so
// per-agent attribution, mode enforcement, and the MITM proxy path are
// preserved.
func superviseLocalLiteLLM(ctx context.Context, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		cmd := exec.CommandContext(ctx, "litellm",
			"--host", "127.0.0.1",
			"--port", strconv.Itoa(litellmLocalProxyPort),
			"--config", litellmLocalConfigPath)
		logger.Info("starting local litellm proxy",
			"port", litellmLocalProxyPort, "config", litellmLocalConfigPath)
		if err := cmd.Run(); err != nil {
			logger.Warn("local litellm proxy exited", "error", err)
		} else {
			logger.Warn("local litellm proxy exited cleanly; restarting")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(litellmRestartDelay):
		}
	}
}

func parseEndpointList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
