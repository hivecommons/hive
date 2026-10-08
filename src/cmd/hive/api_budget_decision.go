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
	if cfg == nil {
		return apiBudgetEffectiveEvalInterval(300, 1800, 2, apiBudgetModeForClient(client))
	}
	return apiBudgetEffectiveEvalInterval(cfg.Governor.EvalIntervalS, cfg.Governor.EvalIntervalMaxS, cfg.Governor.ConserveIntervalMultiplier, apiBudgetModeForClient(client))
}
