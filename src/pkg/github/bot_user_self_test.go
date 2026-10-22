package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestMarkerAuthorTrustedResolvesOwnLogin(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"login":"pat-hive","id":1}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "hivecommons", []string{"hivecommons/hive"})

	for i := 0; i < 5; i++ {
		if !c.markerAuthorTrusted(&gh.User{Login: gh.Ptr("pat-hive")}) {
			t.Fatal("pat-hive marker should be trusted")
		}
		if !c.markerAuthorTrusted(&gh.User{Login: gh.Ptr("PAT-Hive")}) {
			t.Fatal("login match should be case-insensitive")
		}
		if c.markerAuthorTrusted(&gh.User{Login: gh.Ptr("mallory")}) {
			t.Fatal("mallory marker must not be trusted")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("GET /user calls = %d, want 1", n)
	}
}

func TestMarkerAuthorTrustedFallsBackWhenSelfLookupFails(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "hivecommons", []string{"hivecommons/hive"})

	for i := 0; i < 5; i++ {
		if !c.markerAuthorTrusted(&gh.User{Login: gh.Ptr("mallory")}) {
			t.Fatal("legacy any-author fallback should apply when the lookup fails")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("GET /user calls = %d, want 1 within the retry-after window", n)
	}
}

func TestCachedSelfLoginDoesNotBlockDuringInflightResolve(t *testing.T) {
	entered := make(chan struct{})
	gate := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"login":"pat-hive","id":1}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "hivecommons", []string{"hivecommons/hive"})

	resolved := make(chan string, 1)
	go func() { resolved <- c.resolveSelfLogin(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("GET /user was never issued")
	}

	cached := make(chan string, 1)
	go func() { cached <- c.cachedSelfLogin() }()
	select {
	case got := <-cached:
		if got != "" {
			t.Fatalf("cachedSelfLogin = %q before the lookup finished, want empty", got)
		}
	case <-time.After(2 * time.Second):
		close(gate)
		t.Fatal("cachedSelfLogin blocked while resolveSelfLogin was in flight")
	}

	close(gate)
	select {
	case got := <-resolved:
		if got != "pat-hive" {
			t.Fatalf("resolveSelfLogin = %q, want pat-hive", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolveSelfLogin did not finish")
	}
	if got := c.cachedSelfLogin(); got != "pat-hive" {
		t.Fatalf("cachedSelfLogin = %q after resolve, want pat-hive", got)
	}
}
