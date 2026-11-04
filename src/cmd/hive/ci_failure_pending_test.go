package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/github"
)

func pendingCIFailureTest(t *testing.T) (*ciFailureKicker, github.CIFailureSignal, *time.Time, *[]time.Duration) {
	t.Helper()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetCIFailureSignalsForTest(func() time.Time { return now }))
	k, _ := newCIFailureKickerForTest(t, ciKickTestConfig())
	k.now = func() time.Time { return now }
	var delays []time.Duration
	k.wait = func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		now = now.Add(delay)
		return true
	}
	sig := github.CIFailureSignal{Repo: "acme/widgets", Number: 7, HeadSHA: "abc", Check: "build"}
	github.CIFailureSignals().Record(sig)
	return k, sig, &now, &delays
}

func TestCIFailureKicker_BusyRetryAndPendingDedupe(t *testing.T) {
	k, sig, now, delays := pendingCIFailureTest(t)
	var logs bytes.Buffer
	k.logger = slog.New(slog.NewTextHandler(&logs, nil))
	attempts, delivered := 0, 0
	busy := true
	k.send = func(context.Context, string, string) error {
		attempts++
		if busy {
			return fmt.Errorf("prompt not reached: %w", crux.ErrAgentBusy)
		}
		delivered++
		return nil
	}
	if out := k.React(sig); !out.Kicked {
		t.Fatalf("first dispatch: %+v", out)
	}
	if attempts != 3 || len(k.pending) != 1 || len(*delays) != 2 || (*delays)[0] != time.Second || (*delays)[1] != 2*time.Second {
		t.Fatalf("attempts=%d pending=%d delays=%v", attempts, len(k.pending), *delays)
	}
	if out := k.React(sig); out.Kicked || out.Reason != "pending" {
		t.Fatalf("duplicate signal: %+v", out)
	}
	k.Drain() // backoff has not elapsed
	if attempts != 3 {
		t.Fatal("drain ignored backoff")
	}
	busy = false
	*now = now.Add(4 * time.Second)
	k.Drain() // same head is still inside the webhook's cooldown
	k.Drain()
	if attempts != 4 || delivered != 1 || len(k.pending) != 0 {
		t.Fatalf("attempts=%d delivered=%d pending=%d", attempts, delivered, len(k.pending))
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("recoverable busy kick warned: %s", logs.String())
	}
}

func TestCIFailureKicker_BusyFirstAttemptRecoversWithBackoff(t *testing.T) {
	k, sig, _, delays := pendingCIFailureTest(t)
	attempts := 0
	k.send = func(context.Context, string, string) error {
		attempts++
		if attempts == 1 {
			return crux.ErrAgentBusy
		}
		return nil
	}
	k.React(sig)
	if attempts != 2 || len(*delays) != 1 || (*delays)[0] != time.Second || len(k.pending) != 0 {
		t.Fatalf("attempts=%d delays=%v pending=%d", attempts, *delays, len(k.pending))
	}
}

func TestCIFailureKicker_PendingKeysIncludeRepoPRAndCheck(t *testing.T) {
	k, sig, now, _ := pendingCIFailureTest(t)
	k.send = func(context.Context, string, string) error { return crux.ErrAgentBusy }
	k.React(sig)
	*now = now.Add(k.cfg().CIFailureKickCooldown() + time.Second)
	otherCheck, otherPR, otherRepo := sig, sig, sig
	otherCheck.Check = "lint"
	otherPR.Number, otherPR.HeadSHA = 8, "def"
	otherRepo.Repo = "acme/other"
	for _, other := range []github.CIFailureSignal{otherCheck, otherPR, otherRepo} {
		github.CIFailureSignals().Record(other)
		if out := k.React(other); !out.Kicked {
			t.Fatalf("distinct signal: %+v", out)
		}
	}
	if out := k.React(sig); out.Kicked || out.Reason != "pending" || len(k.pending) != 4 {
		t.Fatalf("duplicate after cooldown: %+v pending=%d", out, len(k.pending))
	}
	*now = now.Add(3 * time.Hour)
	k.Drain()
	if len(k.pending) != 0 {
		t.Fatalf("expired signals retained: %d", len(k.pending))
	}
}

