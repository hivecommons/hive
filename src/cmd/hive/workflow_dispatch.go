package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// Workflow-model agent dispatch, eval-cycle wiring (cncf/techdocs#395).
//
// Per cycle, after the generic kicks are planned from an actionable set
// that no longer offers model-owned items (scheduler.WithholdWorkflowOwned):
//
//  1. collect every enumerated item whose bound model hands its state to a
//     profile, plus the open drafts of repositories whose model dispatches
//     a pull-request state (the enumeration leaves drafts out);
//  2. plan the hand-offs (scheduler.PlanWorkflowDispatch): the agent that
//     fills the profile, the triggering reply for a reply-started profile,
//     and the idempotency key;
//  3. for an issue, take the agent claim on the issue-claims ledger FIRST;
//     a claim someone else holds stops the hand-off;
//  4. append one kick per hand-off to the cycle's messages. On delivery the
//     key is written to the durable dispatch log as a pending hand-off, so
//     neither a later cycle nor a restart repeats it; a failed or deferred
//     kick leaves the key free for the next cycle;
//  5. on later cycles, review each pending hand-off whose item is still in
//     its state (workflow_dispatch_handoff.go): an agent that declined or
//     ended its turn without the deliverable did not start it, and the
//     item is handed once more to a different agent that qualifies
//     (hivecommons/hive#18749).

// workflowDispatchLogFile is the durable log under the workflow store root.
const workflowDispatchLogFile = "workflow-dispatch.json"

// workflowDispatchPendingTTL bounds how long a planned, not yet delivered
// hand-off blocks re-planning in this process. A kick a later gate
// withholds (provider budget, quota hold, upgrade drain) reports nothing,
// so the in-flight mark lapses on its own and the next cycle plans again.
const workflowDispatchPendingTTL = 30 * time.Minute

// workflowDraftListTTL bounds how often one bound repository's open drafts
// are listed. Webhook-driven cycles can run minutes apart or less; a draft
// that appeared since is picked up within this window.
const workflowDraftListTTL = 5 * time.Minute

// workflowDispatchClaimTTL is the agent claim taken before the drafter is
// kicked: long enough for a draft to reach its PR (the covering PR then
// holds the issue), short enough that an abandoned hand-off frees it.
const workflowDispatchClaimTTL = 8 * time.Hour

type workflowDispatchRuntime struct {
	once    sync.Once
	log     *scheduler.WorkflowDispatchLog
	mu      sync.Mutex
	pending map[string]time.Time
	// replies caches LatestDispatchReply per item ref, keyed on the item's
	// updated_at: a new comment moves it, so an unchanged item costs no call.
	replies map[string]workflowReplyCacheEntry
	// drafts caches ListWorkflowDraftPRs per repository for
	// workflowDraftListTTL.
	drafts map[string]workflowDraftCacheEntry
	// seen are the items in a dispatched state as of the latest pass
	// (workflowHandoffSeenKey), so the dashboard lists a hand-off that was
	// not started only while its item is still waiting in that state.
	seen map[string]bool
	// absences counts, per hand-off key, the consecutive passes its item
	// was missing from the dispatched items. Only the eval cycle touches it.
	absences map[string]int
}

type workflowDraftCacheEntry struct {
	at  time.Time
	prs []github.PullRequest
}

type workflowReplyCacheEntry struct {
	updatedAt time.Time
	reply     github.DispatchReply
	found     bool
}

var workflowDispatchRT = &workflowDispatchRuntime{pending: map[string]time.Time{}, replies: map[string]workflowReplyCacheEntry{}, drafts: map[string]workflowDraftCacheEntry{}}

// draftsFor returns repo's open drafts, listing them at most once per
// workflowDraftListTTL.
func (rt *workflowDispatchRuntime) draftsFor(ctx context.Context, ghClient *github.Client, repo string, now time.Time) ([]github.PullRequest, error) {
	rt.mu.Lock()
	if rt.drafts == nil {
		rt.drafts = map[string]workflowDraftCacheEntry{}
	}
	cached, ok := rt.drafts[repo]
	rt.mu.Unlock()
	if ok && now.Sub(cached.at) < workflowDraftListTTL {
		return cached.prs, nil
	}
	prs, err := ghClient.ListWorkflowDraftPRs(ctx, repo)
	if err != nil {
		return nil, err
	}
	rt.mu.Lock()
	rt.drafts[repo] = workflowDraftCacheEntry{at: now, prs: prs}
	rt.mu.Unlock()
	return prs, nil
}

