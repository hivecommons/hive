package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newLastGoodTestCache builds a cache with a pinned clock and an isolated
// last-good directory so no test touches /data or sleeps.
func newLastGoodTestCache(t *testing.T, now *time.Time) *cliModelCache {
	t.Helper()
	dir := t.TempDir()
	prev := cliModelLastGoodDir
	cliModelLastGoodDir = dir
	t.Cleanup(func() { cliModelLastGoodDir = prev })
	return newCLIModelCacheAt(func() time.Time { return *now })
}

// TestCopilotSDKHelperExecErr_DeadlineNamesTimeout: a helper SIGKILLed by its
// exec deadline must be reported as a timeout naming the budget, not as the
// bare "signal: killed (stderr: )" an operator cannot act on (#11865).
func TestCopilotSDKHelperExecErr_DeadlineNamesTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := copilotSDKHelperExecErr(ctx, errors.New("signal: killed"), "")
	if !errors.Is(err, errCopilotSDKHelperTimeout) {
		t.Fatalf("expected errCopilotSDKHelperTimeout, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"timed out", copilotSDKProbeTimeout.String(), "killed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "stderr:") {
		t.Errorf("timeout error must not carry an empty stderr suffix: %q", msg)
	}
}

// TestCopilotSDKHelperExecErr_OtherKeepsStderr: a genuine helper failure keeps
// its exit error and capped stderr exactly as before.
func TestCopilotSDKHelperExecErr_OtherKeepsStderr(t *testing.T) {
	err := copilotSDKHelperExecErr(context.Background(), errors.New("exit status 1"), "  Not authenticated\n")
	if errors.Is(err, errCopilotSDKHelperTimeout) {
		t.Fatalf("non-deadline failure misclassified as timeout: %v", err)
	}
	if got := err.Error(); got != "sdk helper: exit status 1 (stderr: Not authenticated)" {
		t.Fatalf("got %q", got)
	}
	long := copilotSDKHelperExecErr(context.Background(), errors.New("exit status 1"), strings.Repeat("z", copilotSDKStderrLogLimit*2))
	if n := strings.Count(long.Error(), "z"); n != copilotSDKStderrLogLimit {
		t.Fatalf("stderr not capped: %d z's, want %d", n, copilotSDKStderrLogLimit)
	}
}

