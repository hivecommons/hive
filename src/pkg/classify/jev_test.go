package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestJevHighConfidenceStaysAdvisory(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServer(t, jevTestAnswers(0.95))
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 1, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(1, 0)}, config.TriageConfig{})
	if got.Classification.Lane != LaneScanner || got.Classification.Tier != TierMedium || got.Classification.Model != ModelSonnet || got.Classification.Source != SourceKeywords {
		t.Fatalf("classification = %+v, want keyword result despite high-confidence Jev", got.Classification)
	}
	if got.Triage == nil || got.Triage.Verdict != TriageFix {
		t.Fatalf("triage = %+v, want keyword triage", got.Triage)
	}
	stats := CurrentStats()
	if stats.Decisions[DecisionLane].Disagree != 1 || stats.Decisions[DecisionTier].Disagree != 1 || stats.Decisions[DecisionTriage].Disagree != 1 {
		t.Fatalf("stats = %+v, want advisory disagreements", stats)
	}
}

func TestJevLowConfidenceFallsBack(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServer(t, jevTestAnswers(0.3))
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 2, Title: "Fix typo", Body: longTriageBody(), UpdatedAt: time.Unix(2, 0)}, config.TriageConfig{})
	if got.Classification.Source != SourceKeywords || got.Classification.Tier != TierSimple || got.Classification.Lane != LaneScanner {
		t.Fatalf("classification = %+v, want keyword fallback", got.Classification)
	}
	stats := CurrentStats()
	if stats.Decisions[DecisionLane].Fallback == 0 || stats.Decisions[DecisionTier].Fallback == 0 {
		t.Fatalf("stats = %+v, want low-confidence fallback counters", stats)
	}
}

func TestJevTierAdvisoryDoesNotRecomputeTriageFromJev(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServer(t, map[string]any{
		"model": "jev-1.13.0",
		"answers": map[string]any{
			DecisionTier: map[string]any{"type": "choice", "choice": string(TierComplex), "confidence": 0.95, "probabilities": map[string]float64{string(TierComplex): 0.95}},
		},
		"usage": map[string]int{"input_tokens": 200, "output_tokens": 10},
	})
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second, Decisions: []string{DecisionTier}}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 20, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(20, 0)}, config.TriageConfig{})
	if got.Classification.Tier != TierMedium {
		t.Fatalf("tier = %s, want keyword Medium", got.Classification.Tier)
	}
	if got.Triage == nil || got.Triage.Verdict != TriageFix {
		t.Fatalf("triage = %+v, want keyword triage from Medium tier", got.Triage)
	}
}

func TestJevTimeoutFallsBack(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Nanosecond}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 3, Title: "Fix typo", UpdatedAt: time.Unix(3, 0)}, config.TriageConfig{})
	if got.Classification.Source != SourceKeywords || got.Classification.Tier != TierSimple {
		t.Fatalf("classification = %+v, want keyword fallback", got.Classification)
	}
}

func TestJevShadowNeverChangesResultButCounts(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServer(t, jevTestAnswers(0.95))
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 4, Title: "Fix typo", Body: longTriageBody(), UpdatedAt: time.Unix(4, 0)}, config.TriageConfig{})
	if got.Classification.Source != SourceKeywords || got.Classification.Tier != TierSimple || got.Classification.Lane != LaneScanner {
		t.Fatalf("classification = %+v, want keyword decision in shadow", got.Classification)
	}
	stats := CurrentStats()
	if stats.Decisions[DecisionLane].Disagree == 0 || stats.Decisions[DecisionTier].Disagree == 0 || stats.EstimatedInputTokens == 0 || stats.EstimatedSpendUSD == 0 {
		t.Fatalf("stats = %+v, want disagreement and spend counters", stats)
	}
}

