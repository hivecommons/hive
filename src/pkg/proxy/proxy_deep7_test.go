package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// ---------- handleInferenceRequest: response translation fails ----------

func TestHandleInferenceRequestTranslationFail(t *testing.T) {
	p := newTestProxy()

	// Mock backend returns invalid JSON
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not valid json response"))
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test"}
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Body = io.NopCloser(strings.NewReader(body))

	go func() {
		p.handleInferenceRequest(proxyConn, req, "scanner", route)
		proxyConn.Close()
	}()

	var collected []byte
	buf := make([]byte, 4096)
	for {
		clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, err := clientConn.Read(buf)
		if n > 0 {
			collected = append(collected, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	response := string(collected)
	if !strings.Contains(response, "502") {
		t.Errorf("translation fail should return 502, got: %s", response)
	}
}

// ---------- StartInferenceTranslator: read body error ----------

func TestStartInferenceTranslatorReadBodyError(t *testing.T) {
	caCert, caX509, _ := generateCA()
	p := &GitHubProxy{
		caCert:     caCert,
		caX509:     caX509,
		logger:     slog.Default(),
		violations: make(map[string]int),
		certCache:  make(map[string]cachedCert),
		inference:  newInferenceRouter(),
	}
	p.inference.Set("agent1", &InferenceRoute{Backend: "vllm", Endpoint: "http://localhost:1", Model: "test"})

	translator := httptest.NewServer(p.inferenceTranslatorHandler())
	defer translator.Close()

	conn, err := net.DialTimeout("tcp", translator.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("connect to translator: %v", err)
	}
	_, _ = conn.Write([]byte("POST /v1/messages HTTP/1.1\r\nHost: localhost\r\nx-api-key: sk-hive-agent1\r\nContent-Length: 999999\r\n\r\npartial"))
	_ = conn.Close()
}

// ---------- handleInferenceRequest: streaming SSE error ----------

func TestHandleInferenceRequestSSEError(t *testing.T) {
	p := newTestProxy()

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		data, _ := json.Marshal(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"delta": map[string]string{"content": "partial"}, "finish_reason": nil},
			},
		})
		w.Write([]byte("data: "))
		w.Write(data)
		w.Write([]byte("\n\n"))
		f.Flush()
	}))
	defer mock.Close()

	route := &InferenceRoute{Backend: "vllm", Endpoint: mock.URL, Model: "test"}
	body := `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`

	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	req.Body = io.NopCloser(strings.NewReader(body))

	go func() {
		p.handleInferenceRequest(proxyConn, req, "scanner", route)
		proxyConn.Close()
	}()

	var result []byte
	buf := make([]byte, 8192)
	for {
		clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, err := clientConn.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	output := string(result)
	if !strings.Contains(output, "text/event-stream") {
		t.Logf("output: %q", output)
	}
}

// ---------- handleConnectDirect: CONNECT response write fail ----------

func TestHandleConnectDirectWriteConnectFail(t *testing.T) {
	p := newTestProxy()
	p.inference = newInferenceRouter()

	host := "github-test-write-fail.example.com"
	RegisterGitHubHost(host)
	defer delete(githubHosts, host)

	clientConn, proxyClient := net.Pipe()

	req, _ := http.NewRequest("CONNECT", "http://"+host+":443", nil)
	req.Host = host + ":443"

	clientConn.Close()
	p.handleConnectDirect(proxyClient, req)
	proxyClient.Close()
}

// ---------- proxyHTTP: git path upstream write error ----------

func TestProxyHTTPGitPathUpstreamWriteError(t *testing.T) {
	p := newTestProxy()

	clientConn, proxyClient := net.Pipe()
	upstreamConn, proxyUpstream := net.Pipe()

	upstreamConn.Close()

	go p.proxyHTTP(proxyClient, proxyUpstream, "scanner", agent.ModeIssuesAndPRs, agent.AgentCapabilities{})

	fmt.Fprintf(clientConn, "POST /org/repo.git/info/refs HTTP/1.1\r\nHost: github.com\r\nContent-Length: 0\r\n\r\n")
	time.Sleep(200 * time.Millisecond)
	clientConn.Close()
}