// dispatchLog opens the durable log once per process.
func (rt *workflowDispatchRuntime) dispatchLog(cfg *config.Config, logger *slog.Logger) *scheduler.WorkflowDispatchLog {
	rt.once.Do(func() {
		path := ""
		if cfg != nil {
			path = filepath.Join(wfstore.Root(cfg.Data.WorkflowsDir), workflowDispatchLogFile)
		}
		l, err := scheduler.OpenWorkflowDispatchLog(path)
		if err != nil && logger != nil {
			logger.Warn("workflow dispatch log unreadable; starting empty", "path", path, "error", err)
		}
		if cfg != nil {
			// One-time: records the governor wrote under the short repo form
			// join the owner's qualified ones (hivecommons/hive#18901).
			if n, merr := l.MigrateQualifiedRefs(cfg.Project.Org); merr != nil && logger != nil {
				logger.Warn("workflow dispatch log: migrating to qualified refs failed", "path", path, "error", merr)
			} else if n > 0 && logger != nil {
				logger.Info("workflow dispatch log: migrated records to qualified refs", "records", n)
			}
		}
		rt.log = l
	})
	return rt.log
}

func (rt *workflowDispatchRuntime) inFlight(key string, now time.Time) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	at, ok := rt.pending[key]
	if ok && now.Sub(at) > workflowDispatchPendingTTL {
		delete(rt.pending, key)
		return false
	}
	return ok
}

func (rt *workflowDispatchRuntime) markPending(key string, now time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.pending[key] = now
}

func (rt *workflowDispatchRuntime) clearPending(key string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.pending, key)
}

// workflowDispatchKick is one planned hand-off and the record its delivery
// writes.
type workflowDispatchKick struct {
	Message  scheduler.KickMessage
	Entry    scheduler.WorkflowDispatchEntry
	Dispatch github.WorkflowDispatch
}

// workflowDispatchActive reports whether the cycle dispatches at all.
func workflowDispatchActive(cfg *config.Config) bool {
	return cfg != nil && cfg.Workflow.Enabled && cfg.Workflow.Dispatch.DispatchEnabled()
}

// withholdWorkflowOwnedForKicks removes model-owned items from the set the
// generic kicks are built from. Model ownership is the model's, so it holds
// whatever workflow.dispatch says; only workflow.enabled (no model derives
// anything) turns it off.
func withholdWorkflowOwnedForKicks(cfg *config.Config, ghClient *github.Client, actionable *github.ActionableResult, logger *slog.Logger) *github.ActionableResult {
	if cfg == nil || !cfg.Workflow.Enabled || ghClient == nil {
		return actionable
	}
	out, withheld := scheduler.WithholdWorkflowOwned(actionable, ghClient)
	if withheld > 0 && logger != nil {
		logger.Info("workflow dispatch: model-owned items withheld from generic kicks", "withheld", withheld)
	}
	return out
}

// workflowDispatchItems collects the cycle's model-owned items.
func workflowDispatchItems(ctx context.Context, cfg *config.Config, ghClient *github.Client, actionable *github.ActionableResult, logger *slog.Logger) []github.WorkflowDispatch {
	var items []github.WorkflowDispatch
	if actionable != nil {
		for _, issue := range actionable.Issues.Items {
			if d, ok := ghClient.WorkflowDispatchForIssue(issue); ok {
				items = append(items, d)
			}
		}
		for _, pr := range actionable.PRs.Items {
			if d, ok := ghClient.WorkflowDispatchForPR(pr); ok {
				items = append(items, d)
			}
		}
	}
	for _, b := range cfg.Project.WorkflowBindings {
		p := ghClient.WorkflowModelFor(b.Repo)
		if !dispatchesPRState(p) {
			continue
		}
		drafts, err := workflowDispatchRT.draftsFor(ctx, ghClient, qualifyClaimRepo(cfg.Project.Org, b.Repo), time.Now())
		if err != nil {
			if logger != nil {
				logger.Warn("workflow dispatch: listing drafts failed", "repo", b.Repo, "error", err)
			}
			continue
		}
		for _, pr := range drafts {
			if d, ok := ghClient.WorkflowDispatchForPR(pr); ok {
				items = append(items, d)
			}
		}
	}
	return items
}

