package scheduler

// This file exists solely to give tests in OTHER packages a supported way to
// redirect the scheduler's on-disk policy roots. It is deliberately NOT a
// _test.go file: Go does not export test files across package boundaries, and
// pkg/dashboard's tests exercise Scheduler.ResolveTemplate, which reads those
// roots. See hivecommons/hive#7477.

import "sync"

// policyDirMu serialises SetPolicyDirsForTest against itself so two tests that
// both install a seam cannot interleave their save/restore and leak a temp dir
// into the package globals. It does NOT make the globals safe to mutate while
// other goroutines read them — a test that installs the seam must not run in
// parallel with one that resolves templates.
var policyDirMu sync.Mutex

// SetPolicyDirsForTest points userSavedPolicyDir and clonedPoliciesDir at the
// given directories and returns a function that restores the previous values.
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
	policyDirMu.Lock()
	defer policyDirMu.Unlock()
	prevUserSaved, prevCloned := userSavedPolicyDir, clonedPoliciesDir
	userSavedPolicyDir, clonedPoliciesDir = userSaved, cloned
	var once sync.Once
	return func() {
		once.Do(func() {
			policyDirMu.Lock()
			defer policyDirMu.Unlock()
			userSavedPolicyDir, clonedPoliciesDir = prevUserSaved, prevCloned
		})
	}
}
