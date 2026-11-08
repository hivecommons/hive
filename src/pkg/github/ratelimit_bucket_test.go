package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests pin which core rate-limit window the budget card and the API
// budget mode follow when the daemon sees more than one.
//
// Live on hosted-projectbluefin-knuckle: after the 2026-11-08 18:30:53Z
// restart the first GET /rate_limit read 6945 (reset 19:21:44Z) while the
// hive's own responses reported 3195 (reset 18:56:25Z, the installation window
// that chains hourly and returned 403s the day before). The card kept
// observed_at 18:30:55Z for the rest of that window. Earlier the same day,
// 11:31-11:35Z, responses alternated between the two windows and the mode
// flipped normal <-> conserve every few seconds.

// coreHeaders builds the rate-limit headers GitHub sends on a core response.
func coreHeaders(limit, remaining int, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Resource", githubCoreRateLimitResource)
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

// rateLimitServer serves GET /rate_limit with whatever core reading next()
// returns, the way GitHub answers it.
func rateLimitServer(t *testing.T, next func() (remaining int, reset time.Time)) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remaining, reset := next()
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resources": map[string]any{
				"core": map[string]any{"limit": 7000, "remaining": remaining, "reset": reset.Unix()},
			},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestRateLimits_StrayFirstReadingYieldsToInstallationResponses(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	setStoreNow := resetRESTAccountingForTest(t, now)
	installReset := now.Add(25 * time.Minute)
	strayReset := now.Add(50 * time.Minute)

	// /rate_limit keeps answering with the stray window, as it did live.
	server := rateLimitServer(t, func() (int, time.Time) { return 6945, strayReset })
	c := newTestClient(t, server, "org", []string{"repo1"})
	clock := now
	c.rateLimits.now = func() time.Time { return clock }

	first, err := c.RateLimits(context.Background())
	if err != nil {
		t.Fatalf("RateLimits: %v", err)
	}
	if first.Core.Remaining != 6945 {
		t.Fatalf("first read = %d, want 6945 (nothing else known yet)", first.Core.Remaining)
	}

	// The hive's own enumeration then reports the installation window.
	t1 := now.Add(5 * time.Second)
	setStoreNow(t1)
	clock = t1
	RecordRESTRequest("hive:eval_cycle", http.MethodGet, "/repos/org/repo1/issues/1", http.StatusOK, coreHeaders(7000, 3195, installReset))

	second, err := c.RateLimits(context.Background())
	if err != nil {
		t.Fatalf("RateLimits: %v", err)
	}
	if second.Core.Remaining != 3195 || !second.Core.Reset.Equal(installReset) {
		t.Fatalf("card = %d reset %s, want 3195 reset %s: the first /rate_limit reading latched the card",
			second.Core.Remaining, second.Core.Reset.Format(time.RFC3339), installReset.Format(time.RFC3339))
	}
	if !second.Core.ObservedAt.Equal(t1) {
		t.Fatalf("observed_at = %s, want %s", second.Core.ObservedAt, t1)
	}
	if second.APIBudget.Remaining != 3195 {
		t.Fatalf("api_budget remaining = %d, want 3195", second.APIBudget.Remaining)
	}

	// It keeps following the installation window; the stray /rate_limit
	// answer never takes it back.
	t2 := now.Add(70 * time.Second)
	setStoreNow(t2)
	clock = t2
	RecordRESTRequest("hive:pr_batch", http.MethodGet, "/repos/org/repo1/pulls", http.StatusOK, coreHeaders(7000, 3010, installReset))
	third, err := c.RateLimits(context.Background())
	if err != nil {
		t.Fatalf("RateLimits: %v", err)
	}
	if third.Core.Remaining != 3010 || !third.Core.ObservedAt.Equal(t2) || !third.Core.Reset.Equal(installReset) {
		t.Fatalf("card = %d observed %s reset %s, want 3010 observed %s reset %s",
			third.Core.Remaining, third.Core.ObservedAt, third.Core.Reset, t2, installReset)
	}
}

