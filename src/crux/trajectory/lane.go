package trajectory

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// AgentView is the minimal read model the lane needs for one running agent.
type AgentView struct {
	Name       string
	Running    bool
	Intent     string // the agent's last kick message (its assigned intent)
	Transcript string // recent transcript tail
	// Idle is the manager's own verdict that the agent has no turn in
	// progress — its last kicked turn has ended, or it is parked at the
	// prompt waiting on CI. It is derived from agent/governor state, never
	// from transcript content: a stalled transcript on an agent whose turn
	// is still running is a warning signal, not idleness (ADR-0009).
	Idle bool
	// LastOutputAt is when the agent's pane content last changed, as seen by
	// the pane poller. Zero means unknown, which the lane treats as "may
	// have new output" — it never skips on an unproven signal.
	LastOutputAt time.Time
}

// DefaultMaxSkipIntervals bounds how many consecutive review intervals an
// idle agent may go unreviewed. After this many intervals the review is
// overdue and runs regardless of idle state, so even a long-idle agent is
// re-read at least every N intervals (hivecommons/hive#12132).
const DefaultMaxSkipIntervals = 6

// Skip and run reasons recorded per agent on every lane pass.
const (
	ReasonFirstReview = "first-review"
	ReasonOverdue     = "overdue"
	ReasonActive      = "active"
	ReasonNewOutput   = "new-output"
	ReasonIdle        = "idle-no-new-output"
)

// AgentReviewState is the lane's per-agent bookkeeping, exposed for the
// dashboard trajectory panel.
type AgentReviewState struct {
	LastReviewed time.Time `json:"last_reviewed"`
	LastDecision string    `json:"last_decision"` // "reviewed" or "skipped"
	LastReason   string    `json:"last_reason"`
	Skipped      int       `json:"skipped"` // consecutive skips since the last review
}

// Stats are the lane's cumulative counters.
type Stats struct {
	Runs            int                         `json:"runs"`
	Reviewed        int                         `json:"reviewed"`
	Skipped         int                         `json:"skipped"`
	SkippedByReason map[string]int              `json:"skipped_by_reason"`
	RanByReason     map[string]int              `json:"ran_by_reason"`
	Agents          map[string]AgentReviewState `json:"agents"`
	LastRun         time.Time                   `json:"last_run"`
}

// AgentSource supplies the running agents to review and the actions the lane
// can take. The manager implements this; keeping it an interface keeps the
// package testable and free of an import cycle.
type AgentSource interface {
	// TrajectoryAgents returns a snapshot of currently-running agents with
	// their intent and transcript. Paused/stopped agents are omitted.
	TrajectoryAgents() []AgentView
	// Pause stops an agent with the given trigger/reason (same semantics as a
	// dashboard pause).
	Pause(name, trigger, reason string) error
}

// Sink records the lane's decisions. The dashboard implements this (audit log,
// system alert, notification). All methods must be safe to call from the
// governor goroutine.
type Sink interface {
	Audit(agent, action, detail string)
	Alert(id, severity, message string)
	Notify(title, message string)
}

// Lane runs the periodic review over all running agents.
type Lane struct {
	reviewer     *Reviewer
	src          AgentSource
	sink         Sink
	logger       *slog.Logger
	interval     time.Duration
	onDivergence string
	exempt       map[string]bool
	lastRun      time.Time
	maxSkip      time.Duration

	statsMu sync.Mutex
	stats   Stats
}

// LaneConfig configures the driver.
type LaneConfig struct {
	IntervalS    int
	OnDivergence string
	ExemptAgents []string
	// MaxSkipIntervals is how many review intervals an idle agent may be
	// skipped before a review is overdue; <=0 uses DefaultMaxSkipIntervals.
	MaxSkipIntervals int
}

