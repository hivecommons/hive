package upstreamwatch

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

const testMarker = "<!-- upstream-ref: a/b#1 -->"

func rateLimited(w http.ResponseWriter) {
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", "1")
	w.WriteHeader(http.StatusForbidden)
	writeJSON(w, `{"message":"API rate limit exceeded"}`)
}

func TestFindMarker_WaitsOutSearchRateLimit(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			rateLimited(w)
			return
		}
		writeJSON(w, `{"total_count":1,"items":[{"number":7,"state":"open","body":"x `+testMarker+`"}]}`)
	})
	f := NewGitHubFiler(newTestClient(t, mux), "fork", "repo")
	var waits atomic.Int32
	f.wait = func(context.Context, time.Duration) error {
		waits.Add(1)
		return nil
	}
	got, found, err := f.FindMarker(context.Background(), testMarker)
	if err != nil || !found || got.Number != 7 {
		t.Fatalf("FindMarker = %+v, %v, %v", got, found, err)
	}
	if calls.Load() != 2 || waits.Load() != 1 {
		t.Fatalf("calls = %d, waits = %d; want one retry after one wait", calls.Load(), waits.Load())
	}
}

func TestFindMarker_RateLimitWaitInterrupted(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, _ *http.Request) { rateLimited(w) })
	f := NewGitHubFiler(newTestClient(t, mux), "fork", "repo")
	f.wait = func(context.Context, time.Duration) error { return context.Canceled }
	if _, _, err := f.FindMarker(context.Background(), testMarker); err == nil {
		t.Fatal("want an error when the wait is interrupted")
	}
}
