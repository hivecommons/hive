package config

import (
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultReviewEventDebounceS is the per-PR quiet period before a
	// webhook-triggered review dispatch fires (hivecommons/hive#11091).
	DefaultReviewEventDebounceS = 90
	// MaxReviewEventDebounceS bounds review.event_debounce_s; anything longer
	// is slower than a typical governor cadence and defeats the purpose.
	MaxReviewEventDebounceS = 3600
	// DefaultPendingReviewTTL matches review.DefaultPendingReviewTTL;
	// duplicated here to avoid a config→review import cycle.
	DefaultPendingReviewTTL = 45 * time.Minute
	// DefaultPendingFixTTL matches review.DefaultPendingFixTTL; duplicated
	// here for the same reason.
	DefaultPendingFixTTL = 2 * time.Hour
	// DefaultMaxRedispatch matches review.DefaultMaxRedispatch.
	DefaultMaxRedispatch               = 3
	ReviewSelfAuthoredDecisionResolve  = "resolve"
	ReviewSelfAuthoredDecisionEscalate = "escalate"
)

// ReviewEventDrivenEnabled reports whether pull_request webhooks may trigger
// review dispatch early. review.event_driven unset means on exactly when the
// hive receives signed webhooks (webhooksConfigured). It is always off while
// the review swarm itself is off (require_approval and fan_out).
func (r ReviewConfig) ReviewEventDrivenEnabled(webhooksConfigured bool) bool {
	if !r.RequireApproval || !r.FanOut {
		return false
	}
	if r.EventDriven != nil {
		return *r.EventDriven
	}
	return webhooksConfigured
}

// ReviewEventDebounce is review.event_debounce_s as a duration, defaulting to
// DefaultReviewEventDebounceS.
func (r ReviewConfig) ReviewEventDebounce() time.Duration {
	if r.EventDebounceS <= 0 {
		return DefaultReviewEventDebounceS * time.Second
	}
	return time.Duration(r.EventDebounceS) * time.Second
}

// ValidateReviewEventDispatch rejects a negative or oversized
// review.event_debounce_s.
func (r ReviewConfig) ValidateReviewEventDispatch() error {
	if r.EventDebounceS < 0 || r.EventDebounceS > MaxReviewEventDebounceS {
		return fmt.Errorf("review: event_debounce_s %d out of range (0-%d; 0 means the %ds default)",
			r.EventDebounceS, MaxReviewEventDebounceS, DefaultReviewEventDebounceS)
	}
	return nil
}

