package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// hostNeedsMITM decides whether a GitHub-family host must be TLS-intercepted
// rather than opaquely tunneled.
//
// Without injection this is exactly the historical NeedsMITM: only
// api.github.com, where request-level ACMM inspection happens; github.com and
// registered GHE hosts are tunneled without decryption (their traffic — git
// smart HTTP, OAuth device flow — is gated elsewhere).
//
// With injection (#1861) an opaque tunnel is a hole, not an optimization: the
// agent's git credential helper now serves the inert placeholder, so a
// git-over-HTTPS push through an un-MITM'd tunnel would reach GitHub carrying
// the placeholder and 401. Every GitHub-family host must therefore be
// intercepted so rewriteGitHubAuth can replace the placeholder with the real
// scoped token — which also means an agent-supplied credential can never
// sneak to ANY GitHub host through a tunnel the proxy declined to open.
func (p *GitHubProxy) hostNeedsMITM(host string) bool {
	if NeedsMITM(host) {
		return true
	}
	return p.injectGHAuth && IsGitHubHost(host)
}

// hostNeedsMITMFor is hostNeedsMITM for a known caller. A lane under
// write_surface.enforce has every GitHub-family host intercepted, not only
// api.github.com, because git push travels over github.com's smart HTTP and
// an opaque tunnel could not refuse it. Other lanes are unaffected.
func (p *GitHubProxy) hostNeedsMITMFor(host, agentName string) bool {
	if p.hostNeedsMITM(host) {
		return true
	}
	return IsGitHubHost(host) && p.laneWriteSurfaceEnforced(agentName)
}

// Start begins listening. Blocks until the listener is closed.
// Handles both explicit HTTP CONNECT proxy requests and transparent
// iptables-redirected TLS connections (detected by TLS ClientHello).
func (p *GitHubProxy) Start() error {
	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.listenAddr, err)
	}
	defer func() { _ = ln.Close() }()
	p.logger.Info("proxy listening", "addr", p.listenAddr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go p.handleConn(conn)
	}
}

// handleConn peeks at the first byte to distinguish HTTP CONNECT requests
// (explicit proxy) from raw TLS ClientHello (iptables-redirected traffic).
func (p *GitHubProxy) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	peeked := make([]byte, 1)
	_ = conn.SetReadDeadline(time.Now().Add(transparentProxyTimeout))
	n, err := conn.Read(peeked)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil || n == 0 {
		return
	}

	// TLS ClientHello starts with byte 0x16 (ContentType handshake).
	// HTTP methods start with ASCII letters (C for CONNECT, G for GET, etc.).
	const tlsHandshakeContentType = 0x16
	if peeked[0] == tlsHandshakeContentType {
		p.handleTransparentTLS(conn, peeked)
		return
	}

	// Parse the HTTP request directly instead of using http.Server.Serve,
	// which closes the connection on shutdown — racing with hijacked CONNECT
	// handlers.
	prefixed := &prefixConn{Conn: conn, prefix: peeked[:n]}
	_ = conn.SetReadDeadline(time.Now().Add(httpReadTimeout))
	req, err := http.ReadRequest(bufio.NewReader(prefixed))
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}

	if req.Method == http.MethodConnect {
		p.handleConnectDirect(conn, req)
	} else {
		p.forwardPlainDirect(conn, req)
	}
}

const (
	transparentProxyTimeout = 5 * time.Second
	httpReadTimeout         = 30 * time.Second
	httpWriteTimeout        = 60 * time.Second
	// upstreamDialTimeout bounds the proxy's OWN upstream dial + TLS handshake
	// to the real GitHub host. Before this, a stalled upstream (DNS, TCP, or
	// TLS) blocked forever — the agent's curl sat in poll() waiting for the
	// MITM response that never came, permanently wedging the request and
	// freezing the agent's session (the multi-hour "egress hang").
	// tls.DialWithDialer's dialer Timeout covers connect + handshake; the
	// deadline is lifted once established so long-lived relays (git smart
	// HTTP) are unaffected.
	upstreamDialTimeout = 15 * time.Second

	// clientTLSHandshakeTimeout bounds the proxy's TLS handshake with the
	// inbound client. It intentionally matches upstreamDialTimeout: both are
	// short setup phases, unlike long-lived request/response relays.
	clientTLSHandshakeTimeout = upstreamDialTimeout
)

