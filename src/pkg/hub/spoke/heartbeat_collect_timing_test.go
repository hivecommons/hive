package spoke

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests pin the diagnosis and recovery added after the hosted hive kept
// sending MINIMAL beats on a build that had already moved the agent transcript
// walk off the collect path (#13444): a 10 s budget blown every beat by a
// collector nobody had measured. collectWithTimeout now (a) names the slowest
// sections on timeout and (b) caches a collect that finishes LATE as the
// last-good payload so the next beat carries stats instead of identity only.

// TestTimedSection_RecordsFinishedAndInFlight: a finished section is reported
// with its duration and a section still running at report time is marked
// in-flight; the slowest sorts first.
func TestTimedSection_RecordsFinishedAndInFlight(t *testing.T) {
	c := newCollectTimings()
	currentCollectTimings.Store(c)
	t.Cleanup(func() { currentCollectTimings.CompareAndSwap(c, nil) })

	if got := TimedSection("fast", func() int { return 7 }); got != 7 {
		t.Fatalf("TimedSection returned %d, want 7 (must pass the value through)", got)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		TimedSection("slow", func() struct{} {
			close(started)
			<-release
			return struct{}{}
		})
	}()
	<-started
	// Pin the in-flight section's start into the past so ordering does not
	// depend on scheduler timing.
	c.mu.Lock()
	c.inFlight["slow"] = time.Now().Add(-time.Second)
	c.mu.Unlock()

	rows := c.slowest(collectTimingTopN)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want fast + slow", rows)
	}
	if rows[0].Name != "slow" || !rows[0].InFlight {
		t.Fatalf("slowest row = %+v, want the in-flight slow section first", rows[0])
	}
	if rows[1].Name != "fast" || rows[1].InFlight {
		t.Fatalf("second row = %+v, want finished fast section", rows[1])
	}
	s := c.summary(collectTimingTopN)
	if !strings.HasPrefix(s, "slow=") || !strings.Contains(s, "(in-flight)") || !strings.Contains(s, " fast=") {
		t.Fatalf("summary = %q, want slow=...(in-flight) fast=...", s)
	}

	close(release)
	<-finished
	rows = c.slowest(collectTimingTopN)
	for _, r := range rows {
		if r.InFlight {
			t.Fatalf("after completion row %+v still in-flight", r)
		}
	}
}

// TestTimedSection_NoRecorderJustRuns: outside a collect there is nothing to
// record into; the section runs and its value is returned.
func TestTimedSection_NoRecorderJustRuns(t *testing.T) {
	currentCollectTimings.Store(nil)
	ran := false
	if got := TimedSection("x", func() string { ran = true; return "ok" }); got != "ok" || !ran {
		t.Fatalf("TimedSection without recorder = %q ran=%v", got, ran)
	}
}

// TestCollectTimings_SlowestCapsAndSorts: more sections than the cap are
// trimmed to the N slowest, descending.
func TestCollectTimings_SlowestCapsAndSorts(t *testing.T) {
	c := newCollectTimings()
	for i, d := range []time.Duration{3 * time.Second, time.Second, 5 * time.Second, 2 * time.Second, 4 * time.Second, 6 * time.Second} {
		c.finished[string(rune('a'+i))] = d
	}
	rows := c.slowest(3)
	if len(rows) != 3 || rows[0].Elapsed != 6*time.Second || rows[1].Elapsed != 5*time.Second || rows[2].Elapsed != 4*time.Second {
		t.Fatalf("slowest(3) = %+v", rows)
	}
	if newCollectTimings().summary(3) != "" {
		t.Fatal("summary of an empty recorder must be empty so no breakdown line is logged")
	}
}

// TestCollectWithTimeout_LogsSlowestSectionsOnTimeout: the timeout path emits
// the breakdown naming the section that was still running.
func TestCollectWithTimeout_LogsSlowestSectionsOnTimeout(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, nil))

	release := make(chan struct{})
	exited := make(chan struct{})
	t.Cleanup(func() { close(release); <-exited })
	collect := func() *HeartbeatPayload {
		defer close(exited)
		TimedSection("cheap", func() int { return 1 })
		return TimedSection("evidence", func() *HeartbeatPayload {
			<-release
			return &HeartbeatPayload{HiveID: "late"}
		})
	}

	if got := collectWithTimeout(context.Background(), collect, 20*time.Millisecond, logger); got != nil {
		t.Fatalf("expected nil on timeout, got %+v", got)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "slowest collectors") || !strings.Contains(out, "evidence=") || !strings.Contains(out, "(in-flight)") {
		t.Fatalf("timeout log lacks the per-section breakdown naming the in-flight collector:\n%s", out)
	}
	if currentCollectTimings.Load() != nil {
		t.Fatal("recorder must be cleared when collectWithTimeout returns")
	}
}

// TestCollectWithTimeout_LateCompletionPopulatesLastGood is the fix for the
// permanent-MINIMAL symptom: a collect that misses the budget but does finish
// becomes the last-good payload, so the NEXT beat sends stale stats, not an
// identity-only beat.
func TestCollectWithTimeout_LateCompletionPopulatesLastGood(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()

	if loadLastGoodPayload() != nil {
		t.Fatal("precondition: last-good cache must start empty")
	}

	release := make(chan struct{})
	stored := make(chan struct{})
	collect := func() *HeartbeatPayload {
		<-release
		return &HeartbeatPayload{HiveID: "slow-hive", Org: "org", Agents: []AgentSummary{{Name: "a"}}}
	}
	// Wrap so the test can observe when the goroutine has finished its
	// post-collect work (the store happens before the buffered send).
	wrapped := func() *HeartbeatPayload {
		defer close(stored)
		return collect()
	}

	if got := collectWithTimeout(context.Background(), wrapped, 20*time.Millisecond, nil2Logger()); got != nil {
		t.Fatalf("expected nil on timeout, got %+v", got)
	}
	if loadLastGoodPayload() != nil {
		t.Fatal("nothing must be cached before the late collect completes")
	}

	close(release)
	select {
	case <-stored:
	case <-time.After(2 * time.Second):
		t.Fatal("late collect never completed")
	}
	// The store runs after collect() returns inside the goroutine; wait for
	// it to become visible rather than sleeping.
	deadline := time.After(2 * time.Second)
	for {
		if cached := loadLastGoodPayload(); cached != nil {
			if cached.HiveID != "slow-hive" || len(cached.Agents) != 1 {
				t.Fatalf("cached payload = %+v, want the late collect's full payload", cached)
			}
			if id := minimalLivenessPayload(); id == nil || id.HiveID != "slow-hive" {
				t.Fatalf("identity = %+v, want published from the late payload", id)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("late collect's payload was never cached as last-good — the next beat would be MINIMAL again")
		default:
		}
	}
}

// TestCollectWithTimeout_InTimeCompletionDoesNotDoubleStore: a collect that
// finishes within budget is returned to the caller (who caches it); the
// goroutine itself must not store, or a nil-on-purpose payload would be cached.
func TestCollectWithTimeout_InTimeCompletionDoesNotDoubleStore(t *testing.T) {
	t.Cleanup(ResetHeartbeatStateForTest)
	ResetHeartbeatStateForTest()
	got := collectWithTimeout(context.Background(), func() *HeartbeatPayload {
		return &HeartbeatPayload{HiveID: "fast"}
	}, time.Second, nil2Logger())
	if got == nil || got.HiveID != "fast" {
		t.Fatalf("got %+v", got)
	}
	if loadLastGoodPayload() != nil {
		t.Fatal("in-time collect must leave caching to sendHeartbeat")
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
