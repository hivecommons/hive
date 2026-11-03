package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Tests for the webhook → sentinel queue (#13379): one evaluation per
// pending PR, duplicates coalesce, the worker drains in order.
func TestSentinelEventQueueCoalescesAndDrains(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	done := make(chan struct{}, 8)
	q := newSentinelEventQueue(func(_ context.Context, repo string, number int) {
		mu.Lock()
		ran = append(ran, repo+"#"+itoa(number))
		mu.Unlock()
		done <- struct{}{}
	})
	if q.Enqueue("", 1) || q.Enqueue("o/r", 0) {
		t.Fatal("empty repo or number must not enqueue")
	}
	if !q.Enqueue("o/r", 1) || q.Enqueue("o/r", 1) || !q.Enqueue("o/r", 2) {
		t.Fatal("first enqueue accepted, duplicate refused, distinct accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not drain")
		}
	}
	mu.Lock()
	got := append([]string(nil), ran...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "o/r#1" || got[1] != "o/r#2" {
		t.Fatalf("ran=%v", got)
	}
	// Once run, the same PR may be queued again (a new head).
	if !q.Enqueue("o/r", 1) {
		t.Fatal("re-enqueue after run should be accepted")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not run the re-enqueued PR")
	}
	var nilQ *sentinelEventQueue
	if nilQ.Enqueue("o/r", 1) {
		t.Fatal("nil queue must refuse")
	}
}
