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

func clientTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestClient_ReadPage_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages/test-page", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		json.NewEncoder(w).Encode(pageResponse{
			Slug:       "test-page",
			Title:      "Test Page",
			Body:       "This is the body content.",
			Type:       "pattern",
			Status:     "active",
			Confidence: 0.9,
			Tags:       []string{"go", "testing"},
			Sources:    []string{"pr-123"},
			Backlinks:  []string{"other-page"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, clientTestLogger())
	page, err := c.ReadPage(context.Background(), "test-page")
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if page.Slug != "test-page" {
		t.Errorf("slug = %q, want %q", page.Slug, "test-page")
	}
	if page.Title != "Test Page" {
		t.Errorf("title = %q, want %q", page.Title, "Test Page")
	}
	if page.Body != "This is the body content." {
		t.Errorf("body = %q", page.Body)
	}
	if page.Confidence != 0.9 {
		t.Errorf("confidence = %f, want 0.9", page.Confidence)
	}
	if len(page.Tags) != 2 {
		t.Errorf("tags len = %d, want 2", len(page.Tags))
	}
	if len(page.Sources) != 1 {
		t.Errorf("sources len = %d, want 1", len(page.Sources))
	}
	if len(page.Backlinks) != 1 {
		t.Errorf("backlinks len = %d, want 1", len(page.Backlinks))
	}
}

func TestClient_ReadPage_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages/missing", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("page not found"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, clientTestLogger())
	_, err := c.ReadPage(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error for missing page")
	}
}

func TestClient_ReadPage_MalformedJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages/bad", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, clientTestLogger())
	_, err := c.ReadPage(context.Background(), "bad")
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestClient_ReadPage_Unreachable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", clientTestLogger())
	_, err := c.ReadPage(context.Background(), "any-slug")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestClient_Search_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "auth" {
			t.Errorf("unexpected query: %s", r.URL.Query().Get("q"))
		}
		if r.URL.Query().Get("limit") != "5" {
			t.Errorf("unexpected limit: %s", r.URL.Query().Get("limit"))
		}
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{
				{Slug: "auth-pattern", Title: "Auth Pattern", Score: 0.95, Confidence: 0.8},
				{Slug: "auth-gotcha", Title: "Auth Gotcha", Score: 0.7, Confidence: 0.6},
			},
			Total: 2,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, clientTestLogger())
	results, err := c.Search(context.Background(), "auth", "", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Slug != "auth-pattern" {
		t.Errorf("first result slug = %q", results[0].Slug)
	}
	if results[0].Score != 0.95 {
		t.Errorf("first result score = %f", results[0].Score)
	}
}

func TestClient_Search_Unreachable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", clientTestLogger())
	_, err := c.Search(context.Background(), "query", "", 10)
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestClient_UpdatePage_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages/fail-slug", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("access denied"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, clientTestLogger())
	err := c.UpdatePage(context.Background(), "fail-slug", pageUpdateRequest{
		Title: "Update", Body: "body",
	})
	if err == nil {
		t.Fatal("expected error for forbidden response")
	}
}

func TestClient_UpdatePage_Unreachable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", clientTestLogger())
	err := c.UpdatePage(context.Background(), "slug", pageUpdateRequest{Title: "t"})
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestClient_IngestFacts_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("service unavailable"))
	}))
	defer server.Close()

	c := NewClient(server.URL, clientTestLogger())
	facts := []ExtractedFact{
		{Title: "Fact", Body: "body", Type: "gotcha"},
	}
	err := c.IngestFacts(context.Background(), facts)
	if err == nil {
		t.Fatal("expected error for service unavailable")
	}
}

func TestClient_IngestFacts_Unreachable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", clientTestLogger())
	err := c.IngestFacts(context.Background(), []ExtractedFact{{Title: "f"}})
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestClient_Get_CancelledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(statsResponse{TotalPages: 1})
	}))
	defer server.Close()

	c := NewClient(server.URL, clientTestLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := c.Stats(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestClient_DeletePage_CancelledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := NewClient(server.URL, clientTestLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.DeletePage(ctx, "slug")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestClient_PostJSON_CancelledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewClient(server.URL, clientTestLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.postJSON(ctx, "/api/test", "data")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestNewClient_DefaultFields(t *testing.T) {
	c := NewClient("http://example.com", clientTestLogger())
	if c.baseURL != "http://example.com" {
		t.Errorf("baseURL = %q", c.baseURL)
	}
	if c.httpClient == nil {
		t.Error("httpClient is nil")
	}
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("timeout = %v, want %v", c.httpClient.Timeout, defaultTimeout)
	}
}
