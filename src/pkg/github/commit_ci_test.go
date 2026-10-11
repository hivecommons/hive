package github

import (
	"context"
	"encoding/base64"
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
			name:   "expected name missing no longer blocks unknown required set",
			checks: []map[string]string{{"name": "fast", "status": "completed", "conclusion": "success"}},
			opts: CommitCIOptions{
				ExpectedChecks: map[string]bool{"test (rest 1/4)": true},
				MinHeadAge:     3 * time.Minute,
				HeadPushedAt:   now.Add(-10 * time.Minute),
				Now:            func() time.Time { return now },
			},
			wantGreen: true,
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
			name:   "failure with unknown required set is left to merge endpoint after fresh-head guard",
			checks: []map[string]string{{"name": "build-gate", "status": "completed", "conclusion": "failure"}},
			opts: CommitCIOptions{
				ExpectedChecks: map[string]bool{"slow": true},
				MinHeadAge:     3 * time.Minute,
				HeadPushedAt:   now.Add(-time.Minute),
				Now:            func() time.Time { return now },
			},
			want: "pending: head pushed 1m0s ago (< min_head_age)",
		},
		{
			name: "failure with known-empty required set blocks locally",
			checks: []map[string]string{
				{"name": "build", "status": "completed", "conclusion": "failure"},
				{"name": "lint", "status": "completed", "conclusion": "success"},
			},
			opts: CommitCIOptions{
				RequiredKnownEmpty: true,
				RequireEvidence:    true,
				MinHeadAge:         3 * time.Minute,
				HeadPushedAt:       now.Add(-10 * time.Minute),
				Now:                func() time.Time { return now },
			},
			want: "check-failure",
		},
		{
			name: "require evidence refuses empty actual-required repos with no statuses",
			opts: CommitCIOptions{
				RequireEvidence: true,
			},
			want: "ci-unverified",
		},
		{
			name:   "pending check still blocks unknown required set",
			checks: []map[string]string{{"name": "build-gate", "status": "in_progress"}},
			opts: CommitCIOptions{
				MinHeadAge:   3 * time.Minute,
				HeadPushedAt: now.Add(-10 * time.Minute),
				Now:          func() time.Time { return now },
			},
			want: "check-pending",
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

			tt.opts.UnknownRequiredChecksServerEnforced = true
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
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/actions/runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}})
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

// TestExpectedCommitChecksFromRefExcludesPullRequestClosedOnlyWorkflow is the
// regression test for #9794: hivecommons/hotshot's close-linked-issues.yml
// triggers only on `pull_request: types: [closed]`, so its job can never run
// on a PR's pre-merge head. It must not survive into the expected-checks set
// that the merge CI gate waits on, or the gate deadlocks forever.
func TestExpectedCommitChecksFromRefExcludesPullRequestClosedOnlyWorkflow(t *testing.T) {
	const closedOnlyWorkflow = "on:\n  pull_request:\n    types: [closed]\njobs:\n  close-linked-issue:\n    runs-on: ubuntu-latest\n    steps: []\n"
	const verifyWorkflow = "on:\n  pull_request: {}\njobs:\n  verify:\n    runs-on: ubuntu-latest\n    steps: []\n"

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/refsha/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []map[string]any{
				{"name": "verify", "status": "completed", "conclusion": "success", "pull_requests": []map[string]any{{"number": 7}}},
				{"name": "close-linked-issue", "status": "completed", "conclusion": "success", "pull_requests": []map[string]any{{"number": 7}}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/actions/runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "workflow_runs": []map[string]any{
				{"id": 101, "path": ".github/workflows/verify.yml"},
				{"id": 102, "path": ".github/workflows/close-linked-issues.yml"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/actions/runs/101/jobs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []map[string]any{
				{"name": "verify"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/actions/runs/102/jobs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []map[string]any{
				{"name": "close-linked-issue"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/contents/.github/workflows/verify.yml":
			json.NewEncoder(w).Encode(map[string]any{
				"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(verifyWorkflow)),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/contents/.github/workflows/close-linked-issues.yml":
			json.NewEncoder(w).Encode(map[string]any{
				"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(closedOnlyWorkflow)),
			})
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
	if len(got) != 1 || !got["verify"] || got["close-linked-issue"] {
		t.Fatalf("ExpectedCommitChecksFromRef = %v, want only \"verify\" (close-linked-issue is pull_request:closed-only)", got)
	}
}

// TestExpectedCommitChecksFromRefKeepsCheckWhenWorkflowFetchFails asserts the
// fail-safe direction: if the workflow file that produced a check cannot be
// fetched (e.g. deleted, API error), the check stays in the expected set
// rather than being silently dropped.
func TestExpectedCommitChecksFromRefKeepsCheckWhenWorkflowFetchFails(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/refsha/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]any{
				{"name": "close-linked-issue", "status": "completed", "conclusion": "success", "pull_requests": []map[string]any{{"number": 7}}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/actions/runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []map[string]any{
				{"id": 102, "path": ".github/workflows/close-linked-issues.yml"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/actions/runs/102/jobs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []map[string]any{
				{"name": "close-linked-issue"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/contents/.github/workflows/close-linked-issues.yml":
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
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
	if len(got) != 1 || !got["close-linked-issue"] {
		t.Fatalf("ExpectedCommitChecksFromRef = %v, want check kept when workflow fetch fails (fail-safe)", got)
	}
}

func TestWorkflowIsPostMergeOnly(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{"closed only", "on:\n  pull_request:\n    types: [closed]\n", true},
		{"closed plus another type", "on:\n  pull_request:\n    types: [closed, opened]\n", false},
		{"bare pull_request has default types", "on: pull_request\n", false},
		{"pull_request map with no types", "on:\n  pull_request: {}\n", false},
		{"pull_request plus push", "on:\n  push: {}\n  pull_request:\n    types: [closed]\n", false},
		{"sequence form", "on: [push, pull_request]\n", false},
		{"unrelated event closed-like name", "on:\n  issues:\n    types: [closed]\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := workflowIsPostMergeOnly([]byte(tt.doc))
			if err != nil {
				t.Fatalf("workflowIsPostMergeOnly returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("workflowIsPostMergeOnly(%q) = %v, want %v", tt.doc, got, tt.want)
			}
		})
	}
}

func TestRequiredChecksMismatchNames(t *testing.T) {
	got := requiredChecksMismatchNames(
		map[string]bool{"build-gate": true, "observed-legacy": true, "validate": true},
		map[string]bool{"validate": true},
		map[string]bool{"observed-legacy": true},
	)
	if len(got) != 1 || got[0] != "build-gate" {
		t.Fatalf("requiredChecksMismatchNames = %v, want [build-gate]", got)
	}
}
