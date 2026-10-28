package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// dialCopilotUpstream connects to the real Copilot host over TLS (or the test
// seam when set).
func (p *GitHubProxy) dialCopilotUpstream(host string) (net.Conn, error) {
	if p.copilotDial != nil {
		return p.copilotDial(host)
	}
	// SO_MARK the outbound socket so the forced-egress redirect exempts the
	// proxy's own upstream traffic (see proxyEgressMark). The override hook above
	// still wins when set, so tests are unaffected.
	return tls.DialWithDialer(markDialer(upstreamDialTimeout), "tcp", net.JoinHostPort(host, "443"), &tls.Config{ServerName: host})
}

// copilotInternalPathPrefix marks the Copilot CLI's auth-exchange endpoints on
// the GitHub API host (/copilot_internal/v2/token, /copilot_internal/user).
// The CLI calls them with the USER's Copilot OAuth token
// (COPILOT_GITHUB_TOKEN / the /data/copilot-user-token device-flow token) to
// obtain its short-lived Copilot session token and discover its completion
// host. A GitHub App installation token cannot perform that exchange, so
// rewriting these requests would cut every copilot-backend agent off from its
// model the moment injection is on (#9586). They are passed through with the
// agent's own Authorization untouched - nothing stripped, nothing injected.
//
// Residual (documented in security-model.md): the Copilot user OAuth token
// stays agent-readable and spendable on these paths; moving it server-side too
// is a follow-up. The hub-held App tokens injection protects are never
// attached here.
const copilotInternalPathPrefix = "/copilot_internal/"

// gheAPIPathPrefix is the REST prefix GitHub Enterprise Server serves its API
// under (https://<ghe-host>/api/v3/...), so the Copilot exchange on a GHE host
// arrives as /api/v3/copilot_internal/...
const gheAPIPathPrefix = "/api/v3"

// isCopilotAuthExchangePath reports whether path is a Copilot CLI
// auth-exchange endpoint that must keep the agent's own credential (see
// copilotInternalPathPrefix), on api.github.com or a GHE /api/v3 host.
func isCopilotAuthExchangePath(path string) bool {
	return strings.HasPrefix(path, copilotInternalPathPrefix) ||
		strings.HasPrefix(path, gheAPIPathPrefix+copilotInternalPathPrefix)
}

// handleCopilotSniff MITMs a GitHub Copilot completion-API connection to read
// the OpenAI-shaped token usage out of each /chat/completions response, then
// records it via the same token sink the inference path uses so Copilot cost
// goes live instead of only tallying at session shutdown.
//
// Unlike the inference reroute, this does NOT translate or reroute: the client's
// request is forwarded verbatim to the real Copilot host (with one exception —
// streaming requests get stream_options.include_usage injected so the terminal
// SSE usage chunk is emitted), and the upstream response is streamed back
// byte-for-byte to the client. The proxy only reads a copy of the response to
// extract usage.
//
// It is only invoked when a token sink is active and the agent is identified;
// callers fall back to opaque tunneling otherwise, so a missing sink or unknown
// agent never breaks Copilot traffic.
func (p *GitHubProxy) handleCopilotSniff(conn net.Conn, host, agentName string) {
	if _, err := fmt.Fprintf(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		p.logger.Error("copilot sniff: CONNECT response failed", "error", err)
		return
	}
	p.sniffCopilotOnTLS(conn, host, agentName)
}

