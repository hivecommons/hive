package proxy

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// cacheSink records gateway usage with the cached split, like
// *tokens.InferenceSink.
type cacheSink struct {
	memUsageSink
	in, out, cacheRead int64
	calls              int
}

func (c *cacheSink) RecordWithCache(agent, model string, in, out, cacheRead, cacheCreation int64) {
	c.calls++
	c.in, c.out, c.cacheRead = in, out, cacheRead
}

func TestExtractOpenAIUsageDetailedReadsCachedTokens(t *testing.T) {
	body := []byte(`{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800}}}`)
	u := extractOpenAIUsageDetailed(body)
	if u.Input != 1000 || u.Output != 50 || u.Cached != 800 {
		t.Fatalf("usage = %+v", u)
	}
	if in, out := extractOpenAIUsage(body); in != 1000 || out != 50 {
		t.Fatalf("legacy extract = %d %d", in, out)
	}
	if u := extractOpenAIUsageDetailed([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":1}}`)); u.Cached != 0 {
		t.Fatalf("no details must mean no cached tokens: %+v", u)
	}
}

func TestSSEUsageReportsCachedTokens(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":1000}}}`,
		`data: [DONE]`,
	}, "\n") + "\n"
	var out bytes.Buffer
	u, err := translateOpenAISSEToAnthropicUsage(strings.NewReader(stream), &out, "deepseek/deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	if u.Input != 1200 || u.Output != 30 || u.Cached != 1000 {
		t.Fatalf("usage = %+v", u)
	}
}

// OpenAI-shaped usage counts cached tokens INSIDE prompt_tokens: the sink gets
// the uncached remainder as input and the cached part as cache reads.
func TestRecordGatewayUsageSplitsCachedPrompt(t *testing.T) {
	p := &GitHubProxy{logger: slog.Default()}
	sink := &cacheSink{}
	p.SetTokenSink(sink)
	p.recordGatewayUsage("a", "m", gatewayUsage{Input: 1000, Output: 50, Cached: 800})
	if sink.calls != 1 || sink.in != 200 || sink.out != 50 || sink.cacheRead != 800 {
		t.Fatalf("split = %+v", sink)
	}
	// Cached larger than the prompt is clamped, never a negative input.
	p.recordGatewayUsage("a", "m", gatewayUsage{Input: 10, Output: 1, Cached: 99})
	if sink.in != 0 || sink.cacheRead != 10 {
		t.Fatalf("clamp = %+v", sink)
	}
	// No cached tokens: the plain Record path, as before.
	p.recordGatewayUsage("b", "m", gatewayUsage{Input: 7, Output: 3})
	if recs := sink.forAgent("b"); len(recs) != 1 {
		t.Fatalf("plain record = %+v", recs)
	}
	// Nothing to record, and a nil sink, are no-ops.
	p.recordGatewayUsage("c", "m", gatewayUsage{})
	(&GitHubProxy{logger: slog.Default()}).recordGatewayUsage("c", "m", gatewayUsage{Input: 1, Cached: 1})
}
