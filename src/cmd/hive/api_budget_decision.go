package main

import (
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type evalBudgetDecision struct {
	Mode         github.APIBudgetMode
	Cycle        uint64
	RunOptional  bool
	SkipOptional bool
	SkippedSteps []string
}

var optionalEvalSteps = []string{
	"advisory_ensure",
	"recommendations",
	"duplicate_guard",
	"escalation",
	"review_enrichment",
	"review_threads",
	"sha_hold",
	"supersession_sweep",
	"closing_keyword_post_check",
	"reporter_trust",
	"attribution_collectors",
	"cost_collectors",
	"pr_issue_counts_collectors",
	"bead_synth",
}

func decideEvalBudgetWork(mode github.APIBudgetMode, optionalEvery int, cycle uint64) evalBudgetDecision {
	if optionalEvery < 1 {
		optionalEvery = 1
	}
	runOptional := cycle == 0 || cycle%uint64(optionalEvery) == 0
	d := evalBudgetDecision{Mode: mode, Cycle: cycle, RunOptional: runOptional}
	if mode != github.APIBudgetNormal || !runOptional {
		d.SkipOptional = true
		d.SkippedSteps = append(d.SkippedSteps, optionalEvalSteps...)
	}
	return d
}

func apiBudgetEffectiveEvalInterval(baseSeconds, maxSeconds, conserveMultiplier int, mode github.APIBudgetMode) time.Duration {
	if baseSeconds <= 0 {
		baseSeconds = 300
	}
	multiplier := 1
	switch mode {
	case github.APIBudgetCritical:
		multiplier = 4
	case github.APIBudgetConserve:
		if conserveMultiplier < 1 {
			conserveMultiplier = 2
		}
		multiplier = conserveMultiplier
	}
	effective := baseSeconds * multiplier
	if maxSeconds > 0 && effective > maxSeconds {
		effective = maxSeconds
	}
	return time.Duration(effective) * time.Second
}

func apiBudgetModeForClient(client *github.Client) github.APIBudgetMode {
	if client == nil {
		return github.APIBudgetNormal
	}
	mode, _ := client.APIBudgetMode()
	return mode
}

func apiBudgetIntervalForConfig(cfg *config.Config, client *github.Client) time.Duration {
	if cfg == nil {
		return apiBudgetEffectiveEvalInterval(300, 1800, 2, apiBudgetModeForClient(client))
	}
	return apiBudgetEffectiveEvalInterval(cfg.Governor.EvalIntervalS, cfg.Governor.EvalIntervalMaxS, cfg.Governor.ConserveIntervalMultiplier, apiBudgetModeForClient(client))
}

// webhookAwareEvalInterval composes the API-budget stretch with the webhook
// interval (#11177): while webhooks are healthy the base interval rises to
// governor.eval_interval_webhook_s, and the larger of that and the budget
// stretch wins. governor.eval_interval_max_s caps the webhook interval too.
func webhookAwareEvalInterval(budgetInterval time.Duration, webhookSeconds, maxSeconds int, webhooksHealthy bool) time.Duration {
	if !webhooksHealthy || webhookSeconds <= 0 {
		return budgetInterval
	}
	if maxSeconds > 0 && webhookSeconds > maxSeconds {
		webhookSeconds = maxSeconds
	}
	if webhook := time.Duration(webhookSeconds) * time.Second; webhook > budgetInterval {
		return webhook
	}
	return budgetInterval
}

// evalIntervalForConfig is the governor loop's effective eval interval: the
// API-budget stretch composed with the webhook-healthy interval. It also
// publishes the webhook health window (2 × eval_interval_s) and WARNs once per
// healthy → stale transition.
func evalIntervalForConfig(cfg *config.Config, client *github.Client, logger *slog.Logger) time.Duration {
	budget := apiBudgetIntervalForConfig(cfg, client)
	base, webhookS, maxS := 300, 900, 1800
	if cfg != nil {
		base, webhookS, maxS = cfg.Governor.EvalIntervalS, cfg.Governor.EvalIntervalWebhookS, cfg.Governor.EvalIntervalMaxS
		if base <= 0 {
			base = 300
		}
	}
	github.SetWebhookHealthWindow(2 * time.Duration(base) * time.Second)
	healthy, becameStale := github.ObserveWebhookHealth()
	if becameStale && logger != nil {
		snap := github.WebhookHealthSnapshot()
		args := []any{"window_seconds", 2 * base, "eval_interval_s", base}
		if snap.LastEventAt != nil {
			args = append(args, "last_event_at", snap.LastEventAt.Format(time.RFC3339))
		}
		logger.Warn("github webhooks went stale; falling back to the configured eval interval and TTL-bound PR cache", args...)
	}
	return webhookAwareEvalInterval(budget, webhookS, maxS, healthy)
}
