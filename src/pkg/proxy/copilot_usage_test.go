package proxy

import (
	"encoding/json"
	"testing"
)

func TestIsCopilotAPIHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"api.githubcopilot.com", true},
		{"api.enterprise.githubcopilot.com", true},
		{"proxy.enterprise.githubcopilot.com", true},
		{"githubcopilot.com", true},
		{"api.github.com", false},
		{"github.com", false},
		{"api.anthropic.com", false},
		{"evilgithubcopilot.com.attacker.example", false},
		{"", false},
		// Not a subdomain (no leading dot before githubcopilot.com) and not the
		// bare apex, so it must NOT match.
		{"notgithubcopilot.com", false},
	}
	for _, c := range cases {
		if got := IsCopilotAPIHost(c.host); got != c.want {
			t.Errorf("IsCopilotAPIHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestIsCopilotCompletionsPath(t *testing.T) {
	cases := map[string]bool{
		"/chat/completions":        true,
		"/v1/chat/completions":     true,
		"/models":                  false,
		"/chat/completions/stream": false,
		"":                         false,
		"/completions":             false,
	}
	for path, want := range cases {
		if got := isCopilotCompletionsPath(path); got != want {
			t.Errorf("isCopilotCompletionsPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParseCopilotRequest(t *testing.T) {
	model, streaming := parseCopilotRequest([]byte(`{"model":"claude-sonnet-4.5","stream":true}`))
	if model != "claude-sonnet-4.5" || !streaming {
		t.Errorf("got (%q,%v), want (claude-sonnet-4.5,true)", model, streaming)
	}

	model, streaming = parseCopilotRequest([]byte(`{"model":"gpt-5"}`))
	if model != "gpt-5" || streaming {
		t.Errorf("got (%q,%v), want (gpt-5,false)", model, streaming)
	}

	model, streaming = parseCopilotRequest([]byte(`not json`))
	if model != "" || streaming {
		t.Errorf("bad json should yield empty, got (%q,%v)", model, streaming)
	}
}

func TestEnsureStreamUsageRequested(t *testing.T) {
	// Streaming request without stream_options -> injected.
	in := []byte(`{"model":"gpt-5","stream":true,"messages":[]}`)
	out := ensureStreamUsageRequested(in)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output not valid json: %v", err)
	}
	so, ok := obj["stream_options"]
	if !ok {
		t.Fatal("stream_options not injected")
	}
	var opts openaiStreamOptions
	if json.Unmarshal(so, &opts) != nil || !opts.IncludeUsage {
		t.Errorf("include_usage not true, got %s", so)
	}
	// Other fields preserved.
	if _, ok := obj["messages"]; !ok {
		t.Error("messages field dropped")
	}
	if string(obj["model"]) != `"gpt-5"` {
		t.Errorf("model changed: %s", obj["model"])
	}
}

func TestEnsureStreamUsageRequested_NonStreaming(t *testing.T) {
	in := []byte(`{"model":"gpt-5","stream":false}`)
	out := ensureStreamUsageRequested(in)
	if string(out) != string(in) {
		t.Errorf("non-streaming request should be unchanged, got %s", out)
	}
}

func TestEnsureStreamUsageRequested_NoStreamField(t *testing.T) {
	in := []byte(`{"model":"gpt-5"}`)
	out := ensureStreamUsageRequested(in)
	if string(out) != string(in) {
		t.Errorf("request without stream field should be unchanged, got %s", out)
	}
}

func TestEnsureStreamUsageRequested_AlreadyRequested(t *testing.T) {
	in := []byte(`{"model":"gpt-5","stream":true,"stream_options":{"include_usage":true}}`)
	out := ensureStreamUsageRequested(in)
	if string(out) != string(in) {
		t.Errorf("already-requested body should be unchanged, got %s", out)
	}
}

func TestEnsureStreamUsageRequested_BadJSON(t *testing.T) {
	in := []byte(`not json`)
	out := ensureStreamUsageRequested(in)
	if string(out) != string(in) {
		t.Errorf("bad json should be returned unchanged")
	}
}

func TestEnsureStreamUsageRequested_StreamOptionsPresentNoUsage(t *testing.T) {
	// stream_options present but include_usage false -> gets overwritten to true.
	in := []byte(`{"model":"gpt-5","stream":true,"stream_options":{"include_usage":false}}`)
	out := ensureStreamUsageRequested(in)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("bad output json: %v", err)
	}
	var opts openaiStreamOptions
	if json.Unmarshal(obj["stream_options"], &opts) != nil || !opts.IncludeUsage {
		t.Errorf("include_usage should be forced true, got %s", obj["stream_options"])
	}
}

func TestExtractCopilotSSEUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":42}}\n" +
		"data: [DONE]\n"
	in, out := extractCopilotSSEUsage([]byte(sse))
	if in != 100 || out != 42 {
		t.Errorf("got (%d,%d), want (100,42)", in, out)
	}
}

func TestExtractCopilotSSEUsage_NoUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n" +
		"data: [DONE]\n"
	in, out := extractCopilotSSEUsage([]byte(sse))
	if in != 0 || out != 0 {
		t.Errorf("got (%d,%d), want (0,0)", in, out)
	}
}

func TestExtractCopilotUsage_NonStreaming(t *testing.T) {
	body := []byte(`{"model":"gpt-5","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	in, out := extractCopilotUsage("application/json", body)
	if in != 7 || out != 3 {
		t.Errorf("got (%d,%d), want (7,3)", in, out)
	}
}

func TestExtractCopilotUsage_Streaming(t *testing.T) {
	sse := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":5}}\n"
	in, out := extractCopilotUsage("text/event-stream; charset=utf-8", []byte(sse))
	if in != 9 || out != 5 {
		t.Errorf("got (%d,%d), want (9,5)", in, out)
	}
}

func TestExtractCopilotResponseModel_NonStreaming(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4.5","usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	if m := extractCopilotResponseModel(body, "application/json"); m != "claude-sonnet-4.5" {
		t.Errorf("got %q, want claude-sonnet-4.5", m)
	}
}

func TestExtractCopilotResponseModel_Streaming(t *testing.T) {
	sse := "data: {\"model\":\"gpt-5-mini\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n" +
		"data: [DONE]\n"
	if m := extractCopilotResponseModel([]byte(sse), "text/event-stream"); m != "gpt-5-mini" {
		t.Errorf("got %q, want gpt-5-mini", m)
	}
}

func TestExtractCopilotResponseModel_None(t *testing.T) {
	if m := extractCopilotResponseModel([]byte(`{}`), "application/json"); m != "" {
		t.Errorf("got %q, want empty", m)
	}
	if m := extractCopilotResponseModel([]byte(`bad`), "application/json"); m != "" {
		t.Errorf("bad json got %q, want empty", m)
	}
	if m := extractCopilotResponseModel([]byte("data: [DONE]\n"), "text/event-stream"); m != "" {
		t.Errorf("sse with no model got %q, want empty", m)
	}
}
