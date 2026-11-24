package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	inferenceRelayTestHost = "hive-inference.ns.svc"
	inferenceRelayGWKey    = "gw-UPSTREAM-SECRET"
)

type inferenceRelayHarness struct {
	relay  *SandboxInferenceRelay
	client *http.Client
	logs   *bytes.Buffer

	mu       sync.Mutex
	upstream []string // Authorization headers the gateway saw
}

func newInferenceRelayHarness(t *testing.T) *inferenceRelayHarness {
	t.Helper()
	h := &inferenceRelayHarness{logs: &bytes.Buffer{}}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.upstream = append(h.upstream, r.Header.Get("Authorization"))
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, keyResolverOKResponse)
	}))
	t.Cleanup(gw.Close)

	p := newKeyResolverTestProxy(h.logs)
	p.inference.Set("scanner", &InferenceRoute{Backend: "vllm", Endpoint: gw.URL, Model: "m", APIKey: inferenceRelayGWKey})
	relay, err := NewSandboxInferenceRelay(p, SandboxInferenceRelayOptions{Endpoint: "https://" + inferenceRelayTestHost + ":18446"})
	if err != nil {
		t.Fatal(err)
	}
	h.relay = relay
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// Served over TLS only, exactly as boot does it.
	go func() { _ = relay.Serve(tls.NewListener(ln, relay.TLSConfig())) }()

	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)
	addr := ln.Addr().String()
	h.client = &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: inferenceRelayTestHost, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	return h
}

func (h *inferenceRelayHarness) post(t *testing.T, baseURL, key string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(keyResolverTestBody))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A Job's kick key reaches the translator for its own agent, the gateway sees
// hive's upstream key and never the kick key, and release revokes the key.
func TestSandboxInferenceRelayServesKickKeyAndRevokes(t *testing.T) {
	h := newInferenceRelayHarness(t)
	baseURL, key, bundle, release, err := h.relay.Enroll("scanner")
	if err != nil {
		t.Fatal(err)
	}
	if baseURL != "https://"+inferenceRelayTestHost+":18446" || !strings.HasPrefix(key, sandboxInferenceKeyPrefix) {
		t.Fatalf("baseURL=%q key prefix wrong", baseURL)
	}
	if !bytes.Contains(bundle, []byte("BEGIN CERTIFICATE")) {
		t.Fatal("Enroll must hand the Job the hive CA")
	}
	if code := h.post(t, baseURL, key); code != http.StatusOK {
		t.Fatalf("live kick key: status %d, want 200", code)
	}
	h.mu.Lock()
	seen := append([]string(nil), h.upstream...)
	h.mu.Unlock()
	if len(seen) != 1 || seen[0] != "Bearer "+inferenceRelayGWKey {
		t.Fatalf("gateway saw %v, want hive's upstream key only", seen)
	}
	release()
	release() // idempotent
	if code := h.post(t, baseURL, key); code != http.StatusUnauthorized {
		t.Fatalf("released kick key: status %d, want 401", code)
	}
	if strings.Contains(h.logs.String(), key) || strings.Contains(h.logs.String(), key[len(sandboxInferenceKeyPrefix):]) {
		t.Fatal("the kick key was logged")
	}
}

// The loopback identity scheme does not apply here: an "sk-hive-<agent>" name
// or a missing key is refused even with HIVE_PROXY_ADVISORY_OK set.
func TestSandboxInferenceRelayRefusesNonKickKeys(t *testing.T) {
	h := newInferenceRelayHarness(t)
	if !h.relay.p.proxyAdvisoryOK {
		t.Fatal("harness must run with advisory-ok to prove it is not honoured")
	}
	base := "https://" + inferenceRelayTestHost + ":18446"
	for _, key := range []string{"", inferenceAPIKeyPrefix + "scanner", sandboxInferenceKeyPrefix + "guess"} {
		if code := h.post(t, base, key); code != http.StatusUnauthorized {
			t.Errorf("key %q: status %d, want 401", key, code)
		}
	}
}

func TestSandboxInferenceRelayKeysArePerKick(t *testing.T) {
	h := newInferenceRelayHarness(t)
	_, k1, _, r1, _ := h.relay.Enroll("scanner")
	_, k2, _, r2, _ := h.relay.Enroll("scanner")
	defer r2()
	if k1 == k2 {
		t.Fatal("two kicks got the same key")
	}
	r1()
	if _, ok := h.relay.identify(httptest.NewRequest(http.MethodPost, "/v1/messages", nil)); ok {
		t.Fatal("a request with no key was identified")
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+k2)
	if name, ok := h.relay.identify(req); !ok || name != "scanner" {
		t.Fatalf("second kick's key: (%q,%v)", name, ok)
	}
	req.Header.Set("Authorization", "Bearer "+k1)
	if _, ok := h.relay.identify(req); ok {
		t.Fatal("revoking one kick's key must not leave it live")
	}
}

func TestNewSandboxInferenceRelayRejectsBadEndpoint(t *testing.T) {
	p := newInferenceTestProxy()
	for _, ep := range []string{"", "http://h:1", "https://u:p@h:1", "h:1"} {
		if _, err := NewSandboxInferenceRelay(p, SandboxInferenceRelayOptions{Endpoint: ep}); err == nil {
			t.Errorf("endpoint %q accepted", ep)
		}
	}
	if _, err := NewSandboxInferenceRelay(nil, SandboxInferenceRelayOptions{Endpoint: "https://h:1"}); err == nil {
		t.Error("nil proxy accepted")
	}
}
