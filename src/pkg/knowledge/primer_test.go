package knowledge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildQuery(t *testing.T) {
	tests := []struct {
		name      string
		filePaths []string
		keywords  []string
		want      string
	}{
		{
			name:      "empty",
			filePaths: nil,
			keywords:  nil,
			want:      "",
		},
		{
			name:      "keywords only",
			filePaths: nil,
			keywords:  []string{"auth", "jwt"},
			want:      "auth jwt",
		},
		{
			name:      "file paths only",
			filePaths: []string{"src/hooks/useSearchIndex.ts", "src/components/CardWrapper.tsx"},
			keywords:  nil,
			want:      "useSearchIndex CardWrapper",
		},
		{
			name:      "mixed",
			filePaths: []string{"pkg/dashboard/server.go"},
			keywords:  []string{"dashboard", "SSE"},
			want:      "server dashboard SSE",
		},
		{
			name:      "index files skipped",
			filePaths: []string{"src/index.ts"},
			keywords:  []string{"setup"},
			want:      "setup",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildQuery(tt.filePaths, tt.keywords)
			if got != tt.want {
				t.Errorf("buildQuery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMergeWithPrecedence(t *testing.T) {
	p := &Primer{config: PrimerConfig{MaxFacts: 25}}

	facts := []Fact{
		{Slug: "dco-signing", Title: "DCO", Layer: LayerCommunity, Confidence: 0.8},
		{Slug: "dco-signing", Title: "DCO (org override)", Layer: LayerOrg, Confidence: 0.95},
		{Slug: "guard-join", Title: "Guard .join()", Layer: LayerProject, Confidence: 0.9},
		{Slug: "guard-join", Title: "Guard .join() (personal)", Layer: LayerPersonal, Confidence: 1.0},
		{Slug: "envtest", Title: "envtest timeout", Layer: LayerCommunity, Confidence: 0.7},
	}

	merged := p.mergeWithPrecedence(facts)

	if len(merged) != 3 {
		t.Fatalf("expected 3 merged facts, got %d", len(merged))
	}

	bySlug := map[string]Fact{}
	for _, f := range merged {
		bySlug[f.Slug] = f
	}

	if f, ok := bySlug["dco-signing"]; !ok || f.Layer != LayerOrg {
		t.Errorf("dco-signing should come from org layer, got %v", f.Layer)
	}
	if f, ok := bySlug["guard-join"]; !ok || f.Layer != LayerPersonal {
		t.Errorf("guard-join should come from personal layer, got %v", f.Layer)
	}
	if f, ok := bySlug["envtest"]; !ok || f.Layer != LayerCommunity {
		t.Errorf("envtest should come from community layer, got %v", f.Layer)
	}
}

func TestApplyPriority(t *testing.T) {
	p := &Primer{
		config: PrimerConfig{
			Priority: []string{"regression", "gotcha", "pattern", "decision"},
		},
	}

	facts := []Fact{
		{Slug: "a", Type: FactDecision, Confidence: 1.0},
		{Slug: "b", Type: FactGotcha, Confidence: 0.8},
		{Slug: "c", Type: FactRegression, Confidence: 0.9},
		{Slug: "d", Type: FactPattern, Confidence: 0.95},
	}

	sorted := p.applyPriority(facts)

	expectedOrder := []string{"c", "b", "d", "a"}
	for i, slug := range expectedOrder {
		if sorted[i].Slug != slug {
			t.Errorf("position %d: expected %s, got %s", i, slug, sorted[i].Slug)
		}
	}
}

func TestPrimeWithMockWiki(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := searchResponse{
			Total: 2,
			Results: []searchResult{
				{
					Slug:       "guard-join",
					Title:      "Guard .join() against undefined",
					Score:      0.95,
					Type:       "gotcha",
					Status:     "verified",
					Confidence: 0.95,
					Tags:       []string{"typescript", "hooks"},
					Snippet:    "Always use (arr || []).join()",
				},
				{
					Slug:       "isDemoData-wiring",
					Title:      "isDemoData wiring is mandatory",
					Score:      0.88,
					Type:       "pattern",
					Status:     "verified",
					Confidence: 0.92,
					Tags:       []string{"react", "cards"},
					Snippet:    "Every card using useCached* hooks MUST destructure isDemoData",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	logger := slog.Default()
	layers := []LayerConfig{
		{Type: LayerProject, URL: srv.URL, Shared: true},
	}
	config := PrimerConfig{
		MaxFacts: 25,
		Priority: []string{"regression", "gotcha", "pattern"},
	}

	primer := NewPrimer(layers, config, logger)
	result := primer.Prime(context.Background(), []string{"src/hooks/useSearchIndex.ts"}, []string{"hooks"})

	if len(result.Facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(result.Facts))
	}

	if result.Facts[0].Type != FactGotcha {
		t.Errorf("expected gotcha first (higher priority), got %s", result.Facts[0].Type)
	}
	if result.Facts[1].Type != FactPattern {
		t.Errorf("expected pattern second, got %s", result.Facts[1].Type)
	}

	prompt := result.FormatForPrompt()
	if prompt == "" {
		t.Error("FormatForPrompt returned empty string")
	}
	if !contains(prompt, "Guard .join()") {
		t.Error("prompt should contain fact title")
	}
}

func TestPrimeGracefulDegradation(t *testing.T) {
	logger := slog.Default()
	layers := []LayerConfig{
		{Type: LayerOrg, URL: "http://127.0.0.1:1", Shared: true},
	}
	config := PrimerConfig{MaxFacts: 25}

	primer := NewPrimer(layers, config, logger)
	result := primer.Prime(context.Background(), nil, []string{"test"})

	if len(result.Facts) != 0 {
		t.Errorf("expected 0 facts when wiki unreachable, got %d", len(result.Facts))
	}
}

func TestPrime_TruncatesToMaxFacts(t *testing.T) {
	// Return 5 results but set MaxFacts to 2 — should truncate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := searchResponse{
			Total: 5,
			Results: []searchResult{
				{Slug: "fact-1", Title: "Fact 1", Score: 0.9, Type: "gotcha", Status: "verified", Confidence: 0.9},
				{Slug: "fact-2", Title: "Fact 2", Score: 0.8, Type: "gotcha", Status: "verified", Confidence: 0.8},
				{Slug: "fact-3", Title: "Fact 3", Score: 0.7, Type: "pattern", Status: "verified", Confidence: 0.7},
				{Slug: "fact-4", Title: "Fact 4", Score: 0.6, Type: "pattern", Status: "verified", Confidence: 0.6},
				{Slug: "fact-5", Title: "Fact 5", Score: 0.5, Type: "pattern", Status: "verified", Confidence: 0.5},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	logger := slog.Default()
	layers := []LayerConfig{
		{Type: LayerProject, URL: srv.URL, Shared: true},
	}
	config := PrimerConfig{
		MaxFacts: 2,
		Priority: []string{"gotcha"},
	}

	primer := NewPrimer(layers, config, logger)
	result := primer.Prime(context.Background(), []string{"file.go"}, nil)

	if len(result.Facts) != 2 {
		t.Errorf("expected 2 facts (MaxFacts=2), got %d", len(result.Facts))
	}
}

func TestFormatForPromptEmpty(t *testing.T) {
	pk := &PrimedKnowledge{}
	if pk.FormatForPrompt() != "" {
		t.Error("empty knowledge should produce empty prompt")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
