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
	// DefaultMaxRedispatch matches review.DefaultMaxRedispatch.
	DefaultMaxRedispatch = 3
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

// EffectiveMaxRedispatch resolves review.max_redispatch.
func (r ReviewConfig) EffectiveMaxRedispatch() int {
	if r.MaxRedispatch <= 0 {
		return DefaultMaxRedispatch
	}
	return r.MaxRedispatch
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
