package github

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A secondary-limit 403 (Retry-After present) must engage the caution window,
// and cautious requests must be globally paced — the post-reset wave becomes a
// trickle instead of the stampede that re-tripped the limit hourly on
// kubestellar/console (2026-08-23).
func TestSlowStart_PacesAfterSecondaryLimit(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		starts = append(starts, time.Now())
		mu.Unlock()
		if n == 1 {
			// First request trips the secondary limit.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := newSlowStartTransport(http.DefaultTransport)
	tr.state.gap = 60 * time.Millisecond
	tr.state.jitter = time.Millisecond
	tr.state.window = time.Second
	client := &http.Client{Transport: tr}

	// Trip the limit.
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Burst: with Retry-After=0 the reset is immediate; the caution window is
	// what must pace the burst.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, gerr := client.Get(srv.URL)
			if gerr == nil {
				r.Body.Close()
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 5 {
		t.Fatalf("want 5 requests, got %d", len(starts))
	}
	// The 4 cautious requests (2..5) must be spaced by >= gap (small epsilon
	// for scheduler slop).
	for i := 2; i < len(starts); i++ {
		d := starts[i].Sub(starts[i-1])
		if d < tr.state.gap-10*time.Millisecond {
			t.Errorf("requests %d->%d only %v apart, want >= %v (stampede not paced)", i-1, i, d, tr.state.gap)
		}
	}
}

// A permissions-style 403 (no Retry-After) must NOT engage pacing, and
// requests outside a caution window run unpaced.
func TestSlowStart_Plain403AndNormalTrafficUnpaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // no Retry-After
	}))
	defer srv.Close()

	tr := newSlowStartTransport(http.DefaultTransport)
	tr.state.gap = time.Hour // if pacing engaged, the test would hang far past its deadline
	tr.state.window = time.Hour
	client := &http.Client{Transport: tr}

	start := time.Now()
	for i := 0; i < 3; i++ {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("plain 403s must not engage pacing; 3 requests took %v", elapsed)
	}
}
