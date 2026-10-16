package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

// Regression for #12101: a failed non-required check must block the merge gate.
func TestEvaluateCommitCIFailOnAnyFailedCheck(t *testing.T) {
	checks := []map[string]string{
		{"name": "build-gate", "status": "completed", "conclusion": "success"},
		{"name": "syntax-check", "status": "completed", "conclusion": "failure"},
		{"name": "Playwright", "status": "completed", "conclusion": "failure"},
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/commits/sha/status":
			json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0})
		case "/repos/acme/widget/commits/sha/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(checks), "check_runs": checks})
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

	opts := CommitCIOptions{Required: map[string]bool{"build-gate": true}, RequiredKnown: true}
	st, err := EvaluateCommitCI(context.Background(), client, "acme", "widget", "sha", opts)
	if err != nil || !st.Green {
		t.Fatalf("without option = (%v,%q,%v), want green", st.Green, st.Reason, err)
	}
	opts.FailOnAnyFailedCheck = true
	st, err = EvaluateCommitCI(context.Background(), client, "acme", "widget", "sha", opts)
	if err != nil {
		t.Fatalf("EvaluateCommitCI returned error: %v", err)
	}
	if st.Green || st.Reason != "check-failing:syntax-check" {
		t.Fatalf("with option = (%v,%q), want red check-failing:syntax-check", st.Green, st.Reason)
	}
}

// hivecommons/hive#12457: the required-only gate walks past red optional
// checks, but reports them so the stage engine can keep the PR out of the
// merge-queue stage. Meta contexts (tide) are not red checks; a red REQUIRED
// check is the blocker, not an optional one.
func TestEvaluateCommitCIReportsFailingNonRequired(t *testing.T) {
	checks := []map[string]string{
		{"name": "build-gate", "status": "completed", "conclusion": "success"},
		{"name": "relative links and anchors resolve", "status": "completed", "conclusion": "failure"},
		{"name": "Playwright", "status": "completed", "conclusion": "cancelled"},
		{"name": "lint", "status": "in_progress"},
		{"name": "docs", "status": "completed", "conclusion": "skipped"},
	}
	statuses := []map[string]string{
		{"context": "tide", "state": "failure"},
		{"context": "ci/legacy", "state": "error"},
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widget/commits/sha/status":
			json.NewEncoder(w).Encode(map[string]any{"state": "failure", "total_count": len(statuses), "statuses": statuses})
		case "/repos/acme/widget/commits/sha/check-runs":
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(checks), "check_runs": checks})
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

	opts := CommitCIOptions{Required: map[string]bool{"build-gate": true}, RequiredKnown: true}
	st, err := EvaluateCommitCI(context.Background(), client, "acme", "widget", "sha", opts)
	if err != nil || !st.Green {
		t.Fatalf("required-only = (%v,%q,%v), want green", st.Green, st.Reason, err)
	}
	want := []string{"Playwright", "ci/legacy", "relative links and anchors resolve"}
	if !reflect.DeepEqual(st.FailingNonRequired, want) {
		t.Fatalf("FailingNonRequired=%v want %v", st.FailingNonRequired, want)
	}

	opts.Required = map[string]bool{"build-gate": true, "relative links and anchors resolve": true}
	st, err = EvaluateCommitCI(context.Background(), client, "acme", "widget", "sha", opts)
	if err != nil || st.Green || st.Reason != "required-check-failing:relative links and anchors resolve" {
		t.Fatalf("required red = (%v,%q,%v)", st.Green, st.Reason, err)
	}
	if want := []string{"Playwright", "ci/legacy"}; !reflect.DeepEqual(st.FailingNonRequired, want) {
		t.Fatalf("FailingNonRequired=%v want %v", st.FailingNonRequired, want)
	}
}

// HeadCommitPushedAt reads the head commit's committer date (#12557), clamps
// it to the PR's creation, and reports errors instead of guessing.
func TestHeadCommitPushedAt(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/r/git/commits/abc":
			_, _ = w.Write([]byte(`{"sha":"abc","committer":{"name":"c","date":"2026-10-16T17:15:00Z"}}`))
		case "/repos/o/r/git/commits/nodate":
			_, _ = w.Write([]byte(`{"sha":"nodate"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer api.Close()
	client := gh.NewClient(nil)
	base, err := url.Parse(api.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.BaseURL = base
	ctx := context.Background()
	committed := time.Date(2026, 10, 16, 17, 15, 0, 0, time.UTC)

	pr := &gh.PullRequest{
		CreatedAt: &gh.Timestamp{Time: committed.Add(-time.Hour)},
		UpdatedAt: &gh.Timestamp{Time: committed.Add(3 * time.Hour)},
	}
	if got, err := HeadCommitPushedAt(ctx, client, "o", "r", "abc", pr); err != nil || !got.Equal(committed) {
		t.Fatalf("HeadCommitPushedAt = %v, %v; want %v", got, err, committed)
	}
	created := committed.Add(time.Hour)
	if got, err := HeadCommitPushedAt(ctx, client, "o", "r", "abc", &gh.PullRequest{CreatedAt: &gh.Timestamp{Time: created}}); err != nil || !got.Equal(created) {
		t.Fatalf("clamped HeadCommitPushedAt = %v, %v; want %v", got, err, created)
	}
	for _, sha := range []string{"nodate", "missing", " "} {
		if got, err := HeadCommitPushedAt(ctx, client, "o", "r", sha, pr); err == nil || !got.IsZero() {
			t.Fatalf("sha %q: got %v, %v; want zero and an error", sha, got, err)
		}
	}
	if _, err := HeadCommitPushedAt(ctx, nil, "o", "r", "abc", pr); err == nil {
		t.Fatal("nil client must error")
	}
}
