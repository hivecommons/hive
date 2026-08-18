package trajectory

import (
	"context"
	"log/slog"
	"time"
)

// AgentView is the minimal read model the lane needs for one running agent.
type AgentView struct {
	Name       string
	Running    bool
	Intent     string // the agent's last kick message (its assigned intent)
	Transcript string // recent transcript tail
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
}

// LaneConfig configures the driver.
type LaneConfig struct {
	IntervalS    int
	OnDivergence string
	ExemptAgents []string
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
	return &Lane{
		reviewer:     reviewer,
		src:          src,
		sink:         sink,
		logger:       logger,
		interval:     time.Duration(intervalS) * time.Second,
		onDivergence: onDiv,
		exempt:       exempt,
	}
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
	l.lastRun = time.Now()
	agents := l.src.TrajectoryAgents()
	for _, a := range agents {
		if !a.Running || l.exempt[a.Name] {
			continue
		}
		if a.Intent == "" || a.Transcript == "" {
			continue // nothing to compare yet
		}
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