func TestCIFailureKicker_ExhaustionWarnsOnce(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%t", busy), func(t *testing.T) {
			k, sig, now, _ := pendingCIFailureTest(t)
			var logs bytes.Buffer
			k.logger = slog.New(slog.NewTextHandler(&logs, nil))
			attempts := 0
			k.send = func(context.Context, string, string) error {
				attempts++
				if busy {
					return crux.ErrAgentBusy
				}
				return errors.New("pane gone")
			}
			k.React(sig)
			for range ciFailureKickMaxAttempts + 2 {
				*now = now.Add(time.Minute)
				k.Drain()
			}
			want := 1
			if busy {
				want = ciFailureKickMaxAttempts
			}
			if attempts != want || len(k.pending) != 0 || strings.Count(logs.String(), "level=WARN") != 1 || !strings.Contains(logs.String(), fmt.Sprintf("attempts=%d", want)) {
				t.Fatalf("attempts=%d pending=%d logs=%s", attempts, len(k.pending), logs.String())
			}
		})
	}
}

func TestCIFailureKicker_DeferredGatesAndResolution(t *testing.T) {
	k, sig, now, _ := pendingCIFailureTest(t)
	attempts := 0
	k.send = func(context.Context, string, string) error {
		attempts++
		return crux.ErrAgentBusy
	}
	k.React(sig)
	*now = now.Add(time.Minute)
	k.isPaused = func(string) bool { return true }
	k.Drain()
	if attempts != 3 || len(k.pending) != 1 {
		t.Fatalf("paused retry: attempts=%d pending=%d", attempts, len(k.pending))
	}
	// The GitHub webhook/enrichment path forgets green and closed PRs.
	github.CIFailureSignals().Forget(sig.Repo, sig.Number)
	k.Drain()
	if attempts != 3 || len(k.pending) != 0 {
		t.Fatalf("resolved PR: attempts=%d pending=%d", attempts, len(k.pending))
	}
}

func TestCIFailureKicker_PendingBoundAndInFlightDedupe(t *testing.T) {
	k, sig, _, _ := pendingCIFailureTest(t)
	var workers []func()
	k.spawn = func(fn func()) { workers = append(workers, fn) }
	for number := 1; number <= ciFailureKickMaxPending; number++ {
		sig.Number = number
		sig.HeadSHA = fmt.Sprintf("head-%d", number)
		github.CIFailureSignals().Record(sig)
		if out := k.React(sig); !out.Kicked {
			t.Fatalf("entry %d: %+v", number, out)
		}
	}
	if out := k.React(sig); out.Kicked || out.Reason != "pending" {
		t.Fatalf("in-flight duplicate: %+v", out)
	}
	sig.Number++
	if out := k.React(sig); out.Kicked || out.Reason != "pending queue full" {
		t.Fatalf("overflow: %+v", out)
	}
	k.Drain()
	if len(workers) != ciFailureKickMaxPending || len(k.pending) > ciFailureKickMaxPending {
		t.Fatalf("workers=%d pending=%d", len(workers), len(k.pending))
	}
	for _, worker := range workers {
		worker()
	}
	if len(k.pending) != 0 {
		t.Fatalf("pending after success=%d", len(k.pending))
	}
}

func TestCIFailureKicker_CancelDuringBackoff(t *testing.T) {
	k, sig, _, _ := pendingCIFailureTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k.ctx = ctx
	attempts := 0
	k.send = func(context.Context, string, string) error {
		attempts++
		return crux.ErrAgentBusy
	}
	// Use the production timer helper after cancelling, without a real sleep.
	k.wait = func(ctx context.Context, delay time.Duration) bool {
		cancel()
		k.wait = nil
		return k.waitBackoff(ctx, delay)
	}
	k.React(sig)
	k.Drain()
	if attempts != 1 || len(k.pending) != 0 {
		t.Fatalf("cancelled: attempts=%d pending=%d", attempts, len(k.pending))
	}
}
