package classify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestJevEnforceHighConfidenceUsesJev(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServer(t, jevTestAnswers(0.95))
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Mode: "enforce", Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 1, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(1, 0)}, config.TriageConfig{})
	if got.Classification.Lane != LaneArchitect || got.Classification.Tier != TierComplex || got.Classification.Model != ModelOpus || got.Classification.Source != SourceJev {
		t.Fatalf("classification = %+v, want Jev architect/Complex/opus", got.Classification)
	}
	if got.Triage == nil || got.Triage.Verdict != TriageSpec {
		t.Fatalf("triage = %+v, want spec", got.Triage)
	}
}

func TestJevLowConfidenceFallsBack(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := jevTestServer(t, jevTestAnswers(0.3))
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Mode: "enforce", Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 2, Title: "Fix typo", Body: longTriageBody(), UpdatedAt: time.Unix(2, 0)}, config.TriageConfig{})
	if got.Classification.Source != SourceKeywords || got.Classification.Tier != TierSimple || got.Classification.Lane != LaneScanner {
		t.Fatalf("classification = %+v, want keyword fallback", got.Classification)
	}
	stats := CurrentStats()
	if stats.Decisions[DecisionLane].Fallback == 0 || stats.Decisions[DecisionTier].Fallback == 0 {
		t.Fatalf("stats = %+v, want low-confidence fallback counters", stats)
	}
}

func TestJevTierEnforceRecomputesFallbackTriage(t *testing.T) {
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
	d := newJevDecider(config.JevClassifierConfig{Mode: "enforce", Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second, Decisions: []string{DecisionTier}}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 20, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(20, 0)}, config.TriageConfig{})
	if got.Classification.Tier != TierComplex {
		t.Fatalf("tier = %s, want Complex", got.Classification.Tier)
	}
	if got.Triage == nil || got.Triage.Verdict != TriageSpec || got.Triage.Signals[0] != "tier:Complex" {
		t.Fatalf("triage = %+v, want fallback recomputed from enforced Complex tier", got.Triage)
	}
}

func TestJevTimeoutFallsBack(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	d := newJevDecider(config.JevClassifierConfig{Mode: "enforce", Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Nanosecond}, func() string { return "key" }, server.Client())
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
	d := newJevDecider(config.JevClassifierConfig{Mode: "shadow", Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 4, Title: "Fix typo", Body: longTriageBody(), UpdatedAt: time.Unix(4, 0)}, config.TriageConfig{})
	if got.Classification.Source != SourceKeywords || got.Classification.Tier != TierSimple || got.Classification.Lane != LaneScanner {
		t.Fatalf("classification = %+v, want keyword decision in shadow", got.Classification)
	}
	stats := CurrentStats()
	if stats.Decisions[DecisionLane].Disagree == 0 || stats.Decisions[DecisionTier].Disagree == 0 || stats.EstimatedInputTokens == 0 || stats.EstimatedSpendUSD == 0 {
		t.Fatalf("stats = %+v, want disagreement and spend counters", stats)
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
	d := newJevDecider(config.JevClassifierConfig{Mode: "enforce", Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
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
		Mode:    "enforce",
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
	if got.Source != SourceJev || got.Tier != TierComplex {
		t.Fatalf("classification = %+v, want Jev complex", got)
	}
}

func TestJevFallbacksForMissingKeyDisabledAndHTTPError(t *testing.T) {
	ResetForTest()
	defer ResetForTest()
	issue := github.Issue{Repo: "o/r", Number: 8, Title: "Fix typo", UpdatedAt: time.Unix(8, 0)}
	noKey := newJevDecider(config.JevClassifierConfig{Mode: "enforce", Timeout: time.Second}, func() string { return "" }, nil)
	if got := noKey.Decide(context.Background(), issue, config.TriageConfig{}); got.Classification.Source != SourceKeywords || got.Classification.Tier != TierSimple {
		t.Fatalf("missing-key classification = %+v, want keywords", got.Classification)
	}
	disabled := newJevDecider(config.JevClassifierConfig{Mode: "enforce", Decisions: []string{"unknown"}, Timeout: time.Second}, func() string { return "key" }, nil)
	if got := disabled.Decide(context.Background(), issue, config.TriageConfig{}); got.Classification.Source != SourceKeywords {
		t.Fatalf("disabled decisions classification = %+v, want keywords", got.Classification)
	}
	server := jevTestServerFunc(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	})
	defer server.Close()
	httpErr := newJevDecider(config.JevClassifierConfig{Mode: "enforce", Endpoint: server.URL, Timeout: time.Second}, func() string { return "key" }, server.Client())
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
	d := newJevDecider(config.JevClassifierConfig{Mode: "enforce", Endpoint: server.URL, MinConfidence: 0.8, Timeout: time.Second}, func() string { return "key" }, server.Client())
	got := d.Decide(context.Background(), github.Issue{Repo: "o/r", Number: 9, Title: "ordinary", Body: longTriageBody(), UpdatedAt: time.Unix(9, 0)}, config.TriageConfig{})
	if got.Classification.Tier != TierComplex || got.Classification.Lane != LaneScanner {
		t.Fatalf("classification = %+v, want tier from probability confidence and invalid lane fallback", got.Classification)
	}
	if got.Triage == nil || got.Triage.Verdict != TriageSpec {
		t.Fatalf("triage = %+v, want fallback from enforced complex tier", got.Triage)
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
