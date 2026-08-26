package hub

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestAcquireKubectlSlotBoundsConcurrency verifies the per-cluster semaphore
// never admits more than the configured cap simultaneously and that separate
// clusters get independent pools.
func TestAcquireKubectlSlotBoundsConcurrency(t *testing.T) {
	cap := kubectlMaxPerCluster()
	if cap < 1 {
		t.Fatalf("kubectlMaxPerCluster() = %d, want >= 1", cap)
	}

	var inFlight, peak int64
	var wg sync.WaitGroup
	for i := 0; i < cap*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := acquireKubectlSlot("test-cluster-bound")
			defer release()
			n := atomic.AddInt64(&inFlight, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
					break
				}
			}
			atomic.AddInt64(&inFlight, -1)
		}()
	}
	wg.Wait()
	if peak > int64(cap) {
		t.Errorf("peak concurrency %d exceeded cap %d", peak, cap)
	}

	// Different clusters must not share a semaphore: filling one cluster's
	// slots must not block another's acquire.
	releases := make([]func(), 0, cap)
	for i := 0; i < cap; i++ {
		releases = append(releases, acquireKubectlSlot("test-cluster-a"))
	}
	done := make(chan struct{})
	go func() {
		r := acquireKubectlSlot("test-cluster-b")
		r()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("acquire on independent cluster blocked by another cluster's slots")
	}
	for _, r := range releases {
		r()
	}
}