func TestJevDisagreementsProduceRuleSuggestions(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServer(t, jevTestAnswers(0.95))
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	for i := 1; i <= 3; i++ {
		got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 40 + i, Title: "Schema cleanup request", Body: longTriageBody(), UpdatedAt: time.Unix(int64(40+i), 0)}, config.TriageConfig{})
		if got.Classification.Lane != LaneScanner || got.Classification.Tier != TierMedium {
			t.Fatalf("classification = %+v, want keyword scanner/Medium", got.Classification)
		}
	}
	stats := CurrentStats()
	if got := len(stats.Disagreements[DecisionLane]); got != 3 {
		t.Fatalf("lane disagreements = %d, want 3", got)
	}
	if !hasSuggestion(stats.RuleSuggestions, DecisionLane, string(LaneArchitect), "schema", "agents.architect.lane_keywords") {
		t.Fatalf("suggestions = %+v, want architect lane keyword suggestion for schema", stats.RuleSuggestions)
	}
	if !hasSuggestion(stats.RuleSuggestions, DecisionTier, string(TierComplex), "schema", "classifier.complex_signals") {
		t.Fatalf("suggestions = %+v, want complex tier keyword suggestion for schema", stats.RuleSuggestions)
	}
}

func TestJevCachePreventsSecondCall(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	var calls int32
	server := jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(jevTestAnswers(0.95))
	})
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	issue := github.Issue{Repo: "o/r", Number: 5, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(5, 0)}
	_ = d.Decide(context.Background(), issue, config.TriageConfig{})
	_ = d.Decide(context.Background(), issue, config.TriageConfig{})
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	if got := CurrentStats().EstimatedInputTokens; got != 500 {
		t.Fatalf("estimated input tokens = %d, want one request worth", got)
	}
}

func hasSuggestion(suggestions []RuleSuggestion, decision, answer, keyword, target string) bool {
	for _, s := range suggestions {
		if s.Decision == decision && s.Answer == answer && s.Keyword == keyword && s.Target == target && s.Support >= suggestionMinSupport {
			return true
		}
	}
	return false
}

func TestConfigureDeciderKeywordsDoesNoHTTP(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	var calls int32
	server := jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&calls, 1) })
	defer server.Close()
	cfg := &config.Config{Classifier: config.ClassifierConfig{Backend: "keywords", Jev: config.JevClassifierConfig{Endpoint: server.URL}}}
	if err := ConfigureDecider(cfg); err != nil {
		t.Fatalf("ConfigureDecider: %v", err)
	}
	_ = Classify(github.Issue{Repo: "o/r", Number: 6, Title: "Fix typo", UpdatedAt: time.Unix(6, 0)})
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("HTTP calls = %d, want 0", calls)
	}
}

func TestConfigureDeciderJevUsesEnvKey(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServer(t, jevTestAnswers(0.95))
	defer server.Close()
	t.Setenv("JEV_TEST_KEY", "key")
	cfg := &config.Config{Classifier: config.ClassifierConfig{
		Backend: "jev",
		Jev: config.JevClassifierConfig{
			Endpoint:  server.URL,
			APIKeyEnv: "JEV_TEST_KEY",
			Timeout:   time.Second,
		},
	}}
	if err := ConfigureDecider(cfg); err != nil {
		t.Fatalf("ConfigureDecider: %v", err)
	}
	got := Classify(github.Issue{Repo: "o/r", Number: 7, Title: "ordinary", UpdatedAt: time.Unix(7, 0)})
	if got.Source != SourceKeywords || got.Tier != TierMedium {
		t.Fatalf("classification = %+v, want keywords with Jev advisory", got)
	}
}

