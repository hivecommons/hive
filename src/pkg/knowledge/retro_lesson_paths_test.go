package knowledge

// Tests for the retro-lesson ingestion paths in retro_lesson.go that the
// existing dedup test does not reach: the nil-receiver and rejected-lesson
// guards, the ingest-failure propagation, the zero SourceDate default, the
// graph-triple attachment (including its nil-store, empty-source, bead
// fallback, and write-failure branches), and the small pure helpers
// retroLessonTitle and nearRetroLessonKey.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestIngestRetroLessonNilReceiverAndRejectedLesson(t *testing.T) {
	var k *KnowledgeAPI
	if slug, created, err := k.IngestRetroLesson(context.Background(), RetroLesson{Lesson: "A perfectly reasonable process lesson about validation."}); slug != "" || created || err != nil {
		t.Fatalf("nil receiver: got (%q, %v, %v), want empty no-op", slug, created, err)
	}

	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
	if slug, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{Lesson: "too short"}); slug != "" || created || err != nil {
		t.Fatalf("rejected lesson: got (%q, %v, %v), want empty no-op", slug, created, err)
	}
}

func TestIngestRetroLessonPropagatesIngestFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []searchResult{}})
		case "/api/ingest":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	api := NewKnowledgeAPI([]LayerConfig{{Type: LayerProject, URL: srv.URL}}, KnowledgeConfig{Enabled: true}, discardLogger())
	lesson := RetroLesson{
		Lesson:   "Run targeted validation before opening PRs when changes affect CI-sensitive code paths.",
		SourcePR: "kubestellar/hive#7",
	}
	slug, created, err := api.IngestRetroLesson(context.Background(), lesson)
	if err == nil {
		t.Fatal("expected ingest failure to propagate, got nil error")
	}
	if slug != "" || created {
		t.Fatalf("failed ingest reported (%q, created=%v), want empty and false", slug, created)
	}
}

func TestIngestRetroLessonDefaultsZeroSourceDate(t *testing.T) {
	var stored []ExtractedFact
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search":
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []searchResult{}})
		case "/api/ingest":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &stored); err != nil {
				t.Errorf("decode ingest: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	api := NewKnowledgeAPI([]LayerConfig{{Type: LayerProject, URL: srv.URL}}, KnowledgeConfig{Enabled: true}, discardLogger())
	before := time.Now().UTC().Add(-time.Minute)
	_, created, err := api.IngestRetroLesson(context.Background(), RetroLesson{
		Lesson: "Prefer table-driven tests over copy-pasted assertions when covering rate helpers.",
	})
	if err != nil || !created {
		t.Fatalf("ingest created=%v err=%v", created, err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d facts, want 1", len(stored))
	}
	if stored[0].SourceDate.IsZero() || stored[0].SourceDate.Before(before) {
		t.Fatalf("SourceDate = %v, want defaulted to roughly now", stored[0].SourceDate)
	}
}

func TestAddRetroGraphTriple(t *testing.T) {
	t.Run("nil graph store is a no-op", func(t *testing.T) {
		api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
		api.addRetroGraphTriple("retro-lesson-abc", RetroLesson{SourcePR: "org/repo#1"})
	})

	t.Run("empty source adds nothing", func(t *testing.T) {
		api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
		gs := newTestGraphStore(t)
		api.SetGraphStore(gs)
		api.addRetroGraphTriple("retro-lesson-abc", RetroLesson{SourcePR: "  ", SourceBead: ""})
		if got := gs.Count(); got != 0 {
			t.Fatalf("graph has %d triples, want 0", got)
		}
	})

	t.Run("PR source wins and is recorded as derived_from", func(t *testing.T) {
		api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
		gs := newTestGraphStore(t)
		api.SetGraphStore(gs)
		api.addRetroGraphTriple("retro-lesson-abc", RetroLesson{SourcePR: "org/repo#1", SourceBead: "bead-9"})
		out := gs.Outgoing("retro-lesson-abc", PredicateDerivedFrom)
		if len(out) != 1 || out[0].Object != "retro:org/repo#1" {
			t.Fatalf("outgoing = %#v, want one derived_from retro:org/repo#1", out)
		}
	})

	t.Run("bead source is the fallback", func(t *testing.T) {
		api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
		gs := newTestGraphStore(t)
		api.SetGraphStore(gs)
		api.addRetroGraphTriple("retro-lesson-def", RetroLesson{SourceBead: "bead-9"})
		out := gs.Outgoing("retro-lesson-def", PredicateDerivedFrom)
		if len(out) != 1 || out[0].Object != "retro:bead-9" {
			t.Fatalf("outgoing = %#v, want one derived_from retro:bead-9", out)
		}
	})

	t.Run("write failure is logged, not fatal", func(t *testing.T) {
		api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, discardLogger())
		gs := newTestGraphStore(t)
		api.SetGraphStore(gs)
		_ = gs.Close()
		api.addRetroGraphTriple("retro-lesson-ghi", RetroLesson{SourcePR: "org/repo#2"})
	})
}

func TestRetroLessonTitle(t *testing.T) {
	long := strings.Repeat("word ", 20)
	title := retroLessonTitle(long)
	if want := "Retro lesson: " + strings.TrimSpace(strings.Repeat("word ", 12)); title != want {
		t.Fatalf("title = %q, want twelve-word truncation %q", title, want)
	}
	if got := retroLessonTitle(" .. "); got != "Retro lesson: Generalized retro lesson" {
		t.Fatalf("degenerate body title = %q", got)
	}
	if got := retroLessonTitle("Short lesson."); got != "Retro lesson: Short lesson" {
		t.Fatalf("short body title = %q", got)
	}
}

func TestNearRetroLessonKey(t *testing.T) {
	if nearRetroLessonKey("", "anything") || nearRetroLessonKey("anything", "") {
		t.Fatal("empty keys must never match")
	}
	if !nearRetroLessonKey("run validation before prs", "run validation before prs") {
		t.Fatal("identical keys must match")
	}
	if !nearRetroLessonKey("always run validation before prs", "run validation before prs") {
		t.Fatal("candidate contained in existing must match")
	}
	if !nearRetroLessonKey("run validation", "always run validation before prs") {
		t.Fatal("existing contained in candidate must match")
	}
	if nearRetroLessonKey("rotate tokens quarterly", "run validation before prs") {
		t.Fatal("unrelated keys must not match")
	}
}
