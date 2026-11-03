package main

import (
	"context"
	"log/slog"
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
func (r *reviewerAutoscaleRuntime) Run(ctx context.Context, cfg *config.Config, plan review.DispatchPlan, agentMgr crux.Runtime, gov *governor.Governor, logger *slog.Logger, now time.Time) {
	if r == nil || cfg == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !cfg.ReviewerAutoscaleEnabled() {
		if r.ctrl != nil {
			r.ctrl = nil
			r.status = autoscale.Status{}
			if cfg.ReviewerAutoscaleActive != 0 {
				cfg.ReviewerAutoscaleActive = 0
				r.reconcile(ctx, cfg, agentMgr, gov, logger)
			}
			logger.Info("reviewer autoscale: disabled; lane back on declared replicas")
		}
		return
	}
	agent := cfg.ReviewerAutoscaleAgent()
	acfg := cfg.Review.Reviewers.Autoscale
	if r.ctrl == nil {
		r.ctrl = autoscale.New(acfg, agent)
		logger.Info("reviewer autoscale: enabled", "agent", agent, "min", acfg.EffectiveMin(), "max", acfg.EffectiveMax(), "target_backlog_per_reviewer", acfg.EffectiveTargetBacklogPerReviewer(), "cooldown", acfg.EffectiveCooldown().String())
	} else {
		r.ctrl.Reconfigure(acfg, agent)
	}
	names := cfg.ReviewerAutoscaleReplicas()
	if cfg.ReviewerAutoscaleActive == 0 {
		cfg.ReviewerAutoscaleActive = acfg.Clamp(len(names))
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

// reconcile pushes the config's current agent set into the agent manager
// and governor. ReconcileAgents starts nothing itself and kills the tmux
// session of any replica no longer configured, so a retire needs no
// separate stop.
func (r *reviewerAutoscaleRuntime) reconcile(ctx context.Context, cfg *config.Config, agentMgr crux.Runtime, gov *governor.Governor, logger *slog.Logger) {
	if agentMgr == nil {
		return
	}
	if cfg.ReviewerAutoscaleActive == 0 {
		if err := cfg.ExpandAgentReplicas(); err != nil {
			logger.Warn("reviewer autoscale: expanding replicas failed", "error", err)
			return
		}
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