// handleTransparentTLS handles iptables-redirected connections. The agent
// tried to connect to github.com:443 but iptables sent it here instead.
// We extract the SNI hostname from the TLS ClientHello, then MITM the connection.
func (p *GitHubProxy) handleTransparentTLS(conn net.Conn, peeked []byte) {
	// Read the whole ClientHello TLS record before extracting SNI and replaying
	// it. TCP guarantees nothing about record boundaries: a single Read can
	// return only PART of the ClientHello. A partial read makes extractSNI miss
	// the SNI (so the wrong host is chosen and the wrong cert is forged) and
	// mis-frames the record stream handed to tls.Server — the agent-side
	// handshake then fails deterministically (bad certificate / bad record MAC),
	// which is exactly why every Copilot agent could not authenticate. Loop until
	// the full record (per the record-layer length) is buffered.
	_ = conn.SetReadDeadline(time.Now().Add(transparentProxyTimeout))
	fullBuf, err := readClientHelloRecord(conn, peeked)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}

	host := extractSNI(fullBuf)
	if host == "" {
		host = "github.com"
	}

	agentName := p.attributeTransparentConn(conn)

	// Copilot completion host under iptables redirection: MITM to read live
	// token usage (same guard as the explicit-CONNECT path). The peeked
	// ClientHello is replayed via prefixConn so the TLS handshake sees the full
	// record.
	if IsCopilotAPIHost(host) && p.tokenSink != nil && agentName != "" {
		p.sniffCopilotOnTLS(&prefixConn{Conn: conn, prefix: fullBuf}, host, agentName)
		return
	}

	if !NeedsInspection(host) && !p.hostNeedsMITMFor(host, agentName) {
		// Host we neither inspect nor (under #1861 injection) intercept:
		// tunnel directly. SO_MARK the socket
		// so the forced-egress redirect exempts this proxy-originated dial.
		upstream, err := p.dialTunnelUpstream(host)
		if err != nil {
			return
		}
		defer func() { _ = upstream.Close() }()
		if _, err := upstream.Write(fullBuf); err != nil {
			return
		}
		relayTunnel(conn, upstream)
		return
	}

	mode := readAgentMode(agentName)
	caps := readAgentCaps(agentName)

	// MITM: forge a cert, TLS-wrap the client, connect to real upstream.
	tlsCert, err := p.forgeCert(host)
	if err != nil {
		p.logger.Error("transparent proxy forge cert failed", "host", host, "error", err)
		return
	}

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	prefixed := &prefixConn{Conn: conn, prefix: fullBuf}
	tlsClientConn := tls.Server(prefixed, tlsConfig)
	// Bound the client TLS handshake: a client that connects and then stops
	// sending handshake bytes would otherwise wedge this connection forever
	// (empty queues on both sides — the observed egress-hang signature).
	_ = conn.SetDeadline(time.Now().Add(clientTLSHandshakeTimeout))
	handshakeErr := tlsClientConn.Handshake()
	_ = conn.SetDeadline(time.Time{})
	if handshakeErr != nil {
		if isClientHangup(handshakeErr) {
			p.logger.Debug("proxy client hung up before TLS handshake", "error", handshakeErr)
		} else {
			p.logger.Warn("transparent proxy TLS handshake failed", "error", handshakeErr)
		}
		return
	}
	defer func() { _ = tlsClientConn.Close() }()

	upstreamConn, err := tls.DialWithDialer(markDialer(upstreamDialTimeout), "tcp", host+":443", &tls.Config{ServerName: host})
	if err != nil {
		p.logger.Error("transparent proxy upstream dial failed", "host", host, "error", err)
		return
	}
	defer func() { _ = upstreamConn.Close() }()

	p.proxyHTTPHost(tlsClientConn, upstreamConn, host, agentName, mode, caps)
}

