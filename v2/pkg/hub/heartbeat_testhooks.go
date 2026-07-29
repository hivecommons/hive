package hub

import "time"

// This file exposes write access to the process-global heartbeat atomics so
// tests in *other* packages (notably pkg/dashboard, which owns the /api/livez
// handler) can simulate heartbeat states without standing up a real hub and
// heartbeat loop. The production paths that set these live in heartbeat.go.
//
// These are deliberately exported rather than kept in an _test.go file: Go
// only compiles _test.go files into their own package's test binary, so a
// helper defined in hub's tests would be invisible to dashboard's tests.
// Nothing in the production code calls them.

// SetHeartbeatStateForTest overrides the heartbeat loop state used by liveness
// and health reporting. A zero attempt or success time clears that timestamp,
// making it read as "never happened".
func SetHeartbeatStateForTest(enabled bool, lastAttempt, lastSuccess time.Time) {
	heartbeatLoopStarted.Store(enabled)
	storeUnixOrZero(&lastHeartbeatAttemptUnix, lastAttempt)
	storeUnixOrZero(&lastHeartbeatSuccessUnix, lastSuccess)
}

// ResetHeartbeatStateForTest returns the heartbeat atomics to their
// process-start values (no loop, no attempts, no successes). Tests that mutate
// this global state should defer it so they don't leak into other tests.
func ResetHeartbeatStateForTest() {
	SetHeartbeatStateForTest(false, time.Time{}, time.Time{})
}
