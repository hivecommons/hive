package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
)

// swapClaimsEffectRetry makes retries instant and bounded for a test,
// recording every pause, and restores the production policy afterwards.
func swapClaimsEffectRetry(t *testing.T, attempts int, wait func(context.Context, time.Duration) bool) *[]time.Duration {
	t.Helper()
	prevAttempts, prevBackoff, prevWait := claimsEffectAttempts, claimsEffectBackoff, claimsEffectWait
	t.Cleanup(func() {
		claimsEffectAttempts, claimsEffectBackoff, claimsEffectWait = prevAttempts, prevBackoff, prevWait
	})
	var pauses []time.Duration
	claimsEffectAttempts = attempts
	claimsEffectBackoff = time.Second
	claimsEffectWait = func(ctx context.Context, d time.Duration) bool {
		pauses = append(pauses, d)
		if wait != nil {
			return wait(ctx, d)
		}
		return true
	}
	return &pauses
}

func serverError() error {
	return &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}
}

// A claim's label or stage write that times out behind the per-repo label
// writer is retried with a growing pause until it lands
// (hivecommons/hive#14766): one 15s attempt left #14633 / #14674 at 010
// while an agent worked them.
func TestRetryClaimEffect_TransientFailuresAreRetried(t *testing.T) {
	pauses := swapClaimsEffectRetry(t, 4, nil)
	script := []error{context.DeadlineExceeded, serverError(), mutation.ErrNeedsReconciliation, nil}
	calls := 0
	n := retryClaimEffect(context.Background(), nil, "claimed stage", "acme/widgets#7", func() error {
		err := script[calls]
		calls++
		return err
	})
	if n != 4 || calls != 4 {
		t.Fatalf("attempts = %d (calls %d), want 4: retry until the write lands", n, calls)
	}
	if len(*pauses) != 3 || (*pauses)[0] != time.Second || (*pauses)[1] != 2*time.Second || (*pauses)[2] != 4*time.Second {
		t.Fatalf("pauses = %v, want doubling 1s, 2s, 4s", *pauses)
	}
}

func TestRetryClaimEffect_GivesUpAfterBudget(t *testing.T) {
	pauses := swapClaimsEffectRetry(t, 3, nil)
	calls := 0
	n := retryClaimEffect(context.Background(), nil, "claimed-by label", "acme/widgets#7", func() error {
		calls++
		return context.DeadlineExceeded
	})
	if n != 3 || calls != 3 || len(*pauses) != 2 {
		t.Fatalf("attempts = %d calls = %d pauses = %v, want exactly the 3-attempt budget", n, calls, *pauses)
	}
}

// A definite GitHub answer (422, 404, a canceled parent) is not retried:
// the same request would fail the same way.
func TestRetryClaimEffect_PermanentFailureIsNotRetried(t *testing.T) {
	pauses := swapClaimsEffectRetry(t, 4, nil)
	for name, err := range map[string]error{
		"validation": &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnprocessableEntity}},
		"not-found":  &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}},
		"canceled":   context.Canceled,
		"plain":      errors.New("label writer closed"),
	} {
		calls := 0
		n := retryClaimEffect(context.Background(), nil, "claimed stage", "acme/widgets#7", func() error {
			calls++
			return err
		})
		if n != 1 || calls != 1 {
			t.Errorf("%s: attempts = %d calls = %d, want one", name, n, calls)
		}
	}
	if len(*pauses) != 0 {
		t.Fatalf("pauses = %v, want none for permanent failures", *pauses)
	}
}

// A hub shutting down mid-pause stops retrying instead of outliving its
// context.
func TestRetryClaimEffect_StopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	swapClaimsEffectRetry(t, 4, func(ctx context.Context, _ time.Duration) bool {
		cancel()
		return ctx.Err() == nil
	})
	calls := 0
	n := retryClaimEffect(ctx, nil, "released stage", "acme/widgets#7", func() error {
		calls++
		return context.DeadlineExceeded
	})
	if n != 1 || calls != 1 {
		t.Fatalf("attempts = %d calls = %d, want the retry abandoned once the hub context ended", n, calls)
	}
}

func TestClaimEffectRetryable(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":            {nil, false},
		"timeout":        {context.DeadlineExceeded, true},
		"canceled":       {context.Canceled, false},
		"5xx":            {serverError(), true},
		"429":            {&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusTooManyRequests}}, true},
		"422":            {&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnprocessableEntity}}, false},
		"reconciliation": {mutation.ErrNeedsReconciliation, true},
		"wrapped":        {errors.Join(errors.New("claimed stage"), context.DeadlineExceeded), true},
	}
	for name, c := range cases {
		if got := claimEffectRetryable(c.err); got != c.want {
			t.Errorf("%s: claimEffectRetryable(%v) = %v, want %v", name, c.err, got, c.want)
		}
	}
}
