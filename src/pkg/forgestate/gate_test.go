package forgestate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBudgetGateSearchBucket(t *testing.T) {
	clock := newClock()
	m := NewCounters()
	g := NewBudgetGate(BudgetConfig{PerMinute: map[Bucket]int{BucketSearch: 2}, Now: clock.Now, Metrics: m})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := g.Admit(ctx, BucketSearch); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if err := g.Admit(ctx, BucketSearch); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("third search call = %v, want ErrBudgetExhausted", err)
	}
	if got := m.Value(MetricBudgetDenied, "bucket", "search"); got != 1 {
		t.Fatalf("denied metric = %d", got)
	}
	if g.Used(BucketSearch) != 2 {
		t.Fatalf("Used = %d", g.Used(BucketSearch))
	}
	// Core is unlimited unless configured.
	for i := 0; i < 10; i++ {
		if err := g.Admit(ctx, BucketCore); err != nil {
			t.Fatal(err)
		}
	}
	if g.Used(BucketCore) != 10 {
		t.Fatalf("core Used = %d", g.Used(BucketCore))
	}
	// A new window refills the bucket.
	clock.Advance(time.Minute)
	if err := g.Admit(ctx, BucketSearch); err != nil {
		t.Fatalf("after window: %v", err)
	}
	// A clock that steps backwards also starts a new window.
	clock.Advance(-2 * time.Minute)
	if err := g.Admit(ctx, BucketSearch); err != nil || g.Used(BucketSearch) != 1 {
		t.Fatalf("after clock step back: err=%v used=%d", err, g.Used(BucketSearch))
	}
}

func TestBudgetGateCriticalRefusesSearchOnly(t *testing.T) {
	critical := true
	g := NewBudgetGate(BudgetConfig{Critical: func() bool { return critical }})
	ctx := context.Background()
	if err := g.Admit(ctx, BucketSearch); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("search under critical = %v", err)
	}
	if err := g.Admit(ctx, BucketCore); err != nil {
		t.Fatalf("core under critical = %v, want admitted", err)
	}
	critical = false
	if err := g.Admit(ctx, BucketSearch); err != nil {
		t.Fatalf("search after critical cleared = %v", err)
	}
}

func TestBudgetGateCancelledContext(t *testing.T) {
	g := NewBudgetGate(BudgetConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Admit(ctx, BucketCore); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreDefaultGateHasSearchBudget(t *testing.T) {
	clock := newClock()
	s, err := New(Options{Repos: []string{"o/r"}, Fetcher: newFetcher(), SearchBudget: 1, Now: clock.Now, Metrics: NewCounters()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Admit(ctx, BucketSearch); err != nil {
		t.Fatal(err)
	}
	if err := s.Admit(ctx, BucketSearch); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second search = %v, want the configured budget enforced", err)
	}
}

func TestCountersSnapshot(t *testing.T) {
	c := NewCounters()
	c.Reconcile("o/r", resultOK)
	c.Reconcile("o/r", resultOK)
	c.Drift("o/r", 4)
	c.Drift("o/r", 2)
	c.Stale("o/r", true)
	c.WebhookPatch("o/r", resultOK)
	c.WriteThrough("o/r", string(OpMerge))
	c.ReadThrough("o/r", resultCached)
	c.BudgetDenied(BucketSearch)

	if got := c.Value(MetricReconcile, "repo", "o/r", "result", resultOK); got != 2 {
		t.Errorf("reconcile counter = %d, want 2", got)
	}
	if got := c.Value(MetricDrift, "repo", "o/r"); got != 2 {
		t.Errorf("drift gauge = %d, want the latest value 2", got)
	}
	snap := c.Snapshot()
	if len(snap) != 7 {
		t.Fatalf("Snapshot has %d series, want 7: %+v", len(snap), snap)
	}
	for i := 1; i < len(snap); i++ {
		if snap[i-1].Name > snap[i].Name {
			t.Fatalf("Snapshot not sorted: %q before %q", snap[i-1].Name, snap[i].Name)
		}
	}
	if snap[0].Name != MetricBudgetDenied || snap[0].Labels["bucket"] != "search" || snap[0].Value != 1 {
		t.Fatalf("first sample = %+v", snap[0])
	}
	snap[0].Labels["bucket"] = "mutated"
	if c.Snapshot()[0].Labels["bucket"] != "search" {
		t.Fatal("Snapshot must copy labels")
	}
	if c.Value("missing") != 0 {
		t.Fatal("unknown series must read 0")
	}
}
