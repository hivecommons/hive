package dashboard

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	gh "github.com/google/go-github/v72/github"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// delegatedGatherRetryFallback is the wait named when GitHub refused for
// quota without saying until when: GitHub's own rule for that case is to
// wait at least a minute.
const delegatedGatherRetryFallback = time.Minute

// delegatedGatherError answers a "Let hive decide" request whose live read
// of the item failed. On a hosted hive the Ingress swaps every 502 and 503
// for its canned "hive unavailable" body (custom-http-errors, see
// pkg/hub/saas_provision.go), so an error GitHub explained must not use
// them: projectbluefin's previews all read "(hive unavailable)" for half an
// hour while the hive was up and its App was out of quota.
func delegatedGatherError(w http.ResponseWriter, err error, now time.Time) {
	status, msg, retry := delegatedGatherErrorStatus(err, now)
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
	}
	jsonError(w, msg, status)
}

// delegatedGatherErrorStatus maps a gather error to its answer: a GitHub
// rate limit (primary, secondary, or a request the pacer shed while one is
// in force) is 429 with the wait and the time it lifts; any other answer
// from GitHub (404, a permission 403, …) or a hive-side refusal (the item
// is closed, no snapshot yet) is 424 with the real message; only a request
// that never got an answer (network error, timeout) stays 502.
func delegatedGatherErrorStatus(err error, now time.Time) (int, string, time.Duration) {
	if delay, limited := ghpkg.RateLimitRetryDelay(err, now); limited || errors.Is(err, ghpkg.ErrPacingShed) {
		if delay <= 0 {
			delay = delegatedGatherRetryFallback
		}
		var primary *gh.RateLimitError
		if errors.As(err, &primary) {
			return http.StatusTooManyRequests, "GitHub API rate limit for this hive is exhausted; resets at " + now.Add(delay).UTC().Format("15:04Z"), delay
		}
		// Round up so "retry after" is never early.
		at := now.Add(delay + time.Minute - time.Nanosecond).UTC().Truncate(time.Minute).Format("15:04Z")
		if !limited {
			return http.StatusTooManyRequests, "This hive is pacing its GitHub requests after a rate limit; retry after " + at, delay
		}
		return http.StatusTooManyRequests, "GitHub secondary rate limit reached for this hive; retry after " + at, delay
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) {
		return http.StatusFailedDependency, err.Error(), 0
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusBadGateway, err.Error(), 0
	}
	return http.StatusFailedDependency, err.Error(), 0
}
