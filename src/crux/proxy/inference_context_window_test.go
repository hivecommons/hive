package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// modelsServer answers /v1/models with body.
func modelsServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The window is read from whichever field the server uses (vLLM, gufo and
// OpenRouter, LiteLLM), in that precedence (hivecommons/hive#19913).
func TestQueryMaxModelLen_ReadsEveryWindowField(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"vllm max_model_len", `{"data":[{"id":"m","max_model_len":4096}]}`, 4096},
		{"gufo context_length", `{"data":[{"id":"m","context_length":262144}]}`, 262144},
		{"litellm max_input_tokens", `{"data":[{"id":"m","max_input_tokens":128000}]}`, 128000},
		{"max_model_len wins over the others", `{"data":[{"id":"m","max_model_len":8192,"context_length":16384,"max_input_tokens":32768}]}`, 8192},
		{"context_length wins over max_input_tokens", `{"data":[{"id":"m","context_length":16384,"max_input_tokens":32768}]}`, 16384},
		{"null field skipped", `{"data":[{"id":"m","max_model_len":null,"context_length":65536}]}`, 65536},
		{"float spelling", `{"data":[{"id":"m","context_length":262144.0}]}`, 262144},
		{"no window field", `{"data":[{"id":"m"}]}`, 0},
		{"negative ignored", `{"data":[{"id":"m","max_model_len":-1,"context_length":2048}]}`, 2048},
		{"absurd value ignored", `{"data":[{"id":"m","max_model_len":1e300}]}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelsServer(t, tc.body)
			if got := queryMaxModelLen(srv.URL, "m", "", ""); got != tc.want {
				t.Fatalf("window = %d, want %d", got, tc.want)
			}
		})
	}
}

// On a multi-model gateway an unmatched model must not take another model's
// window; a single-model server may still answer for its only model.
func TestQueryMaxModelLen_NoCrossModelFallback(t *testing.T) {
	multi := modelsServer(t, `{"data":[{"id":"big","context_length":1048576},{"id":"small","context_length":262144}]}`)
	if got := queryMaxModelLen(multi.URL, "absent", "", ""); got != 0 {
		t.Fatalf("multi-model unmatched = %d, want 0", got)
	}
	if got := queryMaxModelLen(multi.URL, "small", "", ""); got != 262144 {
		t.Fatalf("multi-model matched = %d, want 262144", got)
	}
	single := modelsServer(t, `{"data":[{"id":"only","context_length":32768}]}`)
	if got := queryMaxModelLen(single.URL, "alias", "", ""); got != 32768 {
		t.Fatalf("single-model fallback = %d, want 32768", got)
	}
}

// A discovered window is cached by endpoint and model, and a later route for
// the same model (a relaunch, another agent) carries it from the moment it is
// set, so the CLI launched right after can be told its window.
func TestSetInferenceRoute_SeedsWindowFromEarlierDiscovery(t *testing.T) {
	// Two models, so the single-model fallback cannot answer for "vllm/DeepSeek"
	// below and its background probe resolves nothing.
	srv := modelsServer(t, `{"data":[{"id":"vllm/Qwen","context_length":262144},{"id":"vllm/Other","context_length":4096}]}`)
	p := newInferenceTestProxy()

	p.SetInferenceRoute("quality", &InferenceRoute{Backend: "litellm", Endpoint: srv.URL, Model: "vllm/Qwen"})
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for w, _ := p.InferenceContextWindow("quality"); w != 262144; w, _ = p.InferenceContextWindow("quality") {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("background probe never resolved the window")
		}
	}

	caller := &InferenceRoute{Backend: "litellm", Endpoint: srv.URL, Model: "vllm/Qwen"}
	p.SetInferenceRoute("supervisor", caller)
	w, model := p.InferenceContextWindow("supervisor")
	if w != 262144 || model != "vllm/Qwen" {
		t.Fatalf("seeded route = (%d, %q), want (262144, vllm/Qwen)", w, model)
	}
	if caller.MaxContextLen != 0 {
		t.Fatal("SetInferenceRoute mutated the caller's route")
	}

	// A different model on the same endpoint is not seeded.
	p.SetInferenceRoute("scanner", &InferenceRoute{Backend: "litellm", Endpoint: srv.URL, Model: "vllm/DeepSeek"})
	if w, _ := p.InferenceContextWindow("scanner"); w != 0 {
		t.Fatalf("other model seeded with %d, want 0", w)
	}
}

func TestInferenceContextWindow_NoRoute(t *testing.T) {
	p := newInferenceTestProxy()
	if w, model := p.InferenceContextWindow("nobody"); w != 0 || model != "" {
		t.Fatalf("no route = (%d, %q), want (0, \"\")", w, model)
	}
}

func TestUpdateMaxContextLen_CachesOnlyAppliedPositiveWindows(t *testing.T) {
	router := newInferenceRouter()
	router.Set("a", &InferenceRoute{Endpoint: "http://e", Model: "m"})

	if router.UpdateMaxContextLen("a", "http://e", "other", 4096) {
		t.Fatal("stale update applied")
	}
	if got := router.cachedContextWindow("http://e", "other"); got != 0 {
		t.Fatalf("stale update cached %d", got)
	}
	router.UpdateMaxContextLen("a", "http://e", "m", 0)
	if got := router.cachedContextWindow("http://e", "m"); got != 0 {
		t.Fatalf("zero window cached as %d", got)
	}
	router.UpdateMaxContextLen("a", "http://e", "m", 8192)
	if got := router.cachedContextWindow("http://e", "m"); got != 8192 {
		t.Fatalf("cached = %d, want 8192", got)
	}
}
