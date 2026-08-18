package knowledge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsUpwardPromotion(t *testing.T) {
	tests := []struct {
		from, to LayerType
		want     bool
	}{
		{LayerPersonal, LayerProject, true},
		{LayerProject, LayerOrg, true},
		{LayerOrg, LayerCommunity, true},
		{LayerPersonal, LayerCommunity, true},
		{LayerCommunity, LayerOrg, false},
		{LayerOrg, LayerProject, false},
		{LayerProject, LayerPersonal, false},
		{LayerProject, LayerProject, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.from)+"→"+string(tt.to), func(t *testing.T) {
			got := isUpwardPromotion(tt.from, tt.to)
			if got != tt.want {
				t.Errorf("isUpwardPromotion(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestPromoteSuccess(t *testing.T) {
	var ingested []ExtractedFact

	sourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := pageResponse{
			Slug:       "guard-join",
			Title:      "Guard .join() against undefined",
			Body:       "Always use (arr || []).join()",
			Type:       "gotcha",
			Status:     "verified",
			Confidence: 0.95,
			Tags:       []string{"typescript"},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(page)
	}))
	defer sourceSrv.Close()

	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &ingested)
		w.WriteHeader(http.StatusCreated)
	}))
	defer targetSrv.Close()

	logger := slog.Default()
	layers := []LayerConfig{
		{Type: LayerProject, URL: sourceSrv.URL},
		{Type: LayerOrg, URL: targetSrv.URL},
	}

	promoter := NewPromoter(layers, CuratorConfig{AutoPromoteThreshold: 0.9}, logger)
	result := promoter.Promote(context.Background(), PromoteRequest{
		Slug:      "guard-join",
		FromLayer: LayerProject,
		ToLayer:   LayerOrg,
		Reason:    "useful for all repos",
		Promoter:  "alice",
	})

	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}
	if len(ingested) != 1 {
		t.Fatalf("expected 1 ingested fact, got %d", len(ingested))
	}
	if ingested[0].Title != "Guard .join() against undefined" {
		t.Errorf("title = %q", ingested[0].Title)
	}
}

func TestPromoteDownwardBlocked(t *testing.T) {
	logger := slog.Default()
	layers := []LayerConfig{
		{Type: LayerOrg, URL: "http://org.test"},
		{Type: LayerProject, URL: "http://project.test"},
	}

	promoter := NewPromoter(layers, CuratorConfig{}, logger)
	result := promoter.Promote(context.Background(), PromoteRequest{
		Slug:      "some-fact",
		FromLayer: LayerOrg,
		ToLayer:   LayerProject,
		Promoter:  "bob",
	})

	if result.Success {
		t.Error("expected failure for downward promotion")
	}
	if result.Error == "" {
		t.Error("expected error message")
	}
}

func TestPromoteMissingLayer(t *testing.T) {
	logger := slog.Default()
	promoter := NewPromoter(nil, CuratorConfig{}, logger)
	result := promoter.Promote(context.Background(), PromoteRequest{
		Slug:      "test",
		FromLayer: LayerProject,
		ToLayer:   LayerOrg,
		Promoter:  "test",
	})

	if result.Success {
		t.Error("expected failure for missing layer")
	}
}

func TestAutoPromoteCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := searchResponse{
			Results: []searchResult{
				{Slug: "high-conf", Confidence: 0.95, Status: "verified"},
				{Slug: "low-conf", Confidence: 0.5, Status: "verified"},
				{Slug: "high-but-draft", Confidence: 0.95, Status: "draft"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	logger := slog.Default()
	layers := []LayerConfig{
		{Type: LayerProject, URL: srv.URL},
		{Type: LayerOrg, URL: "http://org.test"},
	}

	promoter := NewPromoter(layers, CuratorConfig{AutoPromoteThreshold: 0.9}, logger)
	candidates, err := promoter.AutoPromoteCandidates(context.Background(), LayerProject, LayerOrg)
	if err != nil {
		t.Fatalf("AutoPromoteCandidates: %v", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (high confidence + verified), got %d", len(candidates))
	}
	if candidates[0].Slug != "high-conf" {
		t.Errorf("expected slug 'high-conf', got %q", candidates[0].Slug)
	}
}

func TestAutoPromoteCandidatesDownwardBlocked(t *testing.T) {
	logger := slog.Default()
	promoter := NewPromoter(nil, CuratorConfig{}, logger)
	_, err := promoter.AutoPromoteCandidates(context.Background(), LayerOrg, LayerProject)
	if err == nil {
		t.Error("expected error for downward auto-promote")
	}
}
