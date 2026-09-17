package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Decision points of runEvalCycle, extracted verbatim so they are testable.
// runEvalCycle calls each in the same place and order as before; side effects
// that are not part of the decision (SendKick, dashboard state, the probe
// stamp) stay at the call site in main.go.

// workSourceIssuesForCycle is the non-GitHub work-source overlay (#4187,
// #4731, #4975). Only the Issues half is replaced; PRs always come from
// GitHub. Both error paths FAIL CLOSED (empty issues, PR maintenance intact)
// so the hive never dispatches against a stale list or the GitHub issues it
// was configured to ignore. The success path applies the same exempt-label
// and issue-filter gates as GitHub enumeration (#4731). wsErr is the
// worksource.FromConfig error; when non-nil, ws is not consulted.
func workSourceIssuesForCycle(
	ctx context.Context,
	ws worksource.WorkSource,
	wsErr error,
	exempt []string,
	filter config.IssueFilterConfig,
	logger *slog.Logger,
) github.IssueResult {
	if wsErr != nil {
		logger.Error("work_source config error; failing closed for issues while preserving GitHub PR maintenance", "error", wsErr)
		return github.IssueResultFromItems([]github.Issue{})
	}
	wsIssues, listErr := ws.ListIssues(ctx)
	if listErr != nil {
		logger.Error("work_source enumeration failed; failing closed for issues while preserving GitHub PR maintenance", "source", ws.SourceType(), "error", listErr)
		return github.IssueResultFromItems([]github.Issue{})
	}
	items := github.FilterExemptIssues(worksource.ToGitHubIssues(wsIssues), exempt)
	filtered := items[:0]
	for _, issue := range items {
		if filter.Admits(issue.Labels) {
			filtered = append(filtered, issue)
		}
	}
	return github.IssueResultFromItems(filtered)
}

// mergeResumeKicks appends crash-restarted agents to the due list ONLY through
// the governor's gate (#2573, #2627): unconditional resume kicks let a
// crash-looping CLI burn tokens on every eval cycle, past any cadence or
// budget. allow is Governor.AllowResumeKick. An agent already due keeps its
// single slot; order is governor-due first, then admitted restarts.
func mergeResumeKicks(agentsDue, restartedAgents []string, allow func(string) bool, logger *slog.Logger) []string {
	if len(restartedAgents) == 0 {
		return agentsDue
	}
	dueSet := make(map[string]bool, len(agentsDue))
	for _, a := range agentsDue {
		agent, _ := config.SplitCadenceTargetKey(a)
		dueSet[agent] = true
	}
	for _, a := range restartedAgents {
		if dueSet[a] {
			continue
		}
		if !allow(a) {
			logger.Info("restarted agent NOT resume-kicked (cadence/budget gate); it will be kicked at its next scheduled slot", "agent", a)
			continue
		}
		agentsDue = append(agentsDue, a)
		logger.Info("adding restarted agent to kick list", "agent", a)
	}
	return agentsDue
}

// kickSkipReason returns a non-empty reason when the governor must never kick
// name on cadence, or "" when it is kickable. Single source of truth for the
// gate so the filter and the audit log cannot drift apart.
func kickSkipReason(
	name string,
	agents map[string]config.AgentConfig,
	onDemandSet map[string]bool,
	isPaused func(string) bool,
) string {
	name, _ = config.SplitCadenceTargetKey(name)
	if ac, ok := agents[name]; ok && ac.OnDemand {
		return "on-demand"
	}
	if onDemandSet[name] {
		return "on-demand-in-pack"
	}
	if isPaused(name) {
		return "operator-paused"
	}
	return ""
}

// partitionKickableAgents splits the due list into the agents that will
// actually be kicked and the ones being skipped, each annotated with why.
// The skipped half exists purely so the eval-cycle log can report it: a
// paused agent was previously reported in "agents_due" every cycle and then
// dropped with no log line at all, which reads as a governor that is kicking
// agents that are in fact idle.
func partitionKickableAgents(
	agentsDue []string,
	agents map[string]config.AgentConfig,
	onDemandSet map[string]bool,
	isPaused func(string) bool,
) (kickable []string, skipped []string) {
	for _, name := range agentsDue {
		if reason := kickSkipReason(name, agents, onDemandSet, isPaused); reason != "" {
			skipped = append(skipped, name+" ("+reason+")")
			continue
		}
		kickable = append(kickable, name)
	}
	return kickable, skipped
}

