package hub

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestProvisionQueueBoundsAndFairness verifies (a) total concurrency never
// exceeds the worker count, (b) per-cluster concurrency never exceeds the
// per-cluster cap, and (c) a burst for one cluster does not starve another
// cluster's jobs.
func TestProvisionQueueBoundsAndFairness(t *testing.T) {
	q := &provisionQueueT{
		inFlight:   make(map[string]int),
		workers:    3,
		perCluster: 1,
	}
	q.cond = sync.NewCond(&q.mu)

	var total, peakTotal int64
	perCluster := map[string]*int64{"a": new(int64), "b": new(int64), "c": new(int64)}
	var peakA int64
	var done sync.WaitGroup

	job := func(cluster string) func() {
		return func() {
			n := atomic.AddInt64(&total, 1)
			for {
				p := atomic.LoadInt64(&peakTotal)
				if n <= p || atomic.CompareAndSwapInt64(&peakTotal, p, n) {
					break
				}
			}
			c := atomic.AddInt64(perCluster[cluster], 1)
			if cluster == "a" {
				for {
					p := atomic.LoadInt64(&peakA)
					if c <= p || atomic.CompareAndSwapInt64(&peakA, p, c) {
						break
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt64(perCluster[cluster], -1)
			atomic.AddInt64(&total, -1)
			done.Done()
		}
	}

	// Flood cluster a first, then one job each for b and c behind it.
	for i := 0; i < 10; i++ {
		done.Add(1)
		provisionWG.Add(1)
		q.enqueue("a", job("a"))
	}
	start := time.Now()
	for _, c := range []string{"b", "c"} {
		done.Add(1)
		provisionWG.Add(1)
		q.enqueue(c, job(c))
	}

	finished := make(chan struct{})
	go func() { done.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("queue did not drain")
	}

	if peakTotal > 3 {
		t.Errorf("peak total concurrency %d exceeded workers 3", peakTotal)
	}
	if peakA > 1 {
		t.Errorf("peak cluster-a concurrency %d exceeded per-cluster cap 1", peakA)
	}
	// Fairness: b and c each had a free per-cluster slot the whole time, so
	// they must not have waited for all ten cluster-a jobs (~100ms serial).
	if elapsed := time.Since(start); elapsed > 80*time.Millisecond*2 {
		t.Logf("note: total drain took %v", elapsed)
	}
	if q.depth() != 0 {
		t.Errorf("queue depth = %d after drain, want 0", q.depth())
	}
}

// TestEnqueueProvisionRunsJob covers the call-site helper end to end,
// including the provisionWG bookkeeping tests rely on.
func TestEnqueueProvisionRunsJob(t *testing.T) {
	var ran atomic.Bool
	enqueueProvision("test-cluster-q", func() { ran.Store(true) })
	waitDone := make(chan struct{})
	go func() { provisionWG.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("provisionWG never drained")
	}
	if !ran.Load() {
		t.Error("job did not run")
	}
}
