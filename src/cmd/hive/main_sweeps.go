package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/tracing"
	"github.com/hivecommons/hive/pkg/worksource"
	"go.opentelemetry.io/otel/attribute"
)

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

// hivePRObservations projects the enumerated hive-authored PRs into escalation
// observations (repo fully-qualified, Red == a required check failed). Shared
// by recordRedStaleness and the reaper so both classify PRs identically.
//
// owners maps "org/repo#number" to the agent that opened the PR (see
// auditPRAgents); pass nil when ownership is not needed. Shared/base CI
// breakage is classified across every PR in the pass before human and
// non-Hive observations are removed. A green PR is counter-evidence, and an
// independent human-authored PR must confirm the shared failure.
func hivePRObservations(cfg *config.Config, actionable *github.ActionableResult, owners map[string]string) []escalation.Observation {
	if actionable == nil {
		return nil
	}
	all := allEscalationObservations(cfg, actionable, owners)
	agentObs := all[:0]
	for i, pr := range actionable.PRs.Items {
		if isHiveAgentAuthor(cfg, pr.Author) {
			agentObs = append(agentObs, all[i])
		}
	}
	return agentObs
}

func allEscalationObservations(cfg *config.Config, actionable *github.ActionableResult, owners map[string]string) []escalation.Observation {
	all := make([]escalation.Observation, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	for _, pr := range actionable.PRs.Items {
		all = append(all, escalationObservation(cfg, pr, owners))
	}
	for _, pr := range actionable.PRs.Held {
		all = append(all, escalationObservation(cfg, pr, owners))
	}
	escalation.MarkSharedFailures(all)
	return all
}

// escalationObservation projects one enumerated PR into the fix-loop ledger's
// view of it. Red means a required check concluded failure. Pending means this
// pass could not conclude CI at all — checks still running, no check runs, or
// (per EnrichCIStatus) the check-run fetch errored — which the ledger must
// treat as "no information", never as "went green". Labeled mirrors the forge's
// needs-human label so the ledger and the label can never disagree about
// whether a PR has already been handed to a human. Labels carry the PR's
// current forge labels so Sweep can reconcile reviewer-lane verdicts
// (label-only edits: needs-human removed, reviewer-passed added) back into
// the ledger (#5511, gap G1).
func escalationObservation(cfg *config.Config, pr github.PullRequest, owners map[string]string) escalation.Observation {
	repo := pr.Repo
	if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
		repo = cfg.Project.Org + "/" + repo
	}
	red := pr.HasFailingRequiredCheck()
	return escalation.Observation{
		Repo:               repo,
		Number:             pr.Number,
		HeadSHA:            pr.HeadSHA,
		HeadTree:           pr.HeadTree,
		Red:                red,
		Pending:            !red && pr.CIStatus != "success",
		CIRunning:          red && pr.CIChecksRunning,
		Labeled:            escalation.HasNeedsHumanLabel(pr.Labels),
		Excerpt:            pr.CIFailureExcerpt,
		Labels:             pr.Labels,
		FailingChecks:      pr.FailingChecks,
		BaseRedChecks:      pr.BaseRedChecks,
		SharedCIIncident:   pr.SharedCIIncident,
		Agent:              owners[escalation.Key(repo, pr.Number)],
		IndependentControl: isIndependentCIControl(cfg, pr),
	}
}

// redPRFixOwner resolves who should be asked to repair a red PR: the agent the
// audit trail attributes it to, or — when that agent is unattributed — the
// fleet's primary PR creator, exactly the default formatRedPRFixData applies
// to an unattributed ci-failing.json row. Keeping the two in step is what lets
// a hook predicate (`t.agent == "scanner"`) and the FIX-BEFORE-NEW block name
// the same owner (hivecommons/hive#9474).
func redPRFixOwner(agent string) string {
	if agent = strings.TrimSpace(agent); agent != "" {
		return agent
	}
	return review.DefaultFixerAgent
}

// dependencyBots are forge bots whose PRs are dependency bumps, not hive fix
// attempts. They carry the "[bot]" suffix that otherwise marks a PR as
// agent-authored, but nothing in the hive opened them and no hive agent is
// iterating on them, so a red one is not a fix loop to break: escalating it
// only pages a human with "1 distinct fix attempts" about a crate bump that
// renovate will rebase on its own. Mirrors the hub's default contribute
// deny-authors list (config: hub.contribute_deny_authors).
var dependencyBots = map[string]bool{
	"renovate[bot]":    true,
	"dependabot[bot]":  true,
	"mergeraptor[bot]": true,
}

// isHiveAgentAuthor reports whether a PR author is one of OUR agents — the
// configured ai_author, the App bot identity agents author as, or another
// bot account — excluding the dependency bots above. Shared by every fix-loop
// path (staleness clock, reaper, escalation sweep) so they classify PRs
// identically.
func isHiveAgentAuthor(cfg *config.Config, author string) bool {
	if author == "" {
		return false
	}
	if author == cfg.Project.AIAuthor {
		return true
	}
	if eff := cfg.EffectiveAIAuthor(); eff != "" && author == eff {
		return true
	}
	return strings.HasSuffix(author, "[bot]") && !dependencyBots[author]
}

// isIndependentCIControl reports whether a PR supplies human-authored evidence
// independent of Hive. Bot-authored and Hive-attributed contributor-relay PRs
// cannot prove that a failure also affects unrelated human work.
func isIndependentCIControl(cfg *config.Config, pr github.PullRequest) bool {
	return !isHiveAgentAuthor(cfg, pr.Author) && !pr.AuthorNonHuman && !pr.HiveAttributed
}

// recordRedStaleness updates the shared staleness clock (first-seen-red per red
// head SHA) for every hive-authored PR in this pass. It must run before the
// claim guard and the reaper so their StaleRed() reads reflect the current tick.
// A disabled escalation subsystem skips it (the whole fix-loop machinery is off).
//
// It also fires the escalation_red hook for each red PR, WITH the owning agent
// on the payload: the catalog and hooks.md have always documented `agent` on
// this transition, but nothing ever set it, so a predicate like
// `t.agent == "scanner"` could never match (hivecommons/hive#9474).
func recordRedStaleness(cfg *config.Config, actionable *github.ActionableResult) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	owners := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
	obs := hivePRObservations(cfg, actionable, owners)
	getEscalationStore().ObserveRed(obs)
	for _, ob := range obs {
		if !ob.Red {
			continue
		}
		hookDispatcher().Fire(context.Background(), hooks.Payload{
			Transition: hooks.TransitionEscalationRed,
			Repo:       ob.Repo,
			Agent:      redPRFixOwner(ob.Agent),
			Reason:     "required CI check red",
			Attrs: map[string]string{
				hooks.AttrPR: strconv.Itoa(ob.Number),
				"head_sha":   ob.HeadSHA,
				"excerpt":    ob.Excerpt,
			},
		})
	}
}

// fixKicker is the out-of-band kick path the re-engagement lanes deliver
// through. crux.Runtime satisfies it via agentKicker, which is the same
// adapter the governor tick's replan lane uses.
type fixKicker interface {
	Kick(agent, message string) error
}

// agentAvailability reports, per agent, whether a kick sent right now could
// reach it: the agent must not be paused by the manager, and at least one
// governor mode must schedule it with a non-paused cadence. An agent paused in
// every mode (the v5 L6 pack pauses `strategist` everywhere) can never answer a
// FIX-BEFORE-NEW kick, so charging its red PRs' re-engagement budget only
// escalates them to a human having asked nobody (hivecommons/hive#9472).
func agentAvailability(cfg *config.Config, mgr crux.Runtime) func(string) bool {
	return func(name string) bool {
		if name == "" {
			return false
		}
		if mgr != nil && mgr.IsPaused(name) {
			return false
		}
		return cfg == nil || cadenceReachable(cfg, name)
	}
}

// cadenceReachable reports whether ANY governor mode gives the agent a cadence
// that is not paused/off. An agent with no cadence entry at all is treated as
// reachable: "nothing schedules it" is a fleet-configuration signal of its own
// (NoCadenceAgents), and refusing to route repairs to it would silently widen
// this gate into an agent-disablement policy.
func cadenceReachable(cfg *config.Config, name string) bool {
	configured := false
	for mode := range cfg.Governor.Modes {
		cad := cfg.CadenceValueForMode(name, mode)
		if cad == "" {
			continue
		}
		configured = true
		if !cad.IsPaused() {
			return true
		}
	}
	return !configured
}

// ownerReEngageCooldown is the minimum spacing between two re-engagements of a
// PR owned by this agent: its slowest configured cadence, so a budget of six
// attempts cannot be spent inside a single cadence window where no kick the
// owner could answer would ever fit. The escalation store floors it at
// ReEngageCooldown and caps it at MaxReEngageCooldown.
func ownerReEngageCooldown(cfg *config.Config, name string) time.Duration {
	if cfg == nil || name == "" {
		return 0
	}
	var slowest time.Duration
	for mode := range cfg.Governor.Modes {
		cad := cfg.CadenceValueForMode(name, mode)
		if cad == "" || cad.IsPaused() {
			continue
		}
		d, err := time.ParseDuration(strings.TrimSpace(cad.Interval()))
		if err == nil && d > slowest {
			slowest = d
		}
	}
	return slowest
}

// resolveRedPRFixer picks the agent a stuck red PR's fix kick should go to: its
// owner, else the configured review fixer agent, else the fleet's default
// fixer — the same chain the review-fix kick uses (src/docs/review-swarm.md).
// It returns "" when no candidate is reachable, which tells the caller to leave
// the PR alone rather than spend budget nobody can answer.
func resolveRedPRFixer(cfg *config.Config, owner string, available func(string) bool) string {
	candidates := []string{redPRFixOwner(owner), strings.TrimSpace(cfg.Review.FixerAgent), review.DefaultFixerAgent}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if available == nil || available(c) {
			return c
		}
	}
	return ""
}

// redPRFixKick renders the targeted FIX-BEFORE-NEW kick a re-engagement
// delivers. It names ONE PR — the stuck one — with its evidence, so the kick is
// answerable on its own rather than relying on the owner's next cadence kick
// happening to carry the ci-failing.json block.
func redPRFixKick(o escalation.Observation, rerouted bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 FIX-BEFORE-NEW — %s#%d is red and has not moved\n\n", o.Repo, o.Number)
	if rerouted {
		fmt.Fprintf(&b, "Its owning agent (%s) is not reachable right now, so this repair is routed to you.\n\n", redPRFixOwner(o.Agent))
	}
	b.WriteString("Repair it BEFORE claiming new issues or opening any new PR:\n")
	if slices.Contains(o.Labels, stagelabels.NoDCOLabel) {
		fmt.Fprintf(&b, "  FIRST (required): the PR is %s — it inherited unsigned commits. Re-sign them: gh pr checkout %d --repo %s → git rebase --signoff <base> (original authors preserved) → git push --force-with-lease\n", stagelabels.NoDCOLabel, o.Number, o.Repo)
		b.WriteString("  THEN fix any failing CI the same way.\n")
	}
	fmt.Fprintf(&b, "  gh pr checkout %d --repo %s → fix → git commit -s → git push\n", o.Number, o.Repo)
	b.WriteString("Push to the SAME branch. Do NOT open a replacement PR.\n")
	b.WriteString("After the push, STOP: do not watch, poll, or sleep on CI (no gh run watch/view loops).\n")
	b.WriteString("The automerge sweep merges the PR once it is green; if the failure is infrastructure\n")
	b.WriteString("(runner lost, no space left, checks still queued) leave one comment and end the turn.\n")
	if o.HeadSHA != "" {
		fmt.Fprintf(&b, "\nhead: %s\n", o.HeadSHA)
	}
	if len(o.FailingChecks) > 0 {
		fmt.Fprintf(&b, "failing: %s\n", strings.Join(o.FailingChecks, ", "))
	}
	if excerpt := strings.TrimSpace(o.Excerpt); excerpt != "" {
		if runes := []rune(excerpt); len(runes) > redPRFixKickExcerptRunes {
			excerpt = string(runes[:redPRFixKickExcerptRunes]) + "…"
		}
		fmt.Fprintf(&b, "\nevidence:\n%s\n", excerpt)
	}
	return b.String()
}

// redPRFixKickExcerptRunes bounds the CI evidence carried in a targeted fix
// kick, matching the FIX-BEFORE-NEW block's own budget.
const redPRFixKickExcerptRunes = 400

// mergeReEngageHook builds the Fix #2 re-engagement callback for the merge
// watcher. It normalizes the repo, resolves the PR's owner, DELIVERS a targeted
// fix kick to it (or to the fallback fixer when the owner is unreachable), and
// only then records the attempt under the shared escalation store's per-red-SHA
// cap. Passing an empty head SHA tells the store to reuse the red head SHA it
// last observed for this PR (the eval cycle's ObserveRed keeps it current), so
// the merge watcher does not need to re-fetch the head. Returns false when the
// cap is exhausted or no kick could be delivered, so the watcher can log that
// the escalation path now owns the PR. A disabled escalation subsystem yields a
// nil hook (watcher falls back to quarantine-only).
//
// Delivery is the point: before hivecommons/hive#9472 this only incremented a
// counter, so a PR could be escalated as "6 automated fix re-dispatches" having
// never been mentioned to any agent.
func mergeReEngageHook(cfg *config.Config, kicker fixKicker, available func(string) bool, logger *slog.Logger) github.MergeReEngageFunc {
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
		full := fullRepo(repo)
		owners := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
		o := escalation.Observation{
			Repo:   full,
			Number: number,
			Agent:  owners[escalation.Key(full, number)],
		}
		// Empty head SHA: reuse the store's tracked current red SHA for this PR
		// (do not reset the cap counter). The eval cycle records it via
		// ObserveRed; if the store has never seen this PR red, the store still
		// allows the first MaxReEngagements nudges.
		return deliverReEngagement(cfg, getEscalationStore(), o, "", kicker, available, logger)
	}
}

