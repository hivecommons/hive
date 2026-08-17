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

func promoteExtraLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestPromote_MissingTargetLayer(t *testing.T) {
	sourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(pageResponse{Slug: "fact-1", Title: "Test"})
	}))
	defer sourceSrv.Close()

	layers := []LayerConfig{
		{Type: LayerProject, URL: sourceSrv.URL},
		// No org layer configured
	}
	promoter := NewPromoter(layers, CuratorConfig{}, promoteExtraLogger())

	result := promoter.Promote(context.Background(), PromoteRequest{
		Slug: "fact-1", FromLayer: LayerProject, ToLayer: LayerOrg,
	})
	if result.Success {
		t.Error("expected failure for missing target layer")
	}
	if result.Error == "" {
		t.Error("expected error message")
	}
}

func TestPromote_ReadPageError(t *testing.T) {
	sourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sourceSrv.Close()

	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer targetSrv.Close()

	layers := []LayerConfig{
		{Type: LayerProject, URL: sourceSrv.URL},
		{Type: LayerOrg, URL: targetSrv.URL},
	}
	promoter := NewPromoter(layers, CuratorConfig{}, promoteExtraLogger())

	result := promoter.Promote(context.Background(), PromoteRequest{
		Slug: "bad", FromLayer: LayerProject, ToLayer: LayerOrg,
	})
	if result.Success {
		t.Error("expected failure when reading page fails")
	}
}

func TestPromote_IngestError(t *testing.T) {
	sourceSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(pageResponse{Slug: "fact-1", Title: "Test", Body: "Body"})
	}))
	defer sourceSrv.Close()

	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer targetSrv.Close()

	layers := []LayerConfig{
		{Type: LayerProject, URL: sourceSrv.URL},
		{Type: LayerOrg, URL: targetSrv.URL},
	}
	promoter := NewPromoter(layers, CuratorConfig{}, promoteExtraLogger())

	result := promoter.Promote(context.Background(), PromoteRequest{
		Slug: "fact-1", FromLayer: LayerProject, ToLayer: LayerOrg,
	})
	if result.Success {
		t.Error("expected failure when ingest fails")
	}
}

func TestAutoPromoteCandidates_MissingLayer(t *testing.T) {
	promoter := NewPromoter(nil, CuratorConfig{}, promoteExtraLogger())
	_, err := promoter.AutoPromoteCandidates(context.Background(), LayerProject, LayerOrg)
	if err == nil {
		t.Error("expected error for missing layer")
	}
}

func TestAutoPromoteCandidates_ListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	layers := []LayerConfig{
		{Type: LayerProject, URL: srv.URL},
		{Type: LayerOrg, URL: srv.URL},
	}
	promoter := NewPromoter(layers, CuratorConfig{AutoPromoteThreshold: 0.9}, promoteExtraLogger())
	_, err := promoter.AutoPromoteCandidates(context.Background(), LayerProject, LayerOrg)
	if err == nil {
		t.Error("expected error when list fails")
	}
}

func TestNewPromoter_SkipsEmptyEndpoints(t *testing.T) {
	layers := []LayerConfig{
		{Type: LayerProject, URL: "http://project.test"},
		{Type: LayerOrg, Path: "/local/only"}, // no URL
	}
	promoter := NewPromoter(layers, CuratorConfig{}, promoteExtraLogger())
	if len(promoter.layers) != 1 {
		t.Errorf("layers = %d, want 1 (empty endpoint skipped)", len(promoter.layers))
	}
}

func TestAutoPromoteCandidates_NotUpward(t *testing.T) {
	// Both layers configured but promotion direction is downward
	orgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]pageResponse{})
	}))
	defer orgSrv.Close()

	projectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]pageResponse{})
	}))
	defer projectSrv.Close()

	layers := []LayerConfig{
		{Type: LayerOrg, URL: orgSrv.URL},
		{Type: LayerProject, URL: projectSrv.URL},
	}
	promoter := NewPromoter(layers, CuratorConfig{AutoPromoteThreshold: 0.8}, promoteExtraLogger())
	// org → project is downward — should fail
	_, err := promoter.AutoPromoteCandidates(context.Background(), LayerOrg, LayerProject)
	if err == nil {
		t.Error("expected error for downward auto-promote")
	}
}

func TestIngestToLayer_ServerError(t *testing.T) {
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	layers := []LayerConfig{
		{Type: LayerProject, URL: errSrv.URL},
	}
	promoter := NewPromoter(layers, CuratorConfig{}, promoteExtraLogger())
	facts := []ExtractedFact{{Title: "test", Body: "body"}}
	err := promoter.ingestToLayer(context.Background(), promoter.layers[LayerProject], facts)
	if err == nil {
		t.Error("expected error for 500 from ingest endpoint")
	}
}