// sniffCopilotOnTLS TLS-terminates a client connection (already past any CONNECT
// handshake), dials the real Copilot upstream, and pumps requests/responses
// through proxyCopilotHTTP so usage is recorded. Shared by the explicit-CONNECT
// (handleCopilotSniff) and the transparent-TLS (iptables) entry points so the
// forge/handshake/dial logic lives in one place. clientConn must be the raw
// (pre-TLS) client socket, optionally already wrapped in a prefixConn carrying a
// peeked ClientHello.
func (p *GitHubProxy) sniffCopilotOnTLS(clientConn net.Conn, host, agentName string) {
	tlsCert, err := p.forgeCert(host)
	if err != nil {
		p.logger.Error("copilot sniff: forge cert failed", "host", host, "error", err)
		return
	}

	tlsClientConn := tls.Server(clientConn, &tls.Config{Certificates: []tls.Certificate{tlsCert}})
	_ = clientConn.SetDeadline(time.Now().Add(clientTLSHandshakeTimeout))
	handshakeErr := tlsClientConn.Handshake()
	_ = clientConn.SetDeadline(time.Time{})
	if handshakeErr != nil {
		if isClientHangup(handshakeErr) {
			p.logger.Debug("copilot sniff: client hung up before TLS handshake", "error", handshakeErr)
		} else {
			p.logger.Warn("copilot sniff: client TLS handshake failed", "error", handshakeErr)
		}
		return
	}
	defer func() { _ = tlsClientConn.Close() }()

	upstreamConn, err := p.dialCopilotUpstream(host)
	if err != nil {
		p.logger.Error("copilot sniff: upstream dial failed", "host", host, "error", err)
		return
	}
	defer func() { _ = upstreamConn.Close() }()

	p.proxyCopilotHTTP(tlsClientConn, upstreamConn, host, agentName)
}

// copilotSniffBodyLimit caps how much of a Copilot request/response body the
// proxy buffers in order to read the usage block. Completion payloads are well
// under this; the limit bounds memory against a pathological body. Bodies larger
// than the limit are still forwarded in full (streamed), only usage extraction
// is skipped for the oversized case.
const copilotSniffBodyLimit = 32 * 1024 * 1024 // 32 MiB

// proxyCopilotHTTP reads HTTP requests from the client, forwards them to the
// Copilot upstream, and forwards responses back — extracting token usage from
// completion responses along the way. It keeps the connection alive across
// multiple request/response pairs (HTTP keep-alive), exiting when either side
// closes.
func (p *GitHubProxy) proxyCopilotHTTP(client net.Conn, upstream net.Conn, host, agentName string) {
	clientBuf := bufio.NewReader(client)
	upstreamBuf := bufio.NewReader(upstream)

	for {
		_ = client.SetReadDeadline(time.Now().Add(httpReadTimeout))
		req, err := http.ReadRequest(clientBuf)
		if err != nil {
			_ = client.SetReadDeadline(time.Time{})
			p.logClientRead("copilot sniff: client request read timed out", err, "agent", agentName, "host", host)
			return
		}

		isCompletion := isCopilotCompletionsPath(req.URL.Path)
		isAnthropic := isCopilotAnthropicMessagesPath(req.URL.Path)
		model := ""

		// For completion requests, buffer the body so we can (a) learn the
		// model id and (b) inject include_usage on streaming requests. Other
		// requests (model listings, etc.) are forwarded without buffering.
		if isCompletion && req.Body != nil {
			body, readErr := io.ReadAll(io.LimitReader(req.Body, copilotSniffBodyLimit))
			_ = req.Body.Close()
			if readErr != nil {
				_ = client.SetReadDeadline(time.Time{})
				p.logTimeout("copilot sniff: request body read timed out", readErr, "agent", agentName, "host", host, "path", req.URL.Path)
				return
			}
			var streaming bool
			model, streaming = parseCopilotRequest(body)
			// stream_options is OpenAI-only; Anthropic streams always carry
			// usage in message_start/message_delta.
			if streaming && !isAnthropic {
				body = ensureStreamUsageRequested(body)
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		}

		_ = upstream.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
		if err := req.Write(upstream); err != nil {
			_ = upstream.SetWriteDeadline(time.Time{})
			_ = client.SetReadDeadline(time.Time{})
			p.logTimeout("copilot sniff: request relay timed out", err, "agent", agentName, "host", host, "path", req.URL.Path)
			return
		}
		_ = upstream.SetWriteDeadline(time.Time{})
		_ = client.SetReadDeadline(time.Time{})

		_ = upstream.SetReadDeadline(time.Now().Add(httpReadTimeout))
		resp, err := http.ReadResponse(upstreamBuf, req)
		_ = upstream.SetReadDeadline(time.Time{})
		if err != nil {
			p.logTimeout("copilot sniff: upstream response read timed out", err, "agent", agentName, "host", host, "path", req.URL.Path)
			return
		}

		if isAnthropic && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := p.forwardCopilotAnthropicResponseWithUsage(client, resp, host, agentName, model); err != nil {
				p.logTimeout("copilot sniff: response relay timed out", err, "agent", agentName, "host", host, "path", req.URL.Path)
				return
			}
		} else if isCompletion && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.forwardCopilotResponseWithUsage(client, resp, host, agentName, model)
		} else {
			// Non-completion and non-2xx responses are relayed verbatim. The
			// Write below can legitimately stay open for a long time, so it
			// must be bounded by stall, not by total duration — see
			// stallBoundedWriter.
			if err := resp.Write(&stallBoundedWriter{conn: client, idle: httpWriteTimeout}); err != nil {
				_ = resp.Body.Close()
				_ = client.SetWriteDeadline(time.Time{})
				p.logTimeout("copilot sniff: response relay timed out", err, "agent", agentName, "host", host, "path", req.URL.Path)
				return
			}
			_ = client.SetWriteDeadline(time.Time{})
			_ = resp.Body.Close()
		}

		if req.Close || resp.Close {
			return
		}
	}
}

