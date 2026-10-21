package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// engageCautionForTest puts the shared pacer into a caution window whose
// queue is already queueDepth ahead of now, and clears it on cleanup.
func engageCautionForTest(t *testing.T, st *slowStartState, queueDepth time.Duration) {
	t.Helper()
	now := time.Now()
	st.mu.Lock()
	st.cautiousUntil = now.Add(time.Hour)
	st.nextSlot = now.Add(queueDepth)
	st.abandonedSlots = nil
	st.mu.Unlock()
	t.Cleanup(ResetRateLimitPacingForTest)
}

type countingRoundTripper struct{ calls atomic.Int32 }

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
}

func TestSlowStartPacing_PriorityLadderShedsByClass(t *testing.T) {
	inner := &countingRoundTripper{}
	st := newSlowStartState()
	tr := &slowStartTransport{inner: inner, state: st}
	engageCautionForTest(t, st, 10*time.Second)
	queued := func() time.Time {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.nextSlot
	}
	before := queued()

	// Low priority: 10s ahead exceeds slowStartLowPriorityMaxQueue; shed
	// without touching the queue.
	req, _ := http.NewRequestWithContext(WithPacingClass(context.Background(), PacingLow), http.MethodGet, "http://x/repos/o/r/pulls", nil)
	_, err := tr.RoundTrip(req)
	if !errors.Is(err, ErrPacingShed) {
		t.Fatalf("low-priority err = %v, want ErrPacingShed", err)
	}
	if !queued().Equal(before) {
		t.Fatal("a shed low-priority request must not advance the queue")
	}

	// Normal: 10s is inside slowStartNormalMaxQueue, so it is not shed; a
	// 1s deadline makes it fail on the deadline rule instead of waiting.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "http://x/repos/o/r/pulls", nil)
	_, err = tr.RoundTrip(req)
	if errors.Is(err, ErrPacingShed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("normal err = %v, want deadline-exceeded (not shed)", err)
	}

	// Normal beyond slowStartNormalMaxQueue is shed; high priority is not.
	engageCautionForTest(t, st, slowStartNormalMaxQueue+10*time.Second)
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "http://x/repos/o/r/issues", nil)
	if _, err = tr.RoundTrip(req); !errors.Is(err, ErrPacingShed) {
		t.Fatalf("deep-queue normal err = %v, want ErrPacingShed", err)
	}
	ctx, cancel = context.WithTimeout(WithPacingClass(context.Background(), PacingHigh), time.Second)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "http://x/repos/o/r/issues", nil)
	_, err = tr.RoundTrip(req)
	if errors.Is(err, ErrPacingShed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("high err = %v, want deadline-exceeded (never shed)", err)
	}
	if inner.calls.Load() != 0 {
		t.Fatalf("inner calls = %d, want 0", inner.calls.Load())
	}

	// Outside caution nothing is paced or shed.
	st.mu.Lock()
	st.cautiousUntil = time.Time{}
	st.nextSlot = time.Time{}
	st.mu.Unlock()
	req, _ = http.NewRequestWithContext(WithPacingClass(context.Background(), PacingLow), http.MethodGet, "http://x/repos/o/r/pulls", nil)
	if _, err = tr.RoundTrip(req); err != nil {
		t.Fatalf("uncautious low err = %v", err)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls.Load())
	}
}

func TestShedLowPriorityTick(t *testing.T) {
	ResetRateLimitPacingForTest()
	if ShedLowPriorityTick("t", 5*time.Second) {
		t.Fatal("not cautious: must not shed")
	}
	engageCautionForTest(t, sharedSlowStartState, 2*time.Second)
	if ShedLowPriorityTick("t", 5*time.Second) {
		t.Fatal("queue 2s inside a 5s budget: must not shed")
	}
	if !ShedLowPriorityTick("t", time.Second) {
		t.Fatal("queue 2s beyond a 1s budget: must shed")
	}
	engageCautionForTest(t, sharedSlowStartState, time.Minute)
	if !ShedLowPriorityTick("t", 0) {
		t.Fatal("queue beyond slowStartLowPriorityMaxQueue: must shed even with no budget")
	}
	if eta := PacingQueueETA(); eta < 50*time.Second {
		t.Fatalf("PacingQueueETA = %s, want ~1m", eta)
	}
}

// Installation-token mints bypass the pacer: with the shared queue minutes
// deep and the mint deadline seconds long, the mint still lands.
func TestInstallationTokenMintBypassesPacer(t *testing.T) {
	key, _ := generateTestKey(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_unpaced","expires_at":"2030-01-02T15:04:05Z"}`))
	}))
	defer server.Close()
	engageCautionForTest(t, sharedSlowStartState, 5*time.Minute)

	auth := &AppAuth{appID: 1, installationID: 42, key: key, logger: quietLogger(), apiURL: server.URL, cachePath: t.TempDir() + "/token-cache"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	token, _, err := auth.mintInstallationToken(ctx)
	if err != nil {
		t.Fatalf("mint under a deep pacing queue: %v", err)
	}
	if token != "ghs_unpaced" || hits.Load() != 1 {
		t.Fatalf("token = %q hits = %d, want ghs_unpaced / 1", token, hits.Load())
	}

	// The same queue still paces ordinary installation traffic, proving the
	// bypass is specific to the JWT client.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/repos/o/r", nil)
	_, err = slowStartWrap(http.DefaultTransport).RoundTrip(req)
	if !errors.Is(err, ErrPacingShed) {
		t.Fatalf("ordinary request err = %v, want ErrPacingShed", err)
	}
}

func TestEnumerateActionableRunsHighPriority(t *testing.T) {
	var sawHigh atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "acme", []string{"console"}, quietLogger())
	base := c.client.BaseURL
	c.client = gh.NewClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if PacingClassOf(req.Context()) == PacingHigh {
			sawHigh.Store(true)
		}
		return http.DefaultTransport.RoundTrip(req)
	})})
	c.client.BaseURL = base
	if _, err := c.EnumerateActionable(context.Background()); err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	if !sawHigh.Load() {
		t.Fatal("enumeration requests must carry PacingHigh")
	}
}
