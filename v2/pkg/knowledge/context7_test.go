package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// SearchContext7
// ---------------------------------------------------------------------------

func TestSearchContext7_HappyPath(t *testing.T) {
	want := []Context7SearchResult{
		{ID: "/vercel/next.js", Title: "Next.js", Snippets: 42, Trust: 0.95},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != context7SearchPath {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("libraryName") != "nextjs" {
			t.Errorf("missing/wrong libraryName param")
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing/wrong Authorization header")
		}
		if r.Header.Get("User-Agent") != "hive-knowledge/1.0" {
			t.Errorf("unexpected User-Agent: %q", r.Header.Get("User-Agent"))
		}
		json.NewEncoder(w).Encode(context7SearchResponse{Results: want})
	}))
	defer srv.Close()

	// Temporarily override the base URL.
	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL(srv.URL)

	got, err := SearchContext7(context.Background(), "nextjs", "test-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "/vercel/next.js" {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestSearchContext7_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL(srv.URL)

	_, err := SearchContext7(context.Background(), "test", "")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
}

func TestSearchContext7_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{invalid"))
	}))
	defer srv.Close()

	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL(srv.URL)

	_, err := SearchContext7(context.Background(), "test", "")
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("expected wrapped json.SyntaxError, got %T: %v", err, err)
	}
}

func TestSearchContext7_NoAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("Authorization header should be absent when apiKey is empty")
		}
		json.NewEncoder(w).Encode(context7SearchResponse{})
	}))
	defer srv.Close()

	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL(srv.URL)

	_, err := SearchContext7(context.Background(), "test", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// FetchContext7Docs
// ---------------------------------------------------------------------------

func TestFetchContext7Docs_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/llms.txt") {
			t.Errorf("expected path ending with /llms.txt, got %q", r.URL.Path)
		}
		if r.URL.Query().Get("tokens") != "10000" {
			t.Errorf("missing/wrong tokens param")
		}
		w.Write([]byte("# Next.js Documentation\n\nSome docs here."))
	}))
	defer srv.Close()

	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL(srv.URL)

	got, err := FetchContext7Docs(context.Background(), "/vercel/next.js", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.LibraryID != "/vercel/next.js" {
		t.Errorf("LibraryID = %q, want /vercel/next.js", got.LibraryID)
	}
	if !strings.Contains(got.Content, "Next.js Documentation") {
		t.Errorf("Content should include docs, got %q", got.Content)
	}
}

func TestFetchContext7Docs_WithTopicQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("topic") != "routing" {
			t.Errorf("expected topic=routing, got %q", r.URL.Query().Get("topic"))
		}
		w.Write([]byte("routing docs"))
	}))
	defer srv.Close()

	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL(srv.URL)

	got, err := FetchContext7Docs(context.Background(), "/vercel/next.js", "routing", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got.Content, "routing") {
		t.Errorf("expected routing content, got %q", got.Content)
	}
}

func TestFetchContext7Docs_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL(srv.URL)

	_, err := FetchContext7Docs(context.Background(), "/test/lib", "", "")
	if err == nil {
		t.Fatal("expected error on 429")
	}
}

func TestFetchContext7Docs_CancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should not reach here"))
	}))
	defer srv.Close()

	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := FetchContext7Docs(ctx, "/test/lib", "", "")
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestFetchContext7Docs_RejectsAuthorityInjectionLibraryID(t *testing.T) {
	var hits atomic.Int32
	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL("https://context7.com")

	origClient := context7Client
	defer func() { context7Client = origClient }()
	context7Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hits.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}

	_, err := FetchContext7Docs(context.Background(), "@evil.com/path", "", "")
	if err == nil {
		t.Fatal("expected authority-injection library ID to be rejected")
	}
	if hits.Load() != 0 {
		t.Fatalf("malformed library ID triggered %d HTTP request(s), want 0", hits.Load())
	}
}

func TestFetchContext7Docs_BlocksRedirectToPrivateHost(t *testing.T) {
	var privateHits atomic.Int32
	privateTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer privateTarget.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, privateTarget.URL+"/secret", http.StatusFound)
	}))
	defer redirector.Close()

	origBase := context7BaseURL
	defer func() { setContext7BaseURL(origBase) }()
	setContext7BaseURL(redirector.URL)

	_, err := FetchContext7Docs(context.Background(), "/safe/lib", "", "")
	if err == nil {
		t.Fatal("expected redirect to private httptest host to be blocked")
	}
	if privateHits.Load() != 0 {
		t.Fatalf("redirect guard allowed %d request(s) to private target, want 0", privateHits.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
