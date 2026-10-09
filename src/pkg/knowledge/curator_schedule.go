package knowledge

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Layer defaults for the scheduled promotion sweep. project→org is the
// promotion documented in hive.yaml.example ("Confidence to auto-promote
// project→org") and is the only pair the shipped example config implies.
const (
	defaultPromoteFromLayer = LayerProject
	defaultPromoteToLayer   = LayerOrg
)

// PromotionScheduler drives Promoter.AutoPromoteCandidates on the cadence set
// by knowledge.curator.schedule.
//
// It exists because that schedule field was parsed, defaulted and never read
// (#5430). Wiring it naively would have been a behaviour change for every
// existing deployment, since applyDefaults stamped "daily" onto any hive that
// omitted the key — so the scheduler is gated on CuratorConfig.IsEnabled(),
// which is false unless an operator writes `enabled: true`. A configured
// schedule on a hive that never opted in still does nothing, by design.
//
// The scheduler adds no promotion semantics: it decides WHEN to sweep and
// logs WHAT was swept. Which facts qualify remains entirely
// AutoPromoteCandidates' business, including the AutoPromoteThreshold gate.
type PromotionScheduler struct {
	promoter *Promoter
	config   CuratorConfig
	logger   *slog.Logger

	from LayerType
	to   LayerType

	mu         sync.Mutex
	cancel     context.CancelFunc
	running    bool
	onPromoted func(promoted int)
}

// OnPromoted registers fn to run after every sweep that promoted at least one
// fact (for example to trigger the knowledge publish mirror). A nil fn clears
// the hook.
func (s *PromotionScheduler) OnPromoted(fn func(promoted int)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onPromoted = fn
}

// NewPromotionScheduler builds a scheduler over an existing promoter. It never
// starts anything on its own — callers must invoke Start/StartBackground, and
// those are no-ops unless the config opts in.
func NewPromotionScheduler(promoter *Promoter, config CuratorConfig, logger *slog.Logger) *PromotionScheduler {
	from := LayerType(config.PromoteFrom)
	if from == "" {
		from = defaultPromoteFromLayer
	}
	to := LayerType(config.PromoteTo)
	if to == "" {
		to = defaultPromoteToLayer
	}
	return &PromotionScheduler{
		promoter: promoter,
		config:   config,
		logger:   logger,
		from:     from,
		to:       to,
	}
}

// Interval returns the tick period for the configured schedule. It reuses
// ParseSynthSchedule rather than introducing a second parser: the curator's
// documented vocabulary is "daily" (defaultCuratorSchedule) and the
// synthesizer's parser already maps daily/hourly onto named hour constants, so
// the two agree with no new values needed.
func (s *PromotionScheduler) Interval() time.Duration {
	return ParseSynthSchedule(s.config.Schedule)
}

// Start runs the promotion loop until ctx is cancelled. If the curator is not
// explicitly enabled it returns immediately, having done no work and started
// no ticker — the disabled path costs nothing per tick because there is no
// tick.
func (s *PromotionScheduler) Start(ctx context.Context) {
	if !s.config.IsEnabled() {
		// Only worth a line when an operator set a cadence that will not run;
		// silence would reproduce exactly the confusion #5430 describes.
		if s.config.Schedule != "" && s.logger != nil {
			s.logger.Info("scheduled knowledge promotion is disabled; configured schedule will not run",
				"schedule", s.config.Schedule,
				"hint", "set knowledge.curator.enabled: true to opt in",
			)
		}
		return
	}
	if s.promoter == nil {
		return
	}

	interval := s.Interval()
	s.logger.Info("scheduled knowledge promotion started",
		"schedule", s.config.Schedule,
		"interval", interval,
		"from", s.from,
		"to", s.to,
		"threshold", s.config.AutoPromoteThreshold,
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.RunOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce performs a single promotion sweep. It is exported so the dashboard
// and tests can trigger a sweep without waiting out an interval; it still
// honours the enable gate, so it cannot be used to bypass opt-in.
func (s *PromotionScheduler) RunOnce(ctx context.Context) {
	if !s.config.IsEnabled() || s.promoter == nil {
		return
	}

	candidates, err := s.promoter.AutoPromoteCandidates(ctx, s.from, s.to)
	if err != nil {
		s.logger.Warn("scheduled promotion: listing candidates failed",
			"from", s.from, "to", s.to, "error", err)
		return
	}
	if len(candidates) == 0 {
		return
	}

	var promoted, failed int
	for _, req := range candidates {
		select {
		case <-ctx.Done():
			return
		default:
		}
		result := s.promoter.Promote(ctx, req)
		if result.Success {
			promoted++
			// Per-fact INFO: a background job that writes into a knowledge
			// layer must name what it wrote and where it came from, or an
			// operator has no way to audit or undo it.
			s.logger.Info("scheduled promotion: fact promoted",
				"slug", req.Slug, "from", req.FromLayer, "to", req.ToLayer, "reason", req.Reason)
			continue
		}
		failed++
		s.logger.Warn("scheduled promotion: fact promotion failed",
			"slug", req.Slug, "from", req.FromLayer, "to", req.ToLayer, "error", result.Error)
	}

	s.logger.Info("scheduled promotion sweep complete",
		"from", s.from, "to", s.to,
		"candidates", len(candidates), "promoted", promoted, "failed", failed,
		"threshold", s.config.AutoPromoteThreshold,
	)
	if promoted > 0 {
		s.mu.Lock()
		hook := s.onPromoted
		s.mu.Unlock()
		if hook != nil {
			hook(promoted)
		}
	}
}

// StartBackground launches the loop in a goroutine, tracking it so it can be
// stopped and restarted. It is a no-op when the curator is not enabled.
func (s *PromotionScheduler) StartBackground(parent context.Context) {
	if !s.config.IsEnabled() {
		s.Start(parent) // emits the disabled-with-schedule notice, returns
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.running = true
	go func() {
		s.Start(ctx)
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
}

// Stop cancels the background loop.
func (s *PromotionScheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// IsRunning reports whether the background loop is active.
func (s *PromotionScheduler) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
