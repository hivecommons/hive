package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestPRClaimLinkErrorLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		limited bool
	}{
		{"primary", &gh.RateLimitError{Response: &http.Response{StatusCode: 403}}, true},
		{"secondary", &gh.AbuseRateLimitError{Response: &http.Response{StatusCode: 403}}, true},
		{"wrapped secondary refusal", fmt.Errorf("lookup: %w", &gh.AbuseRateLimitError{Response: &http.Response{StatusCode: 403}}), true},
		{"permission failure", errors.New("Resource not accessible by integration"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			if got := logPRClaimLinkError(logger, "link failed", "org/repo", 7, 1, tc.err); got != tc.limited {
				t.Fatalf("rate limited = %v, want %v", got, tc.limited)
			}
			level := "level=WARN"
			if tc.limited {
				level = "level=INFO"
			}
			if !strings.Contains(logs.String(), level) {
				t.Fatalf("log = %s, want %s", logs.String(), level)
			}
			if got := logPRClaimLinkError(nil, "link failed", "org/repo", 7, 1, tc.err); got != tc.limited {
				t.Fatalf("nil logger changed rate-limit handling: %v", got)
			}
		})
	}
}

func TestSyncIssuePRClaimLabelsDefersRateLimitedLinks(t *testing.T) {
	for _, reference := range []bool{true, false} {
		t.Run(fmt.Sprintf("reference=%v", reference), func(t *testing.T) {
			ResetRateLimitPacingForTest()
			t.Cleanup(ResetRateLimitPacingForTest)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/graphql") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusForbidden)
				// go-github classifies secondary limits by documentation_url,
				// not Retry-After or the message alone. Match GitHub's response
				// so the sweep receives an AbuseRateLimitError, not a generic 403.
				_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`))
			}))
			t.Cleanup(srv.Close)
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			client := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, logger)
			ledger := NewClaimLedger("", testLogger())
			now := time.Now()
			ledger.Reconcile([]IssueClaim{
				{Repo: "hivecommons/hive", Issue: 7, PRNumber: 1, PRState: "open", Reference: reference, ObservedAt: now, FirstObservedAt: now},
				{Repo: "hivecommons/hive", Issue: 8, PRNumber: 2, PRState: "open", Reference: reference, ObservedAt: now, FirstObservedAt: now},
			}, true)
			result := &ActionableResult{}
			result.Issues.Items = []Issue{{Repo: "hivecommons/hive", Number: 7}, {Repo: "hivecommons/hive", Number: 8}}
			for sweep := 1; sweep <= 2; sweep++ {
				SyncIssuePRClaimLabels(context.Background(), client, result, ledger, logger)
				if got := strings.Count(logs.String(), "PR claim links deferred by GitHub rate limit"); got != sweep {
					t.Fatalf("sweep %d: got %d deferrals, want one per sweep; logs: %s", sweep, got, logs.String())
				}
				if strings.Contains(logs.String(), "level=WARN") {
					t.Fatalf("expected backoff must not emit warnings: %s", logs.String())
				}
				for _, issue := range result.Issues.Items {
					if !HasCoveredByPRLabel(issue.Labels) {
						t.Fatalf("issue #%d lost claim annotation: %v", issue.Number, issue.Labels)
					}
				}
			}
		})
	}
}

func TestPRClaimLabelErrorLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		level string
	}{
		{"primary", &gh.RateLimitError{Response: &http.Response{StatusCode: 403}}, "INFO"},
		{"wrapped secondary", fmt.Errorf("label: %w", &gh.AbuseRateLimitError{Response: &http.Response{StatusCode: 403}}), "INFO"},
		{"permission", errors.New("Resource not accessible by integration"), "WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			logPRClaimLabelError(logger, "ensuring verified-open label failed", tc.err, "repo", "org/repo", "issue", 7)
			if !strings.Contains(logs.String(), "level="+tc.level) || !strings.Contains(logs.String(), "issue=7") {
				t.Fatalf("unexpected log: %s", logs.String())
			}
		})
	}
}

func TestSyncIssuePRClaimLabelsRateLimitedLabelIsInformational(t *testing.T) {
	ResetRateLimitPacingForTest()
	t.Cleanup(ResetRateLimitPacingForTest)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`))
	}))
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	client := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, logger)
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "hivecommons/hive", Number: 7, Labels: []string{VerifiedOpenLabel}}}
	SyncIssuePRClaimLabels(context.Background(), client, result, NewClaimLedger("", testLogger()), logger)
	if !strings.Contains(logs.String(), "level=INFO") || !strings.Contains(logs.String(), "ensuring verified-open label failed") || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("expected informational label failure: %s", logs.String())
	}
}