// TestQueryCLIModels_SDKFailureServesLastGoodCatalog is the #11865 flap: a
// clean SDK probe, then one that dies. The served list must stay the CLI's
// catalog — authoritative, labelled stale with the helper failure as the
// reason — not the degraded HTTP inventory.
func TestQueryCLIModels_SDKFailureServesLastGoodCatalog(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	now := time.Date(2026, 10, 14, 18, 6, 0, 0, time.UTC)
	c := newLastGoodTestCache(t, &now)
	s := &Server{cliModels: c, logger: testLogger()}

	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return []byte(`{"models":[{"id":"gpt-5.4","policyState":"enabled"},{"id":"claude-opus-5.5","policyState":"enabled"}]}`), nil
	})
	first := s.queryCLIModels("copilot")
	if !first.authoritative() || first.stale || first.failed() {
		t.Fatalf("first probe must be clean and live: %+v", first)
	}

	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return nil, errors.New("sdk helper timed out after 20s and was killed")
	})
	now = now.Add(cliModelCacheTTL + time.Second) // expire the cache entry
	second := s.queryCLIModels("copilot")
	if !second.stale || !second.authoritative() || second.degraded || second.fallback {
		t.Fatalf("SDK failure after a good probe must replay the last-good catalog as stale: %+v", second)
	}
	if !equalStrings(second.models, first.models) {
		t.Fatalf("stale models %v != last good %v", second.models, first.models)
	}
	for _, want := range []string{"2026-10-14 18:06Z", "timed out after 20s"} {
		if !strings.Contains(second.discoveryErr, want) {
			t.Errorf("discoveryErr %q lacks %q", second.discoveryErr, want)
		}
	}
	if second.staleAt.IsZero() {
		t.Error("staleAt must carry the discovery time of the replayed catalog")
	}

	// A replay is not itself a new last-good: the retained timestamp stays.
	c.mu.Lock()
	at := c.lastGood["copilot"].at
	c.mu.Unlock()
	if !at.Equal(time.Date(2026, 10, 14, 18, 6, 0, 0, time.UTC)) {
		t.Fatalf("stale replay overwrote lastGood timestamp: %v", at)
	}

	// The /api/config/backends entry says stale (not degraded) and carries
	// stale_at for the client label. Every other CLI backend is pre-cached
	// so the handler spawns no background probes into the temp dir.
	for _, backend := range []string{"claude", "gemini", "goose", "codex", agyBackendID, ompBackendID, bobBackendID} {
		c.set(backend, cliModelResult{models: []string{"placeholder"}, fallback: true})
	}
	w := httptest.NewRecorder()
	s.handleBackends(w, httptest.NewRequest("GET", "/api/config/backends", nil))
	var backends []struct {
		ID        string `json:"id"`
		Stale     bool   `json:"stale"`
		StaleAt   string `json:"stale_at"`
		Degraded  bool   `json:"degraded"`
		Fallback  bool   `json:"fallback"`
		Discovery struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			Stale bool   `json:"stale"`
		} `json:"discovery"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &backends); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, b := range backends {
		if b.ID != "copilot" {
			continue
		}
		if !b.Stale || b.Degraded || b.Fallback || !b.Discovery.Stale || b.Discovery.OK {
			t.Fatalf("copilot entry = %+v", b)
		}
		if b.StaleAt != "2026-10-14T18:06:00Z" {
			t.Fatalf("stale_at = %q", b.StaleAt)
		}
		return
	}
	t.Fatal("copilot entry missing")
}

// TestDiscoverCopilotModels_EntitlementRejectionBypassesLastGood: a seat
// revocation is not a transient failure; the retained catalog must not mask
// it.
func TestDiscoverCopilotModels_EntitlementRejectionBypassesLastGood(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	now := time.Now()
	c := newLastGoodTestCache(t, &now)
	c.set("copilot", cliModelResult{models: []string{"gpt-5.4"}})
	s := &Server{cliModels: c, logger: testLogger()}
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return nil, errors.New("sdk helper: exit status 1 (stderr: user is " + copilotNotLicensedMarker + ")")
	})
	r := s.discoverCopilotModels()
	if r.stale || r.notice == nil || !r.fallback {
		t.Fatalf("entitlement rejection must surface the notice, not the cached catalog: %+v", r)
	}
}

// TestDiscoverCopilotModels_NoLastGoodKeepsDegradedPath: with nothing
// retained the pre-#11865 behaviour is unchanged (fallback carrying the
// helper error when no token is available for the HTTP probe).
func TestDiscoverCopilotModels_NoLastGoodKeepsDegradedPath(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	now := time.Now()
	c := newLastGoodTestCache(t, &now)
	s := &Server{cliModels: c, logger: testLogger()}
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		return nil, errors.New("sdk helper: signal: killed (stderr: )")
	})
	r := s.discoverCopilotModels()
	if r.stale || !r.fallback || !strings.Contains(r.discoveryErr, "signal: killed") {
		t.Fatalf("got %+v", r)
	}
}

// TestCLIModelCache_FailureBackoff: consecutive failures double the re-probe
// interval from cliModelCacheTTL up to cliModelFailureBackoffMax; a success
// resets it.
func TestCLIModelCache_FailureBackoff(t *testing.T) {
	now := time.Now()
	c := newLastGoodTestCache(t, &now)
	failed := cliModelResult{models: []string{"x"}, fallback: true, discoveryErr: "boom"}

	c.set("copilot", failed)
	if got := c.ttlFor("copilot"); got != cliModelCacheTTL {
		t.Fatalf("after 1 failure ttl = %v, want %v", got, cliModelCacheTTL)
	}
	c.set("copilot", failed)
	if got := c.ttlFor("copilot"); got != 2*cliModelCacheTTL {
		t.Fatalf("after 2 failures ttl = %v, want %v", got, 2*cliModelCacheTTL)
	}
	// Within the widened window the entry is still a hit…
	now = now.Add(cliModelCacheTTL + time.Second)
	if _, ok := c.get("copilot"); !ok {
		t.Fatal("expected backoff to keep the failed entry past the base TTL")
	}
	// …and past it, a miss.
	now = now.Add(cliModelCacheTTL)
	if _, ok := c.get("copilot"); ok {
		t.Fatal("expected a miss after the backed-off TTL")
	}
	for i := 0; i < 20; i++ {
		c.set("copilot", failed)
	}
	if got := c.ttlFor("copilot"); got != cliModelFailureBackoffMax {
		t.Fatalf("backoff not capped: %v", got)
	}
	c.set("copilot", cliModelResult{models: []string{"x"}})
	if got := c.ttlFor("copilot"); got != cliModelCacheTTL {
		t.Fatalf("success did not reset backoff: %v", got)
	}
}

// TestCLIModelCache_LastGoodPersistsAcrossRestart: a fresh authoritative
// result is written under the data dir and a new cache (a restarted spoke)
// loads it; an entry older than cliModelLastGoodTTL is ignored.
func TestCLIModelCache_LastGoodPersistsAcrossRestart(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c := newLastGoodTestCache(t, &now)
	c.set("copilot", cliModelResult{models: []string{"gpt-5.4", "claude-opus-5.5"}})
	c.set("gemini", cliModelResult{models: []string{"gemini-x"}, fallback: true}) // not authoritative: not persisted
	c.set("claude", cliModelResult{models: []string{"claude-y"}, stale: true})    // replay: not persisted

	if _, err := os.Stat(c.lastGoodPath("copilot")); err != nil {
		t.Fatalf("copilot last-good not persisted: %v", err)
	}
	for _, b := range []string{"gemini", "claude"} {
		if _, err := os.Stat(c.lastGoodPath(b)); err == nil {
			t.Fatalf("%s must not be persisted", b)
		}
	}

	// cliModelLastGoodDir still points at the temp dir. The clock has to be
	// in place before the load: the fixture is dated, so a wall-clock load
	// would drop it as expired once the calendar passed At + TTL.
	restarted := newCLIModelCacheAt(func() time.Time { return now.Add(time.Hour) })
	r, ok := restarted.lastGoodFor("copilot", "why")
	if !ok || !r.stale || r.discoveryErr != "why" || !equalStrings(r.models, []string{"gpt-5.4", "claude-opus-5.5"}) {
		t.Fatalf("restarted cache did not load last-good: ok=%v %+v", ok, r)
	}
	if !r.staleAt.Equal(now) {
		t.Fatalf("staleAt = %v, want %v", r.staleAt, now)
	}

	expired := newCLIModelCacheAt(func() time.Time { return now.Add(cliModelLastGoodTTL + time.Minute) })
	if _, ok := expired.lastGoodFor("copilot", "why"); ok {
		t.Fatal("expired last-good must not be served")
	}

	// Garbage on disk is skipped, not fatal.
	if err := os.WriteFile(filepath.Join(cliModelLastGoodDir, "cli-models-last-good-junk.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := newCLIModelCache().lastGoodFor("junk", "why"); ok {
		t.Fatal("malformed file must be ignored")
	}
}

// TestQueryCLIModelsForBoot_ServesPersistedLastGood: a restarted spoke's
// first /api/config/backends gets the persisted CLI catalog (stale, refresh
// running) instead of the static list.
func TestQueryCLIModelsForBoot_ServesPersistedLastGood(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	seed := newLastGoodTestCache(t, &now)
	seed.set("copilot", cliModelResult{models: []string{"gpt-5.4"}})

	probed := make(chan struct{})
	var probedOnce sync.Once
	swapSDKHelper(t, func(ctx context.Context, token string) ([]byte, error) {
		probedOnce.Do(func() { close(probed) })
		return nil, errors.New("sdk helper: exit status 1 (stderr: nope)")
	})
	c := newCLIModelCacheAt(func() time.Time { return now.Add(time.Minute) })
	s := &Server{cliModels: c, logger: testLogger()}
	r := s.queryCLIModelsForBoot("copilot")
	if !r.stale || r.fallback || !equalStrings(r.models, []string{"gpt-5.4"}) {
		t.Fatalf("boot must serve the persisted catalog: %+v", r)
	}
	if !strings.Contains(r.discoveryErr, "2026-10-14 12:00Z") || !strings.Contains(r.discoveryErr, "running in background") {
		t.Fatalf("discoveryErr = %q", r.discoveryErr)
	}
	// The background refresh must have been kicked off (and reached the
	// seam) before the seam is restored by cleanup.
	select {
	case <-probed:
	case <-time.After(5 * time.Second):
		t.Fatal("background refresh never ran the SDK probe")
	}
}