// filterKickableAgents drops agents the governor must never kick on cadence:
// on-demand agents flagged in config OR in any ACMM pack level (#808, #815 —
// the pack scan is level-independent because the level may not be settled
// when the loop first runs), and operator-paused agents, which must consume
// nothing (#2573) — filtering here keeps them out of BuildKickMessages and the
// audit log instead of producing a spurious SendKick error every cycle.
// A nil result for "nothing due" is the historical contract.
func filterKickableAgents(
	agentsDue []string,
	agents map[string]config.AgentConfig,
	onDemandSet map[string]bool,
	isPaused func(string) bool,
) []string {
	kickable, _ := partitionKickableAgents(agentsDue, agents, onDemandSet, isPaused)
	return kickable
}

// providerBudgetKickGate is the outcome of gateKickMessagesForProviderBudget.
type providerBudgetKickGate struct {
	Kept     []scheduler.KickMessage // kicks that go out this cycle
	Withheld []string                // agents whose kicks were dropped, in message order
	// ReleaseProbe: exactly one kick was let through to probe the provider
	// window; the caller must re-arm suppression (providerBudgetProbe.markReleased).
	ReleaseProbe bool
}

// gateKickMessagesForProviderBudget applies the PROVIDER SPEND REBUFF (#4294)
// to the fully assembled kick list (governor-due + CEL union + review swarm).
// Fresh latch (suppress): drop EVERY kick — the limit is on the key, no agent
// can succeed. Stale latch (latched, !suppress): release exactly ONE probe;
// its inference call is the only thing that can clear or re-freshen the
// latch. Not latched: pass through. An empty list never releases a probe, so
// the probe stamp cannot advance without a real kick going out.
func gateKickMessagesForProviderBudget(messages []scheduler.KickMessage, suppress, latched bool) providerBudgetKickGate {
	if suppress && len(messages) > 0 {
		withheld := make([]string, 0, len(messages))
		for _, msg := range messages {
			withheld = append(withheld, msg.Agent)
		}
		return providerBudgetKickGate{Kept: nil, Withheld: withheld}
	}
	if latched && len(messages) > 0 {
		gate := providerBudgetKickGate{Kept: messages, ReleaseProbe: true}
		if len(messages) > 1 {
			dropped := make([]string, 0, len(messages)-1)
			for _, msg := range messages[1:] {
				dropped = append(dropped, msg.Agent)
			}
			gate.Withheld = dropped
			gate.Kept = messages[:1]
		}
		return gate
	}
	return providerBudgetKickGate{Kept: messages}
}

// doubleSLAMinutes is the age past which an issue is a "2x SLA breach".
const doubleSLAMinutes = 60

// maxSLANotificationsPerCycle caps pages per eval cycle (915bb96a: ten overdue
// issues on a 5-minute eval was 120 pages an hour). Per CYCLE, not deduped —
// the same issues page again next cycle until someone acts.
const maxSLANotificationsPerCycle = 3

// selectSLABreachNotifications returns the first maxSLANotificationsPerCycle
// issues older than doubleSLAMinutes, in enumeration order; capped reports
// that a further qualifying issue was skipped.
func selectSLABreachNotifications(items []github.Issue) (selected []github.Issue, capped bool) {
	for _, issue := range items {
		if issue.AgeMinutes > doubleSLAMinutes {
			if len(selected) >= maxSLANotificationsPerCycle {
				return selected, true
			}
			selected = append(selected, issue)
		}
	}
	return selected, false
}

// advisoryPostFailure classifies a failed App-authenticated digest post. The
// ORDER is the invariant: (1) a 403 "Resource not accessible by integration"
// is write-forbidden — App installed, write refused, attribute honestly via
// classifyGitHubAppWriteForbidden (#2353); (2) any rate-limit text is skipped
// and never touches the banner — a rate-limited 403 is not "App not
// installed" (#1699); (3) everything else (401, 404, 5xx, network) goes to
// classifyGitHubAppFailure, the same verdict as boot and Re-check (106a95fc,
// #2301).
type advisoryPostFailure int

const (
	advisoryPostWriteForbidden advisoryPostFailure = iota
	advisoryPostRateLimited
	advisoryPostAuthProbe
)

// githubWriteForbiddenText is GitHub's body when an installation token holds
// the permission but the repo is not in the installation's selected repos.
const githubWriteForbiddenText = "Resource not accessible by integration"

func classifyAdvisoryPostError(err error) advisoryPostFailure {
	if err == nil {
		return advisoryPostAuthProbe
	}
	text := err.Error()
	if strings.Contains(text, "403") && strings.Contains(text, githubWriteForbiddenText) {
		return advisoryPostWriteForbidden
	}
	if isGitHubRateLimitText(err) {
		return advisoryPostRateLimited
	}
	return advisoryPostAuthProbe
}