// attributeTransparentConn names the agent behind an iptables-redirected
// connection (#7793). Under transparent redirection there is no
// Proxy-Authorization header and no CONNECT request, so the ONLY identity
// evidence is the kernel socket table: the peer's ephemeral port is looked up
// in /proc/net/tcp{,6} for the owning UID, and the UID in the agent map.
//
// It returns "" — "unidentified", which the mode gate downgrades to ADVISORY —
// when there is no UID map, when the peer address carries no port (a net.Pipe
// or unix socket), when the port is not in the socket table, or when the UID
// belongs to no agent. The one deliberate exception is the hive's own control
// plane: a UID the map reports as internal (root or the proxy user, and never
// an allocated agent UID) is named internalCallerName so a hive-originated
// write is attributed rather than mistaken for a UID-attribution failure.
func (p *GitHubProxy) attributeTransparentConn(conn net.Conn) string {
	if p.uidMap == nil {
		return ""
	}
	_, portStr, splitErr := net.SplitHostPort(conn.RemoteAddr().String())
	if splitErr != nil {
		return ""
	}
	port := 0
	const maxPort = 65535
	for _, c := range portStr {
		port = port*10 + int(c-'0')
		if port > maxPort {
			port = 0
			break
		}
	}
	uid, lookupErr := LookupUIDByLocalPort(port)
	if lookupErr != nil {
		return ""
	}
	if name := p.uidMap.LookupByUID(uid); name != "" {
		return name
	}
	// The hive's own control plane is not an agent, so the lookup above
	// returns "". Under v4's forced-proxy egress that made every
	// hive-originated write — notably the App installation token mint — look
	// like an unattributable agent, which the mode gate blocks. Name it
	// explicitly so it is attributed rather than mistaken for a
	// UID-attribution failure.
	if p.uidMap.IsInternalUID(uid) {
		return internalCallerName
	}
	return ""
}

// dialTunnelUpstream opens the raw TCP leg of a non-inspected transparent
// tunnel. The socket is SO_MARKed so the forced-egress redirect exempts this
// proxy-originated dial. tunnelDial, when set, overrides it (tests).
func (p *GitHubProxy) dialTunnelUpstream(host string) (net.Conn, error) {
	if p.tunnelDial != nil {
		return p.tunnelDial(host)
	}
	return markDialer(transparentProxyTimeout).Dial("tcp", host+":443")
}

const tlsClientHelloMaxSize = 4096

// tlsRecordHeaderLen is the size of the TLS record header: content type(1) +
// version(2) + length(2).
const tlsRecordHeaderLen = 5

// readClientHelloRecord reads the complete first TLS record (the ClientHello)
// off conn, starting from the already-peeked bytes, and returns the full record
// so the caller can both extract the SNI and replay it verbatim into tls.Server.
//
// The prior implementation did a SINGLE conn.Read after the 1-byte peek and
// assumed it captured the whole ClientHello. TCP makes no such guarantee: when
// the ClientHello arrives split across segments, that single read returns only a
// prefix. extractSNI then parses a truncated ClientHello, fails to find the SNI,
// falls back to the wrong host, and the forged cert no longer matches what the
// agent asked for — so the agent-side TLS handshake fails deterministically
// (observed as "bad certificate" or "bad record MAC"), blocking every Copilot
// agent from authenticating.
//
// This reassembles the record using the record-layer length field, reading in a
// loop until the whole ClientHello record is buffered (or the buffer cap /
// deadline is hit), so both SNI extraction and the replay always see an intact
// ClientHello regardless of how TCP fragments the client's bytes. The caller
// sets/clears the read deadline around this call.
func readClientHelloRecord(conn net.Conn, peeked []byte) ([]byte, error) {
	buf := make([]byte, tlsClientHelloMaxSize)
	n := copy(buf, peeked)

	// First, ensure we have at least the record header so we know the record's
	// declared length.
	for n < tlsRecordHeaderLen {
		r, err := conn.Read(buf[n:])
		n += r
		if err != nil {
			if n == 0 {
				return nil, err
			}
			// Return what we have; extractSNI degrades gracefully on a short read.
			return buf[:n], nil
		}
	}

	// record length is bytes 3..4 (big-endian); total record size adds the header.
	recordLen := int(buf[3])<<8 | int(buf[4])
	need := tlsRecordHeaderLen + recordLen
	if need > len(buf) {
		// ClientHello larger than our cap: read to fill the buffer. The remainder
		// (and subsequent records) are served to tls.Server via prefixConn+conn,
		// so the handshake still completes; we only need enough here for SNI.
		need = len(buf)
	}

	for n < need {
		r, err := conn.Read(buf[n:])
		n += r
		if err != nil {
			if n == 0 {
				return nil, err
			}
			return buf[:n], nil
		}
	}
	return buf[:n], nil
}

