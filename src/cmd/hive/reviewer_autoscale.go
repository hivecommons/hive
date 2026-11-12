package main

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/autoscale"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// reviewerAutoscaleSkipReason is the agents_skipped annotation for a
// draining reviewer withheld from the cadence kick.
const reviewerAutoscaleSkipReason = "reviewer-autoscale draining"

// unknownProviderHeadroom tells the controller no headroom reading exists.
const unknownProviderHeadroom = -1

// reviewerAutoscaler is the process-wide reviewer-lane autoscaler
// (hivecommons/hive#14685). It is package-level, like providerBudgetProbe,
// so runEvalCycle's already-long signature does not grow; the eval loop is
// single-threaded but the dashboard reads Status concurrently, hence the
// mutex.
var reviewerAutoscaler = &reviewerAutoscaleRuntime{prober: autoscale.NewProber()}

type reviewerAutoscaleRuntime struct {
	mu       sync.Mutex
	ctrl     *autoscale.Controller
	prober   *autoscale.Prober
	headroom rotation.HeadroomReporter
	status   autoscale.Status
	// warned is the last "switched on but scaling nothing" warning logged,
	// so the same reason is logged once rather than every cycle.
	warned string
}

// SetHeadroomReporter wires the provider headroom probe when rotation is on.
func (r *reviewerAutoscaleRuntime) SetHeadroomReporter(h rotation.HeadroomReporter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headroom = h
}

// Draining reports whether name is a reviewer being retired; the planner
// and the cadence kick both withhold work from it.
func (r *reviewerAutoscaleRuntime) Draining(name string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctrl != nil && r.ctrl.Draining(name)
}