// deliverReEngagement is the one place a fix re-dispatch is both SENT and
// COUNTED. It resolves a reachable fixer, checks the budget without spending
// it, delivers the kick, and only records the attempt once the kick was
// accepted: an undeliverable kick (agent not running, provider backoff,
// restart) must not consume a PR's budget and strand it in needs-human
// (hivecommons/hive#9472).
func deliverReEngagement(
	cfg *config.Config,
	store *escalation.Store,
	o escalation.Observation,
	headSHA string,
	kicker fixKicker,
	available func(string) bool,
	logger *slog.Logger,
) bool {
	fixer := resolveRedPRFixer(cfg, o.Agent, available)
	if fixer == "" {
		// Nobody can be asked right now. Charging budget here is what escalated
		// red strategist PRs on a pack that pauses strategist in every mode.
		if logger != nil {
			logger.Info("re-engagement skipped: no reachable fixer agent",
				"repo", o.Repo, "pr", o.Number, "owner", redPRFixOwner(o.Agent))
		}
		return false
	}
	if !store.ReEngageEligible(o.Repo, o.Number, headSHA, ownerReEngageCooldown(cfg, fixer)) {
		return false
	}
	if kicker == nil {
		return false
	}
	if err := kicker.Kick(fixer, redPRFixKick(o, fixer != redPRFixOwner(o.Agent))); err != nil {
		if logger != nil {
			level := slog.LevelWarn
			// Pool contention is expected backpressure: retry on a later sweep.
			// Keep it below WARN so slogfeed does not file a runtime defect
			// for a healthy shared pool refusing another session (#15350).
			if isSharedPoolBusyErr(err) {
				level = slog.LevelInfo
			}
			logger.Log(context.Background(), level, "re-engagement kick undeliverable; budget not charged",
				"repo", o.Repo, "pr", o.Number, "agent", fixer, "error", err)
		}
		return false
	}
	store.RecordReEngagement(o.Repo, o.Number)
	return true
}

