package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// liteLLMServer answers /v1/models with a listing that names no window, as
// LiteLLM's does, and /model/info with info. It records the Authorization
// header /model/info was called with.
func liteLLMServer(t *testing.T, info string) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"vllm/Qwen","object":"model"},{"id":"vllm/Small","object":"model"}]}`)
		case "/model/info":
			mu.Lock()
			auth = r.Header.Get("Authorization")
			mu.Unlock()
			fmt.Fprint(w, info)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return auth }
}

// LiteLLM's /v1/models reports no window, so every route behind a LiteLLM
// gateway resolved maxContextLen=0 and the CLI was told no window
// (hivecommons/hive#19913). The gateway's /model/info carries
// model_info.max_input_tokens for the deployment named by model_name.
func TestQueryMaxModelLen_FallsBackToLiteLLMModelInfo(t *testing.T) {
	info := `{"data":[` +
		`{"model_name":"vllm/Small","model_info":{"max_input_tokens":131072,"max_tokens":8192}},` +
		`{"model_name":"vllm/Qwen","model_info":{"max_input_tokens":null}},` +
		`{"model_name":"vllm/Qwen","model_info":{"max_input_tokens":196608,"max_tokens":4096}}]}`
	srv, auth := liteLLMServer(t, info)
	for _, endpoint := range []string{srv.URL, srv.URL + "/v1", srv.URL + "/v1/"} {
		if got := queryMaxModelLen(endpoint, "vllm/Qwen", "sk-key", ""); got != 196608 {
			t.Fatalf("endpoint %q: window = %d, want 196608 from /model/info", endpoint, got)
		}
	}
	if got := auth(); got != "Bearer sk-key" {
		t.Errorf("/model/info Authorization = %q, want the route key", got)
	}
	if got := queryMaxModelLen(srv.URL, "vllm/Small", "", ""); got != 131072 {
		t.Fatalf("second model window = %d, want 131072", got)
	}
	// A model the gateway does not list takes no other model's window.
	if got := queryMaxModelLen(srv.URL, "vllm/Absent", "", ""); got != 0 {
		t.Fatalf("unlisted model window = %d, want 0", got)
	}
}

// max_tokens is LiteLLM's output limit for many models, so an entry without
// max_input_tokens yields no window rather than a far too small one.
func TestQueryMaxModelLen_LiteLLMModelInfoIgnoresMaxTokens(t *testing.T) {
	srv, _ := liteLLMServer(t, `{"data":[{"model_name":"vllm/Qwen","model_info":{"max_tokens":4096}}]}`)
	if got := queryMaxModelLen(srv.URL, "vllm/Qwen", "", ""); got != 0 {
		t.Fatalf("window = %d, want 0 without max_input_tokens", got)
	}
}

// A window in the /v1/models listing still wins, and /model/info is not
// asked when the listing answers.
func TestQueryMaxModelLen_ListingWindowWinsOverModelInfo(t *testing.T) {
	var infoHits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/info" {
			mu.Lock()
			infoHits++
			mu.Unlock()
			fmt.Fprint(w, `{"data":[{"model_name":"m","model_info":{"max_input_tokens":999}}]}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"m","max_model_len":4096}]}`)
	}))
	t.Cleanup(srv.Close)
	if got := queryMaxModelLen(srv.URL, "m", "", ""); got != 4096 {
		t.Fatalf("window = %d, want 4096 from the listing", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if infoHits != 0 {
		t.Errorf("/model/info asked %d times though the listing named a window", infoHits)
	}
}
