package knowledge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.Default()
}

func TestParseMarkdownFacts(t *testing.T) {
	input := `# Guard join against undefined

Always use (arr || []).join(',').

## Use mock factories

Use createMockCluster() from test/factories.ts.
`
	facts := parseMarkdownFacts(input)
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
	if facts[0].Title != "Guard join against undefined" {
		t.Errorf("expected title 'Guard join against undefined', got %q", facts[0].Title)
	}
	if facts[1].Title != "Use mock factories" {
		t.Errorf("expected title 'Use mock factories', got %q", facts[1].Title)
	}
}

func TestParseMarkdownFacts_BulletList(t *testing.T) {
	input := `- **Guard join** Always use safe joins
- **Mock factories** Use createMockCluster
`
	facts := parseMarkdownFacts(input)
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
	if facts[0].Title != "Guard join" {
		t.Errorf("unexpected title: %q", facts[0].Title)
	}
}

func TestParseMarkdownFacts_Empty(t *testing.T) {
	facts := parseMarkdownFacts("")
	if len(facts) != 0 {
		t.Errorf("expected 0 facts from empty input, got %d", len(facts))
	}
}

func TestParseJSONFacts(t *testing.T) {
	input := `[{"title":"test fact","body":"body text","type":"gotcha","confidence":0.9}]`
	var facts []ExtractedFact
	if err := parseJSONFacts(input, &facts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	if facts[0].Title != "test fact" {
		t.Errorf("unexpected title: %q", facts[0].Title)
	}
}

func TestParseJSONFacts_Invalid(t *testing.T) {
	var facts []ExtractedFact
	if err := parseJSONFacts("not json", &facts); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestAddRemoveSubscription(t *testing.T) {
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: testLogger(),
	}

	sub := Subscription{URL: "https://wiki.example.com", Layer: LayerOrg, Name: "Test"}
	if err := api.AddSubscription(sub); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	subs := api.Subscriptions()
	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(subs))
	}
	if subs[0].URL != "https://wiki.example.com" {
		t.Errorf("unexpected URL: %q", subs[0].URL)
	}

	if err := api.AddSubscription(sub); err == nil {
		t.Error("expected error for duplicate subscription")
	}

	if err := api.RemoveSubscription("https://wiki.example.com"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(api.Subscriptions()) != 0 {
		t.Error("expected 0 subscriptions after removal")
	}

	if err := api.RemoveSubscription("https://nonexistent.com"); err == nil {
		t.Error("expected error for nonexistent subscription")
	}
}

func TestSubscriptionAddsLayer(t *testing.T) {
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: testLogger(),
	}

	initialLayers := len(api.layers)
	sub := Subscription{URL: "https://wiki.test.com", Layer: LayerCommunity, Name: "Community"}
	if err := api.AddSubscription(sub); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(api.layers) != initialLayers+1 {
		t.Errorf("expected %d layers, got %d", initialLayers+1, len(api.layers))
	}
}

func TestCreateFact_NoEndpoint(t *testing.T) {
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: testLogger(),
	}

	err := api.CreateFact(context.Background(), CreateFactRequest{
		Title: "test", Body: "body", Layer: "project",
	})
	if err == nil {
		t.Error("expected error when no endpoint configured")
	}
}

func TestCreateFact_WithServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/ingest" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var facts []ExtractedFact
		if err := json.NewDecoder(r.Body).Decode(&facts); err != nil {
			t.Errorf("failed to decode body: %v", err)
		}
		if len(facts) != 1 || facts[0].Title != "test fact" {
			t.Errorf("unexpected facts: %+v", facts)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	api := &KnowledgeAPI{
		layers: []layerClient{{layerType: LayerProject, client: NewClient(ts.URL, testLogger())}},
		config: KnowledgeConfig{Enabled: true},
		logger: testLogger(),
	}

	err := api.CreateFact(context.Background(), CreateFactRequest{
		Title: "test fact", Body: "body", Layer: "project", Type: "gotcha",
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDeleteFact_NoEndpoint(t *testing.T) {
	api := &KnowledgeAPI{
		config: KnowledgeConfig{Enabled: true},
		logger: testLogger(),
	}

	err := api.DeleteFact(context.Background(), LayerProject, "test-slug")
	if err == nil {
		t.Error("expected error when no endpoint configured")
	}
}

func TestImportFacts_Markdown(t *testing.T) {
	var ingested int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var facts []ExtractedFact
		json.NewDecoder(r.Body).Decode(&facts)
		ingested = len(facts)
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	api := &KnowledgeAPI{
		layers: []layerClient{{layerType: LayerProject, client: NewClient(ts.URL, testLogger())}},
		config: KnowledgeConfig{Enabled: true},
		logger: testLogger(),
	}

	count, err := api.ImportFacts(context.Background(), LayerProject, "# Fact One\n\nBody one\n\n# Fact Two\n\nBody two", "markdown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 imported, got %d", count)
	}
	if ingested != 2 {
		t.Errorf("expected 2 ingested on server, got %d", ingested)
	}
}

func TestImportFacts_EmptyContent(t *testing.T) {
	api := &KnowledgeAPI{
		layers: []layerClient{{layerType: LayerProject, client: NewClient("http://unused", testLogger())}},
		config: KnowledgeConfig{Enabled: true},
		logger: testLogger(),
	}

	count, err := api.ImportFacts(context.Background(), LayerProject, "", "markdown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 imported for empty content, got %d", count)
	}
}

func TestLayers(t *testing.T) {
	api := &KnowledgeAPI{
		layers: []layerClient{
			{layerType: LayerPersonal},
			{layerType: LayerProject},
			{layerType: LayerProject},
		},
		logger: testLogger(),
	}

	layers := api.Layers()
	if len(layers) != 2 {
		t.Errorf("expected 2 unique layers, got %d", len(layers))
	}
}