// NewLane builds a review-lane driver. The reviewer must be non-nil.
func NewLane(reviewer *Reviewer, src AgentSource, sink Sink, cfg LaneConfig, logger *slog.Logger) *Lane {
	intervalS := cfg.IntervalS
	if intervalS <= 0 {
		intervalS = DefaultIntervalS
	}
	onDiv := cfg.OnDivergence
	if onDiv != OnDivergenceAlert {
		onDiv = OnDivergencePause // default and fallback
	}
	exempt := make(map[string]bool, len(cfg.ExemptAgents))
	for _, a := range cfg.ExemptAgents {
		exempt[a] = true
	}
	maxSkip := cfg.MaxSkipIntervals
	if maxSkip <= 0 {
		maxSkip = DefaultMaxSkipIntervals
	}
	interval := time.Duration(intervalS) * time.Second
	return &Lane{
		reviewer:     reviewer,
		src:          src,
		sink:         sink,
		logger:       logger,
		interval:     interval,
		onDivergence: onDiv,
		exempt:       exempt,
		maxSkip:      interval * time.Duration(maxSkip),
		stats: Stats{
			SkippedByReason: map[string]int{},
			RanByReason:     map[string]int{},
			Agents:          map[string]AgentReviewState{},
		},
	}
}

// Stats returns a copy of the lane's counters. Safe to call from any
// goroutine (the dashboard reads it while the governor runs the lane).
func (l *Lane) Stats() Stats {
	if l == nil {
		return Stats{}
	}
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	out := l.stats
	out.SkippedByReason = make(map[string]int, len(l.stats.SkippedByReason))
	for k, v := range l.stats.SkippedByReason {
		out.SkippedByReason[k] = v
	}
	out.RanByReason = make(map[string]int, len(l.stats.RanByReason))
	for k, v := range l.stats.RanByReason {
		out.RanByReason[k] = v
	}
	out.Agents = make(map[string]AgentReviewState, len(l.stats.Agents))
	for k, v := range l.stats.Agents {
		out.Agents[k] = v
	}
	return out
}

// skipDecision is the review-skip predicate (hivecommons/hive#12132). It
// returns skip=true only when ALL of the following hold:
//
//   - the agent has been reviewed before (a never-reviewed agent always runs);
//   - the review is not overdue (now - lastReviewed < maxSkip);
//   - the manager reports the agent idle (no turn in progress);
//   - the manager has a known last-output time and it precedes the last review.
//
// It deliberately never looks at the transcript. A byte-identical tail on an
// agent that is still mid-turn is a stall — exactly what the reviewer should
// see — so "transcript unchanged" is not evidence of idleness (ADR-0009
// addendum).
func skipDecision(a AgentView, st AgentReviewState, now time.Time, maxSkip time.Duration) (skip bool, reason string) {
	if st.LastReviewed.IsZero() {
		return false, ReasonFirstReview
	}
	if maxSkip > 0 && now.Sub(st.LastReviewed) >= maxSkip {
		return false, ReasonOverdue
	}
	if !a.Idle {
		return false, ReasonActive
	}
	if a.LastOutputAt.IsZero() || a.LastOutputAt.After(st.LastReviewed) {
		return false, ReasonNewOutput
	}
	return true, ReasonIdle
}

func (l *Lane) recordSkip(name, reason string) {
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	l.stats.Skipped++
	l.stats.SkippedByReason[reason]++
	st := l.stats.Agents[name]
	st.LastDecision = "skipped"
	st.LastReason = reason
	st.Skipped++
	l.stats.Agents[name] = st
}

func (l *Lane) recordReview(name, reason string, now time.Time) {
	l.statsMu.Lock()
	defer l.statsMu.Unlock()
	l.stats.Reviewed++
	l.stats.RanByReason[reason]++
	st := l.stats.Agents[name]
	st.LastReviewed = now
	st.LastDecision = "reviewed"
	st.LastReason = reason
	st.Skipped = 0
	l.stats.Agents[name] = st
}

