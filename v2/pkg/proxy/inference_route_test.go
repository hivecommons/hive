package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// ---------------------------------------------------------------------------
// UpdateMaxContextLen — copy-on-write semantics and staleness guards
// ---------------------------------------------------------------------------

func TestUpdateMaxContextLen(t *testing.T) {
	t.Run("updates matching route", func(t *testing.T) {
		router := newInferenceRouter()
		route := &InferenceRoute{Backend: "vllm", Endpoint: "http://vllm:8000", Model: "qwen-7b"}
		router.Set("agent-1", route)

		ok := router.UpdateMaxContextLen("agent-1", "http://vllm:8000", "qwen-7b", 32768)
		if !ok {
			t.Fatal("UpdateMaxContextLen returned false for matching route")
		}

		got := router.Get("agent-1")
		if got.MaxContextLen != 32768 {
			t.Errorf("MaxContextLen = %d, want 32768", got.MaxContextLen)
		}
	})

	t.Run("copy-on-write does not mutate original", func(t *testing.T) {
		router := newInferenceRouter()
		route := &InferenceRoute{Backend: "vllm", Endpoint: "http://vllm:8000", Model: "qwen-7b"}
		router.Set("agent-1", route)

		original := router.Get("agent-1")

		router.UpdateMaxContextLen("agent-1", "http://vllm:8000", "qwen-7b", 16384)

		if original.MaxContextLen != 0 {
			t.Errorf("original route was mutated: MaxContextLen = %d, want 0", original.MaxContextLen)
		}

		updated := router.Get("agent-1")
		if updated.MaxContextLen != 16384 {
			t.Errorf("updated route MaxContextLen = %d, want 16384", updated.MaxContextLen)
		}
	})

	t.Run("preserves other fields on update", func(t *testing.T) {
		router := newInferenceRouter()
		route := &InferenceRoute{
			Backend:  "litellm",
			Endpoint: "http://gw:4000",
			Model:    "gpt-4o",
			APIKey:   "sk-key",
			Preamble: "custom",
		}
		router.Set("agent-2", route)

		router.UpdateMaxContextLen("agent-2", "http://gw:4000", "gpt-4o", 128000)

		got := router.Get("agent-2")
		if got.Backend != "litellm" {
			t.Errorf("Backend = %q, want litellm", got.Backend)
		}
		if got.APIKey != "sk-key" {
			t.Errorf("APIKey = %q, want sk-key", got.APIKey)
		}
		if got.Preamble != "custom" {
			t.Errorf("Preamble = %q, want custom", got.Preamble)
		}
		if got.MaxContextLen != 128000 {
			t.Errorf("MaxContextLen = %d, want 128000", got.MaxContextLen)
		}
	})

	t.Run("rejects unknown agent", func(t *testing.T) {
		router := newInferenceRouter()
		ok := router.UpdateMaxContextLen("unknown", "http://x:8000", "m", 1024)
		if ok {
			t.Error("should return false for unknown agent")
		}
	})

	t.Run("rejects stale endpoint", func(t *testing.T) {
		router := newInferenceRouter()
		route := &InferenceRoute{Backend: "vllm", Endpoint: "http://new:8000", Model: "qwen-7b"}
		router.Set("agent-1", route)

		ok := router.UpdateMaxContextLen("agent-1", "http://old:8000", "qwen-7b", 4096)
		if ok {
			t.Error("should reject stale endpoint")
		}
		if router.Get("agent-1").MaxContextLen != 0 {
			t.Error("MaxContextLen should not have changed")
		}
	})

	t.Run("rejects stale model", func(t *testing.T) {
		router := newInferenceRouter()
		route := &InferenceRoute{Backend: "vllm", Endpoint: "http://vllm:8000", Model: "new-model"}
		router.Set("agent-1", route)

		ok := router.UpdateMaxContextLen("agent-1", "http://vllm:8000", "old-model", 4096)
		if ok {
			t.Error("should reject stale model")
		}
	})
}

// ---------------------------------------------------------------------------
// applyInferenceAuth — header injection and security guards
// ---------------------------------------------------------------------------