func dispatchesPRState(p *workflowmodel.Profile) bool {
	if p == nil {
		return false
	}
	for _, t := range p.Dispatch {
		if t.AppliesTo(workflowmodel.KindPR) {
			return true
		}
	}
	return false
}

// workflowDispatchReplyFor is the cached reply lookup the planner uses.
func workflowDispatchReplyFor(ctx context.Context, ghClient *github.Client) func(d github.WorkflowDispatch) (github.DispatchReply, bool, error) {
	rt := workflowDispatchRT
	return func(d github.WorkflowDispatch) (github.DispatchReply, bool, error) {
		ref := d.Ref() + "|" + d.Target.Reply
		at := d.UpdatedAt
		rt.mu.Lock()
		cached, ok := rt.replies[ref]
		rt.mu.Unlock()
		if ok && !at.IsZero() && cached.updatedAt.Equal(at) {
			return cached.reply, cached.found, nil
		}
		r, found, err := ghClient.LatestDispatchReply(ctx, d.Repo, d.Number, d.Target.Reply)
		if err != nil {
			return github.DispatchReply{}, false, err
		}
		rt.mu.Lock()
		rt.replies[ref] = workflowReplyCacheEntry{updatedAt: at, reply: r, found: found}
		rt.mu.Unlock()
		return r, found, nil
	}
}

// resolveWorkflowDispatchAgent resolves the hive agent that fills the
// item's profile (config.ResolveWorkflowDispatch), telling the resolver the
// item's repository and whether the profile opens a pull request or reviews
// one: every step picks only an agent that may do that there
// (hivecommons/hive#18690, hivecommons/hive#18749). exclude names agents an
// earlier hand-off of the item went to without starting it.
func resolveWorkflowDispatchAgent(cfg *config.Config, d github.WorkflowDispatch, manual bool, exclude ...string) config.WorkflowDispatchResolution {
	need := workflowDispatchNeedFor(d)
	need.Exclude = exclude
	return cfg.ResolveWorkflowDispatch(d.Target.Profile, need, manual)
}

// workflowDispatchFix is where the owner closes a gap, for the log.
func workflowDispatchFix(profile string) string {
	return "pick an agent that may do the profile's work on the repository's workflow row in Settings → Repos (workflow.dispatch.profiles." + profile + ".agent), or raise an agent's mode"
}

// planWorkflowDispatchKicks reviews the hand-offs already made, plans the
// cycle's new ones and claims each issue for its agent before returning the
// kick. turn reads how an agent's most recent kick ended (nil: unknown).
func planWorkflowDispatchKicks(ctx context.Context, cfg *config.Config, ghClient *github.Client, actionable *github.ActionableResult, ledger *claims.Ledger, eligible func(agent string) bool, turn func(agent string) (workflowAgentTurn, bool), logger *slog.Logger) []workflowDispatchKick {
	if !workflowDispatchActive(cfg) || ghClient == nil {
		return nil
	}
	rt := workflowDispatchRT
	dlog := rt.dispatchLog(cfg, logger)
	now := time.Now()
	items := workflowDispatchItems(ctx, cfg, ghClient, actionable, logger)
	reply := workflowDispatchReplyFor(ctx, ghClient)
	if rt.absences == nil {
		rt.absences = map[string]int{}
	}
	seen := reviewWorkflowHandoffs(cfg, dlog, items, reply, turn, ledger, actionable != nil, rt.absences, now, logger)
	if actionable != nil {
		rt.noteSeen(seen)
	}
	if len(items) == 0 {
		return nil
	}
	return buildWorkflowDispatchKicks(cfg, rt, dlog, items, reply, ledger, eligible, now, logger)
}