func TestAPIBudgetMode_AlternatingBucketsFollowTheInstallationBucket(t *testing.T) {
	now := time.Date(2026, 11, 8, 11, 31, 30, 0, time.UTC)
	setStoreNow := resetRESTAccountingForTest(t, now)
	installReset := time.Date(2026, 11, 8, 11, 56, 18, 0, time.UTC)
	strayReset := time.Date(2026, 11, 8, 12, 15, 11, 0, time.UTC)

	var log strings.Builder
	c := &Client{logger: slog.New(slog.NewTextHandler(&log, nil))}
	clock := now
	c.rateLimits.now = func() time.Time { return clock }
	c.SetAPIBudgetThresholds(800, 250)
	// What the process's first /rate_limit read: the stray window.
	c.rateLimits.observe(githubCoreRateLimitResource, RateLimitEntry{Limit: 7000, Remaining: 6900, Reset: strayReset})

	// The readings from the live log, in order, a few seconds apart.
	readings := []struct {
		remaining int
		reset     time.Time
	}{
		{783, installReset}, {6806, strayReset}, {732, installReset}, {6790, strayReset},
		{730, installReset}, {6779, strayReset}, {716, installReset}, {6710, strayReset},
		{549, installReset}, {6705, strayReset}, {380, installReset},
	}
	for i, r := range readings {
		at := now.Add(time.Duration(i*5) * time.Second)
		setStoreNow(at)
		clock = at
		RecordRESTRequest("hive", http.MethodGet, "/repos/projectbluefin/utah/issues", http.StatusOK, coreHeaders(7000, r.remaining, r.reset))
		mode, snap := c.APIBudgetMode()
		if mode != APIBudgetConserve {
			t.Fatalf("reading %d (%d, reset %s): mode = %v remaining %d, want conserve on the installation bucket",
				i, r.remaining, r.reset.Format("15:04:05"), mode, snap.Remaining)
		}
		if !snap.Reset.Equal(installReset) {
			t.Fatalf("reading %d: budget reset = %s, want the installation window %s", i, snap.Reset, installReset)
		}
	}
	if _, snap := c.APIBudgetMode(); snap.Remaining != 380 {
		t.Fatalf("final remaining = %d, want 380", snap.Remaining)
	}
	if n := strings.Count(log.String(), "github api budget mode changed"); n != 1 {
		t.Fatalf("mode changed %d times, want once (normal -> conserve); log:\n%s", n, log.String())
	}
}

func TestRateLimits_LaterLowerReadingAfterRestartUpdatesCard(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	setStoreNow := resetRESTAccountingForTest(t, now)
	reset := now.Add(40 * time.Minute)

	// /rate_limit answers 5000 first, then a late, higher 4100 for the same
	// window than the responses have already shown.
	calls := 0
	server := rateLimitServer(t, func() (int, time.Time) {
		calls++
		if calls == 1 {
			return 5000, reset
		}
		return 4100, reset
	})
	c := newTestClient(t, server, "org", []string{"repo1"})
	clock := now
	c.rateLimits.now = func() time.Time { return clock }

	if got, err := c.RateLimits(context.Background()); err != nil || got.Core.Remaining != 5000 {
		t.Fatalf("first read = %+v, %v; want 5000", got, err)
	}

	t1 := now.Add(30 * time.Second)
	setStoreNow(t1)
	clock = t1
	RecordRESTRequest("hive", http.MethodGet, "/repos/org/repo1/issues", http.StatusOK, coreHeaders(7000, 4000, reset))
	got, err := c.RateLimits(context.Background())
	if err != nil {
		t.Fatalf("RateLimits: %v", err)
	}
	if got.Core.Remaining != 4000 || !got.Core.ObservedAt.Equal(t1) {
		t.Fatalf("card = %d observed %s, want 4000 observed %s", got.Core.Remaining, got.Core.ObservedAt, t1)
	}

	t2 := now.Add(90 * time.Second)
	setStoreNow(t2)
	clock = t2
	RecordRESTRequest("hive", http.MethodGet, "/repos/org/repo1/pulls", http.StatusOK, coreHeaders(7000, 3900, reset))
	got, err = c.RateLimits(context.Background())
	if err != nil {
		t.Fatalf("RateLimits: %v", err)
	}
	if got.Core.Remaining != 3900 || !got.Core.ObservedAt.Equal(t2) {
		t.Fatalf("card = %d observed %s, want 3900 observed %s", got.Core.Remaining, got.Core.ObservedAt, t2)
	}
}

func TestRESTAccounting_AppJWTResponsesDoNotMoveCoreSnapshot(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	resetRESTAccountingForTest(t, now)
	installReset := now.Add(20 * time.Minute)
	RecordRESTRequest("hive", http.MethodGet, "/repos/org/repo1/issues", http.StatusOK, coreHeaders(7000, 3000, installReset))

	// An installation-token mint, authenticated with the App JWT. Its headers
	// describe the JWT bucket, not the installation's.
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		for k, v := range coreHeaders(5000, 120, now.Add(55*time.Minute)) {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"ghs_test","expires_at":"2099-01-01T00:00:00Z"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	if _, _, err := newJWTClient("jwt", server.URL).Apps.CreateInstallationToken(context.Background(), 42, nil); err != nil {
		t.Fatalf("CreateInstallationToken: %v", err)
	}
	if rem, reset, ok := CoreRateLimitSnapshot(); !ok || rem != 3000 || !reset.Equal(installReset) {
		t.Fatalf("snapshot = %d reset %s ok=%v, want 3000 reset %s: an App-JWT response moved the installation snapshot",
			rem, reset, ok, installReset)
	}
	// The mint is still counted for the operator.
	found := false
	for _, c := range RESTTopConsumers(10) {
		if strings.HasSuffix(c.Endpoint, "/access_tokens") {
			found = true
		}
	}
	if !found {
		t.Fatalf("mint not recorded per caller: %+v", RESTTopConsumers(10))
	}
}