// extractSNI reads the SNI hostname from a TLS ClientHello message.
func extractSNI(data []byte) string {
	if len(data) < 5 {
		return ""
	}
	// TLS record: type(1) + version(2) + length(2) + handshake
	recordLen := int(data[3])<<8 | int(data[4])
	if len(data) < 5+recordLen {
		// Partial read — use what we have
		recordLen = len(data) - 5
	}
	handshake := data[5 : 5+recordLen]
	if len(handshake) < 4 {
		return ""
	}
	// Handshake: type(1) + length(3) + ClientHello
	hsLen := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
	if len(handshake) < 4+hsLen {
		hsLen = len(handshake) - 4
	}
	ch := handshake[4 : 4+hsLen]
	if len(ch) < 34 {
		return ""
	}
	// Skip: version(2) + random(32)
	pos := 34
	// Session ID
	if pos >= len(ch) {
		return ""
	}
	sessLen := int(ch[pos])
	pos += 1 + sessLen
	// Cipher suites
	if pos+2 > len(ch) {
		return ""
	}
	csLen := int(ch[pos])<<8 | int(ch[pos+1])
	pos += 2 + csLen
	// Compression methods
	if pos >= len(ch) {
		return ""
	}
	cmLen := int(ch[pos])
	pos += 1 + cmLen
	// Extensions
	if pos+2 > len(ch) {
		return ""
	}
	extLen := int(ch[pos])<<8 | int(ch[pos+1])
	pos += 2
	extEnd := pos + extLen
	if extEnd > len(ch) {
		extEnd = len(ch)
	}
	for pos+4 <= extEnd {
		extType := int(ch[pos])<<8 | int(ch[pos+1])
		eLen := int(ch[pos+2])<<8 | int(ch[pos+3])
		pos += 4
		if pos+eLen > extEnd {
			break
		}
		if extType == 0 { // SNI extension
			sniData := ch[pos : pos+eLen]
			if len(sniData) < 2 {
				break
			}
			sniListLen := int(sniData[0])<<8 | int(sniData[1])
			_ = sniListLen
			sniPos := 2
			for sniPos+3 <= len(sniData) {
				nameType := sniData[sniPos]
				nameLen := int(sniData[sniPos+1])<<8 | int(sniData[sniPos+2])
				sniPos += 3
				if sniPos+nameLen > len(sniData) {
					break
				}
				if nameType == 0 { // host_name
					return string(sniData[sniPos : sniPos+nameLen])
				}
				sniPos += nameLen
			}
		}
		pos += eLen
	}
	return ""
}

// prefixConn wraps a net.Conn and prepends already-read bytes to the stream.
type prefixConn struct {
	net.Conn
	prefix []byte
	offset int
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if c.offset < len(c.prefix) {
		n := copy(b, c.prefix[c.offset:])
		c.offset += n
		return n, nil
	}
	return c.Conn.Read(b)
}

