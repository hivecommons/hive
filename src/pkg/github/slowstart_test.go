package github

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	// Generous window: the burst must still be inside it even if a loaded
	// host delays the goroutines. Pacing cost stays 3 gaps regardless.
	tr.state.window = time.Minute
	client := &http.Client{Transport: tr}

	// Trip the limit.
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Burst: with Retry-After=0 the reset is immediate; the caution window is
	// what must pace the burst.
	burstStart := time.Now()
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
	elapsed := time.Since(burstStart)

	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 5 {
		t.Fatalf("want 5 requests, got %d", len(starts))
	}
	// Each cautious request claims a slot >= gap after the previous claim, so
	// a 4-request burst cannot finish in under 3 gaps. Assert that elapsed
	// lower bound rather than pairwise server-observed arrival gaps: on a
	// loaded host, scheduler and connection latency can COMPRESS the observed
	// spacing between two requests (an early request delayed toward the next
	// one's slot), but can only INCREASE total elapsed time — an unpaced
	// stampede still completes near-instantly and fails this check.
	if want := 3 * tr.state.gap; elapsed < want {
		t.Errorf("4 cautious requests completed in %v, want >= %v (stampede not paced)", elapsed, want)
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

// A secondary-limit 403 WITHOUT Retry-After (GitHub does not reliably send
// it — the 2026-08-23 re-trip slipped past header-only detection) must still
// engage caution via the body phrase, and the peeked body must be restored
// intact for downstream error decoding.
func TestSlowStart_BodySniffEngagesCaution(t *testing.T) {
	body := `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"x"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // deliberately no Retry-After
		io.WriteString(w, body)
	}))
	defer srv.Close()

	tr := newSlowStartTransport(http.DefaultTransport)
	tr.state.window = time.Hour
	client := &http.Client{Transport: tr}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != body {
		t.Errorf("peeked body not restored: got %q", got)
	}

	tr.state.mu.Lock()
	cautious := time.Now().Before(tr.state.cautiousUntil)
	tr.state.mu.Unlock()
	if !cautious {
		t.Fatal("body-sniffed secondary 403 must engage the caution window")
	}
}

func TestSlowStart_CancelledWaiterReclaimsSlot(t *testing.T) {
	starts := make(chan time.Time, 2)
	tr := newSlowStartTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		starts <- time.Now()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
	}))
	tr.state.gap = 120 * time.Millisecond
	tr.state.jitter = 0
	tr.state.deadlineMargin = time.Millisecond
	now := time.Now()
	tr.state.cautiousUntil = now.Add(time.Second)
	initialSlot := now.Add(80 * time.Millisecond)
	tr.state.nextSlot = initialSlot
	client := &http.Client{Transport: tr}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos/hivecommons/hive", nil)
		if err != nil {
			done <- err
			return
		}
		_, err = client.Do(req)
		done <- err
	}()

	waitForSlowStartSlotClaim(t, tr.state, initialSlot)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled waiter unexpectedly reached inner transport")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for cancelled waiter")
	}

	followStart := time.Now()
	go func() {
		resp, err := client.Get("https://api.github.test/repos/hivecommons/hive")
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
	select {
	case started := <-starts:
		if elapsed := started.Sub(followStart); elapsed > tr.state.gap {
			t.Fatalf("follow-on request waited %v; abandoned slot was not reclaimed", elapsed)
		}
	case <-time.After(tr.state.gap):
		t.Fatal("follow-on request missed the reclaimed slot")
	}
}

func TestSlowStart_DeadlineMissFailsFastWithoutClaimingSlot(t *testing.T) {
	innerCalls := make(chan struct{}, 1)
	tr := newSlowStartTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		innerCalls <- struct{}{}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
	}))
	tr.state.gap = 100 * time.Millisecond
	tr.state.jitter = 0
	tr.state.deadlineMargin = 10 * time.Millisecond
	now := time.Now()
	tr.state.cautiousUntil = now.Add(time.Second)
	tr.state.nextSlot = now.Add(80 * time.Millisecond)
	wantNextSlot := tr.state.nextSlot

	ctx, cancel := context.WithDeadline(context.Background(), now.Add(40*time.Millisecond))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos/hivecommons/hive", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&http.Client{Transport: tr}).Do(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got error %v, want context deadline exceeded", err)
	}
	tr.state.mu.Lock()
	gotNextSlot := tr.state.nextSlot
	tr.state.mu.Unlock()
	if !gotNextSlot.Equal(wantNextSlot) {
		t.Fatalf("nextSlot advanced to %v, want unchanged %v", gotNextSlot, wantNextSlot)
	}
	select {
	case <-innerCalls:
		t.Fatal("hopeless request reached inner transport")
	default:
	}
}

func TestSlowStart_OverCapacityDemandStillMakesProgress(t *testing.T) {
	tr := newSlowStartTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
	}))
	tr.state.gap = 20 * time.Millisecond
	tr.state.jitter = 0
	tr.state.deadlineMargin = time.Millisecond
	tr.state.cautiousUntil = time.Now().Add(time.Second)
	client := &http.Client{Transport: tr}

	const requests = 20
	var wg sync.WaitGroup
	successes := make(chan struct{}, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/repos/hivecommons/hive", nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				successes <- struct{}{}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("over-capacity demand did not drain")
	}
	if got := len(successes); got == 0 {
		t.Fatal("over-capacity demand completed with zero successful paced requests")
	}
}

func waitForSlowStartSlotClaim(t *testing.T, st *slowStartState, initialSlot time.Time) {
	t.Helper()
	deadline := time.After(500 * time.Millisecond)
	for {
		st.mu.Lock()
		claimed := st.nextSlot.After(initialSlot) && len(st.abandonedSlots) == 0
		st.mu.Unlock()
		if claimed {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for slow-start slot claim")
		case <-time.After(time.Millisecond):
		}
	}
}