// buildWorkflowDispatchKicks plans the hand-offs of items and claims each
// issue for its agent.
func buildWorkflowDispatchKicks(cfg *config.Config, rt *workflowDispatchRuntime, dlog *scheduler.WorkflowDispatchLog, items []github.WorkflowDispatch, reply func(d github.WorkflowDispatch) (github.DispatchReply, bool, error), ledger *claims.Ledger, eligible func(agent string) bool, now time.Time, logger *slog.Logger) []workflowDispatchKick {
	problems := map[string]string{}
	cands, skips := scheduler.PlanWorkflowDispatch(items, scheduler.WorkflowDispatchPlanDeps{
		Agent: func(d github.WorkflowDispatch) (string, bool) {
			r := resolveWorkflowDispatchAgent(cfg, d, false)
			if !r.OK() {
				problems[d.Ref()] = r.Problem
			}
			return r.Agent, r.OK()
		},
		Eligible: eligible,
		Reply:    reply,
		Done:     func(key string) bool { return dlog.Done(key) || rt.inFlight(key, now) },
		Prior: func(key string) (scheduler.WorkflowDispatchEntry, bool) {
			e, ok := dlog.Get(key)
			return e, ok && e.Retryable()
		},
		RetryAgent: func(d github.WorkflowDispatch, tried []string) (string, bool) {
			r := resolveWorkflowDispatchAgent(cfg, d, false, tried...)
			if !r.OK() {
				problems[d.Ref()] = r.Problem
			}
			return r.Agent, r.OK()
		},
	})
	for _, s := range skips {
		if logger != nil && s.Reason != scheduler.WorkflowSkipDone && s.Reason != scheduler.WorkflowSkipNoReply {
			reason, prov := workflowSkipReasonLog(s, dlog, now)
			logger.Info("workflow dispatch: item not handed off", append([]any{"item", s.Ref, "profile", s.Profile, "reason", reason,
				"problem", problems[s.Ref], "fix", workflowDispatchFix(s.Profile)}, prov...)...)
		}
	}
	var out []workflowDispatchKick
	for _, c := range cands {
		if !claimForWorkflowDispatch(cfg, ledger, c, logger) {
			continue
		}
		c.AgentMode = cfg.AgentInteractionMode(c.Agent).String()
		set := cfg.Workflow.Dispatch.Profile(c.Dispatch.Target.Profile)
		msg := scheduler.BuildWorkflowDispatchPrompt(c, set.PromptTemplate)
		var refs []string
		if c.Dispatch.Kind == workflowmodel.KindIssue {
			refs = []string{c.Dispatch.Ref()}
		}
		rt.markPending(c.Key, now)
		// Provenance for the per-repo open-PRs gate (repo_open_prs.go),
		// recorded before the kick is sent.
		workflowHandoffKicks.record(c.Agent, c.Dispatch.Ref(), msg, now)
		out = append(out, workflowDispatchKick{
			Dispatch: c.Dispatch,
			Message:  scheduler.KickMessage{Agent: c.Agent, Message: msg, IssueRefs: refs},
			Entry: scheduler.NextWorkflowDispatchEntry(scheduler.WorkflowDispatchEntry{
				Key: c.Key, Ref: c.Dispatch.Ref(), Profile: c.Dispatch.Target.Profile,
				State: c.Dispatch.State, Agent: c.Agent, By: "governor",
			}, c.Prior),
		})
		if logger != nil {
			logger.Info("workflow dispatch: handing item to agent", "item", c.Dispatch.Ref(), "model", c.Dispatch.Model,
				"state", c.Dispatch.State, "profile", c.Dispatch.Target.Profile, "agent", c.Agent, "agent_mode", c.AgentMode, "retry", c.Prior != nil)
		}
	}
	return out
}

// claimForWorkflowDispatch takes the agent claim on an issue before the
// kick. A pull request is not claimed (an issue claim on a PR stages it as
// an issue); its idempotency is the dispatch key alone. With no ledger the
// hand-off proceeds: the item is already withheld from every generic lane.
func claimForWorkflowDispatch(cfg *config.Config, ledger *claims.Ledger, c scheduler.WorkflowDispatchCandidate, logger *slog.Logger) bool {
	if c.Dispatch.Kind != workflowmodel.KindIssue || ledger == nil {
		return true
	}
	res, err := ledger.Claim(claims.Request{
		Repo:     qualifyClaimRepo(cfg.Project.Org, c.Dispatch.Repo),
		Issue:    c.Dispatch.Number,
		Holder:   c.Agent,
		HolderID: c.Agent,
		Kind:     claims.KindAgent,
		TTL:      workflowDispatchClaimTTL,
	})
	if err != nil {
		if logger != nil {
			logger.Warn("workflow dispatch: claim refused; not handing off", "item", c.Dispatch.Ref(), "agent", c.Agent, "error", err)
		}
		return false
	}
	if !res.Outcome.Changed() {
		if logger != nil {
			logger.Info("workflow dispatch: item held by another claimant; not handing off", "item", c.Dispatch.Ref(), "agent", c.Agent, "outcome", string(res.Outcome))
		}
		return false
	}
	return true
}

