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
	// SkipUnstick covers the steps that unstick stuck items (sha_hold,
	// supersession sweep, shared-CI rerun, escalation). They are shed only in critical mode.
	SkipUnstick  bool
	SkippedSteps []string
}

// decideEvalBudgetWork tiers the optional steps: enrichment steps are skipped
// off-cadence or whenever the mode is not normal; unstick steps keep running
// through conserve and are skipped only in critical (#12005).
func decideEvalBudgetWork(mode github.APIBudgetMode, optionalEvery int, cycle uint64) evalBudgetDecision {
	if optionalEvery < 1 {
		optionalEvery = 1
	}
	runOptional := cycle == 0 || cycle%uint64(optionalEvery) == 0
	d := evalBudgetDecision{Mode: mode, Cycle: cycle, RunOptional: runOptional}
	if mode != github.APIBudgetNormal || !runOptional {
		d.SkipOptional = true
		d.SkippedSteps = append(d.SkippedSteps, github.APIBudgetEnrichmentSteps...)
	}
	if mode == github.APIBudgetCritical {
		d.SkipUnstick = true
		d.SkippedSteps = append(d.SkippedSteps, github.APIBudgetUnstickSteps...)
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
// API-budget stretch composed with the webhook-mode interval. It also
// publishes the webhook health window (2 × eval_interval_s) and logs once per
// cadence-mode transition (#13381): INFO "webhooks degraded; polling cadence
// restored" on webhook → polling and INFO "webhooks healthy; webhook cadence
// engaged" on polling → webhook.
func evalIntervalForConfig(cfg *config.Config, client *github.Client, logger *slog.Logger) time.Duration {
	interval, _ := evalIntervalAndModeForConfig(cfg, client, logger)
	return interval
}

// evalIntervalAndModeForConfig is evalIntervalForConfig plus the pacing mode
// /api/status reports ("webhook" while webhook mode sets the base tick).
//
// The mode is github.ObserveWebhookMode's state machine, not the raw health
// window: a quiet repository with no deliveries and no GitHub activity stays
// on the webhook cadence, while silence during activity (or deliveries
// failing verification) drops the cadence back to eval_interval_s, and the
// return requires github.WebhookRecoveryHealthyWindows consecutive healthy
// windows.
func evalIntervalAndModeForConfig(cfg *config.Config, client *github.Client, logger *slog.Logger) (time.Duration, string) {
	budget := apiBudgetIntervalForConfig(cfg, client)
	base, webhookS, maxS := 300, 900, 1800
	if cfg != nil {
		base, webhookS, maxS = cfg.Governor.EvalIntervalS, cfg.Governor.EvalIntervalWebhookS, cfg.Governor.EvalIntervalMaxS
		if base <= 0 {
			base = 300
		}
	}
	github.SetWebhookHealthWindow(2 * time.Duration(base) * time.Second)
	mode, degraded, recovered := github.ObserveWebhookMode()
	webhookCadence := mode.Mode == github.WebhookModeWebhook
	interval := webhookAwareEvalInterval(budget, webhookS, maxS, webhookCadence)
	if logger != nil && (degraded || recovered) {
		snap := github.WebhookHealthSnapshot()
		args := []any{"window_seconds", 2 * base, "eval_interval_s", base, "eval_interval_webhook_s", webhookS,
			"effective_interval_s", int(interval.Seconds()), "webhooks_healthy", snap.Healthy}
		if snap.LastEventAt != nil {
			args = append(args, "last_event_at", snap.LastEventAt.Format(time.RFC3339))
		}
		if degraded {
			logger.Info("webhooks degraded; polling cadence restored", append(args, "reason", mode.Reason,
				"recovery_healthy_windows", github.WebhookRecoveryHealthyWindows)...)
		} else {
			logger.Info("webhooks healthy; webhook cadence engaged", args...)
		}
	}
	return interval, evalModeForInterval(webhookCadence, webhookS)
}
