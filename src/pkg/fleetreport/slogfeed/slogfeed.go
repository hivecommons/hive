// Package slogfeed tees WARN and ERROR log records into the fleet-report
// runtime tracker as fleetreport.ClassUnclassifiedRepeated observations, so a
// log line that keeps repeating becomes a self-report even when no hook site
// was ever written for it. The incident that motivated it: a relay watcher
// retried eleven malformed request files for six hours, logging the same
// warning 175 times, and the self-report system — an allow-list fed only by
// hand-placed Observe calls — never noticed.
//
// The handler wraps the process's root slog.Handler. It never blocks or
// fails logging: the wrapped handler runs first and its error is returned
// unchanged; the observation is a bounded, lock-free-on-the-hot-path extra
// that only runs for WARN and above.
package slogfeed

import (
	"context"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

const (
	// maxKeyBytes bounds the normalised message used as the condition key so
	// a pathological log line cannot bloat the tracker.
	maxKeyBytes = 200
	// maxDiagnosticsBytes bounds the attribute excerpt attached to the
	// observation; the tracker scrubs and bounds again on storage.
	//
	// Sized to outlive agent.manager's "pane content at timeout" WARN
	// (src/crux/agent/manager.go), whose head_500/tail_500 attrs are each up
	// to 500 runes and can run ~1500 bytes apiece when the pane is full of
	// multi-byte box-drawing characters (❯, ╰─, ...). diagnostics below
	// writes attrs in record order and stops at the first one that would
	// cross this cap, so a value too small for head_500 alone drops tail_500
	// — the pane's CURRENT state, and the only field useful for diagnosing a
	// stuck CLI — every single time. That is exactly what happened at 512
	// bytes: hivecommons/hive#14586 and #15311 both self-reported this WARN
	// with no usable excerpt attached. 4096 comfortably fits both fields plus
	// the preceding flags with room to spare, while staying well under the
	// tracker's own 8KiB storage bound.
	maxDiagnosticsBytes = 4096
	// minLevel is the lowest level the feed observes.
	minLevel = slog.LevelWarn
)

// Observer is the tracker entry point the feed reports into; it matches
// fleetreport.Observe.
type Observer func(class, key, message, diagnostics string)

// Handler is a slog.Handler that forwards every record to the wrapped handler
// and additionally observes WARN+ records on the fleet-report tracker.
type Handler struct {
	next    slog.Handler
	observe Observer
	// attrs are the attributes bound via WithAttrs, so a logger built as
	// logger.With("agent", name) still contributes them to diagnostics.
	attrs []slog.Attr
}

// New wraps next so WARN+ records are observed on the default tracker.
func New(next slog.Handler) *Handler {
	return NewWithObserver(next, fleetreport.Observe)
}

// NewWithObserver is New with an explicit observer (tests).
func NewWithObserver(next slog.Handler, observe Observer) *Handler {
	if observe == nil {
		observe = fleetreport.Observe
	}
	return &Handler{next: next, observe: observe}
}

// Enabled defers to the wrapped handler.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle forwards the record, then observes it when it is WARN or above.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	err := h.next.Handle(ctx, r)
	if r.Level >= minLevel {
		h.feed(r)
	}
	return err
}

// WithAttrs returns a handler that carries attrs on every record.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &Handler{next: h.next.WithAttrs(attrs), observe: h.observe, attrs: merged}
}

// WithGroup returns a handler that groups subsequent attributes.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), observe: h.observe, attrs: h.attrs}
}

// Key is the condition key the feed derives from a log message: the
// normalised message (numbers, hashes, paths and quoted strings collapsed),
// bounded. Empty when the message should not be observed at all.
func Key(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" || fleetreport.IsBenignMessage(msg) || fleetreport.MatchesExplicitClass(msg) {
		return ""
	}
	key := fleetreport.NormalizeMessage(msg)
	if len(key) > maxKeyBytes {
		key = key[:maxKeyBytes]
	}
	return key
}

