package main

import (
	"fmt"
	"os"
	"regexp"
	"testing"
)

// The heartbeat reporter must identify the PROCESS, not just the pod: two hive
// processes in one pod (#2453, #2496) share a hostname, and a bare-hostname
// reporter kept the hub's duplicate-spoke detector blind while the registry
// alternated beat to beat. "<hostname>/<pid>" makes same-pod duplicates
// distinguishable on the hub with no payload schema change.
func TestReporterNameCarriesProcessIdentity(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("no hostname on this machine — reporterName is legitimately empty")
	}
	want := fmt.Sprintf("%s/%d", host, os.Getpid())
	if reporterName != want {
		t.Fatalf("reporterName = %q, want %q (hostname/pid)", reporterName, want)
	}
	// The shape the hub-side conflict rendering relies on ("pod-x/123 ↔
	// pod-x/456"): identity ends in a slash-separated PID.
	if !regexp.MustCompile(`^.+/\d+$`).MatchString(reporterName) {
		t.Fatalf("reporterName %q does not end in /<pid>", reporterName)
	}
}

// singletonLockPath must honour the env override (so tests and unusual
// layouts can redirect it) and never return an empty path otherwise.
func TestSingletonLockPath(t *testing.T) {
	t.Setenv(singletonLockEnv, "/some/where/hive.lock")
	if got := singletonLockPath(); got != "/some/where/hive.lock" {
		t.Fatalf("env override ignored: %q", got)
	}
	t.Setenv(singletonLockEnv, "")
	if got := singletonLockPath(); got == "" {
		t.Fatal("default lock path is empty")
	}
}