// Due reports whether enough time has elapsed since the last run for the lane
// to run again at time now. The caller (governor tick) gates on this so the
// lane's cadence is independent of, but floored by, the governor interval.
func (l *Lane) Due(now time.Time) bool {
	return l.lastRun.IsZero() || now.Sub(l.lastRun) >= l.interval
}

// Run reviews every eligible running agent once and acts on divergent
// verdicts. It is synchronous and bounded (each review has its own timeout);
// the caller runs it from the governor goroutine. Errors from individual
// reviews are logged and skipped — one bad review never blocks the rest.
func (l *Lane) Run(ctx context.Context) {
	l.runAt(ctx, time.Now())
}

func (l *Lane) runAt(ctx context.Context, now time.Time) {
	l.lastRun = now
	l.statsMu.Lock()
	l.stats.Runs++
	l.stats.LastRun = now
	l.statsMu.Unlock()
	agents := l.src.TrajectoryAgents()
	for _, a := range agents {
		if !a.Running || l.exempt[a.Name] {
			continue
		}
		if a.Intent == "" || a.Transcript == "" {
			continue // nothing to compare yet
		}
		l.statsMu.Lock()
		st := l.stats.Agents[a.Name]
		l.statsMu.Unlock()
		skip, reason := skipDecision(a, st, now, l.maxSkip)
		if skip {
			l.recordSkip(a.Name, reason)
			l.logger.Info("trajectory review skipped", "agent", a.Name, "reason", reason,
				"since_last_review", now.Sub(st.LastReviewed).Round(time.Second).String(),
				"consecutive_skips", st.Skipped+1)
			continue
		}
		l.recordReview(a.Name, reason, now)
		l.logger.Debug("trajectory review running", "agent", a.Name, "reason", reason)
		verdict, err := l.reviewer.Review(ctx, a.Name, a.Intent, a.Transcript)
		if err != nil {
			// Fail open: a reviewer outage must not pause working agents.
			l.logger.Warn("trajectory review error", "agent", a.Name, "err", err.Error())
			continue
		}
		if !verdict.Divergent {
			continue
		}
		l.act(a.Name, verdict)
	}
}

// act applies the configured response to a divergent verdict.
func (l *Lane) act(agent string, v Verdict) {
	detail := "confidence=" + formatConfidence(v.Confidence) + " reason=" + sanitize(v.Reason)
	alertID := "trajectory-" + agent
	if l.onDivergence == OnDivergencePause {
		if err := l.src.Pause(agent, "trajectory-review", v.Reason); err != nil {
			l.logger.Warn("trajectory pause failed", "agent", agent, "err", err.Error())
			// Fall through to alert/notify so the divergence is still surfaced.
		}
		l.sink.Audit(agent, "trajectory-pause", detail)
		l.sink.Alert(alertID, "error", "Agent "+agent+" paused by trajectory review: "+sanitize(v.Reason))
		l.sink.Notify("Trajectory divergence — agent paused", agent+": "+sanitize(v.Reason))
		l.logger.Warn("audit: trajectory divergence — agent paused",
			"agent", agent, "confidence", v.Confidence, "reason", v.Reason)
		return
	}
	// alert-only
	l.sink.Audit(agent, "trajectory-alert", detail)
	l.sink.Alert(alertID, "warning", "Trajectory review flagged "+agent+": "+sanitize(v.Reason))
	l.sink.Notify("Trajectory divergence flagged", agent+": "+sanitize(v.Reason))
	l.logger.Warn("audit: trajectory divergence — flagged (alert-only)",
		"agent", agent, "confidence", v.Confidence, "reason", v.Reason)
}

// formatConfidence renders a [0,1] confidence as a short fixed string.
func formatConfidence(c float64) string {
	// one decimal is plenty for an audit detail
	pct := int(c*100 + 0.5)
	return itoa(pct) + "%"
}

// itoa avoids importing strconv for one small use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// sanitize collapses newlines so a reviewer reason can't break audit-line or
// alert formatting.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			out = append(out, ' ')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
