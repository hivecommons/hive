package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

// anthropicSSEFixture mirrors the event shapes the Copilot CLI receives from
// POST /v1/messages for Claude models.
const anthropicSSEFixture = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4.5","content":[],"stop_reason":null,"usage":{"input_tokens":12,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: ping\n" +
	`data: {"type":"ping"}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":57}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

const anthropicJSONFixture = `{"id":"msg_02","type":"message","role":"assistant","model":"claude-opus-4.5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":25,"output_tokens":9,"cache_read_input_tokens":1000,"cache_creation_input_tokens":50}}`

func TestIsCopilotAnthropicMessagesPath(t *testing.T) {
	cases := map[string]bool{
		"/v1/messages":              true,
		"/proxy/v1/messages":        true,
		"/v1/messages/count_tokens": false,
		"/messages":                 false,
		"/chat/completions":         false,
		"":                          false,
	}
	for path, want := range cases {
		if got := isCopilotAnthropicMessagesPath(path); got != want {
			t.Errorf("isCopilotAnthropicMessagesPath(%q) = %v, want %v", path, got, want)
		}
		wantCompletion := want || strings.HasSuffix(path, "/chat/completions")
		if got := isCopilotCompletionsPath(path); got != wantCompletion {
			t.Errorf("isCopilotCompletionsPath(%q) = %v, want %v", path, got, wantCompletion)
		}
	}
}

func TestExtractAnthropicMessageUsage(t *testing.T) {
	got := extractAnthropicMessageUsage([]byte(anthropicJSONFixture))
	want := copilotAnthropicUsage{Model: "claude-opus-4.5", Input: 25, Output: 9, CacheRead: 1000, CacheCreation: 50}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
	if got := extractAnthropicMessageUsage([]byte("not json")); got.total() != 0 {
		t.Fatalf("bad JSON usage = %+v, want zero", got)
	}
}

func TestExtractAnthropicSSEUsage(t *testing.T) {
	want := copilotAnthropicUsage{Model: "claude-sonnet-4.5", Input: 12, Output: 57, CacheRead: 4000, CacheCreation: 300}
	if got := extractAnthropicSSEUsage([]byte(anthropicSSEFixture)); got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
	crlf := strings.ReplaceAll(anthropicSSEFixture, "\n", "\r\n")
	if got := extractAnthropicSSEUsage([]byte(crlf)); got != want {
		t.Fatalf("CRLF usage = %+v, want %+v", got, want)
	}
}

func TestExtractAnthropicSSEUsage_DeltaCarriesInputAndCache(t *testing.T) {
	sse := `data: {"type":"message_start","message":{"model":"claude-sonnet-4.5","usage":{"input_tokens":0,"output_tokens":1}}}` + "\n" +
		`data: {"type":"message_delta","usage":{"input_tokens":40,"output_tokens":7,"cache_read_input_tokens":90}}`
	want := copilotAnthropicUsage{Model: "claude-sonnet-4.5", Input: 40, Output: 7, CacheRead: 90}
	if got := extractAnthropicSSEUsage([]byte(sse)); got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestAnthropicUsageTee_StreamsByteAtATime(t *testing.T) {
	tee := newAnthropicUsageTee(io.NopCloser(iotest.OneByteReader(strings.NewReader(anthropicSSEFixture))), "text/event-stream; charset=utf-8")
	relayed, err := io.ReadAll(tee)
	if err != nil {
		t.Fatal(err)
	}
	if string(relayed) != anthropicSSEFixture {
		t.Fatalf("tee altered the relayed stream")
	}
	if tee.buf != nil {
		t.Fatalf("streaming tee buffered the body (%d bytes)", len(tee.buf))
	}
	want := copilotAnthropicUsage{Model: "claude-sonnet-4.5", Input: 12, Output: 57, CacheRead: 4000, CacheCreation: 300}
	if got := tee.usage(); got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestAnthropicUsageTee_JSON(t *testing.T) {
	tee := newAnthropicUsageTee(io.NopCloser(strings.NewReader(anthropicJSONFixture)), "application/json")
	if _, err := io.Copy(io.Discard, tee); err != nil {
		t.Fatal(err)
	}
	if got := tee.usage(); got.Input != 25 || got.Output != 9 || got.CacheRead != 1000 || got.CacheCreation != 50 {
		t.Fatalf("usage = %+v", got)
	}
}

func TestAnthropicSSEUsageParser_SkipsOverlongLine(t *testing.T) {
	var p anthropicSSEUsageParser
	p.write([]byte("data: " + strings.Repeat("x", copilotAnthropicMaxSSELine+1) + "\n"))
	p.write([]byte(`data: {"type":"message_delta","usage":{"output_tokens":3}}` + "\n"))
	if got := p.finish(); got.Output != 3 {
		t.Fatalf("usage after overlong line = %+v, want output 3", got)
	}
}

// runAnthropicCopilotSniff drives proxyCopilotHTTP for one POST /v1/messages
// exchange and returns the bytes the client received plus the request body the
// upstream saw.
func runAnthropicCopilotSniff(t *testing.T, p *GitHubProxy, agentName, reqBody, contentType, respBody string) (clientGot, upstreamSaw []byte) {
	t.Helper()
	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamLn.Close()

	sawCh := make(chan []byte, 1)
	go func() {
		conn, err := upstreamLn.Accept()
		if err != nil {
			sawCh <- nil
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			sawCh <- nil
			return
		}
		body, _ := io.ReadAll(req.Body)
		req.Body.Close()
		sawCh <- body
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", contentType, len(respBody), respBody)
	}()

	upstreamConn, err := net.Dial("tcp", upstreamLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer upstreamConn.Close()

	clientLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientLn.Close()

	tlsCert, err := p.forgeCert("api.githubcopilot.com")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)

	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		raw, err := clientLn.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{tlsCert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		p.proxyCopilotHTTP(tlsConn, upstreamConn, "api.githubcopilot.com", agentName)
		tlsConn.Close()
	}()

	clientConn, err := tls.Dial("tcp", clientLn.Addr().String(), &tls.Config{ServerName: "api.githubcopilot.com", RootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	fmt.Fprintf(clientConn, "POST /v1/messages HTTP/1.1\r\nHost: api.githubcopilot.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(reqBody), reqBody)
	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	clientGot, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	<-proxyDone
	return clientGot, <-sawCh
}

func TestProxyCopilotHTTP_AnthropicStreaming(t *testing.T) {
	p, sink := newTestProxyWithCA(t)
	reqBody := `{"model":"claude-sonnet-4.5","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
	got, saw := runAnthropicCopilotSniff(t, p, "worker1", reqBody, "text/event-stream", anthropicSSEFixture)

	if string(got) != anthropicSSEFixture {
		t.Errorf("client did not receive the SSE stream verbatim:\n%s", got)
	}
	if string(saw) != reqBody {
		t.Errorf("Anthropic request body was modified: %s", saw)
	}
	if bytes.Contains(saw, []byte("stream_options")) {
		t.Errorf("OpenAI stream_options injected into Anthropic request: %s", saw)
	}

	recs := sink.forAgent("worker1")
	if len(recs) == 0 {
		t.Fatal("no usage recorded for worker1")
	}
	var in, out, cr, cc int64
	for _, r := range recs {
		if !r.copilot {
			t.Errorf("usage recorded through Record, want the Copilot path: %+v", r)
		}
		if r.model != "claude-sonnet-4.5" {
			t.Errorf("recorded model = %q, want claude-sonnet-4.5", r.model)
		}
		in += r.in
		out += r.out
		cr += r.cacheRead
		cc += r.cacheCreation
	}
	if in != 12 || out != 57 || cr != 4000 || cc != 300 {
		t.Errorf("recorded usage in=%d out=%d cache_read=%d cache_creation=%d, want 12/57/4000/300", in, out, cr, cc)
	}
}

func TestProxyCopilotHTTP_AnthropicJSONAutoModel(t *testing.T) {
	p, sink := newTestProxyWithCA(t)
	reqBody := `{"model":"auto","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	got, _ := runAnthropicCopilotSniff(t, p, "worker2", reqBody, "application/json", anthropicJSONFixture)
	if string(got) != anthropicJSONFixture {
		t.Errorf("client did not receive the JSON body verbatim: %s", got)
	}

	model, in, out := readInferenceUsage(t, sink, "worker2")
	if model != "claude-opus-4.5" || in != 25 || out != 9 {
		t.Errorf("recorded usage = (%q,%d,%d), want (claude-opus-4.5,25,9)", model, in, out)
	}
}