func TestApplyInferenceAuth(t *testing.T) {
	t.Run("no api key sets no auth header", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://localhost:8000/v1/chat/completions", nil)
		route := &InferenceRoute{Backend: "vllm"}

		applyInferenceAuth(req, route)

		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
	})

	t.Run("api key sets bearer token", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://localhost:8000/v1/chat/completions", nil)
		route := &InferenceRoute{Backend: "litellm", APIKey: "sk-test-key"}

		applyInferenceAuth(req, route)

		want := "Bearer sk-test-key"
		if got := req.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
	})

	t.Run("extra headers are set", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://localhost:8000/v1/chat/completions", nil)
		route := &InferenceRoute{
			Backend: "litellm",
			APIKey:  "sk-key",
			ExtraHeaders: map[string]string{
				"X-IBM-Project-ID": "proj-123",
				"X-Custom":         "value",
			},
		}

		applyInferenceAuth(req, route)

		if got := req.Header.Get("X-IBM-Project-ID"); got != "proj-123" {
			t.Errorf("X-IBM-Project-ID = %q, want proj-123", got)
		}
		if got := req.Header.Get("X-Custom"); got != "value" {
			t.Errorf("X-Custom = %q, want value", got)
		}
	})

	t.Run("extra header cannot override Authorization", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://localhost:8000/v1/chat/completions", nil)
		route := &InferenceRoute{
			Backend: "litellm",
			APIKey:  "sk-real",
			ExtraHeaders: map[string]string{
				"Authorization": "Bearer sk-evil",
			},
		}

		applyInferenceAuth(req, route)

		if got := req.Header.Get("Authorization"); got != "Bearer sk-real" {
			t.Errorf("Authorization = %q, want Bearer sk-real (ExtraHeaders must not override)", got)
		}
	})

	t.Run("authorization case-insensitive guard", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://localhost:8000/v1/chat/completions", nil)
		route := &InferenceRoute{
			Backend: "litellm",
			APIKey:  "sk-real",
			ExtraHeaders: map[string]string{
				"authorization": "Bearer sk-sneaky",
			},
		}

		applyInferenceAuth(req, route)

		if got := req.Header.Get("Authorization"); got != "Bearer sk-real" {
			t.Errorf("Authorization = %q, want Bearer sk-real (case-insensitive guard)", got)
		}
	})

	t.Run("empty key and empty value in extra headers are skipped", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://localhost:8000/v1/chat/completions", nil)
		route := &InferenceRoute{
			Backend: "litellm",
			ExtraHeaders: map[string]string{
				"":            "should-skip",
				"X-Empty-Val": "",
			},
		}

		applyInferenceAuth(req, route)

		if got := req.Header.Get("X-Empty-Val"); got != "" {
			t.Errorf("X-Empty-Val should not be set, got %q", got)
		}
	})

	t.Run("nil extra headers does not panic", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://localhost:8000/v1/chat/completions", nil)
		route := &InferenceRoute{Backend: "vllm", APIKey: "sk-ok", ExtraHeaders: nil}

		applyInferenceAuth(req, route)

		if got := req.Header.Get("Authorization"); got != "Bearer sk-ok" {
			t.Errorf("Authorization = %q, want Bearer sk-ok", got)
		}
	})
}

// ---------------------------------------------------------------------------
// DefaultInferenceTools — structural validation
// ---------------------------------------------------------------------------

func TestDefaultInferenceTools(t *testing.T) {
	if len(DefaultInferenceTools) == 0 {
		t.Fatal("DefaultInferenceTools should not be empty")
	}

	names := make(map[string]bool)
	for _, tool := range DefaultInferenceTools {
		if tool.Type != "function" {
			t.Errorf("tool %q type = %q, want function", tool.Function.Name, tool.Type)
		}
		if tool.Function.Name == "" {
			t.Error("tool has empty name")
		}
		if tool.Function.Description == "" {
			t.Errorf("tool %q has empty description", tool.Function.Name)
		}
		var params map[string]interface{}
		if err := json.Unmarshal(tool.Function.Parameters, &params); err != nil {
			t.Errorf("tool %q has invalid parameters JSON: %v", tool.Function.Name, err)
		}
		if names[tool.Function.Name] {
			t.Errorf("duplicate tool name: %q", tool.Function.Name)
		}
		names[tool.Function.Name] = true
	}

	for _, required := range []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep"} {
		if !names[required] {
			t.Errorf("missing required tool: %q", required)
		}
	}
}

