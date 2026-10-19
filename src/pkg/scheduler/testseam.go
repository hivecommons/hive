package scheduler

// This file exists solely to give tests in OTHER packages a supported way to
// redirect the scheduler's on-disk policy roots. It is deliberately NOT a
// _test.go file: Go does not export test files across package boundaries, and
// pkg/dashboard's tests exercise Scheduler.ResolveTemplate, which reads those
// roots. See hivecommons/hive#7477.

import (
	"path/filepath"
	"sync"
)

// SetPolicyDirsForTest points userSavedPolicyDir and clonedPoliciesDir at the
// given directories and returns a function that restores the previous values.
// When userSaved is set, userOverridePolicyDir is pinned to a (normally
// absent) subdirectory of it so the override probe is hermetic too; use
// SetPolicyOverrideDirForTest to point it somewhere specific.
//
// It is intended for tests ONLY. Production code must never call it: the real
// roots are the fixed /data/policies paths that the dashboard prompt editor
// writes to and the policies clone lands in.
//
// Why it is exported from a non-test file: template resolution
// (Scheduler.resolveNamedTemplate) consults these roots BEFORE the embedded
// defaults in pkg/policies/defaults. On a host running a live hive,
// /data/policies is populated, so a test in pkg/dashboard that asserts the
// embedded-default branch silently asserts against whatever that host happens
// to have on disk. Tests pin the roots to a t.TempDir() to get a hermetic
// result, and use the returned restore func with t.Cleanup so no global state
// survives the test.
//
//	restore := scheduler.SetPolicyDirsForTest(t.TempDir(), t.TempDir())
//	t.Cleanup(restore)
func SetPolicyDirsForTest(userSaved, cloned string) (restore func()) {
	return setPolicyDirsForTest("", userSaved, cloned)
}

func setPolicyDirsForTest(agentHome, userSaved, cloned string) (restore func()) {
	policyDirMu.Lock()
	prevAgentHome, prevUserSaved, prevCloned, prevOverride := agentHomeDir, userSavedPolicyDir, clonedPoliciesDir, userOverridePolicyDir
	if agentHome != "" {
		agentHomeDir = agentHome
	}
	if userSaved != "" {
		userSavedPolicyDir = userSaved
		userOverridePolicyDir = filepath.Join(userSaved, ".user-overrides")
	}
	if cloned != "" {
		clonedPoliciesDir = cloned
	}
	policyDirMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			policyDirMu.Lock()
			defer policyDirMu.Unlock()
			agentHomeDir, userSavedPolicyDir, clonedPoliciesDir, userOverridePolicyDir = prevAgentHome, prevUserSaved, prevCloned, prevOverride
		})
	}
}

// SetPolicyOverrideDirForTest points userOverridePolicyDir (the writable
// override root the dashboard saves to when the shipped template is read-only,
// hivecommons/hive#13005) at dir and returns a restore func. Tests ONLY.
func SetPolicyOverrideDirForTest(dir string) (restore func()) {
	policyDirMu.Lock()
	prev := userOverridePolicyDir
	userOverridePolicyDir = dir
	policyDirMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			policyDirMu.Lock()
			defer policyDirMu.Unlock()
			userOverridePolicyDir = prev
		})
	}
}