func TestJevFallbacksForMissingKeyDisabledAndHTTPError(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	issue := github.Issue{Repo: "o/r", Number: 8, Title: "Fix typo", UpdatedAt: time.Unix(8, 0)}
	noKey := newJevDecider(config.JevClassifierConfig{Timeout: time.Second}, func() string { return "" }, nil)
	if got := noKey.Decide(context.Background(), issue, config.TriageConfig{}); got.Classification.Source != SourceKeywords || got.Classification.Tier != TierSimple {
		t.Fatalf("missing-key classification = %+v, want keywords", got.Classification)
	}
	disabled := newJevDecider(config.JevClassifierConfig{Decisions: []string{"unknown"}, Timeout: time.Second}, func() string { return "key" }, nil)
	if got := disabled.Decide(context.Background(), issue, config.TriageConfig{}); got.Classification.Source != SourceKeywords {
		t.Fatalf("disabled decisions classification = %+v, want keywords", got.Classification)
	}
	server := jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	})
	defer server.Close()
	httpErr := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, Timeout: time.Second}, func() string { return "key" }, server.Client())
	if got := httpErr.Decide(context.Background(), issue, config.TriageConfig{}); got.Classification.Source != SourceKeywords {
		t.Fatalf("http-error classification = %+v, want keywords", got.Classification)
	}
}

func TestJevAnswerProbabilityFallbackAndInvalidChoices(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServer(t, map[string]any{
		"model": "jev-1.13.0",
		"answers": map[string]any{
			DecisionLane:   map[string]any{"type": "choice", "choice": "not-a-lane", "probabilities": map[string]float64{"not-a-lane": 0.99}},
			DecisionTier:   map[string]any{"type": "choice", "choice": string(TierComplex), "probabilities": map[string]float64{string(TierComplex): 0.95}},
			DecisionTriage: map[string]any{"type": "choice", "choice": "not-a-verdict", "probabilities": map[string]float64{"not-a-verdict": 0.99}},
		},
		"usage": map[string]int{"input_tokens": 100},
	})
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 9, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(9, 0)}, config.TriageConfig{})
	if got.Classification.Tier != TierMedium || got.Classification.Lane != LaneScanner {
		t.Fatalf("classification = %+v, want keyword result even when Jev probability confidence is valid", got.Classification)
	}
	if got.Triage == nil || got.Triage.Verdict != TriageFix {
		t.Fatalf("triage = %+v, want keyword triage", got.Triage)
	}
}

func TestJevCacheEvictsAtCapacity(t *testing.T) {
	cache := newJevCache(1)
	cache.add("a", jevOutcome{InputTokens: 1})
	cache.add("b", jevOutcome{InputTokens: 2})
	if _, ok := cache.get("a"); ok {
		t.Fatalf("old cache entry still present after cap eviction")
	}
	if got, ok := cache.get("b"); !ok || got.InputTokens != 2 {
		t.Fatalf("new cache entry = %+v, %v; want input tokens 2", got, ok)
	}
}

func jevTestServer(t *testing.T, payload any) *httptest.Server {
	t.Helper()
	return jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(payload) })
}

func jevTestServerFunc(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer key" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		fn(w, r)
	}))
}

func jevTestAnswers(conf float64) map[string]any {
	return map[string]any{
		"model": "jev-1.13.0",
		"answers": map[string]any{
			DecisionLane:   map[string]any{"type": "choice", "choice": string(LaneArchitect), "confidence": conf, "probabilities": map[string]float64{string(LaneArchitect): conf}},
			DecisionTier:   map[string]any{"type": "choice", "choice": string(TierComplex), "confidence": conf, "probabilities": map[string]float64{string(TierComplex): conf}},
			DecisionTriage: map[string]any{"type": "choice", "choice": string(TriageSpec), "confidence": conf, "probabilities": map[string]float64{string(TriageSpec): conf}},
		},
		"usage": map[string]int{"input_tokens": 500, "output_tokens": 20},
	}
}

func longTriageBody() string {
	return "This issue has enough detail to explain the requested behaviour, constraints, and expected outcome."
}

