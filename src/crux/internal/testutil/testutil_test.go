package testutil

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// recorder is a testing.TB double that records Fatalf and Skip calls instead
// of ending the goroutine, so the guard's two branches can be asserted.
type recorder struct {
	testing.TB
	fatal string
	skip  string
}

func (r *recorder) Helper()                           {}
func (r *recorder) Fatalf(format string, args ...any) { r.fatal = fmt.Sprintf(format, args...) }
func (r *recorder) Skip(args ...any)                  { r.skip = fmt.Sprint(args...) }
func (r *recorder) Skipf(format string, args ...any)  { r.skip = fmt.Sprintf(format, args...) }

func TestEventuallyReturnsOnceConditionHolds(t *testing.T) {
	n := 0
	Eventually(t, 2*time.Second, func() bool { n++; return n >= 3 }, "counter never reached 3")
	if n < 3 {
		t.Fatalf("Eventually returned after %d polls", n)
	}
}

func TestEventuallyValueReturnsObservedValue(t *testing.T) {
	n := 0
	got := EventuallyValue(t, 2*time.Second, func() (int, bool) { n++; return n, n >= 2 }, "never ok")
	if got != 2 {
		t.Fatalf("EventuallyValue = %d, want 2", got)
	}
}

func TestEventuallyFailsAtDeadline(t *testing.T) {
	r := &recorder{TB: t}
	EventuallyEvery(r, 20*time.Millisecond, time.Millisecond, func() bool { return false }, "never %s", "true")
	if !strings.Contains(r.fatal, "never true") || !strings.Contains(r.fatal, "condition not met") {
		t.Fatalf("Fatalf message = %q", r.fatal)
	}
}

func TestEventuallyEveryFuncDescribesLazily(t *testing.T) {
	r := &recorder{TB: t}
	calls := 0
	EventuallyEveryFunc(r, 20*time.Millisecond, time.Millisecond, func() bool { return false }, func() string { calls++; return "state" })
	if calls != 1 || !strings.HasPrefix(r.fatal, "state") {
		t.Fatalf("describe called %d times, message %q", calls, r.fatal)
	}
}

func TestSkipUnlessRequiredSkipsByDefault(t *testing.T) {
	t.Setenv(RequireBehaviouralEnv, "")
	r := &recorder{TB: t}
	SkipUnlessRequired(r, "no tmux")
	SkipfUnlessRequired(r, "wrapped: %v", errors.New("boom"))
	if r.fatal != "" || r.skip != "wrapped: boom" {
		t.Fatalf("fatal=%q skip=%q", r.fatal, r.skip)
	}
}

func TestSkipUnlessRequiredFailsWhenArmed(t *testing.T) {
	t.Setenv(RequireBehaviouralEnv, "1")
	r := &recorder{TB: t}
	// The double's Fatalf returns, so the Skipf after it also records; only
	// the Fatalf branch is under test here.
	SkipfUnlessRequired(r, "no %s", "tmux")
	if !strings.Contains(r.fatal, "no tmux") || !strings.Contains(r.fatal, "BROKEN TEST") {
		t.Fatalf("fatal=%q skip=%q", r.fatal, r.skip)
	}
	if !RequireBehavioural() {
		t.Fatal("RequireBehavioural() = false with the flag set")
	}
}
