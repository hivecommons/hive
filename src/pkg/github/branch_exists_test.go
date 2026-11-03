package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBranchExists_DistinguishesMissingFromAPIFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/branches/v6":
			_, _ = w.Write([]byte(`{"name":"v6"}`))
		case "/repos/o/r/branches/v9":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"message":"rate limited"}`, http.StatusForbidden)
		}
	}))
	defer server.Close()
	client := testClient(t, server.URL)

	if exists, err := client.BranchExists(context.Background(), "o/r", "v6"); err != nil || !exists {
		t.Fatalf("existing branch = (%v, %v)", exists, err)
	}
	if exists, err := client.BranchExists(context.Background(), "o/r", "v9"); err != nil || exists {
		t.Fatalf("missing branch = (%v, %v)", exists, err)
	}
	if exists, err := client.BranchExists(context.Background(), "o/r", "v7"); err == nil || exists {
		t.Fatalf("API failure = (%v, %v), want false + error", exists, err)
	}
}
