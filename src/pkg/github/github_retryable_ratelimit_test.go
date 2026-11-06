package github

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// RateLimitRetryDelay names GitHub's quota refusals and the wait GitHub
// gave; every other error is not a rate limit.
func TestRateLimitRetryDelay(t *testing.T) {
	now := time.Date(2026, 11, 6, 17, 10, 7, 0, time.UTC)
	resp := func(code int, h http.Header) *http.Response {
		return &http.Response{StatusCode: code, Header: h, Request: &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/x"}}}
	}
	ninety := 90 * time.Second
	for _, tc := range []struct {
		name    string
		err     error
		wantOK  bool
		wantDur time.Duration
	}{
		{"primary", fmt.Errorf("wrapped: %w", &gh.RateLimitError{Rate: gh.Rate{Reset: gh.Timestamp{Time: now.Add(22 * time.Minute)}}, Response: resp(http.StatusForbidden, http.Header{})}), true, 22 * time.Minute},
		{"secondary", &gh.AbuseRateLimitError{Response: resp(http.StatusForbidden, http.Header{}), RetryAfter: &ninety}, true, ninety},
		{"secondary without a time", &gh.AbuseRateLimitError{Response: resp(http.StatusForbidden, http.Header{})}, true, 0},
		{"bare 429", &gh.ErrorResponse{Response: resp(http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}})}, true, 30 * time.Second},
		{"not found", &gh.ErrorResponse{Response: resp(http.StatusNotFound, http.Header{})}, false, 0},
		{"permission", &gh.ErrorResponse{Response: resp(http.StatusForbidden, http.Header{})}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := RateLimitRetryDelay(tc.err, now)
			if ok != tc.wantOK || d != tc.wantDur {
				t.Fatalf("RateLimitRetryDelay = (%v, %v), want (%v, %v)", d, ok, tc.wantDur, tc.wantOK)
			}
		})
	}
}