func (h *Handler) feed(r slog.Record) {
	if expectedKickHold(r) || rateLimitedError(r) || sentinelPacingSkip(r) || upstreamUnavailableError(r) || failClosedStatus500(r) || hubHeartbeatUnavailable(r) || hubHeartbeatCachedFallback(r) || reviewCommentCapped(r) || inferenceBlockedStall(r) || automergeMergeGuardSkip(r) {
		return
	}
	class := fleetreport.ClassUnclassifiedRepeated
	key := Key(r.Message)
	if key == "" {
		msg := strings.TrimSpace(r.Message)
		if fleetreport.IsBenignMessage(msg) {
			return
		}
		if class = fleetreport.LogFedClass(msg); class == "" {
			return
		}
		key = fleetreport.NormalizeMessage(msg)
		if len(key) > maxKeyBytes {
			key = key[:maxKeyBytes]
		}
	}
	h.observe(class, key, r.Message, diagnostics(r, h.attrs))
}

// expectedKickHold reports whether a "failed to send kick" record carries the
// deliberate post-restart kick cooldown refusal (#7363). The hold is the
// breaker working as designed, not a repeated runtime defect (#14422); other
// kick send failures stay reportable.
func expectedKickHold(r slog.Record) bool {
	if strings.TrimSpace(r.Message) != "failed to send kick" {
		return false
	}
	held := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" && strings.Contains(a.Value.String(), "kick held after restart") {
			held = true
			return false
		}
		return true
	})
	return held
}

// rateLimitedError reports whether the record's error attribute is a GitHub
// rate-limit refusal or a pacing shed (fleetreport.IsGitHubRateLimited). The
// message names the caller ("failed to fetch check runs", "stage labels:
// check digest failed", ...) and differs per call site, so one rate-limit
// episode would otherwise file one unclassified issue per caller
// (hivecommons/hive#15106). The client backs off and callers retry on their
// next tick; the WARN stays in local logs.
func rateLimitedError(r slog.Record) bool {
	limited := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" && fleetreport.IsGitHubRateLimited(a.Value.String()) {
			limited = true
			return false
		}
		return true
	})
	return limited
}

// sentinelPacingSkip handles the sentinel's per-item errors, which are logged
// as detail rather than error. Other skipped PRs remain reportable.
func sentinelPacingSkip(r slog.Record) bool {
	if r.Message != "sentinel sweep: PR skipped" {
		return false
	}
	limited := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "detail" && fleetreport.IsGitHubRateLimited(a.Value.String()) {
			limited = true
			return false
		}
		return true
	})
	return limited
}

// upstreamUnavailableError reports whether the record's error attribute is a
// GitHub 502/503/504 (fleetreport.IsGitHubUpstreamUnavailable). Callers such
// as the self-authored automerge sweep ("fetch-pr") log a per-site message and
// retry on their next tick, so a GitHub-side blip is not a hive defect
// (hivecommons/hive#15365). The WARN stays in local logs.
func upstreamUnavailableError(r slog.Record) bool {
	unavailable := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" && fleetreport.IsGitHubUpstreamUnavailable(a.Value.String()) {
			unavailable = true
			return false
		}
		return true
	})
	return unavailable
}

// inferenceBlockedStall reports whether r is the watchdog's "agent not
// producing" warning raised because the provider refused inference
// ("blocked: inference (<class>)": 429 rate limit, budget_exceeded, ...). The
// cause is upstream and already shown as a blocked agent on the dashboard, so
// the stall is not a hive defect to file as unclassified_error_repeated
// (hivecommons/hive#15986). A stall with any other detail stays reportable.
func inferenceBlockedStall(r slog.Record) bool {
	if strings.TrimSpace(r.Message) != "watchdog: agent not producing" {
		return false
	}
	blocked := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "detail" {
			blocked = strings.HasPrefix(a.Value.String(), "blocked: inference (")
			return false
		}
		return true
	})
	return blocked
}

// automergeMergeGuardSkip reports whether r is an automerge sweep lane
// (queued, trusted-author or contributor) declining to refresh a PR's base
// while a merge to that destination is already in flight (reason
// base-refresh:merge-guard). The guard is self-limiting and the PR is retried
// next tick (hivecommons/hive#16520). Any other sweep skip stays reportable.
func automergeMergeGuardSkip(r slog.Record) bool {
	switch strings.TrimSpace(r.Message) {
	case "automerge sweep skipped PR",
		"trusted-author automerge sweep skipped PR",
		"contributor-lane automerge sweep skipped PR":
	default:
		return false
	}
	reason, err := "", ""
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "reason":
			reason = a.Value.String()
		case "error":
			err = a.Value.String()
		}
		return true
	})
	return reason == "base-refresh:merge-guard" && strings.Contains(err, "destination merge already in progress")
}

