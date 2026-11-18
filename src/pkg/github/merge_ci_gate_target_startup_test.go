package github

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// GitHub blocks pull_request_target workflows by default in public repositories
// from 2026-12-02: the run ends as a zero-job startup_failure. It must not turn
// the verdict red when the required contexts are known and green, and must
// keep doing so otherwise (hivecommons/hive#19038).
func TestMergeCIGate_TargetStartupFailure(t *testing.T) {
	targetRun := ciRun{id: 21, name: "PR Verifier", status: "completed", conc: "startup_failure", event: "pull_request_target", jobs: 0}
	tests := []struct {
		name         string
		fixture      func() *ciFixture
		requiredSet  map[string]bool
		wantMerge    bool
		wantErrParts []string
	}{
		{
			name: "required set known and green: logged, merges",
			fixture: func() *ciFixture {
				f := greenFixture()
				f.runs = append(f.runs, targetRun)
				return f
			},
			requiredSet: map[string]bool{"build": true},
			wantMerge:   true,
		},
		{
			name: "required set known but a required context is missing: still red",
			fixture: func() *ciFixture {
				f := greenFixture()
				f.runs = append(f.runs, targetRun)
				f.requiredChecks = []string{"build", "verify PR contents"}
				return f
			},
			requiredSet:  map[string]bool{"build": true, "verify PR contents": true},
			wantErrParts: []string{"PR Verifier", "without producing a job"},
		},
		{
			name: "required set known but a required context is red: still red",
			fixture: func() *ciFixture {
				f := greenFixture()
				f.runs = append(f.runs, targetRun)
				f.checks = []ciCheck{{name: "build", status: "completed", conclusion: "failure"}}
				return f
			},
			requiredSet:  map[string]bool{"build": true},
			wantErrParts: []string{"PR Verifier", "without producing a job"},
		},
		{
			name: "a startup_failure of another event stays red",
			fixture: func() *ciFixture {
				f := greenFixture()
				f.runs = append(f.runs, ciRun{id: 22, name: "CI", status: "completed", conc: "startup_failure", event: "pull_request", jobs: 0})
				return f
			},
			requiredSet:  map[string]bool{"build": true},
			wantErrParts: []string{`"CI"(22)`, "without producing a job"},
		},
		{
			name: "a failure of the target event that is not a startup failure stays red",
			fixture: func() *ciFixture {
				f := greenFixture()
				f.runs = append(f.runs, ciRun{id: 23, name: "PR Verifier", status: "completed", conc: "failure", event: "pull_request_target", jobs: 0})
				return f
			},
			requiredSet:  map[string]bool{"build": true},
			wantErrParts: []string{`"PR Verifier"(23)`, "without producing a job"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fixture()
			var merges atomic.Int32
			srv := ciGateServer(t, f, &merges)
			defer srv.Close()
			c := testMergeClient(t, srv.URL)
			c.SetRequiredChecks(tc.requiredSet)

			reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
			c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

			if got := merges.Load() == 1; got != tc.wantMerge {
				t.Fatalf("merged = %t, want %t", got, tc.wantMerge)
			}
			if tc.wantMerge {
				return
			}
			resp := readMergeResult(t, reqPath)
			for _, part := range tc.wantErrParts {
				if resp.OK || !strings.Contains(resp.Error, part) {
					t.Errorf("result %+v, want a refusal mentioning %q", resp, part)
				}
			}
		})
	}
}
