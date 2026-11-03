package crux

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSandboxRun_Duration(t *testing.T) {
	start := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		run  SandboxRun
		want time.Duration
	}{
		{"complete record", SandboxRun{StartedAt: start, EndedAt: start.Add(29 * time.Minute)}, 29 * time.Minute},
		{"zero start", SandboxRun{EndedAt: start}, 0},
		{"zero record", SandboxRun{}, 0},
		{"ended before started", SandboxRun{StartedAt: start, EndedAt: start.Add(-time.Second)}, 0},
		{"ended at start", SandboxRun{StartedAt: start, EndedAt: start}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.run.Duration(); got != tc.want {
				t.Fatalf("Duration() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSandboxRunUsage_Total(t *testing.T) {
	if got := (SandboxRunUsage{}).Total(); got != 0 {
		t.Fatalf("zero usage Total() = %d, want 0", got)
	}
	u := SandboxRunUsage{Model: "m", Input: 1, Output: 20, CacheRead: 300, CacheCreate: 4000}
	if got := u.Total(); got != 4321 {
		t.Fatalf("Total() = %d, want 4321 (sum of all four counters)", got)
	}
}

func TestWorkDir(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", "")
	if got := WorkDir(); got != DefaultWorkDir {
		t.Fatalf("WorkDir() with HIVE_WORK_DIR unset = %q, want %q", got, DefaultWorkDir)
	}
	t.Setenv("HIVE_WORK_DIR", "/srv/hive")
	if got := WorkDir(); got != "/srv/hive" {
		t.Fatalf("WorkDir() with HIVE_WORK_DIR set = %q, want /srv/hive", got)
	}
}

func TestSandboxRunPaths(t *testing.T) {
	dir := SandboxRunsDir("/work")
	if dir != filepath.Join("/work", "sandbox-runs") {
		t.Fatalf("SandboxRunsDir = %q", dir)
	}
	if got, want := SandboxRunHistoryPath("/work"), filepath.Join(dir, "runs.jsonl"); got != want {
		t.Fatalf("SandboxRunHistoryPath = %q, want %q", got, want)
	}
}
