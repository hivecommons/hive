package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestEvaluateCommitCIFreshHeadExpectedChecks(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		checks    []map[string]string
		opts      CommitCIOptions
		wantGreen bool
		want      string
	}{
		{
			name:   "fresh head with zero slow checks waits for min age",
			checks: []map[string]string{{"name": "fast", "status": "completed", "conclusion": "success"}},
			opts: CommitCIOptions{
				MinHeadAge:   3 * time.Minute,
				HeadPushedAt: now.Add(-time.Minute),
				Now:          func() time.Time { return now },
			},
			want: "pending: head pushed 1m0s ago (< min_head_age)",
		},
		{
			name:   "expected name missing waits for check-run to start",
			checks: []map[string]string{{"name": "fast", "status": "completed", "conclusion": "success"}},
			opts: CommitCIOptions{
				ExpectedChecks: map[string]bool{"test (rest 1/4)": true},
				MinHeadAge:     3 * time.Minute,
				HeadPushedAt:   now.Add(-10 * time.Minute),
				Now:            func() time.Time { return now },
			},
			want: "pending: test (rest 1/4) has not started",
		},
		{
			name: "all expected present and old enough is green",
			checks: []map[string]string{
				{"name": "fast", "status": "completed", "conclusion": "success"},
				{"name": "test (rest 1/4)", "status": "completed", "conclusion": "success"},
			},
			opts: CommitCIOptions{
				ExpectedChecks: map[string]bool{"test (rest 1/4)": true},
				MinHeadAge:     3 * time.Minute,
				HeadPushedAt:   now.Add(-10 * time.Minute),
				Now:            func() time.Time { return now },
			},
			wantGreen: true,
		},
		{
			name:   "config required all green bypasses young head age",
			checks: []map[string]string{{"name": "build-gate", "status": "completed", "conclusion": "success"}},
			opts: CommitCIOptions{
				Required:                map[string]bool{"build-gate": true},
				RequiredKnown:           true,
				RequiredKnownFromConfig: true,
				MinHeadAge:              3 * time.Minute,
				HeadPushedAt:            now.Add(-time.Minute),
				Now:                     func() time.Time { return now },
			},
			wantGreen: true,
		},
		{
			name:   "failure still fails before fresh-head guard",
			checks: []map[string]string{{"name": "build-gate", "status": "completed", "conclusion": "failure"}},
			opts: CommitCIOptions{
				ExpectedChecks: map[string]bool{"slow": true},
				MinHeadAge:     3 * time.Minute,
				HeadPushedAt:   now.Add(-time.Minute),
				Now:            func() time.Time { return now },
			},
			want: "check-failure",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/status":
					json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/sha/check-runs":
					json.NewEncoder(w).Encode(map[string]any{"total_count": len(tt.checks), "check_runs": tt.checks})
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			client := gh.NewClient(nil)
			base, err := url.Parse(api.URL + "/")
			if err != nil {
				t.Fatalf("ParseURL: %v", err)
			}
			client.BaseURL = base

			st, err := EvaluateCommitCI(context.Background(), client, "acme", "widget", "sha", tt.opts)
			if err != nil {
				t.Fatalf("EvaluateCommitCI returned error: %v", err)
			}
			if st.Green != tt.wantGreen || st.Reason != tt.want {
				t.Fatalf("EvaluateCommitCI = (%v,%q), want (%v,%q)", st.Green, st.Reason, tt.wantGreen, tt.want)
			}
		})
	}
}

func TestExpectedCommitChecksFromRefKeepsOnlyPRContextChecks(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/refsha/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 4, "check_runs": []map[string]any{
				{"name": "test (rest 1/4)", "status": "completed", "conclusion": "success", "pull_requests": []map[string]any{{"number": 7}}},
				{"name": "push-only release", "status": "completed", "conclusion": "success", "pull_requests": []map[string]any{}},
				{"name": "skipped pr check", "status": "completed", "conclusion": "skipped", "pull_requests": []map[string]any{{"number": 7}}},
				{"name": "neutral pr check", "status": "completed", "conclusion": "neutral", "pull_requests": []map[string]any{{"number": 7}}},
			}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()
	client := gh.NewClient(nil)
	base, err := url.Parse(api.URL + "/")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	client.BaseURL = base

	got, err := ExpectedCommitChecksFromRef(context.Background(), client, "acme", "widget", "refsha")
	if err != nil {
		t.Fatalf("ExpectedCommitChecksFromRef returned error: %v", err)
	}
	if len(got) != 1 || !got["test (rest 1/4)"] {
		t.Fatalf("ExpectedCommitChecksFromRef = %v, want only PR-context successful check", got)
	}
}
