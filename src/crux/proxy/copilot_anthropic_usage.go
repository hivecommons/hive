package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// copilotAnthropicMessagesPathSuffix is the Anthropic Messages endpoint the
// Copilot CLI (>= 1.0.88) uses for Claude models instead of the
// OpenAI-shaped /chat/completions.
const copilotAnthropicMessagesPathSuffix = "/v1/messages"

// copilotAnthropicMaxSSELine bounds one buffered SSE line while teeing an
// Anthropic stream. Longer lines (huge content deltas) are skipped; the usage
// events are small and never approach it.
const copilotAnthropicMaxSSELine = 1024 * 1024

// isCopilotAnthropicMessagesPath reports whether a request path targets the
// Anthropic Messages endpoint. /v1/messages/count_tokens and other sub-paths
// do not match.
func isCopilotAnthropicMessagesPath(path string) bool {
	return strings.HasSuffix(path, copilotAnthropicMessagesPathSuffix)
}

// copilotAnthropicUsage is the token usage of one Anthropic Messages response.
// Input excludes cached prompt tokens, which are reported separately.
type copilotAnthropicUsage struct {
	Model         string
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
}

func (u copilotAnthropicUsage) total() int64 {
	return u.Input + u.Output + u.CacheRead + u.CacheCreation
}

type anthropicUsageBlock struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

// merge overlays the counts present in b. Anthropic reports cumulative values,
// so a later block (message_delta) supersedes an earlier one (message_start)
// for every field it carries.
func (u *copilotAnthropicUsage) merge(b *anthropicUsageBlock) {
	if b == nil {
		return
	}
	if b.InputTokens != nil {
		u.Input = *b.InputTokens
	}
	if b.OutputTokens != nil {
		u.Output = *b.OutputTokens
	}
	if b.CacheReadInputTokens != nil {
		u.CacheRead = *b.CacheReadInputTokens
	}
	if b.CacheCreationInputTokens != nil {
		u.CacheCreation = *b.CacheCreationInputTokens
	}
}

// extractAnthropicMessageUsage reads usage and model from a non-streaming
// Anthropic Messages JSON response.
func extractAnthropicMessageUsage(body []byte) copilotAnthropicUsage {
	var msg struct {
		Model string               `json:"model"`
		Usage *anthropicUsageBlock `json:"usage"`
	}
	var u copilotAnthropicUsage
	if json.Unmarshal(body, &msg) != nil {
		return u
	}
	u.Model = msg.Model
	u.merge(msg.Usage)
	return u
}

// anthropicSSEUsageParser incrementally consumes an Anthropic Messages SSE
// stream and tracks usage: message_start carries the model and the initial
// usage (input/cache counts), message_delta carries the cumulative final
// counts (output, and on newer APIs the input/cache counts too).
type anthropicSSEUsageParser struct {
	usage   copilotAnthropicUsage
	line    []byte
	skipped bool // current line overflowed copilotAnthropicMaxSSELine
}

func (p *anthropicSSEUsageParser) write(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.appendLine(b)
			return
		}
		p.appendLine(b[:i])
		if !p.skipped {
			p.handleLine(p.line)
		}
		p.line = p.line[:0]
		p.skipped = false
		b = b[i+1:]
	}
}

func (p *anthropicSSEUsageParser) appendLine(b []byte) {
	if p.skipped {
		return
	}
	if len(p.line)+len(b) > copilotAnthropicMaxSSELine {
		p.skipped = true
		p.line = p.line[:0]
		return
	}
	p.line = append(p.line, b...)
}

// finish handles a trailing line that was not newline-terminated.
func (p *anthropicSSEUsageParser) finish() copilotAnthropicUsage {
	if len(p.line) > 0 && !p.skipped {
		p.handleLine(p.line)
		p.line = p.line[:0]
	}
	return p.usage
}

func (p *anthropicSSEUsageParser) handleLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	// Cheap pre-filter: content deltas are by far the most frequent events and
	// never carry usage.
	if !bytes.Contains(data, []byte(`"message_start"`)) && !bytes.Contains(data, []byte(`"message_delta"`)) {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message *struct {
			Model string               `json:"model"`
			Usage *anthropicUsageBlock `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsageBlock `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			if ev.Message.Model != "" {
				p.usage.Model = ev.Message.Model
			}
			p.usage.merge(ev.Message.Usage)
		}
	case "message_delta":
		p.usage.merge(ev.Usage)
	}
}

// extractAnthropicSSEUsage parses a fully buffered Anthropic SSE stream.
func extractAnthropicSSEUsage(body []byte) copilotAnthropicUsage {
	var p anthropicSSEUsageParser
	p.write(body)
	return p.finish()
}

// anthropicUsageTee wraps an Anthropic Messages response body and observes the
// bytes as they are relayed to the client, so streamed responses are forwarded
// with no added latency while usage is still captured. Non-streaming JSON is
// small and is copied (bounded by copilotSniffBodyLimit) for a final parse.
type anthropicUsageTee struct {
	body      io.ReadCloser
	streaming bool
	sse       anthropicSSEUsageParser
	buf       []byte
	overflow  bool
}

func newAnthropicUsageTee(body io.ReadCloser, contentType string) *anthropicUsageTee {
	return &anthropicUsageTee{body: body, streaming: strings.Contains(contentType, "text/event-stream")}
}

func (t *anthropicUsageTee) Read(b []byte) (int, error) {
	n, err := t.body.Read(b)
	if n > 0 {
		if t.streaming {
			t.sse.write(b[:n])
		} else if !t.overflow {
			if len(t.buf)+n > copilotSniffBodyLimit {
				t.overflow = true
				t.buf = nil
			} else {
				t.buf = append(t.buf, b[:n]...)
			}
		}
	}
	return n, err
}

func (t *anthropicUsageTee) Close() error { return t.body.Close() }

// usage returns the usage observed so far; call after the body is consumed.
func (t *anthropicUsageTee) usage() copilotAnthropicUsage {
	if t.streaming {
		return t.sse.finish()
	}
	if t.overflow {
		return copilotAnthropicUsage{}
	}
	return extractAnthropicMessageUsage(t.buf)
}