// recordWorkflowDispatchDelivered writes the durable record of a delivered
// hand-off: pending until a later pass sees whether it started.
func recordWorkflowDispatchDelivered(cfg *config.Config, k workflowDispatchKick, at time.Time, logger *slog.Logger) {
	rt := workflowDispatchRT
	e := k.Entry
	e.At = at
	if err := rt.dispatchLog(cfg, logger).Record(e); err != nil && logger != nil {
		logger.Warn("workflow dispatch: recording hand-off failed", "item", e.Ref, "error", err)
	}
	rt.clearPending(e.Key)
}

// releaseWorkflowDispatch frees a hand-off whose kick did not land.
func releaseWorkflowDispatch(k workflowDispatchKick) { workflowDispatchRT.clearPending(k.Entry.Key) }

// workflowDispatchKickKey identifies a workflow kick among the cycle's
// messages.
func workflowDispatchKickKey(msg scheduler.KickMessage) string {
	return msg.Agent + "\x00" + msg.Message
}

// workflowDispatchPrepare is the dashboard's WorkflowDispatchPrepare: the
// owner's "Send to <profile>" through the same steps as the automatic
// pass (fresh item read, model state, agent, claim first, prompt). The
// handler kicks the agent and calls Record once the kick landed, so the
// automatic pass does not hand the same state off again.
func workflowDispatchPrepare(cfg *config.Config, client func() *github.Client, logger *slog.Logger) func(ctx context.Context, repo string, number int, by string, ledger *claims.Ledger) (dashboard.WorkflowDispatchPlan, error) {
	return func(ctx context.Context, repo string, number int, by string, ledger *claims.Ledger) (dashboard.WorkflowDispatchPlan, error) {
		gh := client()
		if cfg == nil || gh == nil {
			return dashboard.WorkflowDispatchPlan{}, errors.New("GitHub client unavailable")
		}
		if !cfg.Workflow.Enabled {
			return dashboard.WorkflowDispatchPlan{}, fmt.Errorf("%w (workflow.enabled is off)", dashboard.ErrWorkflowDispatchNotOwned)
		}
		d, ok, err := gh.WorkflowDispatchForRef(ctx, qualifyClaimRepo(cfg.Project.Org, repo), number)
		if err != nil {
			return dashboard.WorkflowDispatchPlan{}, err
		}
		if !ok {
			return dashboard.WorkflowDispatchPlan{}, dashboard.ErrWorkflowDispatchNotOwned
		}
		res := resolveWorkflowDispatchAgent(cfg, d, true)
		if !res.OK() {
			return dashboard.WorkflowDispatchPlan{}, dashboard.WorkflowDispatchNoAgentError(res)
		}
		agent := res.Agent
		c := scheduler.WorkflowDispatchCandidate{Dispatch: d, Agent: agent, Key: scheduler.WorkflowDispatchKey(d, nil), ManualBy: by, AgentMode: res.Mode}
		// An earlier hand-off of this state carries its attempts over, so
		// a send that is not started either is not retried without end.
		var prior *scheduler.WorkflowDispatchEntry
		if e, ok := workflowDispatchRT.dispatchLog(cfg, logger).Get(c.Key); ok {
			prior = &e
			if e.Status == scheduler.WorkflowHandoffNotStarted && e.Agent != agent {
				// The agent that did not start it may still hold the claim.
				releaseWorkflowDispatchClaim(cfg, ledger, d, e.Agent)
			}
		}
		if !claimForWorkflowDispatch(cfg, ledger, c, logger) {
			return dashboard.WorkflowDispatchPlan{}, dashboard.ErrWorkflowDispatchHeld
		}
		entry := scheduler.NextWorkflowDispatchEntry(scheduler.WorkflowDispatchEntry{Key: c.Key, Ref: d.Ref(), Profile: d.Target.Profile, State: d.State, Agent: agent, By: by}, prior)
		message := scheduler.BuildWorkflowDispatchPrompt(c, cfg.Workflow.Dispatch.Profile(d.Target.Profile).PromptTemplate)
		// Provenance for the per-repo open-PRs gate, before the handler
		// sends the kick (repo_open_prs.go).
		workflowHandoffKicks.record(agent, d.Ref(), message, time.Now())
		return dashboard.WorkflowDispatchPlan{
			Ref: d.Ref(), Model: d.Model, State: d.State, Profile: d.Target.Profile, Agent: agent,
			Message: message,
			Record: func(at time.Time) {
				recordWorkflowDispatchDelivered(cfg, workflowDispatchKick{Entry: entry}, at, logger)
			},
		}, nil
	}
}
