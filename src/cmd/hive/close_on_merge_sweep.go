package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// closeOnMergeScheduler decides, per guard pass, how much close-on-merge
// work to do. The full ledger walk and the open-issue backfill each run on
// their own interval; every other pass only looks at claims whose PR merged
// since the last full walk, so a fresh merge still closes its issue promptly
// while the ~thousand settled claims in a mature ledger cost nothing. Both
// are skipped while the API budget governor is not normal.
type closeOnMergeScheduler struct {
	mu  sync.Mutex
	now func() time.Time
	// budgetMode reads the API budget governor; overridable for tests.
	budgetMode   func(*github.Client) github.APIBudgetMode
	lastSweep    time.Time
	lastBackfill time.Time
	// budgetHeld is true while the last pass was skipped for budget, so the
	// skip and the resume are each logged once.
	budgetHeld bool
}

var closeOnMergeSched = newCloseOnMergeScheduler(time.Now)

func newCloseOnMergeScheduler(now func() time.Time) *closeOnMergeScheduler {
	if now == nil {
		now = time.Now
	}
	return &closeOnMergeScheduler{now: now, budgetMode: apiBudgetModeForClient}
}

// closeOnMergePlan is one pass's work.
type closeOnMergePlan struct {
	// Skip is set when the API budget is not normal; nothing runs.
	Skip bool
	// BudgetChanged is set on the pass where Skip flipped, for one log line.
	BudgetChanged bool
	Mode          github.APIBudgetMode
	// FullSweep walks every ledger claim; otherwise MergedAfter limits the
	// walk to claims merged since the last full sweep.
	FullSweep   bool
	MergedAfter time.Time
	Backfill    bool
}

func (s *closeOnMergeScheduler) plan(mode github.APIBudgetMode, sweepEvery, backfillEvery time.Duration) closeOnMergePlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	p := closeOnMergePlan{Mode: mode}
	if mode != github.APIBudgetNormal {
		p.Skip = true
		p.BudgetChanged = !s.budgetHeld
		s.budgetHeld = true
		return p
	}
	p.BudgetChanged = s.budgetHeld
	s.budgetHeld = false
	if s.lastSweep.IsZero() || now.Sub(s.lastSweep) >= sweepEvery {
		p.FullSweep = true
		s.lastSweep = now
	} else {
		p.MergedAfter = s.lastSweep
	}
	if s.lastBackfill.IsZero() || now.Sub(s.lastBackfill) >= backfillEvery {
		p.Backfill = true
		s.lastBackfill = now
	}
	return p
}

func (s *closeOnMergeScheduler) clock() func() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.now == nil {
		return time.Now
	}
	return s.now
}

func (s *closeOnMergeScheduler) mode(client *github.Client) github.APIBudgetMode {
	s.mu.Lock()
	fn := s.budgetMode
	s.mu.Unlock()
	if fn == nil {
		return apiBudgetModeForClient(client)
	}
	return fn(client)
}

// closeOnMergeOptionsFor is the policy both the scheduled sweep and the
// webhook-triggered one (closeOnMergeForDelivery) close issues under.
func closeOnMergeOptionsFor(cfg *config.Config, logger *slog.Logger, now func() time.Time) github.CloseOnMergeOptions {
	return github.CloseOnMergeOptions{
		Identity: hiveIdentity(cfg),
		Logger:   logger,
		TrustedAuthor: func(repo, login string) bool {
			decision := trustedAuthorFunc(cfg)(login, cfg.AutoMerge.TrustedAuthors.EffectiveRequireRole())
			return decision.Allowed
		},
		RecheckAfter:         cfg.Issues.EffectiveCloseOnMergeRecheck(),
		Now:                  now,
		LabelDriven:          cfg.RepoLabelDriven,
		RepoOnGitHub:         cfg.RepoOnGitHub,
		AdvisoryAutocloseOff: !cfg.Dedupe.AdvisoryAutocloseEnabled(),
	}
}

func runCloseOnMergeSweep(ctx context.Context, cfg *config.Config, ghClient *github.Client, ledger *github.ClaimLedger, logger *slog.Logger) {
	runCloseOnMergeSweepWith(ctx, cfg, ghClient, ledger, logger, closeOnMergeSched)
}

func runCloseOnMergeSweepWith(ctx context.Context, cfg *config.Config, ghClient *github.Client, ledger *github.ClaimLedger, logger *slog.Logger, sched *closeOnMergeScheduler) {
	if cfg == nil || ghClient == nil || ledger == nil || sched == nil || !cfg.Issues.CloseOnMergeEnabled() {
		return
	}
	plan := sched.plan(sched.mode(ghClient), cfg.Issues.EffectiveCloseOnMergeInterval(), cfg.Issues.EffectiveCloseOnMergeBackfillInterval())
	if plan.Skip {
		if plan.BudgetChanged && logger != nil {
			logger.Info("close-on-merge: sweeps paused while API budget is constrained", "budget_mode", plan.Mode.String())
		}
		return
	}
	if plan.BudgetChanged && logger != nil {
		logger.Info("close-on-merge: sweeps resumed", "budget_mode", plan.Mode.String())
	}
	opts := closeOnMergeOptionsFor(cfg, logger, sched.clock())
	opts.MergedAfter = plan.MergedAfter
	sweep := ghClient.CloseOnMergeLedgerSweep(ctx, ledger, opts)
	msg := "close-on-merge ledger sweep"
	if !plan.FullSweep {
		msg = "close-on-merge fresh-merge sweep"
	}
	logCloseOnMergeResults(logger, msg, sweep.Results)
	if plan.FullSweep && logger != nil && (sweep.SkippedSettled > 0 || sweep.Marked > 0) {
		logger.Debug(msg+" settled markers", "skipped_settled", sweep.SkippedSettled, "marked", sweep.Marked, "checked", len(sweep.Results))
	}
	if !plan.Backfill {
		return
	}
	backfill := ghClient.CloseOnMergeBackfill(ctx, opts)
	logCloseOnMergeResults(logger, "close-on-merge backfill sweep", backfill)
}
