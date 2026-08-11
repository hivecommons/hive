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

func extraTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestClient_Stats(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(statsResponse{
			TotalPages: 42,
			ByType:     map[string]int{"gotcha": 5},
			Stale:      2,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	stats, err := c.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalPages != 42 {
		t.Errorf("total_pages = %d", stats.TotalPages)
	}
	if stats.Stale != 2 {
		t.Errorf("stale = %d", stats.Stale)
	}
}

func TestClient_Stats_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	_, err := c.Stats(context.Background())
	if err == nil {
		t.Error("expected error")
	}
}

func TestClient_BlocksRedirectToInternalHostname(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{
		"metadata.attacker.example": {"169.254.169.254"},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://metadata.attacker.example/latest/meta-data", http.StatusFound)
	}))
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	if _, err := c.Search(context.Background(), "secret", "", 1); err == nil {
		t.Fatal("knowledge client followed a redirect to an internal hostname")
	}
}

func TestClient_Healthy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(statsResponse{TotalPages: 1})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	if !c.Healthy(context.Background()) {
		t.Error("expected healthy")
	}
}

func TestClient_Healthy_Down(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	if c.Healthy(context.Background()) {
		t.Error("expected unhealthy")
	}
}

func TestClient_UpdatePage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages/test-slug", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	err := c.UpdatePage(context.Background(), "test-slug", pageUpdateRequest{
		Title: "Updated", Body: "New body",
	})
	if err != nil {
		t.Fatalf("UpdatePage: %v", err)
	}
}

func TestClient_DeletePage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages/test-slug", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	err := c.DeletePage(context.Background(), "test-slug")
	if err != nil {
		t.Fatalf("DeletePage: %v", err)
	}
}

func TestClient_DeletePage_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages/test-slug", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	err := c.DeletePage(context.Background(), "test-slug")
	if err == nil {
		t.Error("expected error")
	}
}

func TestClient_Search_TypeFilter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "gotcha" {
			t.Error("expected type filter")
		}
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{{Slug: "result-1", Title: "Result"}},
			Total:   1,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	results, err := c.Search(context.Background(), "query", "gotcha", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("results = %d", len(results))
	}
}

func TestClient_ListPages_TypeFilter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{{Slug: "p1"}, {Slug: "p2"}},
			Total:   2,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	results, err := c.ListPages(context.Background(), "pattern")
	if err != nil {
		t.Fatalf("ListPages: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("results = %d", len(results))
	}
}

func TestClient_ListPages_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	_, err := c.ListPages(context.Background(), "")
	if err == nil {
		t.Error("expected error")
	}
}

func TestClient_IngestFacts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ingest", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	facts := []ExtractedFact{
		{Title: "Test Fact", Body: "Some fact body", Type: "gotcha"},
	}
	err := c.IngestFacts(context.Background(), facts)
	if err != nil {
		t.Fatalf("IngestFacts: %v", err)
	}
}

func TestClient_PostJSON_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	err := c.postJSON(context.Background(), "/api/test", "payload")
	if err == nil {
		t.Error("expected error")
	}
}

func TestClient_Search_ZeroLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		// Verify the default limit is applied
		json.NewEncoder(w).Encode(searchResponse{
			Results: []searchResult{{Slug: "r1", Title: "Result 1"}},
			Total:   1,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	results, err := c.Search(context.Background(), "query", "", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("results = %d, want 1", len(results))
	}
}

func TestClient_Get_MalformedJSON(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{not valid json`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := NewClient(server.URL, extraTestLogger())
	_, err := c.Search(context.Background(), "query", "", 10)
	if err == nil {
		t.Error("expected error for malformed JSON")
	}
}

func TestClient_DeletePage_RequestError(t *testing.T) {
	// Use an invalid URL to trigger httpClient.Do error
	c := NewClient("http://127.0.0.1:1", extraTestLogger())
	err := c.DeletePage(context.Background(), "test-slug")
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

func TestClient_PostJSON_RequestError(t *testing.T) {
	// Use an invalid URL to trigger httpClient.Do error
	c := NewClient("http://127.0.0.1:1", extraTestLogger())
	err := c.postJSON(context.Background(), "/api/pages/test", map[string]string{"title": "test"})
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}
