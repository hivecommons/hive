package main

import (
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
	base, maxSeconds, mult := 300, 1800, 2
	webhookSeconds := 900
	if cfg != nil {
		base = cfg.Governor.EvalIntervalS
		maxSeconds = cfg.Governor.EvalIntervalMaxS
		mult = cfg.Governor.ConserveIntervalMultiplier
		webhookSeconds = cfg.Governor.EvalIntervalWebhookS
	}
	mode := apiBudgetModeForClient(client)
	budget := apiBudgetEffectiveEvalInterval(base, maxSeconds, mult, mode)
	if client == nil {
		return budget
	}
	healthAtBase := time.Duration(base) * time.Second
	if healthAtBase <= 0 {
		healthAtBase = 300 * time.Second
	}
	if client.WebhookHealth(healthAtBase).Healthy {
		webhookBase := base
		if webhookSeconds > webhookBase {
			webhookBase = webhookSeconds
		}
		webhookInterval := time.Duration(webhookBase) * time.Second
		if maxSeconds > 0 && webhookInterval > time.Duration(maxSeconds)*time.Second {
			webhookInterval = time.Duration(maxSeconds) * time.Second
		}
		if webhookInterval > budget {
			client.WebhookHealth(webhookInterval)
			return webhookInterval
		}
	}
	client.WebhookHealth(budget)
	return budget
}
