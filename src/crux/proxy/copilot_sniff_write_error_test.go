package proxy

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// ============================================================
// github_proxy.go — handleCopilotSniff CONNECT-response write failure
//
// If the client hangs up before the "200 Connection established" line goes
// out, the handler must log and return without ever touching the TLS/sniff
// machinery. Only the successful-write path was covered before.
// ============================================================

func TestHandleCopilotSniffConnectWriteError(t *testing.T) {
	var buf bytes.Buffer
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(&buf, nil))}

	client, server := net.Pipe()
	client.Close()
	server.Close() // writing the CONNECT-OK line now fails immediately

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.handleCopilotSniff(server, "api.githubcopilot.com", "worker9")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleCopilotSniff did not return on write failure")
	}

	if !strings.Contains(buf.String(), "CONNECT response failed") {
		t.Errorf("write failure not logged: %s", buf.String())
	}
}
