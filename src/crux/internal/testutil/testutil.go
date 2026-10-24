// Package testutil holds the test helpers the Crux ring's own tests use:
// Eventually and its variants, and the HIVE_TEST_REQUIRE_BEHAVIOURAL skip
// guard. It is a ring-internal copy of the matching helpers in
// internal/testutil so that no ring package imports anything outside the
// ring, even in test code (hivecommons/hive#13536, Stage 2 step 4). At the
// module cut it becomes crux/internal/testutil; the two copies share the
// contract, and internal/testutil keeps the canonical documentation and the
// tests of that contract.
//
// It is test-support code only: nothing in a production package imports it.
package testutil

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// pollInterval is how often Eventually re-evaluates its condition.
const pollInterval = 10 * time.Millisecond

// Eventually polls cond until it returns true or timeout elapses. On the
// deadline it fails the test via t.Fatalf with msg (a printf format) and args.
// See internal/testutil.Eventually for when to use it and when not to.
func Eventually(t testing.TB, timeout time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	EventuallyEvery(t, timeout, pollInterval, cond, msg, args...)
}

// EventuallyEvery is Eventually with a caller-chosen sampling interval.
func EventuallyEvery(t testing.TB, timeout, interval time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (condition not met within %s)", fmt.Sprintf(msg, args...), timeout)
			return
		}
		time.Sleep(interval)
	}
}

// EventuallyValue polls get until it reports ok and returns the value it
// produced, failing the test via t.Fatalf on timeout exactly like Eventually.
func EventuallyValue[T any](t testing.TB, timeout time.Duration, get func() (T, bool), msg string, args ...any) T {
	t.Helper()
	var got T
	Eventually(t, timeout, func() bool {
		v, ok := get()
		if ok {
			got = v
		}
		return ok
	}, msg, args...)
	return got
}

// EventuallyEveryFunc is EventuallyEvery with the failure message produced
// lazily, at the deadline, by describe. describe is called at most once.
func EventuallyEveryFunc(t testing.TB, timeout, interval time.Duration, cond func() bool, describe func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (condition not met within %s)", describe(), timeout)
			return
		}
		time.Sleep(interval)
	}
}

// RequireBehaviouralEnv is the environment variable that turns a permissive
// skip into a failure. It is the same variable internal/testutil and the
// shell suites under src/deploy/ honour, so one flag governs skip discipline
// across the whole test estate (#5388).
const RequireBehaviouralEnv = "HIVE_TEST_REQUIRE_BEHAVIOURAL"

// failureTemplate mirrors internal/testutil's wording: it names the flag and
// states the diagnosis (BROKEN TEST, not unsuitable environment).
const failureTemplate = "%s\n\t%s=1 — the caller asserts this precondition holds here,\n" +
	"\tso this is a BROKEN TEST, not an unsuitable environment (#5388)."

// RequireBehavioural reports whether the caller has asserted that the
// preconditions for behavioural tests hold in this environment.
func RequireBehavioural() bool {
	return os.Getenv(RequireBehaviouralEnv) == "1"
}

// SkipUnlessRequired skips the test with reason, unless
// HIVE_TEST_REQUIRE_BEHAVIOURAL=1, in which case it fails the test instead.
// Like t.Skip and t.Fatal it does not return and must be called from the
// test goroutine.
func SkipUnlessRequired(t testing.TB, reason string) {
	t.Helper()
	if RequireBehavioural() {
		t.Fatalf(failureTemplate, reason, RequireBehaviouralEnv)
	}
	t.Skip(reason)
}

// SkipfUnlessRequired is SkipUnlessRequired with printf-style formatting.
func SkipfUnlessRequired(t testing.TB, format string, args ...any) {
	t.Helper()
	if RequireBehavioural() {
		t.Fatalf(failureTemplate, fmt.Sprintf(format, args...), RequireBehaviouralEnv)
	}
	t.Skipf(format, args...)
}
