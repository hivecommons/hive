package knowledge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func apiTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func wikiServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{
				{Slug: "fact-1", Title: "Fact One", Type: "gotcha", Confidence: 0.9, Score: 0.95},
			},
			Total: 1,
		})
	})
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{
				{Slug: "fact-1", Title: "Fact One"},
				{Slug: "fact-2", Title: "Fact Two"},
			},
			Total: 2,
		})
	})
	mux.HandleFunc("/api/pages/fact-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(pageResponse{
			Slug: "fact-1", Title: "Fact One", Body: "Body content", Type: "gotcha",
		})
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(statsResponse{TotalPages: 10, ByType: map[string]int{"gotcha": 5}})
	})
	mux.HandleFunc("/api/ingest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

func testKnowledgeAPI(serverURL string) *KnowledgeAPI {
	logger := apiTestLogger()
	layers := []layerClient{
		{layerType: LayerProject, client: NewClient(serverURL, logger)},
	}
	return &KnowledgeAPI{
		layers: layers,
		config: KnowledgeConfig{Enabled: true, Engine: "llm-wiki"},
		logger: logger,
	}
}

func TestKnowledgeAPI_SearchAll(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	results := api.SearchAll(context.Background(), "test", "", 10)
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Slug != "fact-1" {
		t.Errorf("slug = %q", results[0].Slug)
	}
	if results[0].Layer != LayerProject {
		t.Errorf("layer = %q", results[0].Layer)
	}
}

func TestKnowledgeAPI_LayerFacts(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	facts := api.LayerFacts(context.Background(), LayerProject, "")
	if len(facts) != 2 {
		t.Fatalf("facts = %d, want 2", len(facts))
	}
}

func TestKnowledgeAPI_LayerFacts_UnknownLayer(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	facts := api.LayerFacts(context.Background(), LayerOrg, "")
	if facts != nil {
		t.Errorf("expected nil for unknown layer")
	}
}

func TestKnowledgeAPI_ReadFact(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	fact, err := api.ReadFact(context.Background(), "fact-1")
	if err != nil {
		t.Fatalf("ReadFact: %v", err)
	}
	if fact == nil {
		t.Fatal("fact is nil")
	}
	if fact.Title != "Fact One" {
		t.Errorf("title = %q", fact.Title)
	}
}

func TestKnowledgeAPI_ReadFact_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	fact, err := api.ReadFact(context.Background(), "missing")
	if err != nil {
		t.Fatalf("ReadFact: %v", err)
	}
	if fact != nil {
		t.Error("expected nil fact for missing slug")
	}
}

func TestKnowledgeAPI_Health(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	statuses := api.Health(context.Background())
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d", len(statuses))
	}
	if !statuses[0].Healthy {
		t.Error("expected healthy")
	}
	if statuses[0].Pages != 10 {
		t.Errorf("pages = %d", statuses[0].Pages)
	}
}

func TestKnowledgeAPI_Health_Unhealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	statuses := api.Health(context.Background())
	if statuses[0].Healthy {
		t.Error("expected unhealthy")
	}
}

func TestKnowledgeAPI_Stats(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	stats := api.Stats(context.Background())
	if stats["enabled"] != true {
		t.Errorf("enabled = %v", stats["enabled"])
	}
	if stats["layers_count"] != 1 {
		t.Errorf("layers_count = %v", stats["layers_count"])
	}
}

func TestKnowledgeAPI_DeleteFact(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	err := api.DeleteFact(context.Background(), LayerProject, "fact-1")
	if err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}
}

func TestKnowledgeAPI_DeleteFact_UnknownLayer(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	err := api.DeleteFact(context.Background(), "nonexistent", "fact-1")
	if err == nil {
		t.Error("expected error for unknown layer")
	}
}

func TestKnowledgeAPI_Subscriptions(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	subs := api.Subscriptions()
	if subs == nil {
		t.Error("expected non-nil subscriptions")
	}
	if len(subs) != 0 {
		t.Errorf("subscriptions = %d", len(subs))
	}
}

func TestKnowledgeAPI_AddSubscription(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	sub := Subscription{URL: "http://example.com/wiki", Layer: LayerOrg, Name: "test"}
	err := api.AddSubscription(sub)
	if err != nil {
		t.Fatalf("AddSubscription: %v", err)
	}
	if len(api.Subscriptions()) != 1 {
		t.Errorf("subscriptions = %d", len(api.Subscriptions()))
	}
}

func TestKnowledgeAPI_AddSubscription_Duplicate(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	sub := Subscription{URL: "http://example.com/wiki", Layer: LayerOrg}
	api.AddSubscription(sub)
	err := api.AddSubscription(sub)
	if err == nil {
		t.Error("expected error for duplicate subscription")
	}
}

func TestKnowledgeAPI_RemoveSubscription(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	sub := Subscription{URL: "http://example.com/wiki", Layer: LayerOrg, Name: "test-sub"}
	api.AddSubscription(sub)

	err := api.RemoveSubscription("http://example.com/wiki")
	if err != nil {
		t.Fatalf("RemoveSubscription: %v", err)
	}
	if len(api.Subscriptions()) != 0 {
		t.Errorf("subscriptions after remove = %d", len(api.Subscriptions()))
	}
}

func TestKnowledgeAPI_RemoveSubscription_NotFound(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	api := testKnowledgeAPI(server.URL)
	err := api.RemoveSubscription("http://nonexistent.com")
	if err == nil {
		t.Error("expected error for missing subscription")
	}
}

func TestNewKnowledgeAPI(t *testing.T) {
	server := wikiServer()
	defer server.Close()

	layers := []LayerConfig{
		{Type: LayerProject, URL: server.URL},
	}
	cfg := KnowledgeConfig{Enabled: true, Engine: "llm-wiki"}
	api := NewKnowledgeAPI(layers, cfg, apiTestLogger())
	if api == nil {
		t.Fatal("NewKnowledgeAPI returned nil")
	}
	if len(api.layers) != 1 {
		t.Errorf("layers = %d", len(api.layers))
	}
}

func TestNewKnowledgeAPI_EmptyEndpoint(t *testing.T) {
	layers := []LayerConfig{
		{Type: LayerProject, URL: ""},
	}
	cfg := KnowledgeConfig{Enabled: true}
	api := NewKnowledgeAPI(layers, cfg, apiTestLogger())
	if len(api.layers) != 0 {
		t.Errorf("expected 0 layers for empty endpoint, got %d", len(api.layers))
	}
}