// failClosedStatus500 reports whether r is one of the callers that fail closed
// when GitHub answers the combined-status read with a 500: the merge-eligible
// lane gate, the trusted-author and contributor-lane automerge sweep skips and
// the stage-label check digest. The PR is retried next tick, so the transient
// 5xx is expected behaviour (hivecommons/hive#15785, #15786, #16778, #16779).
// A 500 elsewhere stays reportable.
func failClosedStatus500(r slog.Record) bool {
	switch strings.TrimSpace(r.Message) {
	case "merge-eligible lane gate could not evaluate PR; leaving it listed",
		"trusted-author automerge sweep skipped PR",
		"contributor-lane automerge sweep skipped PR",
		"stage labels: check digest failed":
	default:
		return false
	}
	match := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			e := a.Value.String() + " "
			match = strings.Contains(e, "/status") && strings.Contains(e, ": 500 ")
			return false
		}
		return true
	})
	return match
}

// hubHeartbeatUnavailable reports whether r is a spoke heartbeat the hub
// answered with 502/503/504 (restarting or temporarily unreachable). The
// heartbeat retries on the next beat, so this is expected transient behaviour
// (hivecommons/hive#15789). 401/403 (bad HIVE_HUB_SECRET) stay reportable.
func hubHeartbeatUnavailable(r slog.Record) bool {
	if strings.TrimSpace(r.Message) != "hub heartbeat rejected" {
		return false
	}
	match := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "status" {
			switch a.Value.String() {
			case "502", "503", "504":
				match = true
			}
			return false
		}
		return true
	})
	return match
}

// hubHeartbeatCachedFallback is the outcome notice for a bounded collector
// timeout: sending stale stats, a minimal liveness beat, skipping a beat before
// identity is known, or dropping the fresh agent overlay is the liveness
// safeguard working as designed (#16507, #16516). Keep the WARN in local logs,
// but do not file a second defect for the fallback itself. The collector
// timeout and its slow-section breakdown, collector panics, and heartbeat send
// failures remain reportable.
func hubHeartbeatCachedFallback(r slog.Record) bool {
	switch strings.TrimSpace(r.Message) {
	case "hub heartbeat collect timed out — sending LAST-GOOD cached stats (marked stale) so the hub keeps this hive online; loop keeps ticking so liveness stays green",
		"hub heartbeat collect timed out with no cached payload — sending MINIMAL liveness beat (identity only, no stats) so the hub keeps this hive online; loop keeps ticking so liveness stays green",
		"hub heartbeat collect timed out and hive identity not yet known — skipping this beat; loop keeps ticking so liveness stays green",
		"hub heartbeat fresh stats collect timed out; sending stale cached stats without fresh agent overlay":
		return true
	}
	return false
}

// reviewCommentCapped reports whether r is the review-request watcher noting
// that the per-head review cap suppressed a duplicate comment while the
// verdict was still recorded. That is the cap working as designed, not a
// runtime defect (hivecommons/hive#15906); the WARN stays in local logs.
func reviewCommentCapped(r slog.Record) bool {
	return strings.TrimSpace(r.Message) == "review-request watcher: comment suppressed by per-head cap, verdict recorded"
}

// Classifier errors stay in local logs only: transport errors can expose the
// private endpoint in both the URL and the nested DNS/dial error.
func redactClassifierError(a slog.Attr) slog.Attr {
	if a.Key == "error" || a.Key == "classifier_error" {
		return slog.String(a.Key, "[withheld: see local logs]")
	}
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		attrs := append([]slog.Attr(nil), a.Value.Group()...)
		for i := range attrs {
			attrs[i] = redactClassifierError(attrs[i])
		}
		a.Value = slog.GroupValue(attrs...)
	}
	return a
}

// diagnostics renders bounded attributes for WARN+ records that passed the filters.
func diagnostics(r slog.Record, bound []slog.Attr) string {
	classifier := fleetreport.LogFedClass(r.Message) == fleetreport.ClassIOScanClassifierUnavail
	var b strings.Builder
	b.WriteString(r.Level.String())
	write := func(a slog.Attr) bool {
		if b.Len() >= maxDiagnosticsBytes {
			return false
		}
		if classifier {
			a = redactClassifierError(a)
		}
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(a.Value.String())
		return true
	}
	for _, a := range bound {
		if !write(a) {
			break
		}
	}
	r.Attrs(write)
	out := b.String()
	if len(out) > maxDiagnosticsBytes {
		out = out[:maxDiagnosticsBytes]
	}
	return out
}
