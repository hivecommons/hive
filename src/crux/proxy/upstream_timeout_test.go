package proxy

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

// readGitHubProxySource returns the concatenated source of the non-test
// github_proxy*.go files, so source-level invariants hold across the files
// GitHubProxy is split into.
func readGitHubProxySource(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(file), "github_proxy*.go"))
	if err != nil {
		t.Fatalf("glob github_proxy*.go: %v", err)
	}
	var sb strings.Builder
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Base(path), err)
		}
		sb.Write(body)
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		t.Fatal("no github_proxy*.go source files found")
	}
	return sb.String()
}

func TestAllTLSUpstreamDialsAreBound(t *testing.T) {
	text := readGitHubProxySource(t)
	if got := strings.Count(text, "copilot sniff: client hung up before TLS handshake"); got != 1 {
		t.Fatalf("copilot sniff handshake hangup demotion sites = %d, want 1", got)
	}
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

func TestAllClientTLSHandshakesAreBound(t *testing.T) {
	text := readGitHubProxySource(t)
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
	if got := strings.Count(text, "isClientHangup(handshakeErr)"); got < 2 {
		t.Fatalf("transparent and direct handshakes must demote client hangups; isClientHangup sites = %d", got)
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
	text := readGitHubProxySource(t)
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
		`p.logClientRead("copilot sniff: client request read timed out"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("deadline failures must be observable; missing log site %s", want)
		}
	}
}

func TestIsClientHangup(t *testing.T) {
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, os.ErrDeadlineExceeded, net.ErrClosed} {
		if !isClientHangup(err) {
			t.Fatalf("isClientHangup(%v) = false, want true", err)
		}
	}
	if !isClientHangup(&net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}) {
		t.Fatal("net.Error timeouts must count as client hangups")
	}
	if isClientHangup(errors.New("tls: failed to verify certificate")) {
		t.Fatal("certificate errors must stay WARN")
	}
}

func TestLogTimeoutBoundsWarnBursts(t *testing.T) {
	var buf bytes.Buffer
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	const msg = "copilot sniff: upstream response read timed out"
	err := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	for i := 0; i < 24; i++ {
		p.logTimeout(msg, err, "agent", "a")
	}
	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Fatalf("WARN lines = %d, want 1", got)
	}
	if got := strings.Count(buf.String(), msg); got != 24 {
		t.Fatalf("timeouts must stay observable; lines = %d, want 24", got)
	}
}

func TestLogTimeoutNonTimeoutDoesNotConsumeWarnWindow(t *testing.T) {
	var buf bytes.Buffer
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	const msg = "copilot sniff: upstream response read timed out"
	p.logTimeout(msg, io.EOF, "agent", "a")
	if buf.Len() != 0 {
		t.Fatalf("non-timeout error must log nothing, got %q", buf.String())
	}
	p.logTimeout(msg, &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, "agent", "a")
	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Fatalf("first real timeout after a hangup must WARN; WARN lines = %d, want 1", got)
	}
}

func TestLogTimeoutWarnWindowReArmsPerMessage(t *testing.T) {
	var buf bytes.Buffer
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	const (
		readMsg  = "copilot sniff: upstream response read timed out"
		relayMsg = "copilot sniff: response relay timed out"
	)
	err := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}

	// Inside the window the second message is independent: a WARN for one
	// timeout site must not swallow the first WARN of another.
	p.logTimeout(readMsg, err, "agent", "a")
	p.logTimeout(relayMsg, err, "agent", "a")
	if got := strings.Count(buf.String(), "level=WARN"); got != 2 {
		t.Fatalf("distinct messages inside the window WARN lines = %d, want 2", got)
	}

	// Age only readMsg past the window: it WARNs again while relayMsg,
	// still inside its window, stays at DEBUG.
	buf.Reset()
	p.timeoutWarnMu.Lock()
	p.timeoutWarnAt[readMsg] = time.Now().Add(-timeoutWarnWindow - time.Second)
	p.timeoutWarnMu.Unlock()
	p.logTimeout(readMsg, err, "agent", "a")
	p.logTimeout(relayMsg, err, "agent", "a")
	out := buf.String()
	if got := strings.Count(out, "level=WARN"); got != 1 {
		t.Fatalf("after the window elapsed WARN lines = %d, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "level=WARN msg=\""+readMsg+"\"") {
		t.Fatalf("the re-armed WARN must be the aged message:\n%s", out)
	}
	if !strings.Contains(out, "level=DEBUG msg=\""+relayMsg+"\"") {
		t.Fatalf("the message still inside its window must stay DEBUG:\n%s", out)
	}

	// Re-arming stamps a fresh window start, so the next repeat is DEBUG again.
	buf.Reset()
	p.logTimeout(readMsg, err, "agent", "a")
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("repeat right after re-arm must be DEBUG:\n%s", buf.String())
	}
}