// reapStuckRedPRs is Fix #4: the governor's backstop sweep. For each
// hive-authored PR that is red on a required check AND stale (StaleRed) AND not
// already escalated to a human, it DELIVERS a targeted FIX-BEFORE-NEW kick to
// the PR's owner (or the fallback fixer when the owner is unreachable) and
// records the re-engagement only if that kick was accepted — deduped and capped
// via the escalation store. Entirely generic: it keys only off check state +
// staleness, never a linter.
func reapStuckRedPRs(
	cfg *config.Config,
	actionable *github.ActionableResult,
	escalatedPRs map[string]bool,
	kicker fixKicker,
	available func(string) bool,
	logger *slog.Logger,
) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	store := getEscalationStore()
	owners := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
	for _, o := range hivePRObservations(cfg, actionable, owners) {
		if !o.Red {
			continue
		}
		if o.Shared {
			// Every failing check is red across the fleet: the PR is not
			// broken, a shared incident is. Nudging its owner to "fix" it
			// wastes budget on work that cannot succeed (hivecommons/hive#9473).
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
		if !deliverReEngagement(cfg, store, o, o.HeadSHA, kicker, available, logger) {
			// Cap reached, cooldown not elapsed, or nothing could be delivered.
			// The distinct-SHA escalation path owns it from here.
			continue
		}
		logger.Info("reaper: re-dispatched fix for stuck red PR",
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

	type prMeta struct{ checks []string }
	meta := map[string]prMeta{}
	owners := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
	all := allEscalationObservations(cfg, actionable, owners)
	itemCount := len(actionable.PRs.Items)

	obs := all[:0]
	for i, o := range all {
		var pr *github.PullRequest
		if i < itemCount {
			pr = &actionable.PRs.Items[i]
		} else {
			pr = &actionable.PRs.Held[i-itemCount]
		}
		if pr.ReporterTrustReason != "" {
			escalationStore.SetReporterTrustReason(o.Repo, o.Number, pr.ReporterTrustReason)
		}
		if (i >= itemCount || !isHiveAgentAuthor(cfg, pr.Author)) && pr.ReporterTrustReason == "" {
			continue
		}
		obs = append(obs, o)
		meta[escalation.Key(o.Repo, o.Number)] = prMeta{checks: o.FailingChecks}
	}
	results := escalationStore.Sweep(obs, cfg.Escalation.EffectiveThreshold())

	for _, o := range obs {
		key := escalation.Key(o.Repo, o.Number)
		r, ok := results[key]
		if !ok {
			continue
		}
		if r.Unparked {
			// The sweep returned this PR to the automated lane by itself: its
			// head went green, or the shared incident it escalated over
			// cleared. Take the label off (retried on later passes until a
			// pass sees it gone) and say why, once.
			unparkEscalatedPR(ctx, writer, o, r, logger)
			continue
		}
		if r.Escalated {
			escalated[key] = true
		}
		if r.BaseRedHold {
			// Every failing check is red on the base branch too: say so
			// once per head, count nothing, kick no one. The hold clears
			// by itself when the base recovers and the head is re-evaluated.
			body := escalation.BaseRedHoldCommentBody(o.BaseRedChecks, o.SharedCIIncident)
			if err := writer.CreateIssueComment(ctx, o.Repo, o.Number, body); err != nil {
				logger.Warn("base-branch-red hold comment failed; will retry next pass", "repo", o.Repo, "pr", o.Number, "error", err)
			} else {
				escalationStore.MarkBaseRedHeld(o.Repo, o.Number, o.HeadSHA)
				logger.Info("fix loop holding: base branch red", "repo", o.Repo, "pr", o.Number, "head_sha", o.HeadSHA, "checks", strings.Join(o.BaseRedChecks, ","))
			}
		}
		if r.NeedsLabel && !o.Labeled {
			// Escalated on an earlier pass but the label never landed (the
			// AddLabels call failed). Retry the LABEL ONLY — the evidence
			// comment already reached the human and must not be repeated.
			if err := writer.AddLabels(ctx, o.Repo, o.Number, []string{escalation.NeedsHumanLabel}); err != nil {
				logger.Warn("escalation label retry failed", "repo", o.Repo, "pr", o.Number, "error", err)
			} else {
				escalationStore.MarkLabelApplied(o.Repo, o.Number)
			}
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
		ev := escalation.Evidence{
			Attempts:      r.Attempts,
			FailingChecks: meta[key].checks,
			SharedChecks:  r.SharedChecks,
			Excerpt:       excerpt,
			Exhausted:     r.Exhausted,
			ReEngagements: r.ReEngagements,
		}
		body := escalation.CommentBody(ev)
		afterReviewerPass := false
		if sha, at, ok := escalationStore.ReviewerPass(o.Repo, o.Number); ok {
			body = escalation.HandoffCommentBody(ev, escalation.ReviewerHandoff{SHA: sha, At: at})
			afterReviewerPass = true
		}
		if err := writer.CreateIssueComment(ctx, o.Repo, o.Number, body); err != nil {
			// Retry next pass rather than marking escalated with no comment:
			// the whole point is that the evidence reaches a human.
			logger.Warn("escalation comment failed; will retry next pass",
				"repo", o.Repo, "pr", o.Number, "error", err)
			continue
		}
		// Mark escalated BEFORE the label call: once the comment is on the
		// PR, nothing may post it again, whatever happens to the label.
		escalationStore.MarkEscalated(o.Repo, o.Number)
		if err := writer.AddLabels(ctx, o.Repo, o.Number, []string{escalation.NeedsHumanLabel}); err != nil {
			logger.Warn("escalation label failed; will retry next pass", "repo", o.Repo, "pr", o.Number, "error", err)
		} else {
			escalationStore.MarkLabelApplied(o.Repo, o.Number)
		}
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

// labelRemover is the optional forge capability the auto-un-park path needs.
// Every real adapter (GitHub, GitLab, Gitea) implements RemoveLabel on
// forge.Forge; the narrow IssueWriter the sweep takes does not declare it, so
// a writer that cannot remove labels simply leaves the PR labeled and logs.
type labelRemover interface {
	RemoveLabel(ctx context.Context, repo string, number int, label string) error
}

// unparkEscalatedPR performs the forge side of an automatic un-park: the
// needs-human label comes off, and — on the pass that decided it — a one-line
// comment says why. Before hivecommons/hive#9473 nothing did this: a PR
// escalated over a base-branch or runner outage stayed parked after the
// incident cleared, and a labeled PR that went green stayed parked forever,
// because the labeled branch returned before the green branch.
func unparkEscalatedPR(ctx context.Context, writer forge.IssueWriter, o escalation.Observation, r escalation.Result, logger *slog.Logger) {
	remover, ok := writer.(labelRemover)
	if !ok {
		logger.Warn("cannot un-park escalated PR: forge writer cannot remove labels",
			"repo", o.Repo, "pr", o.Number)
		return
	}
	if err := remover.RemoveLabel(ctx, o.Repo, o.Number, escalation.NeedsHumanLabel); err != nil {
		// Retried on the next pass: the ledger keeps asking until a pass
		// observes the label gone.
		logger.Warn("un-park label removal failed; will retry next pass",
			"repo", o.Repo, "pr", o.Number, "error", err)
		return
	}
	if r.UnparkReason == "" {
		return // label-removal retry: the comment was posted on the deciding pass
	}
	if err := writer.CreateIssueComment(ctx, o.Repo, o.Number, escalation.UnparkCommentBody(r.UnparkReason)); err != nil {
		logger.Warn("un-park comment failed", "repo", o.Repo, "pr", o.Number, "error", err)
	}
	logger.Info("escalation un-parked", "repo", o.Repo, "pr", o.Number, "reason", r.UnparkReason)
}

// autoMergeSweepInterval is the minimum spacing between label-queued
// auto-merge sweeps. The sweep piggybacks on the governor eval tick, which can
// fire much more often than once a minute; this floor keeps the sweep from
// hammering the GitHub API on short eval intervals.
const autoMergeSweepInterval = time.Minute

// autoMergeSweepTickBudget is how long the queued automerge sweep may wait
// for its first pacing slot before the sweep is shed to the next cycle.
const autoMergeSweepTickBudget = 5 * time.Second

// taskListSweepInterval is the minimum spacing between task-list sweeps. The
// sweep enumerates every open issue in every repo — much heavier than the
// auto-merge sweep's label-scoped query — and "done" moves at PR-merge cadence,
// so a 15-minute floor keeps the API cost modest while still closing completed
// epics on the same day the last box gets ticked.
const taskListSweepInterval = 15 * time.Minute

// supersessionSweepInterval is the minimum spacing between sweeps that retire
// open PRs whose claimed issue was already closed by a different merged PR.
// It walks open PRs and may inspect issue timelines and file lists, so it uses
// the same conservative cadence as the task-list sweep.
const supersessionSweepInterval = 15 * time.Minute

// duplicateFixSweepInterval is the minimum spacing between sweeps that
// resolve two open PRs fixing the same issue (hivecommons/hive#14799). It
// reuses the open-PR listing the pre-dispatch guard caches and reads check
// runs per pair, so it runs on the supersession cadence.
const duplicateFixSweepInterval = 15 * time.Minute

// sharedCIRerunInterval is the minimum spacing between shared-CI rerun
// sweeps. Each pass lists recently closed issues per repo and only walks open
// PRs when a [shared-ci] incident closed inside the lookback window, so it
// shares the supersession sweep's cadence.
const sharedCIRerunInterval = 15 * time.Minute

// duplicateSweepInterval is the minimum spacing between duplicate sweeps. It
// is an hour rather than the task-list sweep's fifteen minutes for two
// reasons. Cost: the sweep fingerprints the changed-file set of every open PR,
// which on a large queue is the heaviest read the hive performs (the head-SHA
// cache makes steady-state passes nearly free, but a cold pass is not).
// Signal: duplicates accumulate at human-PR-opening cadence, so nothing is
// lost by noticing one an hour later, while a tighter loop only multiplies the
// chance of editing a suggestion under a reader's cursor.
const duplicateSweepInterval = time.Hour

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
func runRotationCheck(ctx context.Context, cfg *config.Config, rotMgr *rotation.Manager, gov *governor.Governor, agentMgr crux.Runtime, logger *slog.Logger) {
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
		if proc.State == crux.StateRunning {
			continue
		}

		cadenceS := 0
		if interval := governorShortestActiveInterval(govState, name); interval > 0 {
			cadenceS = int(interval / time.Second)
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
	// Housekeeping against the pacer: while a caution window has the pacing
	// queue deeper than this sweep could wait, skip the sweep (it is due
	// again next cycle) rather than queue behind the enumeration.
	if github.ShedLowPriorityTick("automerge:queued-sweep", autoMergeSweepTickBudget) {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	ctx = github.WithPacingClass(ctx, github.PacingLow)
	// governor.automerge covers the queued, trusted-author and contributor-
	// lane sweeps plus the stage-label pass that follows them. The sweep runs
	// on its own cadence in the run loop, so this is a sibling of
	// governor.eval_cycle rather than a child.
	ctx, sweepSpan := tracing.StartSpan(ctx, "governor.automerge")
	defer sweepSpan.End()
	opts := automerge.Options{
		Logger:                  logger,
		MergerAuthorizer:        trustedMergerFunc(cfg),
		HumanChangeRequests:     review.DefaultHumanChangeRequests(),
		TrustedAuthorizer:       trustedAuthorFunc(cfg),
		TrustedAuthorPolicy:     trustedAuthorPolicyFunc(cfg),
		ContributorLanePolicy:   contributorLanePolicyFunc(cfg),
		ContributorLaneVerdict:  contributorLaneVerdictFunc(cfg, logger),
		ContributorRegistryTier: contributorRegistryTierFunc(),
		StageLabelPolicy:        stageLabelPolicyFunc(cfg),
		StageEngine:             stageEngineOptions(cfg, logger),
	}
	syncStageEngineMode(cfg, ghClient, logger)
	// hivecommons/hive#12436: the contribute hub is the stage-072 nudge sink
	// (relay fix-pr tasks). Guarded so a nil hub never becomes a typed-nil
	// interface.
	if hub := dashSrv.ContributeHub(); hub != nil {
		opts.ContributorNudge = hub
	}
	if cfg != nil {
		opts.SentinelLabel = func() string { return cfg.Sentinel.LabelOrDefault() }
		if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
			opts.RequiredChecks = set
		}
		opts.RequiredChecksForRepo = cfg.AutoMerge.RequiredCheckSetForRepo
		opts.MinHeadAge = cfg.AutoMerge.EffectiveMinHeadAge()
		opts.RepoAutoMergeEnabled = func(repo string) bool { return cfg.RepoAutoMergeEnabled(repo) }
		opts.RepoOnGitHub = func(repo string) bool { return cfg.RepoOnGitHub(repo) }
		opts.HumanMergePaths = func(repo string) []string { return humanMergePathsFor(cfg, repo) }
		opts.HumanMergeRule = func() prstage.HumanMergeRule { return humanMergeRuleFor(cfg) }
		opts.RequireCurrentBase = func(repo string) bool { return cfg.AutoMerge.RequireCurrentBaseFor(repo) }
		opts.RequireCurrentBaseStrict = func(repo string) bool { return cfg.AutoMerge.RequireCurrentBaseStrictFor(repo) }
		opts.MergeForwardTrustedHuman = func(repo string) bool { return cfg.AutoMerge.MergeForwardTrustedHumanFor(repo) }
		opts.MergeTrain = func() automerge.MergeTrainPolicy {
			return automerge.MergeTrainPolicy{Enabled: cfg.AutoMerge.MergeTrain.IsEnabled(), MaxRebaseAttempts: cfg.AutoMerge.MergeTrain.EffectiveMaxRebaseAttempts()}
		}
		opts.TrustedBotAuthors = func() map[string]bool { return cfg.AutoMerge.TrustedBotAuthorSet() }
	}
	audit := func(event automerge.AutoMergeSweepEvent) {
		if dashSrv == nil {
			return
		}
		detail := fmt.Sprintf("repo=%s, pr=%d, author=%s, queued_by=%s, label=%s, tier=%s, head_sha=%s, merge_sha=%s",
			event.Repo, event.Number, event.Author, event.QueuedBy, event.Label, event.Tier, event.HeadSHA, event.MergeSHA)
		dashSrv.AuditLogRecord("system", "automerge-sweep-merged", detail, "", event.Repo, event.Number)
	}
	result, err := automerge.SweepQueuedAutoMerges(ctx, ghClient, opts, automerge.AutoMergeSweepOptions{
		MaxMerges: automerge.DefaultAutoMergeSweepMaxMerges,
		Audit:     audit,
	})
	if err != nil {
		logger.Warn("automerge sweep failed", "error", err)
		return
	}
	if len(result.Merged) > 0 || result.Seen > 0 {
		logger.Info("automerge sweep complete", "seen", result.Seen, "merged", len(result.Merged), "skipped", result.Skipped)
	}
	sweepSpan.SetAttributes(
		attribute.Int("hive.automerge.seen", result.Seen),
		attribute.Int("hive.automerge.merged", len(result.Merged)),
		attribute.Int("hive.automerge.skipped", result.Skipped))
	remaining := automerge.DefaultAutoMergeSweepMaxMerges - len(result.Merged)
	if remaining > 0 && opts.TrustedAuthorPolicy().Enabled {
		trustedResult, trustedErr := automerge.SweepTrustedAuthorAutoMerges(ctx, ghClient, opts, automerge.AutoMergeSweepOptions{
			MaxMerges:              remaining,
			Audit:                  audit,
			SelfAuthoredLaneActive: cfg != nil && cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(cfg.ACMMLevel),
		})
		if trustedErr != nil {
			logger.Warn("trusted-author automerge sweep failed", "error", trustedErr)
		} else {
			result.Seen += trustedResult.Seen
			result.Skipped += trustedResult.Skipped
			result.Merged = append(result.Merged, trustedResult.Merged...)
			if len(trustedResult.Merged) > 0 || trustedResult.Seen > 0 {
				logger.Info("trusted-author automerge sweep complete", "seen", trustedResult.Seen, "merged", len(trustedResult.Merged), "skipped", trustedResult.Skipped)
			}
		}
	}
	remaining = automerge.DefaultAutoMergeSweepMaxMerges - len(result.Merged)
	if remaining > 0 && opts.ContributorLanePolicy().Enabled {
		laneResult, laneErr := automerge.SweepContributorLaneAutoMerges(ctx, ghClient, opts, automerge.AutoMergeSweepOptions{
			MaxMerges: remaining,
			Audit:     audit,
		})
		if laneErr != nil {
			logger.Warn("contributor-lane automerge sweep failed", "error", laneErr)
		} else {
			result.Seen += laneResult.Seen
			result.Skipped += laneResult.Skipped
			result.Merged = append(result.Merged, laneResult.Merged...)
			if len(laneResult.Merged) > 0 || laneResult.Seen > 0 {
				logger.Info("contributor-lane automerge sweep complete", "seen", laneResult.Seen, "merged", len(laneResult.Merged), "skipped", laneResult.Skipped)
			}
		}
	}
	// Stage labels (#12429): the lanes above each recorded their per-PR
	// reason into the process-shared tracker; one pass writes the labels.
	if stageResult, stageErr := automerge.ApplyStageLabels(ctx, ghClient, opts); stageErr != nil {
		logger.Warn("stage labels reconcile failed", "error", stageErr, "items", stageResult.Items, "writes", stageResult.Writes)
	} else if stageResult.Writes+stageResult.Comments > 0 {
		logger.Info("stage labels reconciled", "items", stageResult.Items, "writes", stageResult.Writes, "comments", stageResult.Comments, "healed", stageResult.Healed)
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

// legacyFleetReportSweepInterval is how often the hive that watches the
// fleet-report repo looks for standalone "Fleet report:" issues to close.
const legacyFleetReportSweepInterval = github.LegacyFleetReportSweepInterval

// hiveWatchesRepo reports whether repo is one of project.repos.
func hiveWatchesRepo(cfg *config.Config, repo string) bool {
	if cfg == nil {
		return false
	}
	for _, r := range cfg.Project.Repos {
		if strings.EqualFold(strings.TrimSpace(r), strings.TrimSpace(repo)) {
			return true
		}
	}
	return false
}

// runLegacyFleetReportSweepIfDue closes standalone "Fleet report:" issues a
// spoke still on a pre-#13470 build filed directly in the report repo
// (hivecommons/hive#13567). Only the hive that watches that repo runs it —
// it is the one with write access there — at most once per
// legacyFleetReportSweepInterval.
func runLegacyFleetReportSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil {
		return
	}
	repo := cfg.Governor.FleetReport.EffectiveRepo()
	if !hiveWatchesRepo(cfg, repo) {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < legacyFleetReportSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepLegacyFleetReportIssues(ctx, github.LegacyFleetReportSweepOptions{
		Repo: repo,
		Audit: func(event github.LegacyFleetReportSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, author=%s, title=%s", event.Repo, event.Number, event.Author, event.Title)
			dashSrv.AuditLogRecord("system", "legacy-fleet-report-closed", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("legacy fleet report sweep failed", "error", err)
		return
	}
	for _, e := range result.Errors {
		logger.Warn("legacy fleet report sweep: issue skipped", "detail", e)
	}
	if len(result.Closed) > 0 {
		logger.Info("legacy fleet report sweep complete", "scanned", result.Scanned, "closed", len(result.Closed), "skipped", result.Skipped)
	}
}

// runTaskListSweepIfDue closes hive-filed issues whose task-list bodies are
// fully ticked, at most once per taskListSweepInterval. The finding-granularity
// policy (guidance now in every issue-filing template) tells agents to encode
// multi-part deliverables as `- [ ]` boxes; this is the sink that turns those
// boxes into closures. All safety gates — hive-filed only, at-least-one-box,
// all-boxes-ticked, hold-label respected, per-tick cap — live inside
// SweepCompletedTaskListIssues; this function is only the scheduler and the
// dashboard audit sink, mirroring runAutoMergeSweepIfDue above.
func runTaskListSweepIfDue(ctx context.Context, ghClient *github.Client, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < taskListSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepCompletedTaskListIssues(ctx, github.TaskListSweepOptions{
		MaxCloses: github.DefaultTaskListSweepMaxCloses,
		Audit: func(event github.TaskListSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, author=%s, boxes=%d",
				event.Repo, event.Number, event.Author, event.TotalBoxes)
			dashSrv.AuditLogRecord("system", "task-list-sweep-closed", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("task-list sweep failed", "error", err)
		return
	}
	if len(result.Closed) > 0 || result.Seen > 0 {
		logger.Info("task-list sweep complete", "seen", result.Seen, "closed", len(result.Closed), "skipped", result.Skipped)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "task-list sweep complete",
		Attrs: map[string]string{
			"seen":    strconv.Itoa(result.Seen),
			"closed":  strconv.Itoa(len(result.Closed)),
			"skipped": strconv.Itoa(result.Skipped),
		},
	})
}

func runSupersessionSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < supersessionSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	var supersessionCfg config.SupersessionSweepConfig
	if cfg != nil {
		supersessionCfg = cfg.SupersessionSweep
	}
	result, err := ghClient.SweepSupersededOpenPRs(ctx, github.SupersessionSweepOptions{
		MaxActions:          github.DefaultSupersessionSweepMaxActions,
		CloseContributorPRs: supersessionCfg.CloseContributorPRs,
		GracePeriod:         supersessionCfg.GracePeriod,
		AutoCloseSuperseded: func() bool { return cfg == nil || cfg.Governor.AutoCloseSupersededEnabled() },
		ACMMLevelForRepo: func(repo string) int {
			if cfg == nil {
				return 0
			}
			return cfg.EffectiveACMMLevelForRepo(repo)
		},
		Audit: func(event github.SupersessionSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, issue=%s#%d, closer_pr=%d, action=%s, reason=%s",
				event.Repo, event.Number, event.IssueRepo, event.Issue, event.CloserPR, event.Action, event.Reason)
			dashSrv.AuditLogRecord("system", github.SupersessionSweepAuditPrefix+event.Action, detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("supersession sweep failed", "error", err)
		return
	}
	if len(result.Closed) > 0 || len(result.Commented) > 0 || len(result.Resolved) > 0 || result.Seen > 0 {
		logger.Info("supersession sweep complete", "seen", result.Seen, "closed", len(result.Closed), "commented", len(result.Commented), "resolved", len(result.Resolved), "skipped", result.Skipped)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "supersession sweep complete",
		Attrs: map[string]string{
			"seen":      strconv.Itoa(result.Seen),
			"closed":    strconv.Itoa(len(result.Closed)),
			"commented": strconv.Itoa(len(result.Commented)),
			"resolved":  strconv.Itoa(len(result.Resolved)),
			"skipped":   strconv.Itoa(result.Skipped),
		},
	})
}

// runDuplicateFixSweepIfDue resolves same-issue duplicate open PRs
// (hivecommons/hive#14799) at most once per duplicateFixSweepInterval: the
// earlier green-or-pending PR stays, the later hive PR is closed as
// duplicate-of:<keeper>, a person's later PR gets the comment only, and the
// earlier-red-later-green case is parked for the owner. It follows the
// pre-dispatch guard's switch: scanner.skip_issues_with_open_pr off turns
// this off too, since both exist to keep one issue on one PR.
func runDuplicateFixSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	if cfg != nil && !cfg.Scanner.IsSkipIssuesWithOpenPR() {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < duplicateFixSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepDuplicateFixPRs(ctx, github.DuplicateFixSweepOptions{
		MaxActions: github.DefaultDuplicateFixSweepMaxActions,
		Audit: func(event github.DuplicateFixEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, issue=%s#%d, keeper_pr=%d, action=%s, reason=%s",
				event.Repo, event.Number, event.IssueRepo, event.Issue, event.KeeperPR, event.Action, event.Reason)
			dashSrv.AuditLogRecord("system", github.DuplicateFixSweepAuditPrefix+event.Action, detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("duplicate-fix sweep failed", "error", err)
		return
	}
	if result.Pairs > 0 {
		logger.Info("duplicate-fix sweep complete", "pairs", result.Pairs, "closed", len(result.Closed), "commented", len(result.Commented), "escalated", len(result.Escalated), "skipped", result.Skipped)
	}
}

// runSharedCIRerunSweepIfDue re-runs failed jobs on open PRs deferred to a
// [shared-ci] incident that has since closed (hivecommons/hive#12253), at most
// once per sharedCIRerunInterval. Gates and dedupe live in
// SweepSharedCIReruns; this is the scheduler and dashboard audit sink.
func runSharedCIRerunSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	if cfg != nil && !cfg.SharedCIRerun.IsEnabled() {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < sharedCIRerunInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepSharedCIReruns(ctx, github.SharedCIRerunOptions{
		MaxPRs: github.DefaultSharedCIRerunMaxPRs,
		Audit: func(event github.SharedCIRerunEvent) {
			if dashSrv == nil {
				return
			}
			incidents := make([]string, len(event.Incidents))
			for i, n := range event.Incidents {
				incidents[i] = "#" + strconv.Itoa(n)
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, incident=%s, head=%s, runs=%d",
				event.Repo, event.Number, strings.Join(incidents, ","), event.HeadSHA, len(event.RunIDs))
			dashSrv.AuditLogRecord("system", "shared-ci-rerun", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("shared-ci rerun sweep failed", "error", err)
		return
	}
	if len(result.Rerun) > 0 {
		logger.Info("shared-ci rerun sweep complete", "incidents", result.Incidents, "seen", result.Seen, "rerun", len(result.Rerun), "skipped", result.Skipped)
	}
}

// blockedIssuesSweepInterval is how often the periodic sweep reconciles open
// issues carrying the `blocked` label.
const blockedIssuesSweepInterval = 5 * time.Minute

func runBlockedIssuesSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < blockedIssuesSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	var repos []string
	if cfg != nil {
		repos = cfg.Project.Repos
	}
	res, err := ghClient.SweepBlockedIssues(ctx, github.BlockedSweepOptions{Repos: repos})
	if err != nil {
		logger.Warn("blocked issues reconciliation sweep failed", "error", err)
		return
	}
	if res.Unblocked+res.Retargeted+res.Stripped+res.Errors > 0 {
		logger.Info("blocked issues reconciliation sweep complete", "items", res.Items, "unblocked", res.Unblocked, "retargeted", res.Retargeted, "help_wanted_stripped", res.Stripped, "errors", res.Errors)
	}
}

// runDuplicateSweepIfDue clusters open PRs by changed-file set and suggests
// which one to keep, at most once per duplicateSweepInterval
// (hivecommons/hive#7469 capability B).
//
// Fail-closed and opt-in at both levels. `duplicate_sweep.enabled` is off by
// default, so a hive that has not asked for this performs no extra API calls
// at all; `duplicate_sweep.post_comments` is a SECOND, separately-off grant
// for the write, so the natural first configuration is a report-only pass an
// operator can read in the log before the hive says anything on a
// contributor's PR.
//
// The sweep only ever comments. It does not close, label, approve or merge,
// and the reviewer gains no permission from its existence: the write is the
// hive's own App token going through the same canary-gated, scrubbed comment
// path every other hive-authored comment uses.
func runDuplicateSweepIfDue(ctx context.Context, cfg *config.Config, ghClient *github.Client, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || !cfg.DuplicateSweep.Enabled {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < duplicateSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepDuplicatePRs(ctx, github.DuplicateSweepOptions{
		PostComments:  cfg.DuplicateSweep.PostComments,
		MaxComments:   cfg.DuplicateSweep.MaxComments,
		MaxPRsPerRepo: cfg.DuplicateSweep.MaxPRsPerRepo,
		BotAuthors:    cfg.DuplicateSweep.BotAuthors,
		Audit: func(event github.DuplicateSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, survivor=%d, superseded=%d, confidence=%s, commented=%d",
				event.Repo, event.Survivor, len(event.Superseded), event.Confidence, len(event.Commented))
			dashSrv.AuditLogRecord("system", "duplicate-sweep-suggested", detail, "", event.Repo, event.Survivor)
		},
	})
	if err != nil {
		logger.Warn("duplicate sweep reported a problem", "error", err)
	}
	if result == nil {
		return
	}
	if len(result.Clusters) > 0 || result.Scanned > 0 {
		logger.Info("duplicate sweep complete",
			"scanned", result.Scanned,
			"skipped", result.Skipped,
			"clusters", len(result.Clusters),
			"commented", result.Commented,
			"post_comments", cfg.DuplicateSweep.PostComments)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "duplicate sweep complete",
		Attrs: map[string]string{
			"scanned":   strconv.Itoa(result.Scanned),
			"clusters":  strconv.Itoa(len(result.Clusters)),
			"commented": strconv.Itoa(result.Commented),
		},
	})
}

// sentinelSweepInterval is the reconcile cadence of the full sentinel pass
// (#13379). `pull_request` opened/synchronize/reopened webhooks evaluate the
// named PR as they arrive (runSentinelPREvent); the full pass enumerates
// open PRs from the forgestate cache, skips every head it already evaluated
// without a GitHub call, and exists to catch a PR whose delivery was lost.
// Fifteen minutes bounds how long such a PR sits unlabelled.
const sentinelSweepInterval = github.SentinelReconcileInterval

// sentinelSweepOptions is the one option set both sentinel entry points use,
// so the webhook path and the reconcile pass cannot disagree on label,
// trust or rules.
func sentinelSweepOptions(cfg *config.Config, ghClient *github.Client, dashSrv *dashboard.Server) github.SentinelSweepOptions {
	sc := cfg.Sentinel
	return github.SentinelSweepOptions{
		Label:               sc.LabelOrDefault(),
		LabelColor:          config.DefaultSentinelLabelColor,
		LabelDescription:    config.DefaultSentinelLabelDescription,
		Evaluator:           sc.EvaluatorConfig(),
		TrustedAuthor:       sentinelTrustedAuthorFunc(cfg, ghClient),
		TrustedAuthorsBlock: sc.TrustedAuthorsBlock,
		RepoAllowed:         sc.RepoAllowed,
		MaxActions:          sc.MaxActionsOrDefault(),
		Audit: func(event github.SentinelSweepEvent) {
			if dashSrv == nil {
				return
			}
			// Findings the write-back gate refused still reach the Security
			// tab and /api/sentinel: the scan happened, only the GitHub
			// write did not (#writeback containment).
			dashSrv.RecordSentinelFinding(event)
			detail := fmt.Sprintf("repo=%s, pr=%d, author=%s, head=%s, rules=%s",
				event.Repo, event.Number, event.Author, event.HeadSHA, strings.Join(event.Rules(), ","))
			action := "sentinel-alert"
			if event.Suppressed {
				action = "sentinel-finding-suppressed"
				detail += ", suppressed=" + event.SuppressedReason
			}
			dashSrv.AuditLogRecord("system", action, detail, "", event.Repo, event.Number)
		},
	}
}

// runSentinelPREvent is the dashboard.Dependencies.SentinelPREvent hook: a
// verified `pull_request` opened/synchronize/reopened delivery evaluates
// that one PR now, reading only it and its files. Default on with the
// sweep; `sentinel.enabled: false` disables both.
func runSentinelPREvent(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, repo string, number int, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || !cfg.Sentinel.IsEnabled() {
		return
	}
	result, err := ghClient.SweepSentinelPR(ctx, repo, number, sentinelSweepOptions(cfg, ghClient, dashSrv))
	if err != nil {
		logger.Warn("sentinel webhook evaluation failed", "repo", repo, "pr", number, "error", err)
		return
	}
	logSentinelResult(result, logger)
}

// runSentinelSweepIfDue flags open PRs that look like security overrides,
// privilege escalation or codebase damage (pkg/sentinel) at most once per
// sentinelSweepInterval. Default on; `sentinel.enabled: false` disables it.
func runSentinelSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil || cfg == nil || !cfg.Sentinel.IsEnabled() {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < sentinelSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepSentinel(ctx, sentinelSweepOptions(cfg, ghClient, dashSrv))
	if err != nil {
		logger.Warn("sentinel sweep failed", "error", err)
		return
	}
	logSentinelResult(result, logger)
	if len(result.Flagged) > 0 || len(result.Notified) > 0 || len(result.Remediated) > 0 || result.Seen > 0 {
		logger.Info("sentinel sweep complete", "seen", result.Seen, "flagged", len(result.Flagged), "notified", len(result.Notified), "suppressed", len(result.Suppressed), "remediated", len(result.Remediated), "skipped", result.Skipped, "errors", len(result.Errors), "cached_repos", result.CachedRepos, "listed_repos", result.ListedRepos)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "sentinel sweep complete",
		Attrs: map[string]string{
			"seen":       strconv.Itoa(result.Seen),
			"flagged":    strconv.Itoa(len(result.Flagged)),
			"notified":   strconv.Itoa(len(result.Notified)),
			"remediated": strconv.Itoa(len(result.Remediated)),
			"skipped":    strconv.Itoa(result.Skipped),
		},
	})
}

func logSentinelResult(result *github.SentinelSweepResult, logger *slog.Logger) {
	for _, e := range result.Errors {
		logger.Warn("sentinel sweep: PR skipped", "detail", e)
	}
	for _, ev := range result.Flagged {
		logger.Warn("sentinel alert", "repo", ev.Repo, "pr", ev.Number, "author", ev.Author, "rules", strings.Join(ev.Rules(), ","))
	}
	for _, ev := range result.Notified {
		logger.Info("sentinel notice", "repo", ev.Repo, "pr", ev.Number, "author", ev.Author, "trusted_reason", ev.TrustedReason, "rules", strings.Join(ev.Rules(), ","))
	}
	for _, ev := range result.Remediated {
		logger.Info("sentinel unflagged trusted author PR", "repo", ev.Repo, "pr", ev.Number, "author", ev.Author, "trusted_reason", ev.TrustedReason, "head", ev.HeadSHA)
	}
}

// issueUnparkSweepInterval is the minimum spacing between un-park sweeps. A
// maintainer who replies "/hive approve" is waiting for something to happen, so
// this runs more often than the task-list sweep; it only reads the comments of
// issues that are actually parked, which keeps the API cost proportional to the
// backlog a person is waiting on.
const issueUnparkSweepInterval = 5 * time.Minute

// issueUnparkSweepWebhookInterval is the minimum spacing when an `issues`
// webhook has marked a label change (hivecommons/hive#11313): a maintainer who
// just added `triage/accepted` should not wait out the polling interval, but a
// burst of label events must not turn into a sweep per event.
const issueUnparkSweepWebhookInterval = 30 * time.Second

// runIssueUnparkSweepIfDue re-queues parked issues a maintainer has answered
// with a `/hive` command, and keeps the "What to reply" block current on every
// parked issue (hivecommons/hive#9879). Every gate — human author, live write
// permission, first-line command, recent unedited comment, per-tick cap — lives
// inside SweepIssueUnparkCommands; this function is only the scheduler and the
// dashboard audit sink, mirroring runTaskListSweepIfDue above. Repos with
// project.repo_policies[].label_driven skip the notice and keep their parking
// labels (hivecommons/hive#10537).
func runIssueUnparkSweepIfDue(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() {
		since := now.Sub(*lastRun)
		if since < issueUnparkSweepWebhookInterval {
			return
		}
		if since < issueUnparkSweepInterval && github.TakeDirtyIssueCount() == 0 {
			return
		}
	}
	if lastRun != nil {
		*lastRun = now
	}
	// `/hive approve` on a PR is answered with what the merge lane will do
	// with it (hivecommons/hive#13087); the engine here only reads policy.
	approveOpts := automerge.Options{
		MergerAuthorizer:       trustedMergerFunc(cfg),
		TrustedAuthorizer:      trustedAuthorFunc(cfg),
		TrustedAuthorPolicy:    trustedAuthorPolicyFunc(cfg),
		ContributorLanePolicy:  contributorLanePolicyFunc(cfg),
		ContributorLaneVerdict: contributorLaneVerdictFunc(cfg, logger),
	}
	if cfg != nil {
		approveOpts.RepoAutoMergeEnabled = cfg.RepoAutoMergeEnabled
		approveOpts.HumanMergePaths = func(repo string) []string { return humanMergePathsFor(cfg, repo) }
		approveOpts.HumanMergeRule = func() prstage.HumanMergeRule { return humanMergeRuleFor(cfg) }
		approveOpts.TrustedBotAuthors = func() map[string]bool { return cfg.AutoMerge.TrustedBotAuthorSet() }
	}
	approveEngine := automerge.New(ghClient, approveOpts)
	result, err := ghClient.SweepIssueUnparkCommands(ctx, github.IssueUnparkSweepOptions{
		PRApproveReply: approveEngine.PRApproveReply,
		MaxActions:     github.DefaultIssueUnparkSweepMaxActions,
		// cfg is nil only in tests; nil leaves every repo on the default path.
		LabelDriven:   func(repo string) bool { return cfg.RepoLabelDriven(repo) },
		LabelApproval: labelApprovalPolicyResolver(cfg),
		LabelApprovalAudit: func(event github.IssueLabelApprovalEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, label=%s, actor=%s, trust=%s, removed=%s, released=%t, pending=%t",
				event.Repo, event.Number, event.Label, event.Actor, event.TrustReason, strings.Join(event.Removed, ","), event.Released, event.Pending)
			dashSrv.AuditLogRecord("system", "issue-approved-by-label", detail, "", event.Repo, event.Number)
		},
		Audit: func(event github.IssueUnparkEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, actor=%s, decision=%s",
				event.Repo, event.Number, event.Actor, event.Decision)
			dashSrv.AuditLogRecord("system", "issue-unparked-by-reply", detail, "", event.Repo, event.Number)
		},
		ClosedAudit: func(event github.IssueUnparkEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, actor=%s", event.Repo, event.Number, event.Actor)
			dashSrv.AuditLogRecord("system", "issue-closed-by-fixed", detail, "", event.Repo, event.Number)
		},
		// `/hive decision` on a hive-authored PR at 085 (hivecommons/hive#13041)
		// is routed to the fix lane under the automerge lane's merger trust.
		HumanChangeRequests: review.DefaultHumanChangeRequests(),
		TrustedMerger:       trustedMergerFunc(cfg),
		PRAudit: func(event github.PullRequestDecisionEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, head=%s, actor=%s, decision=%s",
				event.Repo, event.Number, event.HeadSHA, event.Actor, event.Decision)
			dashSrv.AuditLogRecord("system", "pr-decision-routed-to-fix", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("issue un-park sweep failed", "error", err)
		return
	}
	if result == nil {
		return
	}
	if len(result.Unparked) > 0 || len(result.Closed) > 0 || result.Replies > 0 || len(result.LabelApproved) > 0 || len(result.PRDecisions) > 0 {
		logger.Info("issue un-park sweep complete",
			"seen", result.Seen, "unparked", len(result.Unparked), "closed", len(result.Closed), "label_approved", len(result.LabelApproved), "pr_decisions", len(result.PRDecisions), "replies", result.Replies, "skipped", result.Skipped)
	}
}

// closedIssueStageSweepInterval spaces the closed-issue clean-up. A closed
// issue wearing stage/045 is a stale dashboard row, not a person waiting, so
// this runs at the task-list cadence; each pass is one listing per repo.
const closedIssueStageSweepInterval = 10 * time.Minute

// closedIssueStageSweepSlack widens each pass's `since` so an issue that
// closed while the previous pass was running is not missed.
const closedIssueStageSweepSlack = 10 * time.Minute

// runClosedIssueStageSweepIfDue strips hive stage and park labels from issues
// that closed while carrying them (hivecommons/homebrew-hive#127): the
// issue-stage reconciler only visits open issues, so a `Fixes #N` merge or a
// by-hand close left `stage/045-needs-decision` behind and the tracker kept
// reading it as a pending decision. Every rule lives in
// SweepClosedIssueStageLabels; this is the scheduler and audit sink.
func runClosedIssueStageSweepIfDue(ctx context.Context, ghClient *github.Client, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	var since time.Time
	if lastRun != nil && !lastRun.IsZero() {
		if now.Sub(*lastRun) < closedIssueStageSweepInterval {
			return
		}
		since = lastRun.Add(-closedIssueStageSweepSlack)
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepClosedIssueStageLabels(ctx, github.ClosedIssueStageSweepOptions{
		Since:      since,
		MaxActions: github.DefaultClosedIssueStageSweepMaxActions,
		Audit: func(event github.ClosedIssueStageEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, issue=%d, stage=%s, needs_human=%t",
				event.Repo, event.Number, strings.Join(event.Stage, ","), event.NeedsHuman)
			dashSrv.AuditLogRecord("system", "closed-issue-stage-cleared", detail, "", event.Repo, event.Number)
		},
	})
	if err != nil {
		logger.Warn("closed-issue stage sweep failed", "error", err)
	} else if result != nil && (len(result.Cleaned) > 0 || result.Skipped > 0) {
		logger.Info("closed-issue stage sweep complete", "seen", result.Seen, "cleaned", len(result.Cleaned), "skipped", result.Skipped)
	}
	runReviewBacklogSplitSweep(ctx, ghClient, dashSrv, since, logger)
}

// runReviewBacklogSplitSweep re-files still-unchecked review findings folded
// into an issue that has since closed (hivecommons/hive#13914). It shares the
// closed-issue sweep's cadence and window; every rule lives in
// SweepReviewBacklogSplits. It then runs the one-off
// BackfillReviewBacklogSplits for hosts closed before that window
// (hivecommons/hive#13947), a no-op once the ledger records it complete.
func runReviewBacklogSplitSweep(ctx context.Context, ghClient *github.Client, dashSrv *dashboard.Server, since time.Time, logger *slog.Logger) {
	audit := func(event github.ReviewBacklogSplitEvent) {
		if dashSrv == nil {
			return
		}
		detail := fmt.Sprintf("repo=%s, host=%d, issue=%d, reused=%t, key=%s",
			event.Repo, event.Host, event.Number, event.Reused, event.Key)
		dashSrv.AuditLogRecord("system", "review-backlog-split", detail, "", event.Repo, event.Host)
	}
	result, err := ghClient.SweepReviewBacklogSplits(ctx, github.ReviewBacklogSplitSweepOptions{
		Since:      since,
		MaxActions: github.DefaultReviewBacklogSplitMaxActions,
		Audit:      audit,
	})
	if err != nil {
		logger.Warn("review backlog split sweep failed", "error", err)
		return
	}
	if result != nil && (len(result.Split) > 0 || result.Skipped > 0 || result.Deferred > 0) {
		logger.Info("review backlog split sweep complete", "hosts", result.Hosts, "split", len(result.Split), "skipped", result.Skipped, "deferred", result.Deferred)
	}
	backfill, err := ghClient.BackfillReviewBacklogSplits(ctx, github.ReviewBacklogSplitSweepOptions{
		MaxActions: github.DefaultReviewBacklogSplitMaxActions,
		Audit:      audit,
	})
	if err != nil {
		logger.Warn("review backlog split backfill failed", "error", err)
		return
	}
	if backfill != nil && (len(backfill.Split) > 0 || backfill.Skipped > 0 || backfill.Deferred > 0) {
		logger.Info("review backlog split backfill pass complete", "hosts", backfill.Hosts, "split", len(backfill.Split), "skipped", backfill.Skipped, "deferred", backfill.Deferred)
	}
}

// labelApprovalPolicyResolver reads label-driven approval settings
// (hivecommons/hive#11313) live from cfg. It answers ok=false for every repo
// without project.repo_policies[].label_driven, which keeps the feature off
// there; a nil cfg (tests) opts nothing in.
func labelApprovalPolicyResolver(cfg *config.Config) func(repo string) (github.LabelApprovalPolicy, bool) {
	return func(repo string) (github.LabelApprovalPolicy, bool) {
		if cfg == nil {
			return github.LabelApprovalPolicy{}, false
		}
		labels := cfg.LabelDrivenApprovalLabels(repo)
		if len(labels) == 0 {
			return github.LabelApprovalPolicy{}, false
		}
		return github.LabelApprovalPolicy{
			Labels:              labels,
			TrustOrgMembers:     cfg.LabelDrivenTrustOrgMembers(),
			TrustedBots:         cfg.LabelDrivenTrustedBots(),
			SupersedeParkActors: cfg.LabelDrivenSupersedeParkActors(repo),
		}, true
	}
}

var (
	ciFailingPath      = "/var/run/hive-metrics/ci-failing.json"
	intentVerdictsPath = "/var/run/hive-metrics/intent-verdicts.json"
)

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

// issueStageSyncTimeout bounds one eval pass's issue stage-label sync so a
// rate-limit pacing queue cannot stall the run loop (hivecommons/hive#12983).
const issueStageSyncTimeout = 2 * time.Minute

// recheckSoakHubURL is the hub the soak triage probes, "" when this hive has
// no hub (the probe then reads the spoke's own /api/version).
func recheckSoakHubURL(cfg *config.Config) string {
	if cfg == nil || !cfg.Hub.Enabled {
		return ""
	}
	return cfg.Hub.URL
}

// applyDuplicatePRGuard filters issues already claimed by an open hive-authored
// PR out of the actionable set. Failures are logged, never fatal: the guard is
// a safety net, and a broken net must not take the hive down with it.
// reconcileLedgerCoverRecords runs github.BackfillCoveredByLedger with the
// cycle's red+stale valve. It is not an optional step: with webhook-driven
// cached cycles the reconcile pass (and the stage sweep with it) can be
// hours apart, and a record at 010 under an open covering PR re-offers the
// issue to the scanner for all of that time.
func reconcileLedgerCoverRecords(ctx context.Context, cfg *config.Config, ghClient *github.Client, actionable *github.ActionableResult, logger *slog.Logger) {
	ledger := getClaimLedger(logger)
	if ledger == nil || ledger.Len() == 0 || ghClient == nil {
		return
	}
	backfillCtx, cancel := context.WithTimeout(ctx, issueStageSyncTimeout)
	defer cancel()
	res := ghClient.BackfillCoveredByLedger(backfillCtx, ledger, claimingPRRedStale(cfg, actionable))
	if res.Covered+res.Errors > 0 && logger != nil {
		logger.Info("stage backfill: issue records reconciled with the claim ledger",
			"checked", res.Checked, "covered", res.Covered, "skipped_red_stale", res.SkippedRedStale,
			"skipped_not_open", res.SkippedNotOpen, "errors", res.Errors)
	}
}

func applyDuplicatePRGuard(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	actionable *github.ActionableResult,
	logger *slog.Logger,
) {
	// The PR-claim guard runs FIRST: an open PR that fixes the issue is the
	// stronger evidence, and the guards below only walk what is still in
	// the actionable set. With the live-claim filter ahead of it, an issue
	// whose agent claim was still live when its PR opened was withheld as
	// "claimed" before the ledger could bucket it "claimed_by_pr", so every
	// sweep re-derived 020 under the PR-opened 040 and the stage store
	// recorded a spurious regression (hivecommons/hive#13174 et al.). The
	// ledger's open-issue set (OpenIssuesOf) likewise needs the full list.
	ledger := getClaimLedger(logger)
	if ledger != nil {
		github.ApplyDuplicatePRGuard(ctx, ghClient, ledger, hiveIdentity(cfg), actionable, claimingPRRedStale(cfg, actionable), logger)
	}
	// #8380: a LIVE issue claim covers an issue the same way an open PR does,
	// for every kick prompt (the scanner's included). The claim fields are
	// only ever set while governor.claims.enabled is on, so with the feature
	// off this touches nothing.
	// hivecommons/hive#13579: a contributor claim on an issue in flap
	// cool-off, or one that has starved hive agents past the configured
	// window with no PR, is advisory — the issue stays actionable.
	if cfg != nil && cfg.Governor.Claims.Enabled {
		if withheld := contributorClaimGuard.Load().FilterLiveIssueClaims(actionable, time.Now(), logger); withheld > 0 {
			logger.Info("issue-claim guard applied", "withheld", withheld)
		}
	}
	if ledger == nil {
		return
	}
	// The in-progress sweep runs after the PR-claim guard so an issue whose
	// PR just opened sheds the start mark here, and a lapsed mark frees the
	// issue before kicks are built. Independent of governor.claims: the
	// label is read from the issue, the marker from the comment, on every hive.
	if withheld := github.SweepInProgressIssues(ctx, ghClient, actionable, time.Now(), logger); withheld > 0 {
		logger.Info("in-progress guard applied", "withheld", withheld)
	}
	// hivecommons/hive#12436: with every bucket final, mirror the cycle's
	// decision about each open issue onto its stage/NNN label (and strip the
	// retired claimed / hive/in-progress / 🐝 covered by PR labels).
	// hivecommons/hive#12983: the run loop holds cfgReloadMu for the whole
	// pass, so bound the sync; a paced request whose slot would miss the
	// deadline fails fast and the best-effort sync retries next cycle.
	stageCtx, stageCancel := context.WithTimeout(ctx, issueStageSyncTimeout)
	stageCtx, stageSpan := tracing.StartSpan(stageCtx, "governor.stage_labels")
	syncStageEngineMode(cfg, ghClient, logger)
	res := github.SyncIssueStageLabelsWith(stageCtx, ghClient, actionable, nil, time.Now(), logger, stageEngineOptions(cfg, logger))
	stageSpan.SetAttributes(
		attribute.Int("hive.stage_labels.items", res.Items),
		attribute.Int("hive.stage_labels.writes", res.Writes))
	stageSpan.End()
	stageCancel()
	if res.Writes+res.Comments > 0 {
		logger.Info("issue stage labels applied", "items", res.Items, "writes", res.Writes, "comments", res.Comments, "healed", res.Healed)
	}
	// hivecommons/hive#13093: issues parked on an unmerged PR come back once
	// that PR settles, and never advertise `help wanted` while parked.
	// hivecommons/hive#13311: ops-tracking soak checklists past their
	// recheck-after deadline get evidence from the hub (or this spoke).
	github.SetRecheckSoakProbe(github.NewHTTPSoakProbe(recheckSoakHubURL(cfg), fmt.Sprintf("http://localhost:%d", cfg.Dashboard.Port), nil))
	blockedCtx, blockedCancel := context.WithTimeout(ctx, issueStageSyncTimeout)
	// hivecommons/hive#13833: review-backlog items wait on the PR they were
	// filed from in every destination; lift them when it merges, and when it
	// closes unmerged unblock them with a re-triage note (never auto-close).
	// Runs first so the items get the sweep's own note rather than the
	// generic one from the sweep below.
	if rb := github.ReconcileReviewBacklogBlocks(blockedCtx, ghClient, logger); rb.Lifted+rb.Retriaged+rb.Errors > 0 {
		logger.Info("review backlog blocks reconciled", "checked", rb.Checked, "lifted", rb.Lifted, "retriaged", rb.Retriaged, "errors", rb.Errors)
	}
	if br := github.ReconcileBlockedIssues(blockedCtx, ghClient, actionable, logger); br.Unblocked+br.Retargeted+br.Stripped+br.SoakTriaged+br.Errors > 0 {
		logger.Info("blocked issues reconciled", "items", br.Items, "unblocked", br.Unblocked, "retargeted", br.Retargeted, "help_wanted_stripped", br.Stripped, "soak_triaged", br.SoakTriaged, "errors", br.Errors)
	}
	blockedCancel()
	// Closing an issue is irreversible: refuse while the snapshot is stale and
	// re-read an item whose copy is older than DecisionFreshnessBound.
	runCloseOnMergeSweep(github.WithIrreversibleGuard(ctx, ghClient.SnapshotFreshnessGuard(actionable, time.Now)), cfg, ghClient, ledger, logger)
}

// inProgressSetting resolves the pr-in-progress stage label setting from config.
// The label follows the claims label mirror: an operator who set
// governor.claims.label: false asked for no hive-managed claim labels, and
// gets none here either; otherwise the start signal is always labelled. The
// mark lasts the agent claim TTL, so both the ledger and the label agree on
// when an agent's hold lapses.
func inProgressSetting(cfg *config.Config) github.InProgressSetting {
	if cfg == nil {
		return github.InProgressSetting{}
	}
	_, agentTTL, _, _ := cfg.Governor.Claims.EffectiveTTLs()
	return github.InProgressSetting{
		Enabled: !(cfg.Governor.Claims.IsEnabled() && !cfg.Governor.Claims.LabelEnabled()),
		HiveID:  cfg.HiveID,
		TTL:     agentTTL,
	}
}

func logCloseOnMergeResults(logger *slog.Logger, msg string, results []github.CloseOnMergeResult) {
	if logger == nil || len(results) == 0 {
		return
	}
	var closed, awaiting, skipped, noop int
	for _, r := range results {
		switch r.Action {
		case github.CloseOnMergeClosed:
			closed++
		case github.CloseOnMergeAwaiting:
			awaiting++
		case github.CloseOnMergeSkipped:
			skipped++
		default:
			noop++
		}
	}
	if closed > 0 || awaiting > 0 || skipped > 0 {
		logger.Info(msg, "closed", closed, "awaiting_confirmation", awaiting, "skipped", skipped, "noop", noop)
	}
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
		// A person reopening an issue after its closing PR merged voids the
		// merged claim right away (hivecommons/hive#9945 sat likely-done for
		// hours after a /reopen); the enumeration pass is the fallback when
		// webhooks are not delivered.
		github.SetIssueReopenedHook(issueReopenedHookFor(ledger, logger))
	})
	return claimLedger
}

// issueReopenedHookFor builds the webhook hook that voids a merged claim on a
// reopened issue and persists the ledger, logging `reopened issue re-queued`.
func issueReopenedHookFor(ledger *github.ClaimLedger, logger *slog.Logger) github.IssueReopenedHook {
	return func(repo string, number int, at time.Time) {
		if ledger == nil || !ledger.MarkIssueReopened(repo, number, at) {
			return
		}
		claim, _ := ledger.LookupAny(repo, number)
		if logger != nil {
			logger.Info("reopened issue re-queued: merged PR claim voided",
				"repo", repo, "issue", number, "claimed_by_pr", claim.PRNumber, "pr_repo", claim.PRRepo,
				"pr_url", claim.PRURL, "merged_at", claim.MergedAt, "reopened_at", at, "source", "webhook")
		}
		if err := ledger.Save(); err != nil && logger != nil {
			logger.Warn("reopened issue: failed to persist claim ledger", "repo", repo, "issue", number, "error", err)
		}
	}
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

func fullRepoName(repo, org string) string {
	if strings.Contains(repo, "/") || org == "" {
		return repo
	}
	return org + "/" + repo
}

// reviewAgentCapability describes one configured lane the way the review
// dispatcher sees it, so the relay judges reviewer-ness by the same rule.
// An unknown lane has only its name to go on.
func reviewAgentCapability(cfg *config.Config, agent string) review.AgentCapability {
	if cfg != nil {
		if ac, ok := cfg.Agents[agent]; ok {
			return review.CapabilityFromConfig(agent, ac)
		}
	}
	return review.AgentCapability{Name: agent}
}

// reviewAuthorBase resolves an authoring lane to its base agent over the full
// configured agent set, enabled or not. A replica name the current config no
// longer materialises (its replicas: count was reduced) still maps to the
// declared base it was derived from, so the base and its remaining replicas
// stay excluded from reviewing it.
func reviewAuthorBase(cfg *config.Config, agent string) string {
	agent = strings.TrimSpace(agent)
	if cfg == nil || agent == "" {
		return agent
	}
	if _, ok := cfg.Agents[agent]; ok {
		return cfg.BaseAgentName(agent)
	}
	if i := strings.LastIndex(agent, "-"); i > 0 {
		if n, err := strconv.Atoi(agent[i+1:]); err == nil && n >= 2 {
			if ac, ok := cfg.Agents[agent[:i]]; ok && ac.ReplicaOf == "" {
				return agent[:i]
			}
		}
	}
	return agent
}

// installReviewRelaySettings gives a (possibly rebuilt) GitHub client the
// review-relay knobs that live in cfg.Review: which repos may be revised in
// place, which perspectives a verdict may name, and whether comments carry a
// confidence score (hivecommons/hive#8182). One place, so an app-auth rebuild
// cannot silently drop a setting the first client had — the perspective set
// was previously installed only on the boot path.
func installReviewRelaySettings(client *github.Client, cfg *config.Config, logger *slog.Logger) {
	if client == nil || cfg == nil {
		return
	}
	client.SetReviseRepos(cfg.Review.ReviseRepos)
	client.SetPerspectives(reviewPerspectiveSet(cfg, logger))
	client.SetReviewerAgentFunc(func(agent string) bool {
		return review.ReviewCapable(reviewAgentCapability(cfg, agent), review.ReviewerAgentSet(cfg.Review.ReviewerAgents))
	})
	client.SetAgentBaseFunc(func(agent string) string {
		return reviewAuthorBase(cfg, agent)
	})
	client.SetConfidenceScore(func() bool { return cfg.Review.ConfidenceScore })
	client.SetReviewBacklog(func() (bool, int) {
		return !cfg.Review.OutOfScopeBacklogDisabled, cfg.Review.MaxOutOfScopeBacklogIssues
	})
	client.SetReviewBacklogRouting(func() github.ReviewBacklogRouting {
		return reviewBacklogRouting(client, cfg, logger)
	})
	client.SetReviewEvidence(func(repo string) github.ReviewEvidenceSettings {
		return reviewEvidenceSettings(cfg, repo)
	})
	// hivecommons/hive#14581: issue-author credit on every merge path, read
	// live so a reload of auto_merge.credit_issue_authors applies.
	client.SetCreditIssueAuthors(func(string) bool { return cfg.AutoMerge.CreditIssueAuthorsEnabled() })
	// hivecommons/hive#13241: an issue a person reopens after its closing PR
	// merged is a change failure on that PR's evidence record, from both the
	// issues.reopened webhook and the enumeration reopen check.
	github.SetIssueReopenRecorder(client.RecordReviewEvidenceIssueReopen)
	// #8380: issue claims are read at enumeration time only while
	// governor.claims.enabled is on; the setting is read live so the Features
	// toggle applies without a client rebuild.
	client.SetIssueClaims(func() (bool, time.Duration) {
		return cfg.Governor.Claims.Enabled, cfg.Governor.Claims.EffectiveTTL()
	})
	client.SetInProgress(func() github.InProgressSetting { return inProgressSetting(cfg) })
	// hivecommons/hive#12436: the issue stage reconciler follows the same
	// governor.labels.stage policy as the PR one, read live.
	client.SetIssueStageLabels(func() github.IssueStagePolicy {
		p := stageLabelPolicyFunc(cfg)()
		return github.IssueStagePolicy{Enabled: p.Enabled, Dwell: p.Dwell}
	})
	client.SetReviewCadenceLimits(
		func() bool { return cfg.Review.CombinedPerspectives },
		func() int { return cfg.Review.MaxReviewsPerHead },
	)
	// review.require_labels (PR approval allow-list): the relay's half of the
	// gate, read live so a Features-tab edit applies to the next request.
	client.SetReviewRequireLabels(func() []string { return cfg.Review.RequireLabels })
}

// reviewBacklogRouting resolves review.severity / review.backlog and the
// backlog sink for the active work source (hivecommons/hive#11089). A
// destination whose work source cannot be built falls back to GitHub issues;
// the reason is logged so the operator can fix the work-source config.
func reviewBacklogRouting(client *github.Client, cfg *config.Config, logger *slog.Logger) github.ReviewBacklogRouting {
	ghToken := cfg.GitHub.Token
	if ghToken == "" {
		ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
	}
	sink, err := worksource.NewReviewBacklogSink(cfg.Governor.WorkSource, cfg.Review.Backlog, client, ghToken, cfg.Project.Org, logger)
	if err != nil && logger != nil {
		logger.Warn("review backlog: destination unavailable, filing GitHub issues", "error", err)
	}
	return github.ReviewBacklogRouting{Severity: cfg.Review.Severity, Backlog: cfg.Review.Backlog, Sink: sink}
}

// installReviewBots installs classification.review_bots on a (possibly
// rebuilt) GitHub client (hivecommons/hive#7360). hive.yaml's block wins;
// otherwise the same key is read from hive-project.yaml
// (config.DefaultProjectYAMLPath, overridable via HIVE_PROJECT_YAML — the
// path the bash pipeline stages already honour). Nil-safe: a hive without
// GitHub credentials runs with a nil client for the life of the process.
// Also installs review.fix_human_prs (hivecommons/hive#9361), so the
// review-thread reconciler follows PRs the hive did not open whenever the
// fixer is already allowed to push to them.
func installReviewBots(client *github.Client, cfg *config.Config, logger *slog.Logger) {
	if client == nil || cfg == nil {
		return
	}
	rb, err := cfg.EffectiveReviewBots(os.Getenv("HIVE_PROJECT_YAML"))
	if err != nil && logger != nil {
		logger.Warn("classification.review_bots: project file unreadable; review-thread reconciler stays off", "error", err)
	}
	client.SetReviewBots(rb)
	client.SetFixHumanPRs(cfg.Review.FixHumanPRsEnabled())
	if logger != nil && rb.Enabled() {
		logger.Info("review-thread reconciler enabled",
			"review_bots", rb.Logins,
			"max_attempts_per_thread", rb.MaxAttempts(),
			"resolve_after_fix", rb.ResolveAfterFixEnabled(),
			"fix_human_prs", cfg.Review.FixHumanPRsEnabled())
	}
}

// reviewThreadsRefreshInterval throttles the review-thread monitor: the eval
// tick runs about once a minute, but every hive-authored open PR costs one
// GraphQL query per pass, and bot threads move on the scale of minutes, not
// seconds. A var so tests can drive it.
var reviewThreadsRefreshInterval = 5 * time.Minute

// reviewThreadsLastRefresh is the wall-clock of the last CollectReviewThreads
// pass (zero = never). Package-level because the eval tick is a free
// function; only the eval goroutine touches it.
var reviewThreadsLastRefresh time.Time

// writeReviewThreads is the eval-tick half of the #7360 reconciler. It runs
// CollectReviewThreads over the governor's actionable PRs at most once per
// reviewThreadsRefreshInterval, stamps each PR with the agent whose relay
// request opened it (auditPRAgents — the same attribution ci-failing.json
// carries) and with the escalation sweep's needs-human verdict, and writes
// review-threads.json. The feature-off case still writes the (empty) file
// each refresh so a reader can tell "off" from "never ran".
func writeReviewThreads(ctx context.Context, client *github.Client, actionable *github.ActionableResult, org string, escalatedPRs map[string]bool, logger *slog.Logger) {
	if client == nil || actionable == nil {
		return
	}
	now := time.Now()
	if !reviewThreadsLastRefresh.IsZero() && now.Sub(reviewThreadsLastRefresh) < reviewThreadsRefreshInterval {
		return
	}
	reviewThreadsLastRefresh = now

	report := client.CollectReviewThreads(ctx, actionable.PRs.Items, now)
	if len(report.PRs) > 0 {
		prAgents := auditPRAgents(org, now.Add(-auditPRAttributionWindow), "")
		for i := range report.PRs {
			pr := &report.PRs[i]
			pr.Agent = prAgents[fmt.Sprintf("%s#%d", pr.Repo, pr.Number)]
			pr.Escalated = escalatedPRs[escalation.Key(pr.Repo, pr.Number)]
		}
	}
	if err := github.WriteReviewThreadsReport("", report); err != nil {
		logger.Warn("failed to write review-threads.json", "error", err)
		return
	}
	if report.Enabled {
		logger.Info("review-threads.json refreshed", "prs", len(report.PRs), "threads", report.TotalThreads)
	}
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

// applyHumanDecisionLabels mirrors the review swarm's "a human must decide"
// holds onto the operator's existing triage label, so they can be filtered
// from the PR list instead of being found by opening threads.
//
// This is strictly additive to the marker the reviewer writes into the review
// body. Every failure path here — no label configured, label absent from the
// repo, API refusal — is logged and skipped, because a mislabelled hold is a
// smaller problem than a hold that never got reported.
func applyHumanDecisionLabels(ctx context.Context, cfg *config.Config, ghClient *github.Client, actionable *github.ActionableResult, plan review.DispatchPlan, logger *slog.Logger) {
	if cfg == nil || ghClient == nil {
		return
	}
	label := strings.TrimSpace(cfg.Review.HumanDecisionLabel)
	if label == "" || len(plan.State.Human) == 0 {
		return
	}
	// Holds persist across cycles, so re-deriving them every pass would re-ask
	// GitHub to apply a label the PR already carries. The enumeration already
	// fetched each PR's labels, so skipping the settled ones costs nothing.
	//
	// Holds carry the owner/repo form (they descend from review reports, which
	// record GitHub's full name) while PRs.Items carry whatever governor.repos
	// says — usually the bare repo under project.org. Key both through
	// fullRepoName or the skip never matches and every cycle re-labels every
	// hold (observed on a hive whose repos were configured bare).
	org := cfg.Project.Org
	labeled := map[string]bool{}
	if actionable != nil {
		for _, pr := range actionable.PRs.Items {
			for _, have := range pr.Labels {
				if strings.EqualFold(strings.TrimSpace(have), label) {
					labeled[strings.ToLower(fmt.Sprintf("%s#%d", fullRepoName(pr.Repo, org), pr.Number))] = true
				}
			}
		}
	}
	failing := map[string]struct{}{}
	defer func() { humanLabelWarn.retain(failing) }()
	for _, hold := range plan.State.Human {
		if labeled[strings.ToLower(fmt.Sprintf("%s#%d", fullRepoName(hold.Repo, org), hold.Number))] {
			continue
		}
		if err := ghClient.ApplyHumanDecisionLabel(ctx, hold.Repo, hold.Number, label); err != nil {
			// Missing optional labels are configuration notices, not runtime
			// failures: first warnings across different PRs can trigger fleet
			// reports even with per-PR deduplication (#13857).
			key := prWarnKey(hold.Repo, hold.Number)
			missing := errors.Is(err, github.ErrHumanDecisionLabelMissing)
			if missing {
				key += ":missing-label"
			}
			failing[key] = struct{}{}
			level := humanLabelWarn.level(key)
			if missing && level == slog.LevelWarn {
				level = slog.LevelInfo
			}
			logger.Log(ctx, level, "human decision label not applied; review marker still stands",
				"repo", hold.Repo, "pr", hold.Number, "label", label, "error", err)
			continue
		}
		logger.Info("human decision label applied", "repo", hold.Repo, "pr", hold.Number, "label", label, "reason", hold.Reason)
	}
}

// parseReviseCutoff reads Review.ReviseVerdictsBefore. A malformed value is
// logged and ignored rather than defaulting to "now", because a cutoff that
// silently becomes the current time would re-open every verdict in the
// artifact at once — the opposite of the narrow, deliberate correction this
// setting exists for.
// reviewPerspectiveSet resolves the hive's configured review perspectives.
//
// A bad configuration falls back to the built-in set and says so, rather than
// disabling review. Silently reviewing nothing would look identical to a healthy
// hive with an empty queue; reviewing with the defaults while logging the error
// keeps coverage up and makes the mistake findable.
func reviewPerspectiveSet(cfg *config.Config, logger *slog.Logger) review.PerspectiveSet {
	if cfg == nil {
		return review.PerspectiveSet{}
	}
	set, err := review.NewPerspectiveSet(cfg.Review.Perspectives, cfg.Review.PerspectivePrompts)
	if err != nil {
		if logger != nil {
			logger.Warn("review.perspectives is invalid; reviewing with the built-in set until it is fixed",
				"error", err)
		}
		set = review.PerspectiveSet{}
	}
	// plan_match (#8317) is a separate switch from the selection so turning
	// it on never requires spelling out the whole set. Applied on the
	// fallback path too: a typo elsewhere in the selection must not silently
	// turn plan matching off.
	if cfg.Review.PlanMatch.Enabled {
		set = set.WithPlanMatch()
	}
	return set
}

// planWaveFor renders the approved plan a PR's run trailers name, for the
// plan_match perspective (#8317). The Hive-Plan ref is tried first, then the
// Hive-Run key, since either may carry the epic ID or the source issue ref.
// Empty when plan_match is off, the PR has no trailer, or no store holds the
// plan -- the kick tells the reviewer which of those it is.
func planWaveFor(cfg *config.Config, stores map[string]*beads.Store, planRef, runKey string) string {
	if cfg == nil || !cfg.Review.PlanMatch.Enabled {
		return ""
	}
	for _, ref := range []string{planRef, runKey} {
		if tree, ok := planning.FindPlanTree(stores, ref); ok {
			return tree.WaveText()
		}
	}
	return ""
}

func parseReviseCutoff(raw string, logger *slog.Logger) time.Time {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}
	}
	cutoff, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		if logger != nil {
			logger.Warn("review revise_verdicts_before is not RFC3339; ignoring",
				"value", trimmed, "error", err)
		}
		return time.Time{}
	}
	return cutoff
}

// reviewPlannerGitHub is the slice of the GitHub client the review planner
// reads: a PR's changed paths for the bot-author model route
// (review.bot_author_full_review_paths) and the compare between the head
// the hive last judged and the current one for the delta re-review
// (review.delta_rereview).
type reviewPlannerGitHub interface {
	ReviewPRChangedFiles(ctx context.Context, repo string, number int) ([]string, bool)
	ReviewDeltaDiff(ctx context.Context, repo, previousHead, head string) review.DeltaDiff
}

func planReviewDispatch(cfg *config.Config, actionable *github.ActionableResult, agentMgr crux.Runtime, beadStores map[string]*beads.Store, logger *slog.Logger) review.DispatchPlan {
	return planReviewDispatchWith(context.Background(), cfg, actionable, agentMgr, beadStores, nil, logger)
}

// reviewBotAuthorRoute is review.bot_authors / bot_author_model /
// bot_author_full_review_paths resolved for the planner.
func reviewBotAuthorRoute(cfg *config.Config) review.BotAuthorRoute {
	backend, model := cfg.Review.BotAuthorModelRef()
	return review.BotAuthorRoute{Authors: cfg.Review.EffectiveBotAuthors(), Backend: backend, Model: model, FullReviewPaths: cfg.Review.EffectiveBotAuthorFullReviewPaths()}
}

// reviewBotRouteFilesFor fetches a PR's changed paths when the bot-author
// route may apply to it: route on, author a listed bot, no verdict at this
// head and no review in flight (one listing per head, not per cycle). nil,
// false otherwise, which the planner reads as unknown: no downgrade.
func reviewBotRouteFilesFor(ctx context.Context, cfg *config.Config, lister reviewPlannerGitHub, route review.BotAuthorRoute, artifact review.Artifact, state review.DispatchState, repo string, number int, author, headSHA, bodyDigest string) ([]string, bool) {
	if lister == nil || !route.Enabled() || !review.IsBotAuthor(author, route.Authors, cfg.EffectiveAIAuthor()) {
		return nil, false
	}
	if _, judged := artifact.AggregateForBody(repo, number, headSHA, bodyDigest); judged {
		return nil, false
	}
	if !state.PendingSince(repo, number, headSHA).IsZero() {
		return nil, false
	}
	return lister.ReviewPRChangedFiles(ctx, repo, number)
}

// reviewDeltaDiffFor fetches the delta compare for one PR when it is a
// delta re-review candidate: delta mode on, no verdict yet at this head, no
// dispatch entry for it (a compare per cycle for a review in flight would be
// API spend for nothing), and a prior verdict at another head to diff from.
// nil otherwise, which the planner reads as "not fetched".
func reviewDeltaDiffFor(ctx context.Context, cfg *config.Config, differ reviewPlannerGitHub, artifact review.Artifact, state review.DispatchState, repo string, number int, headSHA, bodyDigest string) *review.DeltaDiff {
	if differ == nil || !cfg.Review.DeltaRereviewEnabled() || strings.TrimSpace(headSHA) == "" {
		return nil
	}
	if _, judged := artifact.AggregateForBody(repo, number, headSHA, bodyDigest); judged {
		return nil
	}
	if !state.PendingSince(repo, number, headSHA).IsZero() {
		return nil
	}
	prior, ok := artifact.PriorAggregateFor(repo, number, headSHA)
	if !ok {
		return nil
	}
	diff := differ.ReviewDeltaDiff(ctx, repo, prior.HeadSHA, headSHA)
	return &diff
}

func planReviewDispatchWith(ctx context.Context, cfg *config.Config, actionable *github.ActionableResult, agentMgr crux.Runtime, beadStores map[string]*beads.Store, ghr reviewPlannerGitHub, logger *slog.Logger) review.DispatchPlan {
	if cfg == nil || actionable == nil || !cfg.Review.RequireApproval || !cfg.Review.FanOut {
		return review.DispatchPlan{}
	}
	botRoute := reviewBotAuthorRoute(cfg)
	state, err := review.LoadDispatchState("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review dispatch state unavailable; starting fresh", "error", err)
	}
	// classification.review_bots, read the same way installReviewBots reads
	// it for the thread reconciler (#7360): hive.yaml wins, hive-project.yaml
	// is the fallback. Feeds the reviewer's read step (#9360); empty/off
	// leaves ReviewBotLogins empty and the prompt section is skipped.
	reviewBots, rbErr := cfg.EffectiveReviewBots(os.Getenv("HIVE_PROJECT_YAML"))
	if rbErr != nil && logger != nil {
		logger.Warn("classification.review_bots: project file unreadable; reviewer will not see bot findings this pass", "error", rbErr)
	}
	artifact, err := review.LoadArtifact("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review verdict artifact unavailable for dispatch planning", "error", err)
	}
	prAgents := auditPRAgents(cfg.Project.Org, time.Now().Add(-auditPRAttributionWindow), "")
	candidates := make([]github.PullRequest, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	seenPRs := map[string]bool{}
	labelGated := 0
	addCandidates := func(items []github.PullRequest) {
		for _, pr := range items {
			fullRepo := fullRepoName(pr.Repo, cfg.Project.Org)
			key := github.ReviewQueueKey(fullRepo, pr.Number)
			if fullRepo == "" || pr.Number <= 0 || seenPRs[key] {
				continue
			}
			seenPRs[key] = true
			// review.require_labels: an unlabelled PR is never dispatched
			// to the swarm. The relay refuses it independently.
			if !cfg.Review.AdmitsPRLabels(pr.Labels) {
				labelGated++
				continue
			}
			candidates = append(candidates, pr)
		}
	}
	addCandidates(actionable.PRs.Items)
	addCandidates(actionable.PRs.Held)
	if labelGated > 0 && logger != nil {
		logger.Info("review label gate withheld PRs from review dispatch",
			"withheld", labelGated, "candidates", len(candidates),
			"require_labels", strings.Join(cfg.Review.NormalizedRequireLabels(), ","))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, rj := candidates[i].ReviewRank, candidates[j].ReviewRank
		if ri > 0 && rj > 0 && ri != rj {
			return ri < rj
		}
		return ri > 0 && rj <= 0
	})

	scopeReader := reviewScopeLinearReader(cfg, logger)
	prs := make([]review.PullRequest, 0, len(candidates))
	for _, pr := range candidates {
		lane := classify.Classify(github.Issue{Title: pr.Title, Labels: pr.Labels}).Lane
		fullRepo := fullRepoName(pr.Repo, cfg.Project.Org)
		item := review.PullRequest{
			Repo:       pr.Repo,
			Number:     pr.Number,
			Title:      pr.Title,
			Author:     pr.Author,
			Labels:     append([]string(nil), pr.Labels...),
			HeadSHA:    pr.HeadSHA,
			UpdatedAt:  pr.UpdatedAt,
			BodyDigest: pr.BodyDigest,
			URL:        pr.URL,
			Lane:       string(lane),
			// Attribution trailer, carried for consumers and the audit trail.
			// Not a planner input: the fix gate decides on the login author
			// alone (#8421, #13124).
			HiveAttributed: pr.HiveAttributed,
			// The authoring model from the same trailer: what review_models
			// exclusions compare against, and stamped into the verdict as
			// author_model (hivecommons/hive#13243).
			AuthorModel: pr.HiveModel,
			// Grounding anchor for the review prompt. Repo access is the
			// measured active ingredient in review quality (17%→67% hit rate,
			// 61% fewer false positives), so the reviewer is told which commit
			// to read rather than being left to infer from the diff.
			MergeBase:   pr.BaseSHA,
			AuthorAgent: prAgents[fmt.Sprintf("%s#%d", fullRepo, pr.Number)],
			// The author's base over every configured lane, enabled or not,
			// so a disabled or retired authoring replica still excludes its
			// family from reviewing it (hivecommons/hive#13552).
			AuthorBase: reviewAuthorBase(cfg, prAgents[fmt.Sprintf("%s#%d", fullRepo, pr.Number)]),
			// Run trailers (#8310) and the plan they name, for plan_match
			// (#8317). Identifiers and planner output only: the PR body
			// itself still never reaches the prompt.
			RunKey:        pr.HiveRun,
			PlanRef:       pr.HivePlan,
			PlanWave:      planWaveFor(cfg, beadStores, pr.HivePlan, pr.HiveRun),
			ScopeContract: pr.ScopeContract,
			// A maintainer's changes_requested review or `/hive decision`
			// on this head (hivecommons/hive#13041): the planner kicks the
			// fix lane with it instead of reviewing the head again.
			HumanChangeRequest: review.HumanChangeRequestFor(review.DefaultHumanChangeRequests(), fullRepo, pr.Number, pr.HeadSHA),
			// The compare from the last judged head, for the delta
			// re-review and its trivial skip (review.delta_rereview).
			DeltaDiff:     reviewDeltaDiffFor(ctx, cfg, ghr, artifact, state, fullRepo, pr.Number, pr.HeadSHA, pr.BodyDigest),
			BaseDiffEmpty: pr.BaseDiffEmpty,
		}
		// The linked Linear issue's text for the scope contract, read here
		// so reviewers never need a Linear token (hivecommons/hive#14906);
		// only for a PR about to be reviewed at this head.
		if _, judged := artifact.AggregateForBody(fullRepo, pr.Number, pr.HeadSHA, pr.BodyDigest); !judged && state.PendingSince(fullRepo, pr.Number, pr.HeadSHA).IsZero() {
			reviewScopeContractFor(ctx, scopeReader, &item, logger)
		}
		// Changed paths for the bot-author model route's path exceptions
		// (review.bot_author_full_review_paths); fetched only for a listed
		// bot's PR that is about to be reviewed.
		item.ChangedFiles, item.ChangedFilesKnown = reviewBotRouteFilesFor(ctx, cfg, ghr, botRoute, artifact, state, fullRepo, pr.Number, pr.Author, pr.HeadSHA, pr.BodyDigest)
		prs = append(prs, item)
	}
	// PRs a review webhook just fired for go first, so the early eval cycle
	// it triggered spends its review budget on them (hivecommons/hive#11091).
	prs = prioritizeEventPRs(prs, activeReviewEvents.Load())
	agents := make([]review.AgentCapability, 0, len(cfg.Agents))
	for name, ac := range cfg.EnabledAgents() {
		cap := review.CapabilityFromConfig(name, ac)
		cap.Paused = cap.Paused || (agentMgr != nil && agentMgr.IsPaused(name))
		cap.Draining = reviewerAutoscaler.Draining(name)
		agents = append(agents, cap)
	}
	reviewBotLogins := []string{}
	if reviewBots.Enabled() {
		reviewBotLogins = reviewBots.Logins
	}
	plan := review.PlanDispatch(prs, artifact, state, review.DispatchOptions{
		RequireApproval:       cfg.Review.RequireApproval,
		FanOut:                cfg.Review.FanOut,
		MaxParallelReviews:    cfg.Review.EffectiveMaxParallelReviews(),
		MaxPerspectivesPerPR:  cfg.Review.MaxPerspectivesPerPR,
		Perspectives:          reviewPerspectiveSet(cfg, logger),
		CombinedPerspectives:  cfg.Review.CombinedPerspectives,
		ReviewerAgents:        cfg.Review.ReviewerAgents,
		FixerAgent:            cfg.Review.FixerAgent,
		PostComments:          cfg.Review.PostComments,
		WritingGuideSection:   cfg.Project.WritingGuideSection(),
		FixHumanPRs:           cfg.Review.FixHumanPRsEnabled(),
		AcknowledgeNoFindings: cfg.Review.AcknowledgeNoFindings,
		ReviseRepos:           cfg.Review.ReviseRepos,
		ReviseVerdictsBefore:  parseReviseCutoff(cfg.Review.ReviseVerdictsBefore, logger),
		PendingTTL:            cfg.Review.PendingReviewTTL(),
		PendingFixTTL:         cfg.Review.PendingReviewFixTTL(),
		MaxRedispatch:         cfg.Review.EffectiveMaxRedispatch(),
		HeadMaxRetries:        cfg.Review.HeadMaxRetries,
		ProjectOrg:            cfg.Project.Org,
		AIAuthor:              cfg.EffectiveAIAuthor(),
		Agents:                agents,
		ReviewBotLogins:       reviewBotLogins,
		ReviewBotMinPriority:  reviewBots.MinPriority,
		PerspectiveModels:     cfg.Review.PerspectiveModels,
		BotAuthorRoute:        botRoute,
		DeltaRereview:         cfg.Review.DeltaRereviewEnabled(),
		DeltaTrivialLines:     cfg.Review.EffectiveDeltaTrivialLines(),
		DeltaMaxAge:           cfg.Review.EffectiveDeltaMaxAge(),
	})
	recordReviewDeltaRestamps(plan, logger)
	for _, k := range plan.ReviewKicks {
		if k.BotAuthorRouted {
			logger.Info("review routed bot-authored PR to bot_author_model", "repo", k.Repo, "pr", k.Number, "head", k.HeadSHA, "agent", k.Agent, "review_model", k.ReviewModel, "review_backend", k.ReviewBackend)
		}
	}
	for _, sk := range plan.SkippedReviewModels {
		logger.Info("review model fallback skipped perspective", "repo", sk.Repo, "pr", sk.Number, "head", sk.HeadSHA, "perspective", sk.Perspective, "agent", sk.Agent, "author_model", sk.AuthorModel)
	}
	expiredNow := map[string]struct{}{}
	for _, e := range plan.Expired {
		// Warn once per PR while it keeps expiring (#13687).
		key := prWarnKey(e.Repo, e.Number)
		expiredNow[key] = struct{}{}
		logger.Log(context.Background(), reviewExpiredWarn.level(key), "review pending expired", "repo", e.Repo, "pr", e.Number, "head", e.HeadSHA, "perspective", e.Perspective, "agent", e.Agent, "age", e.Age.Round(time.Second).String(), "expiries", e.Count)
	}
	reviewExpiredWarn.retain(expiredNow)
	for _, h := range plan.Exhausted {
		logger.Warn("review dispatch exhausted; holding PR for a human", "repo", h.Repo, "pr", h.Number, "head", h.HeadSHA, "reason", h.Reason)
	}
	for _, f := range plan.ForcedFixes {
		logger.Warn("review fix forced: changes_requested head starved by the revisit path", "repo", f.Repo, "pr", f.Number, "head", f.HeadSHA, "reason", f.Reason, "cycles", f.Cycles, "limit", review.RevisitStarvationCycles)
	}
	promotedNow := map[string]struct{}{}
	for _, w := range plan.Promoted {
		key := prWarnKey(w.Repo, w.Number)
		promotedNow[key] = struct{}{}
		logger.Log(context.Background(), reviewPromotedWarn.level(key), "review dispatch promoted: head starved of a reviewer walked ahead of the queue", "repo", w.Repo, "pr", w.Number, "head", w.HeadSHA, "reason", w.Reason, "cycles", w.Cycles, "since", w.FirstAt, "limit", review.ReviewWaitStarvationCycles)
	}
	reviewPromotedWarn.retain(promotedNow)
	for _, w := range plan.Waiting {
		logger.Info("review dispatch waiting: perspectives still owed, no reviewer this cycle", "repo", w.Repo, "pr", w.Number, "head", w.HeadSHA, "reason", w.Reason)
	}
	if len(plan.ReviewKicks)+len(plan.FixKicks)+len(plan.FixExclusions) > 0 {
		logger.Info("review swarm dispatch planned", "review_kicks", len(plan.ReviewKicks), "fix_kicks", len(plan.FixKicks), "fix_excluded", len(plan.FixExclusions), "fix_excluded_reasons", fixExclusionReasonSummary(plan.FixExclusions))
	}
	return plan
}

// fixExclusionReasonSummary renders the cycle's fix exclusions as
// "reason=count,..." (sorted) for the dispatch summary line, so fix_kicks: 0
// is never printed without saying why (hivecommons/hive#14665).
func fixExclusionReasonSummary(exclusions []review.FixDispatchExclusion) string {
	if len(exclusions) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, x := range exclusions {
		counts[x.Reason]++
	}
	reasons := make([]string, 0, len(counts))
	for r := range counts {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, r := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", r, counts[r]))
	}
	return strings.Join(parts, ",")
}

// recordReviewDeltaRestamps writes the verdicts the planner carried forward
// onto a new head without a session (trivial delta, review.delta_rereview)
// into the verdict artifact, and logs each one. A write failure is logged,
// not fatal: the next cycle plans the same restamp again.
func recordReviewDeltaRestamps(plan review.DispatchPlan, logger *slog.Logger) {
	if len(plan.DeltaRestamps) == 0 {
		return
	}
	for _, r := range plan.DeltaRestamps {
		note := ""
		if r.Aggregate.Delta != nil {
			note = r.Aggregate.Delta.Note
		}
		logger.Info("review delta re-stamped verdict without a session", "repo", r.Repo, "pr", r.Number, "head", r.HeadSHA, "previous_head", r.PreviousHead, "verdict", r.Aggregate.Verdict, "note", note)
	}
	if _, err := review.RecordDeltaRestamps("", plan.DeltaRestamps, time.Now().UTC()); err != nil {
		logger.Warn("failed to record review delta re-stamps", "error", err, "count", len(plan.DeltaRestamps))
	}
}

// reviewPlannerGitHubFor returns the GitHub client as a reviewPlannerGitHub,
// or nil for a nil client: a typed nil in the interface would not read as nil.
func reviewPlannerGitHubFor(c *github.Client) reviewPlannerGitHub {
	if c == nil {
		return nil
	}
	return c
}

func refreshReviewVerdicts(cfg *config.Config, logger *slog.Logger) {
	if cfg == nil || !shouldRefreshReviewVerdicts(cfg) {
		return
	}
	artifact, err := review.CollectAndMerge("", "", review.AggregateOptions{
		// Unanimity is judged against what a PR was eligible to receive. Without
		// this the cap makes approve unreachable and every PR aggregates to
		// requires_human.
		MaxPerspectivesPerPR:       cfg.Review.MaxPerspectivesPerPR,
		Perspectives:               reviewPerspectiveSet(cfg, logger),
		SelfAuthoredDecisionPolicy: cfg.Review.EffectiveSelfAuthoredDecisionPolicy(),
		SensitivePaths:             cfg.Security.SensitivePaths,
		ModelStamps:                reviewModelStamps(logger),
		ExpectedHeads:              cfg.ExpectedReviewHeads(),
		MissingHeads:               reviewMissingHeads(),
	}, time.Now().UTC())
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("failed to refresh review verdicts", "error", err)
		}
		return
	}
	logger.Info("review verdict artifact refreshed", "aggregates", len(artifact.Items))
}

// reviewModelStamps reads the dispatch state so a verdict whose report
// omitted author_model / review_model is stamped with what the planner knew
// when it dispatched that review (hivecommons/hive#13243). No state is no
// stamps, never an error: the artifact is still correct, just unstamped.
func reviewModelStamps(logger *slog.Logger) review.ModelStamps {
	state, err := review.LoadDispatchState("")
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("review dispatch state unavailable for model stamping", "error", err)
		}
		return nil
	}
	return review.ModelStampsFromState(state)
}

func shouldRefreshReviewVerdicts(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.Review.RequireApproval {
		return true
	}
	entries, err := os.ReadDir(review.ReportDir(""))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, review.ReviewReportFilePrefix) && strings.HasSuffix(name, review.ReviewReportFileSuffix) {
			return true
		}
	}
	return false
}

// reviewOutcomeResolveCap bounds how many vanished PRs one eval cycle asks
// GitHub about. A hive pointed at a busy org can see dozens of PRs merge
// between cycles; the rest are resolved next cycle, and an unresolved PR
// reads as still open, never as merged.
const reviewOutcomeResolveCap = 40

// prStateGetter is the slice of the GitHub client the outcome ledger needs.
type prStateGetter interface {
	GetPRState(ctx context.Context, repo string, number int) (github.PRState, error)
}

// reviewEvidenceOutcomeRecorder is the optional slice of the GitHub client
// that stamps a resolved outcome onto the PR's evidence bundle, so a PR this
// hive reviewed but did not merge stops reading as "outcome unknown".
type reviewEvidenceOutcomeRecorder interface {
	RecordReviewEvidenceOutcome(repo string, number int, st github.PRState, now time.Time)
}

// recordReviewOutcomes folds this cycle's open-PR enumeration into the
// review-outcome ledger (pkg/review/outcomes.go): every open PR is upserted,
// review evidence from the verdict artifact and the posted-review ledger is
// attached, and PRs that left the list are resolved to merged/closed with one
// GET each. It is what lets the dashboard say whether reviewed PRs merge
// faster than unreviewed ones on THIS hive, rather than counting verdicts.
func recordReviewOutcomes(ctx context.Context, cfg *config.Config, gh prStateGetter, actionable *github.ActionableResult, logger *slog.Logger) {
	if cfg == nil || actionable == nil {
		return
	}
	recordReviewOutcomesAt(ctx, cfg, gh, actionable, "", time.Now().UTC(), logger)
}

func recordReviewOutcomesAt(ctx context.Context, cfg *config.Config, gh prStateGetter, actionable *github.ActionableResult, path string, now time.Time, logger *slog.Logger) {
	ledger, err := review.LoadOutcomeLedger(path)
	if err != nil {
		logger.Warn("review outcomes: ledger unreadable, starting fresh", "error", err)
	}
	aiAuthor := cfg.EffectiveAIAuthor()
	open := make([]review.OpenPR, 0, len(actionable.PRs.Items))
	for _, pr := range actionable.PRs.Items {
		full := config.QualifyRepo(cfg.Project.Org, pr.Repo)
		open = append(open, review.OpenPR{
			Repo:          full,
			Number:        pr.Number,
			Author:        pr.Author,
			AgentAuthored: pr.HiveAttributed || pr.AppAuthored || (aiAuthor != "" && strings.EqualFold(pr.Author, aiAuthor)),
			CreatedAt:     pr.CreatedAt,
		})
	}

	// Review evidence: the verdict artifact knows the verdict; the links
	// ledger knows a review was posted even when its verdict could not bind
	// (queue-lane reviews of undispatched PRs). Earliest time wins.
	signals := map[string]review.ReviewSignal{}
	if art, err := review.LoadArtifact(""); err == nil {
		for _, it := range art.Items {
			key := it.Repo + "#" + strconv.Itoa(it.Number)
			prev, ok := signals[key]
			if !ok || (!it.RecordedAt.IsZero() && it.RecordedAt.Before(prev.At)) {
				signals[key] = review.ReviewSignal{At: it.RecordedAt, Verdict: it.Verdict}
			} else if ok && prev.Verdict == "" {
				prev.Verdict = it.Verdict
				signals[key] = prev
			}
		}
	}
	if links, err := github.LoadReviewLinks(""); err == nil {
		for key, link := range links {
			at := link.At
			if at.IsZero() {
				continue
			}
			prev, ok := signals[key]
			if !ok {
				signals[key] = review.ReviewSignal{At: at}
			} else if at.Before(prev.At) {
				prev.At = at
				signals[key] = prev
			}
		}
	}

	missing := ledger.Observe(now, open, signals)
	resolved := 0
	if gh != nil {
		for i, m := range missing {
			if i >= reviewOutcomeResolveCap {
				break
			}
			st, err := gh.GetPRState(ctx, m.Repo, m.Number)
			if err != nil {
				logger.Debug("review outcomes: could not resolve vanished PR", "repo", m.Repo, "number", m.Number, "error", err)
				continue
			}
			ledger.Resolve(m.Repo, m.Number, st.State, st.MergedAt, st.ClosedAt)
			resolved++
			if rec, ok := gh.(reviewEvidenceOutcomeRecorder); ok {
				rec.RecordReviewEvidenceOutcome(m.Repo, m.Number, st, now)
			}
		}
	}
	ledger.Snapshot(now)
	if err := ledger.Save(path, now); err != nil {
		logger.Warn("review outcomes: save failed", "error", err)
		return
	}
	if len(missing) > 0 {
		logger.Info("review outcomes recorded", "open", len(open), "left_queue", len(missing), "resolved", resolved)
	}
}

// reviewFixAuditor is the slice of the dashboard the withheld-fix audit
// needs, so the function is testable without a live server.
type reviewFixAuditor interface {
	AuditLog(user, action, detail, agent string)
}

// auditWithheldReviewFixes puts every fix the planner refused this cycle on
// the audit trail (#8421). A refusal is silent by design — the PR just does
// not get a bot commit — and silence is the one thing an operator cannot
// investigate, so each one names the PR, who opened it, and the
// review.fix_human_prs value at the time (on no longer permits the push:
// hivecommons/hive#12897). The planner records the exclusion and
// the withheld fix only on the cycle that first withholds a head (a persisted
// State.Withheld entry suppresses later cycles), so this does not repeat on
// every eval cycle.
func auditWithheldReviewFixes(plan review.DispatchPlan, auditor reviewFixAuditor, logger *slog.Logger) {
	// One WARN per changes_requested head that ends the cycle without a fix
	// kick: a PR shown as "Fixing" that nobody is fixing must be loud
	// (hivecommons/hive#14665).
	for _, x := range plan.FixExclusions {
		level := slog.LevelWarn
		if x.Reason == review.FixDispatchExcludedNotHiveAuthored || x.Reason == review.FixDispatchExcludedNotHiveAuthoredDCO {
			// Policy skip (the hive never pushes to PRs it did not author), not a fault.
			level = slog.LevelInfo
		}
		logger.Log(context.Background(), level, "review fix dispatch excluded",
			"repo", x.Repo, "pr", x.Number, "head_sha", x.HeadSHA, "author", x.Author, "reason", x.Reason, "attempts", x.Attempts)
	}
	for _, w := range plan.WithheldFixes {
		logger.Info("review fix withheld: PR not hive-authored; review published, nothing pushed",
			"repo", w.Repo, "pr", w.Number, "head_sha", w.HeadSHA, "author", w.Author, "setting", w.Setting, "setting_enabled", w.SettingEnabled)
		if auditor == nil {
			continue
		}
		auditor.AuditLog("governor", "review_fix_withheld",
			fmt.Sprintf("pr=%s#%d head=%s author=%s setting=%s=%t reason=%s", w.Repo, w.Number, w.HeadSHA, w.Author, w.Setting, w.SettingEnabled, w.Reason), "")
	}
}

// reviewMissingHeads reads the review heads that ran out of retries while
// another head answered (hivecommons/hive#13542), so their perspectives count
// as settled for the decision step. No state is none.
func reviewMissingHeads() map[string][]string {
	state, err := review.LoadDispatchState("")
	if err != nil {
		return nil
	}
	return review.MissingHeadsFromState(state)
}

// reviewCommentPoster is the GitHub write postReviewAcknowledgements needs.
type reviewCommentPoster interface {
	CreateIssueCommentBudgeted(ctx context.Context, repo string, number int, body string) error
}

// postReviewAcknowledgements posts the one-line no-findings comment for each
// PR head the review-heads skip rule settled this cycle: every head approved
// every perspective with no findings, so no decision session ran to post it
// (hivecommons/hive#13542). The planner records each head once, so a failed
// post is logged rather than repeated.
func postReviewAcknowledgements(ctx context.Context, poster reviewCommentPoster, plan review.DispatchPlan, logger *slog.Logger) {
	if len(plan.Acknowledgements) == 0 || poster == nil {
		return
	}
	for _, a := range plan.Acknowledgements {
		if err := poster.CreateIssueCommentBudgeted(ctx, a.Repo, a.Number, a.Body); err != nil {
			logger.Warn("review heads: could not post the no-findings acknowledgement", "repo", a.Repo, "pr", a.Number, "head_sha", a.HeadSHA, "error", err)
			continue
		}
		logger.Info("review heads: posted the no-findings acknowledgement", "repo", a.Repo, "pr", a.Number, "head_sha", a.HeadSHA)
	}
}

func persistReviewDispatchState(plan review.DispatchPlan, delivered []review.DispatchKick, logger *slog.Logger) {
	writeReviewDispatchState(plan, func(planned []review.DispatchKick) review.DispatchState {
		return review.ConfirmDelivered(plan.State, planned, delivered)
	}, logger)
}

// persistReviewDispatchStateInProgress is the persist made while the kick pass
// is still delivering: it keeps the pending rows of planned kicks not yet
// confirmed, so a verdict from a kick still in flight binds to its dispatch
// (hivecommons/hive#15559). The end-of-pass persistReviewDispatchState drops
// the kicks that never landed.
func persistReviewDispatchStateInProgress(plan review.DispatchPlan, delivered []review.DispatchKick, logger *slog.Logger) {
	writeReviewDispatchState(plan, func([]review.DispatchKick) review.DispatchState {
		return review.ConfirmDeliveredInProgress(plan.State, delivered)
	}, logger)
}

func writeReviewDispatchState(plan review.DispatchPlan, confirm func(planned []review.DispatchKick) review.DispatchState, logger *slog.Logger) {
	planned := append(append([]review.DispatchKick(nil), plan.ReviewKicks...), plan.FixKicks...)
	if plan.State.GeneratedAt.IsZero() && len(planned) == 0 {
		return
	}
	state := confirm(planned)
	if err := review.WriteDispatchState("", state); err != nil {
		logger.Warn("failed to persist review dispatch state", "error", err)
	}
}

func atomicWrite(path string, data []byte) {
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}
