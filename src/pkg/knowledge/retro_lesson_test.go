package knowledge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidRetroLessonGatesLengthAndSecrets(t *testing.T) {
	if ValidRetroLesson("too short") {
		t.Fatal("short lesson should be rejected")
	}
	if ValidRetroLesson("Store this token ghp_abcdefghijklmnopqrstuvwxyz in documentation for retries") {
		t.Fatal("secret-like lesson should be rejected")
	}
	if !ValidRetroLesson("Run targeted validation before opening PRs for CI-sensitive changes.") {
		t.Fatal("valid process lesson should pass")
	}
}

func TestIngestRetroLessonDeduplicates(t *testing.T) {
	var ingests int
	var stored []ExtractedFact
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			if len(stored) == 0 {
				_ = json.NewEncoder(w).Encode(searchResponse{Results: []searchResult{}})
				return
			}
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []searchResult{{
				Slug:       "retro-lesson-existing",
				Title:      stored[0].Title,
				Snippet:    stored[0].Body,
				Type:       string(stored[0].Type),
				Confidence: stored[0].Confidence,
			}}})
		case "/api/ingest":
			ingests++
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &stored); err != nil {
				t.Fatalf("decode ingest: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	api := NewKnowledgeAPI([]LayerConfig{{Type: LayerProject, URL: srv.URL}}, KnowledgeConfig{Enabled: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lesson := RetroLesson{
		Lesson:     "Run targeted validation before opening PRs when changes affect CI-sensitive code paths.",
		SourceBead: "bead-1",
		SourcePR:   "hivecommons/hive#42",
		SourceDate: time.Now().UTC(),
	}
	if _, created, err := api.IngestRetroLesson(context.Background(), lesson); err != nil || !created {
		t.Fatalf("first ingest created=%v err=%v", created, err)
	}
	if len(stored) != 1 || stored[0].SourcePR != "retro bead:bead-1 pr:hivecommons/hive#42" {
		t.Fatalf("stored payload = %#v", stored)
	}
	if _, created, err := api.IngestRetroLesson(context.Background(), lesson); err != nil || created {
		t.Fatalf("second ingest created=%v err=%v, want duplicate skip", created, err)
	}
	if ingests != 1 {
		t.Fatalf("ingests = %d, want 1", ingests)
	}
}
