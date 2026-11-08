package api

import "testing"

func TestRunPaths(t *testing.T) {
	if PathRuns != "/v1/runs" || RunPath("run_a") != "/v1/runs/run_a" || RunCancelPath("run_a") != "/v1/runs/run_a/cancel" {
		t.Fatalf("paths: %s %s %s", PathRuns, RunPath("run_a"), RunCancelPath("run_a"))
	}
	if CodeUnsupported == CodeNotSupported {
		t.Fatal("unsupported-limit refusals must be distinguishable from a not-configured server")
	}
}

func TestRunCmdlogPaths(t *testing.T) {
	for _, tc := range []struct {
		since int64
		limit int
		want  string
	}{
		{0, 0, "/v1/runs/run_a/cmdlog"},
		{3, 0, "/v1/runs/run_a/cmdlog?since_seq=3"},
		{0, 5, "/v1/runs/run_a/cmdlog?limit=5"},
		{3, 5, "/v1/runs/run_a/cmdlog?limit=5&since_seq=3"},
	} {
		if got := RunCmdlogPagePath("run_a", tc.since, tc.limit); got != tc.want {
			t.Errorf("RunCmdlogPagePath(%d, %d) = %q, want %q", tc.since, tc.limit, got, tc.want)
		}
	}
}
