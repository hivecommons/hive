package meetllm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnthropicURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                               AnthropicEndpoint + "/v1/messages",
		"https://a.example/":             "https://a.example/v1/messages",
		"https://a.example/v1":           "https://a.example/v1/messages",
		"https://a.example/v1/messages/": "https://a.example/v1/messages",
	} {
		if got := anthropicURL(in); got != want {
			t.Errorf("anthropicURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompleteAnthropic(t *testing.T) {
	var got anthropicRequest
	var key, version, path string
	reply := `{"content":[{"type":"thinking","thinking":""},{"type":"text","text":"It ships "},{"type":"text","text":"Friday."}],"stop_reason":"end_turn","usage":{"input_tokens":30,"output_tokens":12}}`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = anthropicRequest{}
		_ = json.NewDecoder(r.Body).Decode(&got)
		key, version, path = r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	usage := filepath.Join(t.TempDir(), "usage.jsonl")
	c := Config{Provider: ProviderAnthropic, Endpoint: srv.URL, Model: "claude-haiku-5-5", APIKey: "ak", Timeout: 5 * time.Second,
		MaxInFlight: 1, System: "be brief", UsageFile: usage, MaxTokens: 100}
	out, err := Complete(context.Background(), c, srv.Client(), "q")
	if err != nil || out != "It ships Friday." {
		t.Fatalf("complete %q, %v", out, err)
	}
	if path != "/v1/messages" || key != "ak" || version != anthropicVersion {
		t.Fatalf("request path %q key %q version %q", path, key, version)
	}
	if got.Model != "claude-haiku-5-5" || got.System != "be brief" || len(got.Messages) != 1 || got.Messages[0].Role != "user" ||
		got.Messages[0].Content != "q" || got.MaxTokens != maxReplyTokens || got.Thinking == nil || got.Thinking.Type != "disabled" {
		t.Fatalf("body %+v", got)
	}
	if total, _ := UsageTotal(usage); total != 42 {
		t.Fatalf("usage %d", total)
	}
	// Other models keep their default thinking.
	c.Model = "claude-opus-5-5"
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); err != nil || got.Thinking != nil {
		t.Fatalf("thinking %+v, %v", got.Thinking, err)
	}
	// The budget is honoured: 84 tokens used of 100, one more call takes it over.
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); !errors.Is(err, ErrBudget) {
		t.Fatalf("budget %v", err)
	}
	c.MaxTokens = 0
	// Refusals, empty text, bad JSON and HTTP errors are errors.
	reply = `{"content":[],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":0}}`
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("refusal %v", err)
	}
	reply = `{"content":[{"type":"text","text":"  "}],"stop_reason":"end_turn"}`
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); err == nil {
		t.Fatal("empty text")
	}
	reply = `nope`
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); err == nil {
		t.Fatal("bad json")
	}
	status = http.StatusTooManyRequests
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("status %v", err)
	}
	if _, err := Complete(context.Background(), Config{Provider: ProviderAnthropic, Model: "m", Timeout: time.Second, MaxInFlight: 1}, srv.Client(), "q"); err == nil {
		t.Fatal("missing key")
	}
	srv.Close()
	if _, err := Complete(context.Background(), Config{Provider: ProviderAnthropic, Endpoint: srv.URL, Model: "m", APIKey: "k", Timeout: time.Second, MaxInFlight: 1}, http.DefaultClient, "q"); err == nil {
		t.Fatal("unreachable")
	}
	if _, err := Complete(context.Background(), Config{Provider: ProviderAnthropic, Endpoint: "http://[::1", Model: "m", APIKey: "k", Timeout: time.Second, MaxInFlight: 1}, http.DefaultClient, "q"); err == nil {
		t.Fatal("bad url")
	}
}

func TestCompleteAnthropicUsageWriteFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	// A directory where the usage file should be makes the append fail.
	usage := filepath.Join(dir, "usage")
	if err := os.Mkdir(usage, 0o700); err != nil {
		t.Fatal(err)
	}
	c := Config{Provider: ProviderAnthropic, Endpoint: srv.URL, Model: "m", APIKey: "k", Timeout: time.Second, MaxInFlight: 1, UsageFile: usage}
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("usage error %v", err)
	}
}
