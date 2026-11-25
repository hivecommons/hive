package proxy

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestTranslateSSEForwardsReasoningAsThinking(t *testing.T) {
	stream := `data: {"choices":[{"delta":{"reasoning_content":"hmm"}}]}

data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	var out strings.Builder
	if _, err := translateOpenAISSEToAnthropicUsage(strings.NewReader(stream), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{`"type":"thinking"`, `"type":"thinking_delta","thinking":"hmm"`, `"text":"hi"`} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %s:\n%s", want, got)
		}
	}
	if strings.Index(got, "thinking_delta") > strings.Index(got, "text_delta") {
		t.Errorf("thinking must precede text:\n%s", got)
	}
}

func TestTranslateSSEPingsWhileUpstreamSilent(t *testing.T) {
	old := sseKeepAliveInterval
	sseKeepAliveInterval = 10 * time.Millisecond
	defer func() { sseKeepAliveInterval = old }()

	pr, pw := io.Pipe()
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
		_ = pw.Close()
	}()
	var out strings.Builder
	if _, err := translateOpenAISSEToAnthropicUsage(pr, &out, "m"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "event: ping") {
		t.Errorf("expected ping event while upstream silent:\n%s", out.String())
	}
}
