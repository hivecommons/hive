package openrouter

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// catalogFixture is a recorded, trimmed copy of the live public catalog
// (id, name, context_length, pricing only). Refresh it in the same PR that
// edits SuggestedModels or DefaultModel:
//
//	curl -s https://openrouter.ai/api/v1/models | jq '{data: [.data[] | {id, name, context_length, pricing: (.pricing | {prompt, completion, input_cache_read} | with_entries(select(.value != null)))}] | sort_by(.id)}' > src/crux/openrouter/testdata/models.json
const catalogFixture = "testdata/models.json"

func loadFixture(t *testing.T) []Model {
	t.Helper()
	f, err := os.Open(catalogFixture)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	models, err := ParseModels(f)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return models
}

// Every curated id, and the default, must be a real OpenRouter id. A stale
// list (claude-3.5-sonnet, gemini-flash-1.5, …) shipped for months because
// nothing checked this.
func TestSuggestedModelsExistInCatalogFixture(t *testing.T) {
	models := loadFixture(t)
	if len(models) == 0 {
		t.Fatal("fixture has no models")
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	if !ids[DefaultModel] {
		t.Errorf("DefaultModel %q is not in %s", DefaultModel, catalogFixture)
	}
	seen := map[string]bool{}
	for _, m := range SuggestedModels {
		if !ids[m.ID] {
			t.Errorf("suggested model %q is not in %s; refresh the fixture or fix the id", m.ID, catalogFixture)
		}
		if seen[m.ID] {
			t.Errorf("suggested model %q listed twice", m.ID)
		}
		seen[m.ID] = true
		if m.Label == "" {
			t.Errorf("suggested model %q has no label", m.ID)
		}
	}
	if !seen[DefaultModel] {
		t.Errorf("DefaultModel %q must be one of SuggestedModels", DefaultModel)
	}
}

func TestParseModelsPricing(t *testing.T) {
	models := loadFixture(t)
	var flash *Model
	var auto *Model
	for i := range models {
		switch models[i].ID {
		case "deepseek/deepseek-v4.1-flash":
			flash = &models[i]
		case "openrouter/auto":
			auto = &models[i]
		}
	}
	if flash == nil {
		t.Fatal("deepseek/deepseek-v4.1-flash missing from fixture")
	}
	if !flash.Pricing.Known() || flash.Pricing.InputCacheRead == nil {
		t.Fatalf("flash pricing not parsed: %+v", flash.Pricing)
	}
	const wantInPerM, wantOutPerM, eps = 0.14, 0.42, 1e-9
	if got := *PerMillion(flash.Pricing.Prompt); got < wantInPerM-eps || got > wantInPerM+eps {
		t.Errorf("input $/M = %v, want %v", got, wantInPerM)
	}
	if got := *PerMillion(flash.Pricing.Completion); got < wantOutPerM-eps || got > wantOutPerM+eps {
		t.Errorf("output $/M = %v, want %v", got, wantOutPerM)
	}
	if flash.ContextLength <= 0 {
		t.Errorf("context_length not parsed")
	}
	// The auto router publishes "-1" (variable): unknown, never a negative price.
	if auto != nil && auto.Pricing.Known() {
		t.Errorf("openrouter/auto must have unknown pricing, got %+v", auto.Pricing)
	}
	if PerMillion(nil) != nil {
		t.Error("PerMillion(nil) must be nil")
	}
}

func TestAvailableSuggestedHidesRetiredIDs(t *testing.T) {
	live := []string{"z-ai/glm-5.3", "anthropic/claude-sonnet-5.5"}
	got := AvailableSuggested(live)
	if len(got) != 2 || got[0].ID != "z-ai/glm-5.3" || got[1].ID != "anthropic/claude-sonnet-5.5" {
		t.Fatalf("AvailableSuggested = %+v", got)
	}
	if all := AvailableSuggested(nil); len(all) != len(SuggestedModels) {
		t.Fatalf("unreadable catalog must keep the full list, got %d", len(all))
	}
	// Default retired: fall back to the first surviving suggestion.
	if d := AvailableDefault(live); d != "z-ai/glm-5.3" {
		t.Errorf("AvailableDefault = %q, want z-ai/glm-5.3", d)
	}
	if d := AvailableDefault(nil); d != DefaultModel {
		t.Errorf("AvailableDefault(nil) = %q", d)
	}
	if d := AvailableDefault([]string{DefaultModel}); d != DefaultModel {
		t.Errorf("AvailableDefault(with default) = %q", d)
	}
	if d := AvailableDefault([]string{"x/y"}); d != DefaultModel {
		t.Errorf("AvailableDefault(no suggestions survive) = %q, want unchanged default", d)
	}
}

func TestCachedPublicModelsReusesAndSurvivesFailure(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a/b","context_length":8,"pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`))
	}))
	defer srv.Close()
	withURLs(t, srv.URL, KeyExchangeURL, KeyInfoURL)
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	prevNow := catalogNow
	catalogNow = func() time.Time { return now }
	t.Cleanup(func() { catalogNow = prevNow })

	if m := CachedPublicModels(); len(m) != 1 || m[0].ID != "a/b" {
		t.Fatalf("first fetch = %+v", m)
	}
	_ = CachedPublicModels()
	if hits.Load() != 1 {
		t.Fatalf("cache not reused: %d hits", hits.Load())
	}
	now = now.Add(catalogCacheTTL + time.Second)
	fail.Store(true)
	if m := CachedPublicModels(); len(m) != 1 {
		t.Fatalf("failed refresh must keep last good catalog, got %+v", m)
	}
	if hits.Load() != 2 {
		t.Fatalf("expired entry not refetched: %d hits", hits.Load())
	}
}
