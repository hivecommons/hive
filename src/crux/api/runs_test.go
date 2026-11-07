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
