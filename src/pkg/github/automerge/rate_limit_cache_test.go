package automerge

import (
	"errors"
	"net/http"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// TestIsRateLimited_RecognisesPreEmptiveRefusal is the guard for the stall this
// fixes. go-github does not only surface rate limits GitHub reports — once it
// has cached Remaining==0 it SYNTHESISES a 403 locally and returns it without
// contacting GitHub ("not making remote request"). Both shapes must be
// recognised, or the self-heal never runs for the case that actually stalls.
func TestIsRateLimited_RecognisesPreEmptiveRefusal(t *testing.T) {
	preEmptive := &gh.RateLimitError{
		Rate:     gh.Rate{Limit: 6900, Remaining: 0, Reset: gh.Timestamp{Time: time.Now().Add(30 * time.Minute)}},
		Response: &http.Response{StatusCode: http.StatusForbidden},
		Message:  "API rate limit of 6900 still exceeded until 2026-08-16 02:34:57 -0400 EDT, not making remote request.",
	}
	if !isRateLimited(preEmptive) {
		t.Error("go-github's pre-emptive rate-limit refusal was not recognised")
	}

	abuse := &gh.AbuseRateLimitError{Response: &http.Response{StatusCode: http.StatusForbidden}}
	if !isRateLimited(abuse) {
		t.Error("secondary (abuse) rate limit was not recognised")
	}

	// Wrapped errors must still match — callers wrap with context like
	// "listing open PRs for owner/repo: %w".
	if !isRateLimited(wrap(preEmptive)) {
		t.Error("a wrapped rate-limit error was not recognised")
	}
}

func wrap(err error) error { return &wrapped{err} }

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "listing open PRs for o/r: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// TestIsRateLimited_IgnoresOtherErrors: the self-heal costs an API call, so it
// must not fire on unrelated failures.
func TestIsRateLimited_IgnoresOtherErrors(t *testing.T) {
	for _, err := range []error{
		errors.New("connection refused"),
		&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}},
		&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnauthorized}},
	} {
		if isRateLimited(err) {
			t.Errorf("non-rate-limit error treated as rate limited: %v", err)
		}
	}
}

// TestRefreshRateLimitCache_NilSafe: the sweep calls this from an error path, so
// it must never panic on a half-constructed client.
func TestRefreshRateLimitCache_NilSafe(t *testing.T) {
	var c *Engine
	c.refreshRateLimitCache(t.Context()) // nil receiver
	(&Engine{}).refreshRateLimitCache(t.Context())
}