// handleConnectDirect handles CONNECT requests on a raw connection (no http.Server).
func (p *GitHubProxy) handleConnectDirect(conn net.Conn, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}

	// Identify from the connection, NOT from r.RemoteAddr: this request came
	// from http.ReadRequest, which leaves RemoteAddr empty.
	agentName := p.identifyAgentFromConn(conn, r)

	// Anthropic hosts with an inference route: reroute to self-hosted backend.
	if IsAnthropicHost(host) {
		if route := p.inference.Get(agentName); route != nil {
			p.handleAnthropicReroute(conn, r, host, agentName, route)
			return
		}
	}

	// Copilot completion host: MITM to read live token usage. Only when a token
	// sink is active and the agent is identified — otherwise fall through to an
	// opaque tunnel so Copilot traffic is never broken by usage capture.
	if IsCopilotAPIHost(host) && p.tokenSink != nil && agentName != "" {
		p.handleCopilotSniff(conn, host, agentName)
		return
	}

	// Hosts we do not inspect: tunnel without interception. github.com is in
	// this set — its OAuth device flow and git smart HTTP are handled by CLI
	// --deny-tool flags. api.github.com and api.linear.app are not: both need
	// request-level inspection for ACMM enforcement. Under #1861 injection,
	// however, every GitHub-family host is intercepted (see hostNeedsMITM),
	// because the agent's credential helper now serves an inert placeholder
	// that only the proxy can replace with the real scoped token.
	if !NeedsInspection(host) && !p.hostNeedsMITMFor(host, agentName) {
		p.tunnelDirect(conn, r)
		return
	}

	mode := readAgentMode(agentName)
	caps := readAgentCaps(agentName)

	// Tell client the tunnel is established.
	if _, err := fmt.Fprintf(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		p.logger.Error("proxy CONNECT response write failed", "error", err)
		return
	}

	// Generate a cert for the target host signed by our CA.
	tlsCert, err := p.forgeCert(host)
	if err != nil {
		p.logger.Error("proxy forge cert failed", "host", host, "error", err)
		return
	}

	// TLS handshake with client (presenting our forged cert).
	tlsClientConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	})
	// Bound the client TLS handshake (see handleTransparentTLS): a client that
	// stops mid-handshake must not wedge this connection forever.
	_ = conn.SetDeadline(time.Now().Add(clientTLSHandshakeTimeout))
	handshakeErr := tlsClientConn.Handshake()
	_ = conn.SetDeadline(time.Time{})
	if handshakeErr != nil {
		if isClientHangup(handshakeErr) {
			p.logger.Debug("proxy client hung up before TLS handshake", "error", handshakeErr)
		} else {
			p.logger.Warn("proxy client TLS handshake failed", "error", handshakeErr)
		}
		return
	}
	defer func() { _ = tlsClientConn.Close() }()

	// Connect to the real GitHub server. SO_MARK the socket so the forced-egress
	// redirect exempts this proxy-originated dial (see proxyEgressMark).
	// A dialer Timeout bounds both the TCP connect and the TLS handshake —
	// without it a stalled upstream wedges this CONNECT forever (the agent's
	// curl sits in poll() waiting for "200 Connection established" that never
	// comes). The deadline is lifted once established so the relay is not
	// time-boxed.
	upstreamConn, err := tls.DialWithDialer(markDialer(upstreamDialTimeout), "tcp", r.Host, &tls.Config{
		ServerName: host,
	})
	if err != nil {
		p.logger.Error("proxy upstream dial failed", "host", r.Host, "error", err)
		return
	}
	defer func() { _ = upstreamConn.Close() }()

	// Proxy HTTP requests, inspecting each one.
	p.proxyHTTPHost(tlsClientConn, upstreamConn, host, agentName, mode, caps)
}

// proxyHTTP reads HTTP requests from the client, checks them against
// mode rules, and either forwards or blocks them.
func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// logUpstreamEvent logs an upstream timeout event at the given level.
func (p *GitHubProxy) logUpstreamEvent(level slog.Level, msg string, err error, attrs ...any) {
	if !isTimeoutError(err) {
		return
	}
	attrs = append(attrs, "error", err)
	p.logger.Log(context.Background(), level, msg, attrs...)
}

// timeoutWarnWindow bounds how often the same timeout message logs at WARN.
const timeoutWarnWindow = 5 * time.Minute

// logTimeout warns on the first occurrence of a timeout message per window;
// repeats within the window drop to Debug. Transient upstream timeouts are
// expected and must not flood WARN consumers such as the fleet self-reporter.
func (p *GitHubProxy) logTimeout(msg string, err error, attrs ...any) {
	// Non-timeout errors (EOF, reset) log nothing, so they must not consume
	// the WARN window ahead of the next real timeout.
	if !isTimeoutError(err) {
		return
	}
	level := slog.LevelDebug
	now := time.Now()
	p.timeoutWarnMu.Lock()
	if last, ok := p.timeoutWarnAt[msg]; !ok || now.Sub(last) >= timeoutWarnWindow {
		if p.timeoutWarnAt == nil {
			p.timeoutWarnAt = make(map[string]time.Time)
		}
		p.timeoutWarnAt[msg] = now
		level = slog.LevelWarn
	}
	p.timeoutWarnMu.Unlock()
	p.logUpstreamEvent(level, msg, err, attrs...)
}

// isClientHangup reports a client that went away (EOF, reset, broken pipe) or
// stalled past the deadline: routine on restarts and liveness probes, not a
// hive defect.
func isClientHangup(err error) bool {
	return isTimeoutError(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed)
}

// logClientRead logs a client-side request read failure at Debug: an idle or
// departed client is expected, unlike an upstream stall.
func (p *GitHubProxy) logClientRead(msg string, err error, attrs ...any) {
	p.logUpstreamEvent(slog.LevelDebug, msg, err, attrs...)
}