// forwardCopilotAnthropicResponseWithUsage relays an Anthropic Messages
// (/v1/messages) response to the client while teeing the body through a usage
// parser, so streamed Claude completions are forwarded as they arrive rather
// than buffered. Usage (including prompt-cache counts) is recorded once the
// body has been fully relayed. resp.Body is consumed and closed here.
func (p *GitHubProxy) forwardCopilotAnthropicResponseWithUsage(client net.Conn, resp *http.Response, host, agentName, model string) error {
	tee := newAnthropicUsageTee(resp.Body, resp.Header.Get("Content-Type"))
	resp.Body = tee
	writeErr := resp.Write(&stallBoundedWriter{conn: client, idle: httpWriteTimeout})
	_ = resp.Body.Close()
	_ = client.SetWriteDeadline(time.Time{})

	// Record whatever was observed even if the client went away mid-stream:
	// the upstream already billed those tokens.
	u := tee.usage()
	if u.total() > 0 {
		resolvedModel := model
		if resolvedModel == "" || resolvedModel == copilotAutoModelSentinel {
			resolvedModel = u.Model
		}
		if resolvedModel != "" && resolvedModel != copilotAutoModelSentinel {
			p.recordCopilotUsageWithCache(agentName, resolvedModel, u.Input, u.Output, u.CacheRead, u.CacheCreation)
			p.logger.Info("copilot usage recorded",
				"agent", agentName,
				"host", host,
				"model", resolvedModel,
				"input", u.Input,
				"output", u.Output,
				"cache_read", u.CacheRead,
				"cache_creation", u.CacheCreation,
			)
		}
	}
	return writeErr
}

// forwardCopilotResponseWithUsage forwards a completion response to the client
// while extracting token usage from a buffered copy. The full body is read into
// memory (completion responses are small), usage is recorded, then the response
// is rewritten to the client verbatim. resp.Body is consumed and closed here.
func (p *GitHubProxy) forwardCopilotResponseWithUsage(client net.Conn, resp *http.Response, host, agentName, model string) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, copilotSniffBodyLimit))
	_ = resp.Body.Close()
	if err != nil {
		// Best effort: nothing to forward.
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if in, out := extractCopilotUsage(contentType, body); in > 0 || out > 0 {
		resolvedModel := model
		if resolvedModel == "" || resolvedModel == copilotAutoModelSentinel {
			// Fall back to the model reported in the response when the request
			// did not pin a concrete id (e.g. --model auto). Never record the
			// "auto" sentinel — it is not a real model row.
			if rm := extractCopilotResponseModel(body, contentType); rm != "" {
				resolvedModel = rm
			}
		}
		if resolvedModel != "" && resolvedModel != copilotAutoModelSentinel {
			p.recordCopilotUsage(agentName, resolvedModel, in, out)
			p.logger.Info("copilot usage recorded",
				"agent", agentName,
				"host", host,
				"model", resolvedModel,
				"input", in,
				"output", out,
			)
		}
	}

	// Rewrite the response to the client verbatim. Replace the body with the
	// buffered copy and drop Content-Length ambiguity by setting it explicitly.
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if err := resp.Write(&stallBoundedWriter{conn: client, idle: httpWriteTimeout}); err != nil {
		p.logger.Warn("copilot sniff: response write to client failed", "agent", agentName, "error", err)
	}
	_ = client.SetWriteDeadline(time.Time{})
}
