package github

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// isRetryableGitHubError reports whether err is a transient GitHub/API
// transport failure where a guarded mutation should be retried instead of
// proceeding with incomplete preflight information.
func isRetryableGitHubError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if isRateLimitErr(err) {
		return true
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		status := ghErr.Response.StatusCode
		if status == http.StatusTooManyRequests {
			return true
		}
		return status >= http.StatusInternalServerError && status <= 599
	}
	return false
}

func retryableGitHubDelay(err error, now time.Time, fallback, fallbackMax, explicitMax time.Duration) time.Duration {
	delay := fallback
	explicit := false
	var rl *gh.RateLimitError
	if errors.As(err, &rl) && !rl.Rate.Reset.Time.IsZero() {
		untilReset := rl.Rate.Reset.Time.Sub(now)
		if untilReset > 0 {
			delay = untilReset
			explicit = true
		}
	}
	if !explicit && rl != nil && rl.Response != nil {
		if headerDelay, ok := retryDelayFromHeaders(rl.Response.Header, now); ok {
			delay = headerDelay
			explicit = true
		}
	}
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &abuse) && abuse.RetryAfter != nil && *abuse.RetryAfter > 0 {
		delay = *abuse.RetryAfter
		explicit = true
	}
	if !explicit && abuse != nil && abuse.Response != nil {
		if headerDelay, ok := retryDelayFromHeaders(abuse.Response.Header, now); ok {
			delay = headerDelay
			explicit = true
		}
	}
	var ghErr *gh.ErrorResponse
	if !explicit && errors.As(err, &ghErr) && ghErr.Response != nil {
		if headerDelay, ok := retryDelayFromHeaders(ghErr.Response.Header, now); ok {
			delay = headerDelay
			explicit = true
		}
	}
	maxDelay := fallbackMax
	if explicit {
		maxDelay = explicitMax
	}
	if delay > maxDelay || delay <= 0 {
		delay = maxDelay
	}
	return delay
}

func retryDelayFromHeaders(h http.Header, now time.Time) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	if retryAfter := h.Get("Retry-After"); retryAfter != "" {
		if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second, true
		}
		if when, err := http.ParseTime(retryAfter); err == nil {
			if delay := when.Sub(now); delay > 0 {
				return delay, true
			}
		}
	}
	if reset := h.Get("X-RateLimit-Reset"); reset != "" {
		if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
			if delay := time.Unix(epoch, 0).Sub(now); delay > 0 {
				return delay, true
			}
		}
	}
	return 0, false
}