// tunnelDirect creates a raw TCP tunnel for non-GitHub CONNECT requests.
func (p *GitHubProxy) tunnelDirect(conn net.Conn, r *http.Request) {
	const tunnelDialTimeout = 10 * time.Second
	// SO_MARK the socket so the forced-egress redirect exempts this
	// proxy-originated tunnel dial (CONNECT targets include :443).
	upstream, err := markDialer(tunnelDialTimeout).Dial("tcp", r.Host)
	if err != nil {
		p.logger.Warn("proxy: CONNECT dial failed", "host", r.Host, "error", err)
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\nconnection failed\n")
		return
	}
	defer func() { _ = upstream.Close() }()

	_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 Connection established\r\n\r\n")

	relayTunnel(conn, upstream)
}

// forwardPlainDirect handles non-CONNECT (plain HTTP) requests on a raw
// connection.
//
// Two policies apply here that the CONNECT path has always applied and this
// one did not: the caller is identified from the connection, and the
// destination must be globally routable (see plain_forward_guard.go). Before
// that, any process that could reach the proxy port could have it open a
// connection, from the proxy's own uid, to any address it named — including
// the hive's loopback services, the cloud metadata endpoint and the
// cluster-internal estate — and nothing in the log said who asked.
func (p *GitHubProxy) forwardPlainDirect(conn net.Conn, r *http.Request) {
	// Identify from the connection, NOT from r.RemoteAddr: this request came
	// from http.ReadRequest, which leaves RemoteAddr empty (see
	// identifyAgentFromConn). "" means the caller could not be attributed,
	// which is logged as such rather than guessed at.
	agentName := p.identifyAgentFromConn(conn, r)

	// Proxy-* hop headers address THIS proxy and must not travel further: the
	// identity they assert is the one identifyAgentFromConn just decided
	// whether to believe, and forwarding it would hand an agent's claimed name
	// (and, in the Basic form, whatever it put in the password field) to every
	// plain-HTTP host it can name.
	r.Header.Del("Proxy-Authorization")
	r.Header.Del("Proxy-Connection")

	resp, err := p.plainForwardTransport().RoundTrip(r)
	if err != nil {
		var denied *plainForwardDenied
		if errors.As(err, &denied) {
			p.refusePlainForward(conn, r, agentName, denied)
			return
		}
		if isLoopbackHost(r.URL.Host) {
			p.logger.Debug("proxy: plain HTTP forward to loopback failed", "agent", agentName, "url", r.URL.String(), "error", err, "hint", "client should bypass the proxy for loopback; NO_PROXY")
		} else {
			p.logger.Warn("proxy: plain HTTP forward failed", "agent", agentName, "url", r.URL.String(), "error", err)
		}
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\nupstream request failed\n")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_ = resp.Write(conn)
}

// refusePlainForward answers a destination the guard barred.
//
// Loopback is treated as the misconfiguration it almost always is rather than
// as an attack: agents are handed NO_PROXY=127.0.0.1,localhost,::1 (#14076),
// so a loopback request arriving here means a client ignored it. It is logged
// at Debug with the same hint as the failure path and does NOT count as a
// violation. Every other class — link-local (cloud metadata), private, CGNAT,
// multicast — is an attempt to reach the estate behind the proxy and is
// counted against the caller.
func (p *GitHubProxy) refusePlainForward(conn net.Conn, r *http.Request, agentName string, denied *plainForwardDenied) {
	if denied.reason == plainForwardReasonLoopback {
		p.logger.Debug("proxy: plain HTTP forward to loopback refused (clients must bypass the proxy for loopback; NO_PROXY)",
			"agent", agentName, "url", r.URL.String(), "target", denied.target)
	} else {
		p.logger.Warn("proxy: plain HTTP forward refused — destination is not globally routable",
			"agent", agentName, "method", r.Method, "url", r.URL.String(), "target", denied.target, "class", denied.reason)
		p.countViolation(agentName)
	}
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"+
		"the hive proxy does not forward plain HTTP to %s addresses (%s); set %s=true to allow it\n",
		denied.reason, denied.target, plainForwardAllowPrivateEnv)
}

// isLoopbackHost reports whether a host or host:port refers to the loopback interface.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