func TestJevFailedIssueIsNotRetriedWithinTTL(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	var calls int32
	server := jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "down", http.StatusBadGateway)
	})
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, Timeout: time.Second}, func() string { return "key" }, server.Client())
	now := time.Unix(1_000_000, 0)
	d.now = func() time.Time { return now }
	issue := github.Issue{Repo: "o/r", Number: 50, Title: "Fix typo", UpdatedAt: time.Unix(50, 0)}

	// One scheduler cycle classifies each issue twice (ClassifyAll, then run
	// triage); the failed revision must not pay the endpoint twice.
	for range 2 {
		if got := d.Decide(context.Background(), issue, config.TriageConfig{}); got.Classification.Source != SourceKeywords || got.Classification.Tier != TierSimple {
			t.Fatalf("classification = %+v, want keyword fallback", got.Classification)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls within failure TTL = %d, want 1", got)
	}
	if fb := CurrentStats().Decisions[DecisionLane].Fallback; fb != 2 {
		t.Fatalf("lane fallback = %d, want 2 (skipped call still counts as fallback)", fb)
	}

	now = now.Add(jevFailureTTL)
	_ = d.Decide(context.Background(), issue, config.TriageConfig{})
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls after failure TTL = %d, want 2", got)
	}
}

func TestJevBreakerSkipsCallsAfterConsecutiveFailuresUntilProbeSucceeds(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	var calls int32
	var healthy atomic.Bool
	server := jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if !healthy.Load() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(jevTestAnswers(0.95))
	})
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, Timeout: time.Second}, func() string { return "key" }, server.Client())
	now := time.Unix(2_000_000, 0)
	d.now = func() time.Time { return now }
	issue := func(n int) github.Issue {
		return github.Issue{Repo: "o/r", Number: n, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(int64(n), 0)}
	}

	for n := 1; n <= 20; n++ {
		_ = d.Decide(context.Background(), issue(n), config.TriageConfig{})
	}
	if got := atomic.LoadInt32(&calls); got != jevBreakerThreshold {
		t.Fatalf("calls with failing endpoint = %d, want breaker to stop after %d", got, jevBreakerThreshold)
	}

	// Cooldown elapsed but the endpoint is still failing: one probe, then the
	// breaker reopens.
	now = now.Add(jevBreakerCooldown)
	for n := 21; n <= 25; n++ {
		_ = d.Decide(context.Background(), issue(n), config.TriageConfig{})
	}
	if got := atomic.LoadInt32(&calls); got != jevBreakerThreshold+1 {
		t.Fatalf("calls after failed probe = %d, want %d", got, jevBreakerThreshold+1)
	}

	// A successful probe closes the breaker for every following issue.
	healthy.Store(true)
	now = now.Add(jevBreakerCooldown)
	for n := 26; n <= 30; n++ {
		_ = d.Decide(context.Background(), issue(n), config.TriageConfig{})
	}
	if got := atomic.LoadInt32(&calls); got != jevBreakerThreshold+1+5 {
		t.Fatalf("calls after successful probe = %d, want %d", got, jevBreakerThreshold+1+5)
	}
}

func TestJevBreakerAdmitsOneProbeAtATime(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	var calls int32
	var healthy atomic.Bool
	var startOnce sync.Once
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	server := jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if !healthy.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		startOnce.Do(func() { close(probeStarted) })
		select {
		case <-releaseProbe:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(jevTestAnswers(0.95))
	})
	defer server.Close()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseProbe) }) }
	defer release() // runs before server.Close so no handler stays blocked
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, Timeout: 2 * time.Second}, func() string { return "key" }, server.Client())
	now := time.Unix(3_000_000, 0)
	var nowMu sync.Mutex
	d.now = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
	issue := func(n int) github.Issue {
		return github.Issue{Repo: "o/r", Number: n, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(int64(n), 0)}
	}
	for n := 1; n <= jevBreakerThreshold; n++ {
		_ = d.Decide(context.Background(), issue(n), config.TriageConfig{})
	}

	healthy.Store(true)
	nowMu.Lock()
	now = now.Add(jevBreakerCooldown)
	nowMu.Unlock()
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		_ = d.Decide(context.Background(), issue(100), config.TriageConfig{})
	}()
	<-probeStarted
	// While the probe is in flight, concurrent callers (a manual kick racing
	// the eval sweep) must not also hit the recovering endpoint.
	for n := 101; n <= 105; n++ {
		_ = d.Decide(context.Background(), issue(n), config.TriageConfig{})
	}
	if got := atomic.LoadInt32(&calls); got != jevBreakerThreshold+1 {
		t.Fatalf("calls with probe in flight = %d, want %d", got, jevBreakerThreshold+1)
	}
	release()
	<-probeDone
}

