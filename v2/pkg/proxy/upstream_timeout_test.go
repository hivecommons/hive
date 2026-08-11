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
	if got := markDialer(upstreamDialTimeout).Timeout; got != upstreamDialTimeout {
		t.Fatalf("markDialer(upstreamDialTimeout).Timeout = %v, want %v", got, upstreamDialTimeout)
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
	if strings.Contains(text, "tls.DialWithDialer(markDialer(0)") {
		t.Fatal("TLS upstream dials must not use an unbounded zero-timeout dialer")
	}
	const wantBoundTLSUpstreamDials = 3
	if got := strings.Count(text, "tls.DialWithDialer(markDialer(upstreamDialTimeout)"); got != wantBoundTLSUpstreamDials {
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
