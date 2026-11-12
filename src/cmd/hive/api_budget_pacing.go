package main

import (
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// apiBudgetPacedEvalInterval composes API budget pacing
// (governor.api_budget_pacing) with the interval the loop would otherwise
// use: the result is never shorter than unpaced, and with the setting off it
// is unpaced exactly. The rule lives in pkg/github (apiBudgetPaceStep); this
// hands it the config and logs once per change of the held factor.
func apiBudgetPacedEvalInterval(cfg *config.Config, unpaced time.Duration, logger *slog.Logger) time.Duration {
	if cfg == nil {
		return unpaced
	}
	reserve := cfg.GitHub.APIReserve
	if reserve <= 0 {
		reserve = config.DefaultGitHubAPIReserve
	}
	in := github.APIBudgetPacingInput{
		Enabled:     cfg.Governor.APIBudgetPacing,
		Interval:    unpaced,
		MaxInterval: time.Duration(cfg.Governor.EvalIntervalMaxS) * time.Second,
		Reserve:     reserve,
	}
	state, changed := github.ObserveAPIBudgetPacing(in)
	if changed && logger != nil {
		logger.Info("github api budget pacing changed",
			"active", state.Active, "factor", state.Factor,
			"remaining", state.Remaining, "reset_in_s", int(state.ResetIn.Seconds()),
			"burn_per_min", state.BurnPerMin,
			"unpaced_interval_s", int(unpaced.Seconds()), "effective_interval_s", state.EffectiveIntervalS,
			"reason", state.Reason)
	}
	paced := time.Duration(state.EffectiveIntervalS) * time.Second
	if !in.Enabled || paced < unpaced {
		return unpaced
	}
	return paced
}

// apiBudgetPacedSweepInterval paces a sweep worker, which runs on its own
// interval and not on the eval tick: while pacing stretches the eval loop,
// the sweep's interval is stretched by the same ratio. With the setting off
// the sweep's interval is returned untouched.
func apiBudgetPacedSweepInterval(base func(*config.Config) time.Duration) func(*config.Config) time.Duration {
	return func(cfg *config.Config) time.Duration {
		d := base(cfg)
		if !cfg.Governor.APIBudgetPacing {
			return d
		}
		return apiBudgetStretchedSweepInterval(d, github.APIBudgetPacingStretch(), cfg.Governor.EvalIntervalMaxS)
	}
}

// apiBudgetStretchedSweepInterval stretches one sweep interval. It is never
// shorter than the sweep's own interval, and pacing never pushes it past
// governor.eval_interval_max_s (a sweep already slower than the cap keeps
// its own interval).
func apiBudgetStretchedSweepInterval(d time.Duration, stretch float64, maxSeconds int) time.Duration {
	if stretch <= 1 || d <= 0 {
		return d
	}
	paced := time.Duration(float64(d) * stretch).Round(time.Second)
	if limit := time.Duration(maxSeconds) * time.Second; limit > 0 && paced > limit {
		paced = limit
	}
	if paced < d {
		paced = d
	}
	return paced
}

// apiBudgetPacingStatus is the pacing block /api/status serves next to the
// eval interval and mode.
func apiBudgetPacingStatus() *github.APIBudgetPacing {
	state := github.APIBudgetPacingSnapshot()
	return &state
}