func TestJevBreakerLogOmitsProviderErrorBody(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "echo: SECRET-ISSUE-BODY", http.StatusBadGateway)
	})
	defer server.Close()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, Timeout: time.Second}, func() string { return "key" }, server.Client())
	for n := 1; n <= jevBreakerThreshold; n++ {
		_ = d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: n, Title: "ordinary", Body: "SECRET-ISSUE-BODY", UpdatedAt: time.Unix(int64(n), 0)}, config.TriageConfig{})
	}
	out := buf.String()
	if !strings.Contains(out, "classify: jev:") || !strings.Contains(out, "HTTP 502") {
		t.Fatalf("breaker log = %q, want open notice with failure status", out)
	}
	if strings.Contains(out, "SECRET-ISSUE-BODY") {
		t.Fatalf("breaker log leaked provider error body: %q", out)
	}
}

func TestJevSweepDeadlineBoundsSlowEndpoint(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	var calls int32
	server := jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Slow but succeeding: wait out the response delay without a fixed
		// time.Sleep, and bail out early if the client already gave up.
		select {
		case <-time.After(60 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(jevTestAnswers(0.95))
	})
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Endpoint: server.URL, Timeout: 100 * time.Millisecond}, func() string { return "key" }, server.Client())
	SetDecider(d)
	issues := make([]github.Issue, 20)
	for i := range issues {
		issues[i] = github.Issue{Repo: "o/r", Number: 100 + i, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(int64(100+i), 0)}
	}

	sweepCtx, cancelSweep := SweepContext(context.Background())
	deadline, hasDeadline := sweepCtx.Deadline()
	cancelSweep()
	if left := time.Until(deadline); !hasDeadline || left > jevMinSweepBudget || left < jevMinSweepBudget-time.Second {
		t.Fatalf("SweepContext deadline = %v (set %v), want ~%s from now", deadline, hasDeadline, jevMinSweepBudget)
	}

	sweep, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	for _, c := range ClassifyAll(sweep, issues) {
		if c.Lane != string(LaneScanner) || c.ComplexityTier != string(TierMedium) {
			t.Fatalf("classified %+v, want keyword scanner/Medium", c)
		}
	}
	elapsed := time.Since(start)
	first := atomic.LoadInt32(&calls)
	// Each call takes >= 60ms and starts only with >= 100ms of deadline left,
	// so a 250ms sweep admits at most 3 calls however many issues are queued.
	if first < 1 || first > 3 {
		t.Fatalf("calls in sweep = %d, want 1..3 (bounded by sweep deadline)", first)
	}
	if elapsed > time.Second {
		t.Fatalf("sweep took %s, want bounded by the sweep deadline", elapsed)
	}
	// The run-triage pass shares the sweep's exhausted deadline.
	for _, issue := range issues {
		_ = Triage(sweep, issue, Classification{}, config.TriageConfig{})
	}
	if got := atomic.LoadInt32(&calls); got != first {
		t.Fatalf("calls in triage pass of the same sweep = %d, want %d", got, first)
	}

	next, cancelNext := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancelNext()
	_ = ClassifyAll(next, issues)
	if got := atomic.LoadInt32(&calls); got <= first {
		t.Fatalf("calls in next sweep = %d, want more than %d", got, first)
	}
}