// providerBudgetAlert is what the eval cycle should do about the provider
// spend latch this cycle (#4294). Extracted from runEvalCycle so the wording
// and the dedup decisions are testable without a dashboard server, an agent
// manager or the package-level latch (#7232).
//
// The operator only ever learns about a clipped provider through this banner,
// so the exact text is load-bearing: it has to say whether kicks are suspended
// or probing, and it has to distinguish "one refused call" from "refused all
// day" — which is the field failure #4294 was filed for.
type providerBudgetAlert struct {
	// Message is the banner to raise; empty means raise nothing.
	Message string
	// Clear means remove any existing banner.
	Clear bool
	// Cause replaces the caller's providerBudgetCause when non-empty. The
	// latched banner text is reused downstream as the cause string.
	Cause string
}

// decideProviderBudgetAlert chooses the provider-spend banner for this cycle.
//
// quotaReason is a func, not a string, on purpose: the original code only
// consulted the agent manager on the NOT-latched branch, and collecting agent
// statuses every cycle while the provider is clipped would be work the old
// code never did. Passing a thunk keeps that laziness exactly.
func decideProviderBudgetAlert(latched, suppress bool, cause string, since time.Time, rebuffs int, quotaReason func() string) providerBudgetAlert {
	if latched {
		state := "agent kicks suspended"
		if !suppress {
			state = "probing with a single agent kick to test whether the provider window has reset"
		}
		msg := fmt.Sprintf("provider spending limit reached — %s: %s", state, cause)
		if rebuffs > 1 {
			msg = fmt.Sprintf("provider spending limit reached (%d refused calls since %s) — %s: %s",
				rebuffs, since.Format(time.RFC1123), state, cause)
		}
		return providerBudgetAlert{Message: msg, Cause: msg}
	}
	if reason := quotaReason(); reason != "" {
		return providerBudgetAlert{Message: "provider quota exhausted — " + reason}
	}
	return providerBudgetAlert{Clear: true}
}

// ── Kick dispatch (#7232) ───────────────────────────────────────────────────

// kickDispatchDeps carries the effects the kick-dispatch loop performs, so the
// loop's DECISIONS can be exercised without a tmux session, a governor, a
// dashboard, or a live tracing exporter.
//
// The header comment above says effects stay at the call site. Dispatch is the
// one place that could not follow that rule: its decisions are not separable
// from its effects, because each decision is defined BY an effect it must or
// must not perform -- "withhold this kick" means SendKick is not called,
// "release the probe" means the stamp is written exactly once. A pure function
// returning a plan would not have pinned the thing that actually matters here,
// which is the ordering and the skip paths.
//
// Every field is required; dispatchAgentKicks does not nil-check them, because
// a silently-skipped effect is precisely the failure this seam exists to
// prevent. The production wiring in runEvalCycle supplies all of them.
type kickDispatchDeps struct {
	// backoffRemaining reports a per-agent provider-error backoff.
	backoffRemaining func(agent string) (time.Duration, string, string, bool)
	// sendKick delivers the kick. A non-nil error means nothing was sent.
	sendKick func(agent, message string) error
	// startKickSpan opens the agent.kick tracing span and returns its closer;
	// the closer takes the SendKick error (nil on success) so a failed kick is
	// recorded on the span before it ends.
	startKickSpan func(agent string) func(err error)
	// onReviewDelivered records a delivered review kick and persists the
	// dispatch state. Split from onDelivered because the original code runs it
	// BEFORE the span closes and before the probe stamp, and this extraction
	// preserves effect ORDER exactly rather than merely preserving the set of
	// effects.
	onReviewDelivered func(msg scheduler.KickMessage)
	// onDelivered runs last, for the effects that must NOT happen when a kick
	// was withheld or failed: governor repo accounting, audit log, lifecycle
	// timeline, token snapshot.
	onDelivered func(msg scheduler.KickMessage)
	// markProbeReleased stamps the provider-budget probe. Called at most once
	// per dispatch, and only after a kick actually goes out.
	markProbeReleased func(at time.Time)
	// now supplies the probe stamp's timestamp.
	now func() time.Time
}

