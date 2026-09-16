package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
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