// Status is the dashboard snapshot; Enabled is false when autoscale is off.
func (r *reviewerAutoscaleRuntime) Status() autoscale.Status {
	if r == nil {
		return autoscale.Status{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// withholdDrainingReviewers removes draining reviewers from the cadence
// kick list, reporting them in agentsSkipped like every other gate.
func (r *reviewerAutoscaleRuntime) withholdDrainingReviewers(agentsDue, agentsSkipped []string) ([]string, []string) {
	if r == nil {
		return agentsDue, agentsSkipped
	}
	kept := agentsDue[:0]
	for _, name := range agentsDue {
		if r.Draining(name) {
			agentsSkipped = append(agentsSkipped, name+" ("+reviewerAutoscaleSkipReason+")")
			continue
		}
		kept = append(kept, name)
	}
	return kept, agentsSkipped
}

// Run folds this cycle's review plan into the controller and applies its
// decision: a scale-up or retire re-expands the lane's replicas and
// reconciles the agent manager and governor the same way a dashboard
// replicas edit does; a drain only changes what Draining answers.
//
// It also follows the lane itself. When autoscale is turned on, turned off,
// or resolves to a different agent than the controller was managing, the
// lane it leaves goes back to its declared replicas and the lane it enters
// starts from its own declared count clamped into the band, straight away
// rather than at the next scaling decision (see resetLane).
func (r *reviewerAutoscaleRuntime) Run(ctx context.Context, cfg *config.Config, plan review.DispatchPlan, agentMgr crux.Runtime, gov *governor.Governor, logger *slog.Logger, now time.Time) {
	if r == nil || cfg == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !cfg.ReviewerAutoscaleEnabled() {
		if r.ctrl != nil {
			prev := r.ctrl.Agent()
			r.ctrl = nil
			r.status = autoscale.Status{}
			retired := r.resetLane(ctx, cfg, agentMgr, gov, logger)
			logger.Info("reviewer autoscale: disabled; lane back on declared replicas", "agent", prev, "retired", retired)
		}
		r.warnNotScaling(cfg, logger)
		return
	}
	r.warned = ""
	agent := cfg.ReviewerAutoscaleAgent()
	acfg := cfg.Review.Reviewers.Autoscale
	if r.ctrl == nil {
		r.ctrl = autoscale.New(acfg, agent)
		r.resetLane(ctx, cfg, agentMgr, gov, logger)
		logger.Info("reviewer autoscale: enabled", "agent", agent, "min", acfg.EffectiveMin(), "max", acfg.EffectiveMax(), "target_backlog_per_reviewer", acfg.EffectiveTargetBacklogPerReviewer(), "cooldown", acfg.EffectiveCooldown().String())
	} else if prev := r.ctrl.Agent(); r.ctrl.Reconfigure(acfg, agent) {
		retired := r.resetLane(ctx, cfg, agentMgr, gov, logger)
		logger.Info("reviewer autoscale: lane agent changed; the old lane is back on its declared replicas", "from", prev, "to", agent, "retired", retired, "replicas", len(cfg.ReviewerAutoscaleReplicas()))
	}
	names := cfg.ReviewerAutoscaleReplicas()
	if cfg.ReviewerAutoscaleActive == 0 || cfg.ReviewerAutoscaleActiveAgent != agent {
		cfg.ReviewerAutoscaleActive = acfg.Clamp(len(names))
		cfg.ReviewerAutoscaleActiveAgent = agent
	}

	obs := autoscale.Observation{
		Now:                 now,
		DwellThreshold:      stagelabels.Dwell(stagelabels.ReviewPending, cfg.Governor.Labels.Stage.DwellOverrides()),
		MaxParallelReviews:  cfg.Review.EffectiveMaxParallelReviews(),
		Resources:           r.prober.Probe(now),
		ProviderHeadroomPct: unknownProviderHeadroom,
	}
	cause, _, _, _ := dashboard.InferenceBudgetExceeded()
	obs.ProviderBudgetExceeded = cause != ""
	if r.headroom != nil {
		obs.ProviderHeadroomPct = lowestProviderHeadroom(r.headroom.HeadroomResponse())
	}
	lane := make(map[string]bool, len(names))
	for _, name := range names {
		lane[name] = true
	}
	for _, p := range plan.State.Pending {
		if lane[p.Agent] {
			obs.InFlight++
		}
	}
	for _, name := range names {
		rv := autoscale.Reviewer{Name: name, Index: cfg.Agents[name].ReplicaIndex}
		if proc, err := agentMgr.GetStatusFast(name); err == nil && proc != nil {
			rv.Busy = proc.State == crux.StateRunning && !proc.KickOutcome.Settled(proc.LastKick)
			rv.Saturated = proc.KicksUndeliverable > 0
		}
		for _, p := range plan.State.Pending {
			if p.Agent == name {
				rv.Busy = true
				break
			}
		}
		obs.Reviewers = append(obs.Reviewers, rv)
	}
	for _, w := range plan.Waiting {
		obs.WaitingKeys = append(obs.WaitingKeys, w.Key())
	}

	d := r.ctrl.Observe(obs)
	switch d.Action {
	case autoscale.ActionHold:
		logger.Debug(d.String(), "agent", agent)
	case autoscale.ActionBlocked:
		logger.Info(d.String(), "agent", agent, "backlog", len(obs.WaitingKeys))
	case autoscale.ActionDrain, autoscale.ActionUndrain:
		logger.Info(d.String(), "agent", agent)
	case autoscale.ActionScaleUp, autoscale.ActionRetire:
		logger.Info(d.String(), "agent", agent)
		if err := cfg.SetReviewerAutoscaleActive(d.To); err != nil {
			logger.Warn("reviewer autoscale: expanding replicas failed", "agent", agent, "target", d.To, "error", err)
			break
		}
		r.reconcile(ctx, cfg, agentMgr, gov, logger)
	}
	r.status = r.ctrl.Status()
}

// warnNotScaling logs, once per reason, that autoscale is switched on but
// resolves to no lane (config.ReviewerAutoscaleWarnings): a hive.yaml that
// names an agent the review swarm cannot dispatch to still loads, and this
// line is how the operator learns the reviewer count is not scaling.
func (r *reviewerAutoscaleRuntime) warnNotScaling(cfg *config.Config, logger *slog.Logger) {
	warning := ""
	if w := config.ReviewerAutoscaleWarnings(cfg); len(w) > 0 {
		warning = w[0]
	}
	if warning != "" && warning != r.warned {
		logger.Warn("reviewer autoscale: not scaling", "issue", warning)
	}
	r.warned = warning
}

// resetLane drops the controller's replica target and re-expands the agents,
// so the lane autoscale just left is back on its declared replicas and the
// lane it just entered (if any) is on its own declared count clamped into
// the band. It returns the replicas that no longer exist, sorted, and
// reconciles the agent manager and governor when the agent set changed. Like
// a dashboard replicas edit, it does not wait for a removed replica's turn
// to end; a review it held is re-dispatched once review.pending_ttl expires.
func (r *reviewerAutoscaleRuntime) resetLane(ctx context.Context, cfg *config.Config, agentMgr crux.Runtime, gov *governor.Governor, logger *slog.Logger) []string {
	before := make(map[string]bool, len(cfg.Agents))
	for name := range cfg.Agents {
		before[name] = true
	}
	if err := cfg.ResetReviewerAutoscaleActive(); err != nil {
		logger.Warn("reviewer autoscale: expanding replicas failed", "error", err)
		return nil
	}
	var retired []string
	for name := range before {
		if _, ok := cfg.Agents[name]; !ok {
			retired = append(retired, name)
		}
	}
	sort.Strings(retired)
	added := false
	for name := range cfg.Agents {
		if !before[name] {
			added = true
			break
		}
	}
	if len(retired) > 0 || added {
		r.reconcile(ctx, cfg, agentMgr, gov, logger)
	}
	return retired
}

// reconcile pushes the config's current agent set into the agent manager
// and governor. ReconcileAgents starts nothing itself and kills the tmux
// session of any replica no longer configured, so a retire needs no
// separate stop.
func (r *reviewerAutoscaleRuntime) reconcile(ctx context.Context, cfg *config.Config, agentMgr crux.Runtime, gov *governor.Governor, logger *slog.Logger) {
	if agentMgr == nil {
		return
	}
	enabled := cfg.EnabledAgents()
	for _, added := range agentMgr.ReconcileAgents(enabled) {
		if ac, ok := cfg.Agents[added]; ok && !ac.OnDemand {
			if err := agentMgr.Start(ctx, added); err != nil {
				logger.Warn("reviewer autoscale: failed to start reviewer", "agent", added, "error", err)
			}
		}
	}
	if gov != nil {
		gov.UpdateAgents(enabled)
	}
}

// lowestProviderHeadroom is the smallest remaining quota across providers
// with a reading, or unknownProviderHeadroom when none has one.
func lowestProviderHeadroom(resp rotation.HeadroomResponse) float64 {
	lowest := float64(unknownProviderHeadroom)
	for _, h := range resp.Providers {
		if h.CapturedAt.IsZero() && !h.Available {
			continue
		}
		pct := float64(h.PctRemaining)
		if lowest < 0 || pct < lowest {
			lowest = pct
		}
	}
	return lowest
}
