package releasesentinel

import (
	"strings"
	"testing"
)

func TestIsBlockingConclusion(t *testing.T) {
	for c, want := range map[string]bool{
		"failure": true, "timed_out": true, "startup_failure": true, " FAILURE ": true,
		"success": false, "cancelled": false, "skipped": false, "neutral": false,
		"stale": false, "action_required": false, "": false,
	} {
		if got := IsBlockingConclusion(c); got != want {
			t.Errorf("IsBlockingConclusion(%q) = %v, want %v", c, got, want)
		}
	}
}

func TestIsCompleted(t *testing.T) {
	for s, want := range map[string]bool{
		"completed": true, "Completed": true, "queued": false, "in_progress": false, "waiting": false, "": false,
	} {
		if IsCompleted(s) != want {
			t.Errorf("IsCompleted(%q) = %v", s, !want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		details  RunDetails
		want     FailureClass
		contains string
	}{
		{"code failure", RunDetails{JobCount: 2, FailedJobs: []string{"test / go test"}, Evidence: []string{"--- FAIL: TestX (0.01s)"}}, ClassFixable, ""},
		{"no jobs", RunDetails{JobCount: 0}, ClassPolicy, "without running any job"},
		{"actions cannot open PRs", RunDetails{JobCount: 1, Evidence: []string{"GraphQL: GitHub Actions is not permitted to create or approve pull requests (createPullRequest)"}}, ClassPolicy, "not permitted"},
		{"workflows permission", RunDetails{JobCount: 1, Evidence: []string{"refusing to allow a GitHub App to create or update workflow `.github/workflows/x.yml` without `workflows` permission"}}, ClassPolicy, "workflows permission"},
		{"integration scope", RunDetails{JobCount: 1, Evidence: []string{"HttpError: Resource not accessible by integration"}}, ClassPolicy, "Resource not accessible"},
		{"403 forbidden", RunDetails{JobCount: 1, Evidence: []string{"HTTP 403: Forbidden"}}, ClassPolicy, "403"},
		{"missing secret", RunDetails{JobCount: 1, Evidence: []string{"secret CROSS_ORG_GHCR_TOKEN is not set, so this release cannot mirror"}}, ClassPolicy, "secret"},
		{"billing", RunDetails{JobCount: 1, Evidence: []string{"The job was not started because your spending limit needs to be increased"}}, ClassPolicy, "billing"},
		{"admin", RunDetails{JobCount: 1, FailedJobs: []string{"protect / You must have admin rights to Repository"}}, ClassPolicy, "admin"},
		{"403 without permission words is code", RunDetails{JobCount: 1, Evidence: []string{"expected 403, got 200"}}, ClassFixable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, why := Classify(BlockingRun{Run: Run{ID: 42, Name: "Tagged Release"}, RunDetails: tc.details})
			if class != tc.want {
				t.Fatalf("class = %s (%q), want %s", class, why, tc.want)
			}
			if tc.contains != "" && !strings.Contains(why, tc.contains) {
				t.Fatalf("reason %q does not mention %q", why, tc.contains)
			}
			if class == ClassPolicy && !strings.Contains(why, "Tagged Release") {
				t.Fatalf("reason %q does not name the run", why)
			}
		})
	}
}

func TestClassifyFailures_OnePolicyFailureTaintsTheRound(t *testing.T) {
	fixable := BlockingRun{Run: Run{ID: 1, Name: "ci"}, RunDetails: RunDetails{JobCount: 1, Evidence: []string{"FAIL"}}}
	policy := BlockingRun{Run: Run{ID: 2, Name: "release"}, RunDetails: RunDetails{JobCount: 0}}
	if class, why := ClassifyFailures([]BlockingRun{fixable}); class != ClassFixable || why != "" {
		t.Fatalf("fixable only = %s %q", class, why)
	}
	class, why := ClassifyFailures([]BlockingRun{fixable, policy})
	if class != ClassPolicy || !strings.Contains(why, "release") {
		t.Fatalf("mixed = %s %q", class, why)
	}
	if class, _ := ClassifyFailures(nil); class != ClassFixable {
		t.Fatalf("empty = %s", class)
	}
}