// dispatchAgentKicks sends this cycle's kick messages, applying the two skip
// rules and the single-probe rule.
//
// It returns the agents whose kicks were actually delivered, in order, which
// is what lets a caller (and a test) distinguish "withheld" from "failed" from
// "sent" without reaching into the effects.
//
// The rules, all of which are load-bearing and individually guarded:
//
//   - An agent inside a provider-error backoff is skipped entirely. No span is
//     opened for it, because a withheld kick is not an attempted kick.
//   - A kick whose send FAILS performs none of the delivered-effects. Recording
//     an audit entry or a timeline kick for a kick that never landed would make
//     the dashboard assert something that did not happen.
//   - The probe stamp is written at most ONCE, after the first kick that
//     actually goes out. Stamping on a withheld or failed kick would re-arm
//     suppression without having learned anything about the provider, which is
//     the entire point of releasing a probe.
func dispatchAgentKicks(msgs []scheduler.KickMessage, releaseProbe bool, deps kickDispatchDeps, logger *slog.Logger) []string {
	var delivered []string
	for _, msg := range msgs {
		if remaining, class, line, ok := deps.backoffRemaining(msg.Agent); ok {
			logger.Warn("provider inference error: withholding agent kick during backoff",
				"agent", msg.Agent,
				"class", class,
				"retry_in", remaining.Round(time.Second),
				"error", line)
			continue
		}
		endSpan := deps.startKickSpan(msg.Agent)
		logger.Info("audit: governor kicking agent", "agent", msg.Agent, "trigger", "governor-eval")
		if err := deps.sendKick(msg.Agent, msg.Message); err != nil {
			endSpan(err)
			logger.Warn("failed to send kick", "agent", msg.Agent, "error", err)
			continue
		}
		deps.onReviewDelivered(msg)
		endSpan(nil)
		if releaseProbe {
			deps.markProbeReleased(deps.now())
			releaseProbe = false
		}
		deps.onDelivered(msg)
		delivered = append(delivered, msg.Agent)
	}
	return delivered
}

// advisoryIngestDeps are the side effects of recording an ioscan canary leak
// found in a newly ingested advisory finding. Decisions (whether a finding is
// scanned, whether a leak blocks it) live in gateAdvisoryFindings; the effects
// stay at the call site in runEvalCycle, same split as kickDispatchDeps.
type advisoryIngestDeps struct {
	// scanCanary runs the canary scan over one finding's report text. nil when
	// ioscan or its canaries are disabled: nothing is scanned, nothing blocked.
	scanCanary func(agent, reportText, source string) (ioscan.CanaryLeak, bool)
	// failClosed is cfg.Ioscan.FailClosed(): a leaking finding is withheld from
	// persistence instead of merely being recorded.
	failClosed bool
	// auditLog records the leak in the dashboard audit trail.
	auditLog func(actor, action, detail, agent string)
	// recordLeakBead persists the critical canary-leak bead for the agent.
	recordLeakBead func(leak ioscan.CanaryLeak)
}

// gateAdvisoryFindings is runEvalCycle's ioscan canary gate over one cycle's
// newly read advisory findings, extracted verbatim behind a seam (#7232).
//
// The rules, each previously unreachable without a live dashboard and bead
// stores:
//
//   - With canaries disabled (scanCanary == nil) every finding passes through
//     unscanned; the gate never blocks on configuration alone.
//   - A leak is ALWAYS recorded — audit entry plus critical bead — whether or
//     not it blocks. Fail-open still leaves evidence.
//   - Only failClosed turns a leak into a withheld finding. The scan text is
//     the finding's title, detail, file, type and severity joined by newlines,
//     so a canary smuggled into any of those fields is caught.
func gateAdvisoryFindings(findings []advisory.Finding, deps advisoryIngestDeps, logger *slog.Logger) []advisory.Finding {
	safeFindings := make([]advisory.Finding, 0, len(findings))
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
		if deps.scanCanary != nil {
			reportText := strings.Join([]string{f.Title, f.Detail, f.File, f.Type, f.Severity}, "\n")
			if leak, ok := deps.scanCanary(f.Agent, reportText, "advisory-finding"); ok {
				detail := fmt.Sprintf("rule=%s, agent=%s, source=%s", ioscan.CanaryLeakRule, leak.Agent, leak.Source)
				deps.auditLog(leak.Agent, "ioscan_canary_leak", detail, leak.Agent)
				deps.recordLeakBead(leak)
				blockFinding = deps.failClosed
			}
		}
		if blockFinding {
			logger.Warn("ioscan fail-closed blocked advisory finding with canary leak", "agent", f.Agent)
			continue
		}
		safeFindings = append(safeFindings, f)
	}
	return safeFindings
}
