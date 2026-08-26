package github

import (
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
