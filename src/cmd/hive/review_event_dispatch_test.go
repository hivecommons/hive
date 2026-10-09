package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/eventdispatch"
)

func firedDispatcher(t *testing.T, heads ...review.PullRequest) *eventdispatch.Dispatcher {
	t.Helper()
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	d := eventdispatch.New(eventdispatch.Options{Now: func() time.Time { return clock }})
	for _, pr := range heads {
		d.Enqueue(eventdispatch.Request{Repo: "org/" + pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA}, time.Second)
	}
	clock = clock.Add(time.Second)
	if got := d.TakeDue(); len(got) != len(heads) {
		t.Fatalf("fired %d, want %d", len(got), len(heads))
	}
	return d
}

func TestPrioritizeEventPRs(t *testing.T) {
	a := review.PullRequest{Repo: "r", Number: 1, HeadSHA: "a"}
	b := review.PullRequest{Repo: "r", Number: 2, HeadSHA: "b"}
	c := review.PullRequest{Repo: "r", Number: 3, HeadSHA: "c"}
	cOld := review.PullRequest{Repo: "r", Number: 3, HeadSHA: "old"}
	tests := []struct {
		name  string
		d     *eventdispatch.Dispatcher
		in    []review.PullRequest
		order []int
	}{
		{"no dispatcher", nil, []review.PullRequest{a, b, c}, []int{1, 2, 3}},
		{"single PR", firedDispatcher(t, c), []review.PullRequest{c}, []int{3}},
		{"nothing fired for these heads", firedDispatcher(t, cOld), []review.PullRequest{a, b, c}, []int{1, 2, 3}},
		{"fired PR moves first", firedDispatcher(t, c), []review.PullRequest{a, b, c}, []int{3, 1, 2}},
		{"stable among fired", firedDispatcher(t, c, b), []review.PullRequest{a, b, c}, []int{2, 3, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := prioritizeEventPRs(tc.in, tc.d)
			if len(got) != len(tc.order) {
				t.Fatalf("len = %d", len(got))
			}
			for i, n := range tc.order {
				if got[i].Number != n {
					t.Fatalf("order = %+v, want %v", got, tc.order)
				}
			}
		})
	}
}

func TestInstallReviewEventsIsIdempotent(t *testing.T) {
	b := &boot{}
	b.installReviewEvents()
	d, wake := b.reviewEvents, b.reviewWake
	if d == nil || wake == nil || activeReviewEvents.Load() != d {
		t.Fatal("dispatcher not installed")
	}
	b.installReviewEvents()
	if b.reviewEvents != d || b.reviewWake != wake {
		t.Fatal("reinstall replaced the dispatcher")
	}
	t.Cleanup(func() { activeReviewEvents.Store(nil) })
}

func TestWakeForReviewEventsNeverBlocks(t *testing.T) {
	b, log, _ := newLoopBoot(t, 60, 0)
	b.reviewWake = make(chan struct{}, 1)
	batch := []eventdispatch.Entry{{Repo: "o/r", Number: 4, HeadSHA: "h"}}
	b.wakeForReviewEvents(batch)
	b.wakeForReviewEvents(batch)
	if len(b.reviewWake) != 1 {
		t.Fatalf("wake channel holds %d, want 1", len(b.reviewWake))
	}
	if !strings.Contains(log.String(), "review webhook dispatch due") {
		t.Fatalf("log = %s", log.String())
	}
	(&boot{reviewWake: make(chan struct{}, 1)}).wakeForReviewEvents(batch)
}

func TestStartReviewEventsWakesTheLoop(t *testing.T) {
	b, _, cancel := newLoopBoot(t, 60, 0)
	(&boot{}).startReviewEvents() // nothing installed: no-op
	start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var offset atomic.Int64
	timers := make(chan chan time.Time, 64)
	b.reviewEvents = eventdispatch.New(eventdispatch.Options{
		Now: func() time.Time { return start.Add(time.Duration(offset.Load())) },
		After: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			timers <- ch
			return ch
		},
	})
	b.installReviewEvents()
	t.Cleanup(func() { activeReviewEvents.Store(nil) })
	b.startReviewEvents()
	b.reviewEvents.Enqueue(eventdispatch.Request{Repo: "o/r", Number: 1, HeadSHA: "h"}, time.Second)
	offset.Store(int64(time.Second))
	deadline := time.After(5 * time.Second)
	for woke := false; !woke; {
		select {
		case <-b.reviewWake:
			woke = true
		case tm := <-timers:
			tm <- start
		case <-deadline:
			t.Fatal("dispatcher did not wake the loop")
		}
	}
	cancel()
}

func TestRunLoopWithRunsAnEarlyCycleOnReviewWake(t *testing.T) {
	b, log, cancel := newLoopBoot(t, 60, 0)
	b.reviewWake = make(chan struct{}, 1)
	gov := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 8)}
	done := runLoopAsync(b, h.deps())
	<-h.persisted // first cycle done

	b.reviewWake <- struct{}{}
	<-h.persisted // early cycle done
	gov.fire()
	<-h.persisted // cadence cycle still runs
	cancel()
	waitDone(t, done)

	if len(h.evals) != 3 {
		t.Fatalf("evals = %d, want first + webhook + cadence", len(h.evals))
	}
	if !strings.Contains(log.String(), "review webhook wake") {
		t.Fatalf("log = %s", log.String())
	}
}
