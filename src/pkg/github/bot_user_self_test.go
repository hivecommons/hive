package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
