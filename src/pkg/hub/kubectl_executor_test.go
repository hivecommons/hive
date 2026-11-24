package hub

import (
	"context"
	"os"
	"path/filepath"
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
			release, err := acquireKubectlSlot(context.Background(), "test-cluster-bound", "test")
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
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
		r, err := acquireKubectlSlot(context.Background(), "test-cluster-a", "test")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	done := make(chan struct{})
	go func() {
		if r, err := acquireKubectlSlot(context.Background(), "test-cluster-b", "test"); err == nil {
			r()
		}
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

// A slot wait ends with an error when its context is cancelled, and the
// exhaustion counter moves.
func TestAcquireKubectlSlotContextCancelled(t *testing.T) {
	const cluster = "test-cluster-ctx"
	var releases []func()
	for i := 0; i < kubectlMaxPerCluster(); i++ {
		r, err := acquireKubectlSlot(context.Background(), cluster, "sleep forever")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	defer func() {
		for _, r := range releases {
			r()
		}
	}()

	before := KubectlPoolExhaustedTotal()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := acquireKubectlSlot(ctx, cluster, "waiter")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the pool is full and ctx expires")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slot wait did not honour context cancellation")
	}
	if KubectlPoolExhaustedTotal() != before+1 {
		t.Errorf("pool-exhausted counter did not increment")
	}
}

// A hung kubectl is killed at its context deadline and releases its slot.
func TestKubectlCmdTimeoutReleasesSlot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cluster := &ClusterConfig{ID: "test-cluster-hung", InCluster: true}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := kubectlForClusterContext(ctx, cluster, "get", "pods").Output(); err == nil {
		t.Fatal("expected the hung command to fail at its deadline")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("hung command was not killed at its timeout")
	}
	kubectlSlotsMu.Lock()
	inUse := kubectlInUse[cluster.ID]
	kubectlSlotsMu.Unlock()
	if inUse != 0 {
		t.Fatalf("slot not released after timeout, in use = %d", inUse)
	}
}