// PendingReviewTTL is review.pending_ttl as a duration. Unset, "0" or an
// unparsable value means DefaultPendingReviewTTL; a negative duration is
// returned as-is and the planner reads it as "never expire".
func (r ReviewConfig) PendingReviewTTL() time.Duration {
	raw := strings.TrimSpace(r.PendingTTL)
	if raw == "" {
		return DefaultPendingReviewTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d == 0 {
		return DefaultPendingReviewTTL
	}
	return d
}

// PendingReviewFixTTL is review.pending_fix_ttl as a duration. Unset, "0" or
// an unparsable value means DefaultPendingFixTTL; a negative duration is
// returned as-is and the planner reads it as "never expire".
func (r ReviewConfig) PendingReviewFixTTL() time.Duration {
	raw := strings.TrimSpace(r.PendingFixTTL)
	if raw == "" {
		return DefaultPendingFixTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d == 0 {
		return DefaultPendingFixTTL
	}
	return d
}

// EffectiveMaxRedispatch resolves review.max_redispatch.
func (r ReviewConfig) EffectiveMaxRedispatch() int {
	if r.MaxRedispatch <= 0 {
		return DefaultMaxRedispatch
	}
	return r.MaxRedispatch
}

func (r ReviewConfig) EffectiveSelfAuthoredDecisionPolicy() string {
	if strings.EqualFold(strings.TrimSpace(r.SelfAuthoredDecisionPolicy), ReviewSelfAuthoredDecisionEscalate) {
		return ReviewSelfAuthoredDecisionEscalate
	}
	return ReviewSelfAuthoredDecisionResolve
}

// ValidatePendingReviewTTL rejects a review.pending_ttl that is not a Go
// duration string.
func (r ReviewConfig) ValidatePendingReviewTTL() error {
	raw := strings.TrimSpace(r.PendingTTL)
	if raw == "" {
		return nil
	}
	if _, err := time.ParseDuration(raw); err != nil {
		return fmt.Errorf("review: pending_ttl %q is not a duration (e.g. 45m); unset means the default %s", r.PendingTTL, DefaultPendingReviewTTL)
	}
	return nil
}

// ValidatePendingFixTTL rejects a review.pending_fix_ttl that is not a Go
// duration string.
func (r ReviewConfig) ValidatePendingFixTTL() error {
	raw := strings.TrimSpace(r.PendingFixTTL)
	if raw == "" {
		return nil
	}
	if _, err := time.ParseDuration(raw); err != nil {
		return fmt.Errorf("review: pending_fix_ttl %q is not a duration (e.g. 2h); unset means the default %s", r.PendingFixTTL, DefaultPendingFixTTL)
	}
	return nil
}

// DefaultReviewBotAuthors is review.bot_authors unset: the dependency and
// automation bots, plus any other `[bot]` login that is not the hive's own.
var DefaultReviewBotAuthors = []string{"dependabot[bot]", "renovate[bot]", "github-actions[bot]", "*[bot]"}

// DefaultReviewBotAuthorFullReviewPaths is review.bot_author_full_review_paths
// unset: a bot PR that edits CI workflows or container builds is reviewed at
// full strength. Lockfiles are deliberately not here: they are the bulk of
// bot PRs and the reason the route exists.
var DefaultReviewBotAuthorFullReviewPaths = []string{".github/workflows/**", "Dockerfile*", "**/Dockerfile*"}

// EffectiveBotAuthors resolves review.bot_authors, trimmed; unset means the
// default list.
func (r ReviewConfig) EffectiveBotAuthors() []string {
	if r.BotAuthors == nil {
		return append([]string(nil), DefaultReviewBotAuthors...)
	}
	return trimmedNonEmpty(r.BotAuthors)
}

// EffectiveBotAuthorFullReviewPaths resolves
// review.bot_author_full_review_paths; unset means the default list.
func (r ReviewConfig) EffectiveBotAuthorFullReviewPaths() []string {
	if r.BotAuthorFullReviewPaths == nil {
		return append([]string(nil), DefaultReviewBotAuthorFullReviewPaths...)
	}
	return trimmedNonEmpty(r.BotAuthorFullReviewPaths)
}

// BotAuthorModelRef splits review.bot_author_model into backend and model:
// `gpt-6-luna` is a model with no backend, `copilot/gpt-6-luna` names both.
// Both empty when the route is off.
func (r ReviewConfig) BotAuthorModelRef() (backend, model string) {
	raw := strings.TrimSpace(r.BotAuthorModel)
	if raw == "" {
		return "", ""
	}
	if i := strings.Index(raw, "/"); i >= 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:])
	}
	return "", raw
}

func trimmedNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

const (
	// DefaultReviewDeltaTrivialLines is review.delta_trivial_lines unset: a
	// re-review delta under this many changed lines, touching only files the
	// previous verdict found nothing in, re-stamps that verdict without a
	// model call.
	DefaultReviewDeltaTrivialLines = 20
	// DefaultReviewDeltaMaxAge is review.delta_max_age unset: a
	// changes_requested verdict with findings older than this anchors no
	// delta re-review.
	DefaultReviewDeltaMaxAge = 72 * time.Hour
)

// DeltaRereviewEnabled resolves review.delta_rereview; unset means on.
func (r ReviewConfig) DeltaRereviewEnabled() bool {
	return r.DeltaRereview == nil || *r.DeltaRereview
}

// EffectiveDeltaTrivialLines resolves review.delta_trivial_lines: 0 is the
// default, a negative value disables the trivial-delta skip (returned as 0).
func (r ReviewConfig) EffectiveDeltaTrivialLines() int {
	switch {
	case r.DeltaTrivialLines == 0:
		return DefaultReviewDeltaTrivialLines
	case r.DeltaTrivialLines < 0:
		return 0
	default:
		return r.DeltaTrivialLines
	}
}

// EffectiveDeltaMaxAge resolves review.delta_max_age; empty or unparsable
// means DefaultReviewDeltaMaxAge.
func (r ReviewConfig) EffectiveDeltaMaxAge() time.Duration {
	raw := strings.TrimSpace(r.DeltaMaxAge)
	if raw == "" {
		return DefaultReviewDeltaMaxAge
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultReviewDeltaMaxAge
	}
	return d
}

// ValidateDeltaMaxAge rejects a review.delta_max_age that is not a Go
// duration string.
func (r ReviewConfig) ValidateDeltaMaxAge() error {
	raw := strings.TrimSpace(r.DeltaMaxAge)
	if raw == "" {
		return nil
	}
	if _, err := time.ParseDuration(raw); err != nil {
		return fmt.Errorf("review: delta_max_age %q is not a duration (e.g. 72h); unset means the default %s", r.DeltaMaxAge, DefaultReviewDeltaMaxAge)
	}
	return nil
}
