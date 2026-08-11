package proxy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUpstreamDialTimeoutConstant(t *testing.T) {
	if upstreamDialTimeout != 15*time.Second {
		t.Fatalf("upstreamDialTimeout = %v, want 15s", upstreamDialTimeout)
	}
	if got := upstreamDialer().Timeout; got != upstreamDialTimeout {
		t.Fatalf("upstreamDialer().Timeout = %v, want %v", got, upstreamDialTimeout)
	}
}

func TestAllTLSUpstreamDialsAreBound(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "github_proxy.go"))
	if err != nil {
		t.Fatalf("read github_proxy.go: %v", err)
	}
	text := string(body)
	if strings.Contains(text, `tls.Dial("tcp"`) {
		t.Fatal("TLS upstream dials must not use unbounded tls.Dial")
	}
	const wantBoundTLSUpstreamDials = 3
	if got := strings.Count(text, "tls.DialWithDialer(upstreamDialer()"); got != wantBoundTLSUpstreamDials {
		t.Fatalf("bounded TLS upstream dials = %d, want %d", got, wantBoundTLSUpstreamDials)
	}
	for _, want := range []string{
		`p.logger.Error("transparent proxy upstream dial failed"`,
		`p.logger.Error("proxy upstream dial failed"`,
		`p.logger.Error("copilot sniff: upstream dial failed"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("timeout/dial failures must be observable; missing log site %s", want)
		}
	}
}

func TestAllClientTLSHandshakesAreBound(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "github_proxy.go"))
	if err != nil {
		t.Fatalf("read github_proxy.go: %v", err)
	}
	text := string(body)
	if clientTLSHandshakeTimeout != upstreamDialTimeout {
		t.Fatalf("clientTLSHandshakeTimeout = %v, want upstreamDialTimeout %v", clientTLSHandshakeTimeout, upstreamDialTimeout)
	}
	const wantClientTLSHandshakes = 4
	if got := strings.Count(text, ".Handshake()"); got != wantClientTLSHandshakes {
		t.Fatalf("client TLS handshakes = %d, want %d", got, wantClientTLSHandshakes)
	}
	if got := strings.Count(text, "SetDeadline(time.Now().Add(clientTLSHandshakeTimeout))"); got != wantClientTLSHandshakes {
		t.Fatalf("bounded client TLS handshakes = %d, want %d", got, wantClientTLSHandshakes)
	}
	for _, want := range []string{
		`p.logger.Warn("transparent proxy TLS handshake failed"`,
		`p.logger.Warn("proxy client TLS handshake failed"`,
		`p.logger.Warn("inference reroute: TLS handshake failed"`,
		`p.logger.Warn("copilot sniff: client TLS handshake failed"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("client TLS handshake failures must be observable; missing log site %s", want)
		}
	}
}

func TestHTTPRelayDeadlinesAreBoundAndObservable(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "github_proxy.go"))
	if err != nil {
		t.Fatalf("read github_proxy.go: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "upstream.SetWriteDeadline(time.Now().Add(httpWriteTimeout))") {
		t.Fatal("request relays must bound upstream writes with httpWriteTimeout")
	}
	if !strings.Contains(text, "client.SetWriteDeadline(time.Now().Add(httpWriteTimeout))") {
		t.Fatal("response relays must bound client writes with httpWriteTimeout")
	}
	if !strings.Contains(text, "upstream.SetReadDeadline(time.Now().Add(httpReadTimeout))") {
		t.Fatal("response-header reads must be bounded with httpReadTimeout")
	}
	if !strings.Contains(text, "client.SetReadDeadline(time.Now().Add(httpReadTimeout))") {
		t.Fatal("client request reads must be bounded with httpReadTimeout")
	}
	for _, want := range []string{
		`p.logTimeout("proxy request relay timed out"`,
		`p.logTimeout("proxy upstream response read timed out"`,
		`p.logTimeout("proxy response relay timed out"`,
		`p.logTimeout("copilot sniff: request relay timed out"`,
		`p.logTimeout("copilot sniff: upstream response read timed out"`,
		`p.logTimeout("copilot sniff: response relay timed out"`,
		`p.logTimeout("inference reroute: client request read timed out"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("deadline failures must be observable; missing log site %s", want)
		}
	}
}
