package proxy

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/tokens"
)

const (
	proxyListenPort        = 18443
	InferenceTranslatePort = 18444
	modeFilePrefix         = "/tmp/.hive-mode-"
	maxViolationLog        = 1000
)

// CACertPath and caKeyPath are the PVC locations of the persisted MITM CA.
// They are vars rather than consts solely so tests can redirect the CA
// read/write to a temporary directory; production never reassigns them.
var (
	CACertPath = "/data/proxy-ca.pem"
	caKeyPath  = "/data/proxy-ca-key.pem"
)

// GitHubProxy is an HTTP CONNECT proxy that performs MITM TLS
// inspection on GitHub API traffic and enforces ACMM mode rules.
type GitHubProxy struct {
	listenAddr   string
	caCert       tls.Certificate
	caX509       *x509.Certificate
	logger       *slog.Logger
	uidMap       *agent.UIDMap
	allowedRepos map[string]bool

	mu         sync.RWMutex
	violations map[string]int // agent name -> blocked request count

	certMu    sync.RWMutex
	certCache map[string]cachedCert

	inference *inferenceRouter

	// entitlements caches the per-key entitled model set for LiteLLM
	// gateways (keyed by endpoint), learned from a key-info probe or a
	// "team not allowed" 403 body, so the dashboard can offer only usable
	// models.
	entitlements *entitlementStore

	// inferenceAuth tracks CONSECUTIVE inference-backend auth failures (a stale
	// gateway key returning 401 on every call) so the spoke can surface a hive
	// that is silently 401'ing as a health signal instead of merely looking
	// quiet. It latches only after inferenceAuthFailureThreshold failures and
	// clears on the next success (self-heal). Never nil after NewGitHubProxy.
	inferenceAuth *inferenceAuthState

	// tokenSink records per-agent token usage for bare-mode inference
	// agents, whose usage the file-scanning token collector cannot see
	// otherwise. May be nil (Record is a safe no-op on a nil sink).
	tokenSink *tokens.InferenceSink

	// copilotDial, when set, overrides how the Copilot-sniff MITM path dials the
	// upstream Copilot host (default: tls.Dial to host:443). It exists solely as
	// a test seam so the CONNECT handler can be driven against a local fake
	// upstream; production leaves it nil.
	copilotDial func(host string) (net.Conn, error)
}

// dialCopilotUpstream connects to the real Copilot host over TLS (or the test
// seam when set).
func (p *GitHubProxy) dialCopilotUpstream(host string) (net.Conn, error) {
	if p.copilotDial != nil {
		return p.copilotDial(host)
	}
	return tls.DialWithDialer(upstreamDialer(), "tcp", net.JoinHostPort(host, "443"), &tls.Config{ServerName: host})
}

// SetTokenSink wires the inference token sink so the translator can record
// per-agent usage from upstream OpenAI responses into the shared token store.
func (p *GitHubProxy) SetTokenSink(sink *tokens.InferenceSink) {
	p.tokenSink = sink
}

type cachedCert struct {
	cert      tls.Certificate
	expiresAt time.Time
}

// NewGitHubProxy creates a proxy with a self-signed CA for MITM.
// The org and repos parameters define which repositories are allowed for
// write operations. Repos should be bare names (e.g. "console"); the org
// is prepended to form "org/repo" keys.
func NewGitHubProxy(logger *slog.Logger, org string, repos []string) (*GitHubProxy, error) {
	caCert, caX509, err := loadOrGenerateCA(logger)
	if err != nil {
		return nil, fmt.Errorf("CA setup: %w", err)
	}

	var uidMap *agent.UIDMap
	if loaded, loadErr := agent.LoadUIDMap(agent.UIDMapPath); loadErr == nil {
		uidMap = loaded
		logger.Info("proxy loaded UID map", "agents", len(uidMap.Agents), "iptables", uidMap.IptablesActive)
	}

	allowed := make(map[string]bool, len(repos))
	for _, repo := range repos {
		key := org + "/" + repo
		allowed[key] = true
	}

	p := &GitHubProxy{
		listenAddr:    fmt.Sprintf("127.0.0.1:%d", proxyListenPort),
		caCert:        caCert,
		caX509:        caX509,
		logger:        logger,
		uidMap:        uidMap,
		allowedRepos:  allowed,
		violations:    make(map[string]int),
		certCache:     make(map[string]cachedCert),
		inference:     newInferenceRouter(),
		entitlements:  newEntitlementStore(),
		inferenceAuth: &inferenceAuthState{},
	}

	// Pre-warm cert cache for known GitHub hosts to avoid startup burst
	for host := range githubHosts {
		if cert, err := p.forgeCert(host); err == nil {
			_ = cert
			logger.Info("pre-warmed cert cache", "host", host)
		}
	}

	return p, nil
}

// ListenAddr returns the proxy listen address.
func (p *GitHubProxy) ListenAddr() string { return p.listenAddr }

// Violations returns a snapshot of per-agent violation counts.
func (p *GitHubProxy) Violations() map[string]int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]int, len(p.violations))
	for k, v := range p.violations {
		out[k] = v
	}
	return out
}

// AgentViolations returns the violation count for a specific agent.
func (p *GitHubProxy) AgentViolations(agentName string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.violations[agentName]
}

// Start begins listening. Blocks until the listener is closed.
// Handles both explicit HTTP CONNECT proxy requests and transparent
// iptables-redirected TLS connections (detected by TLS ClientHello).
func (p *GitHubProxy) Start() error {
	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", p.listenAddr, err)
	}
	defer ln.Close()
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
	defer conn.Close()

	peeked := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(transparentProxyTimeout))
	n, err := conn.Read(peeked)
	conn.SetReadDeadline(time.Time{})
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
	conn.SetReadDeadline(time.Now().Add(httpReadTimeout))
	req, err := http.ReadRequest(bufio.NewReader(prefixed))
	conn.SetReadDeadline(time.Time{})
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
	// upstreamDialTimeout bounds the proxy's own upstream dial + TLS handshake
	// to the real GitHub host. A stalled upstream used to block forever and
	// wedge the agent request waiting for the MITM response. The deadline is
	// lifted after the TLS connection is established, so long-lived relays are
	// unaffected.
	upstreamDialTimeout = 15 * time.Second
)

func upstreamDialer() *net.Dialer {
	return &net.Dialer{Timeout: upstreamDialTimeout}
}

// handleTransparentTLS handles iptables-redirected connections. The agent
// tried to connect to github.com:443 but iptables sent it here instead.
// We extract the SNI hostname from the TLS ClientHello, then MITM the connection.
func (p *GitHubProxy) handleTransparentTLS(conn net.Conn, peeked []byte) {
	// Read enough of the ClientHello to extract SNI.
	buf := make([]byte, tlsClientHelloMaxSize)
	copy(buf, peeked)
	conn.SetReadDeadline(time.Now().Add(transparentProxyTimeout))
	n, err := conn.Read(buf[len(peeked):])
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}
	fullBuf := buf[:len(peeked)+n]

	host := extractSNI(fullBuf)
	if host == "" {
		host = "github.com"
	}

	// Identify agent by UID from /proc/net/tcp
	agentName := ""
	if p.uidMap != nil {
		_, portStr, splitErr := net.SplitHostPort(conn.RemoteAddr().String())
		if splitErr == nil {
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
			if lookupErr == nil {
				agentName = p.uidMap.LookupByUID(uid)
			}
		}
	}

	// Copilot completion host under iptables redirection: MITM to read live
	// token usage (same guard as the explicit-CONNECT path). The peeked
	// ClientHello is replayed via prefixConn so the TLS handshake sees the full
	// record.
	if IsCopilotAPIHost(host) && p.tokenSink != nil && agentName != "" {
		p.sniffCopilotOnTLS(&prefixConn{Conn: conn, prefix: fullBuf}, host, agentName)
		return
	}

	if !IsGitHubHost(host) || !NeedsMITM(host) {
		// Non-GitHub or non-API GitHub host: tunnel directly.
		upstream, err := net.DialTimeout("tcp", host+":443", transparentProxyTimeout)
		if err != nil {
			return
		}
		defer upstream.Close()
		if _, err := upstream.Write(fullBuf); err != nil {
			return
		}
		done := make(chan struct{})
		go func() {
			io.Copy(upstream, conn)
			upstream.(*net.TCPConn).CloseWrite()
			close(done)
		}()
		io.Copy(conn, upstream)
		<-done
		return
	}

	mode := readAgentMode(agentName)

	// MITM: forge a cert, TLS-wrap the client, connect to real upstream.
	tlsCert, err := p.forgeCert(host)
	if err != nil {
		p.logger.Error("transparent proxy forge cert failed", "host", host, "error", err)
		return
	}

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	prefixed := &prefixConn{Conn: conn, prefix: fullBuf}
	tlsClientConn := tls.Server(prefixed, tlsConfig)
	if err := tlsClientConn.Handshake(); err != nil {
		p.logger.Warn("transparent proxy TLS handshake failed", "error", err)
		return
	}
	defer tlsClientConn.Close()

	upstreamConn, err := tls.DialWithDialer(upstreamDialer(), "tcp", host+":443", &tls.Config{ServerName: host})
	if err != nil {
		p.logger.Error("transparent proxy upstream dial failed", "host", host, "error", err)
		return
	}
	defer upstreamConn.Close()

	p.proxyHTTP(tlsClientConn, upstreamConn, agentName, mode)
}

const tlsClientHelloMaxSize = 4096

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

// identifyAgentFromReq determines the agent name for a request. It always
// tries UID-based identification first (unforgeable, works for any client
// including native binaries that don't send Proxy-Authorization), falling
// back to Proxy-Authorization headers when UID lookup fails.
func (p *GitHubProxy) identifyAgentFromReq(r *http.Request) string {
	if p.uidMap != nil {
		if name := p.identifyAgentByUID(r.RemoteAddr); name != "" {
			return name
		}
	}
	return extractAgentName(r)
}

// identifyAgentFromConn identifies the calling agent for a request read off a
// raw connection. It MUST be used instead of identifyAgentFromReq on any path
// where the request came from http.ReadRequest rather than http.Server:
// ReadRequest does not populate Request.RemoteAddr (only http.Server does), so
// the UID lookup silently gets an empty address, finds no agent, and the caller
// falls back to ADVISORY — downgrading every write-capable agent to read-only.
// Taking the peer address from the connection itself removes that trap.
func (p *GitHubProxy) identifyAgentFromConn(conn net.Conn, r *http.Request) string {
	if p.uidMap != nil && conn != nil {
		if name := p.identifyAgentByUID(conn.RemoteAddr().String()); name != "" {
			return name
		}
	}
	return extractAgentName(r)
}

// identifyAgentByUID reads /proc/net/tcp to find the UID of the process
// that owns the socket connected to the proxy, then looks up the agent name.
func (p *GitHubProxy) identifyAgentByUID(remoteAddr string) string {
	_, portStr, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return ""
	}
	port := 0
	const maxPort = 65535
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return ""
		}
		port = port*10 + int(c-'0')
		if port > maxPort {
			return ""
		}
	}
	uid, err := LookupUIDByLocalPort(port)
	if err != nil {
		return ""
	}
	return p.uidMap.LookupByUID(uid)
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

	// Non-GitHub hosts: tunnel without inspection.
	if !IsGitHubHost(host) {
		p.tunnelDirect(conn, r)
		return
	}

	// github.com doesn't need MITM — OAuth device flow and git smart HTTP
	// are handled by CLI --deny-tool flags. Only api.github.com needs
	// request-level inspection for ACMM enforcement.
	if !NeedsMITM(host) {
		p.tunnelDirect(conn, r)
		return
	}

	mode := readAgentMode(agentName)

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
	if err := tlsClientConn.Handshake(); err != nil {
		p.logger.Warn("proxy client TLS handshake failed", "error", err)
		return
	}
	defer tlsClientConn.Close()

	// Connect to the real GitHub server.
	upstreamConn, err := tls.DialWithDialer(upstreamDialer(), "tcp", r.Host, &tls.Config{
		ServerName: host,
	})
	if err != nil {
		p.logger.Error("proxy upstream dial failed", "host", r.Host, "error", err)
		return
	}
	defer upstreamConn.Close()

	// Proxy HTTP requests, inspecting each one.
	p.proxyHTTP(tlsClientConn, upstreamConn, agentName, mode)
}

// proxyHTTP reads HTTP requests from the client, checks them against
// mode rules, and either forwards or blocks them.
func (p *GitHubProxy) proxyHTTP(client net.Conn, upstream net.Conn, agentName string, mode agent.AgentMode) {
	clientBuf := newBufferedConn(client)

	for {
		req, err := http.ReadRequest(clientBuf)
		if err != nil {
			return // client closed or error
		}

		blocked := false
		blockReason := ""

		if req.Method == "POST" && IsGraphQLPath(req.URL.Path) {
			body, readErr := io.ReadAll(io.LimitReader(req.Body, graphQLBodyLimit))
			if req.Body != nil {
				req.Body.Close()
			}
			if readErr != nil {
				return
			}
			allowed, isMutation := GraphQLAllowed(mode, body)
			if !allowed {
				blocked = true
				if isMutation {
					blockReason = "graphql mutation"
				} else {
					blockReason = "graphql"
				}
			}
			req.Body = io.NopCloser(strings.NewReader(string(body)))
			req.ContentLength = int64(len(body))
		} else if !AllowedByMode(mode, req.Method, req.URL.Path) {
			blocked = true
			// An unidentified agent is silently treated as ADVISORY, which turns
			// a permissions bug into an indistinguishable "policy denial". Say so
			// loudly on writes: a write from an unknown caller means UID
			// attribution failed, not that the agent lacks the mode.
			if agentName == "" && writeMethods[req.Method] {
				p.logger.Warn("proxy: agent could not be identified — defaulting to ADVISORY and blocking a write; UID attribution failed (check the uid map and /proc/net/tcp visibility), this is NOT an agent-mode misconfiguration",
					"method", req.Method, "path", req.URL.Path)
			}
			// A hard-deny rule (e.g. direct PR creation) carries an agent-facing
			// directive explaining the sanctioned alternative — surface it so the
			// agent knows to use hive-open-pr rather than seeing only a mode error.
			if msg, denied := DeniedMessage(req.Method, req.URL.Path); denied && msg != "" {
				blockReason = msg
			}
		} else if len(p.allowedRepos) > 0 && !RepoFilterAllowed(p.allowedRepos, req.Method, req.URL.Path) {
			blocked = true
			blockReason = "repo not in hive config"
		}

		if blocked {
			detail := req.URL.Path
			if blockReason != "" {
				detail = blockReason
			}
			p.recordViolation(agentName, req.Method, detail)

			resp := &http.Response{
				StatusCode: http.StatusForbidden,
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(fmt.Sprintf("⛔ ACMM proxy: %s (%s) blocked %s %s\n", agentName, mode, req.Method, detail))),
			}
			resp.Header.Set("Content-Type", "text/plain")
			resp.Header.Set("X-Hive-Proxy-Blocked", "true")
			resp.Write(client)

			if req.Body != nil {
				io.Copy(io.Discard, req.Body)
				req.Body.Close()
			}
			continue
		}

		// Git smart HTTP uses chunked streaming that http.ReadResponse
		// can't handle reliably. After the ACMM check passes, forward
		// the request and switch to raw bidirectional streaming.
		if isGitPath(req.URL.Path) {
			if err := req.Write(upstream); err != nil {
				return
			}
			done := make(chan struct{})
			go func() {
				io.Copy(upstream, client)
				close(done)
			}()
			io.Copy(client, upstream)
			<-done
			return
		}

		// Forward to upstream.
		if err := req.Write(upstream); err != nil {
			return
		}

		resp, err := http.ReadResponse(newBufferedReader(upstream), req)
		if err != nil {
			return
		}

		if err := resp.Write(client); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()
	}
}

func (p *GitHubProxy) recordViolation(agentName, method, path string) {
	p.logger.Warn("proxy request blocked", "agent", agentName, "method", method, "path", path)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.violations[agentName]; exists || len(p.violations) < maxViolationLog {
		p.violations[agentName]++
	}
}

func isGitPath(path string) bool {
	return strings.HasSuffix(path, "/git-receive-pack") ||
		strings.HasSuffix(path, "/git-upload-pack") ||
		strings.HasSuffix(path, "/info/refs")
}

// tunnelDirect creates a raw TCP tunnel for non-GitHub CONNECT requests.
func (p *GitHubProxy) tunnelDirect(conn net.Conn, r *http.Request) {
	const tunnelDialTimeout = 10 * time.Second
	upstream, err := net.DialTimeout("tcp", r.Host, tunnelDialTimeout)
	if err != nil {
		p.logger.Warn("proxy: CONNECT dial failed", "host", r.Host, "error", err)
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\nconnection failed\n")
		return
	}
	defer upstream.Close()

	fmt.Fprintf(conn, "HTTP/1.1 200 Connection established\r\n\r\n")

	done := make(chan struct{})
	go func() {
		io.Copy(upstream, conn)
		if tc, ok := upstream.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
		close(done)
	}()
	io.Copy(conn, upstream)
	<-done
}

func transfer(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	io.Copy(dst, src)
}

// forwardPlainDirect handles non-CONNECT (plain HTTP) requests on a raw connection.
var plainHTTPClient = &http.Client{
	Transport: http.DefaultTransport,
	Timeout:   httpWriteTimeout,
}

func (p *GitHubProxy) forwardPlainDirect(conn net.Conn, r *http.Request) {
	resp, err := plainHTTPClient.Transport.RoundTrip(r)
	if err != nil {
		p.logger.Warn("proxy: plain HTTP forward failed", "url", r.URL.String(), "error", err)
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\nupstream request failed\n")
		return
	}
	defer resp.Body.Close()
	resp.Write(conn)
}

// extractAgentName reads the agent name from the Proxy-Authorization header.
// Supports "hive <name>" (custom) and "Basic <b64>" (standard HTTP proxy auth
// sent automatically when the proxy URL contains userinfo, e.g. http://quality@host:port).
func extractAgentName(r *http.Request) string {
	auth := r.Header.Get("Proxy-Authorization")
	if auth == "" {
		return ""
	}
	if strings.HasPrefix(auth, "hive ") {
		return strings.TrimPrefix(auth, "hive ")
	}
	if strings.HasPrefix(auth, "Basic ") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		if err != nil {
			return ""
		}
		// Format is "username:password" — agent name is the username portion.
		user, _, _ := strings.Cut(string(decoded), ":")
		return user
	}
	return ""
}

// readAgentMode reads the mode from the hot-reloadable mode file.
func readAgentMode(agentName string) agent.AgentMode {
	if agentName == "" {
		return agent.ModeAdvisory
	}
	data, err := os.ReadFile(modeFilePrefix + agentName)
	if err != nil {
		return agent.ModeAdvisory
	}
	mode, ok := agent.ParseAgentMode(strings.TrimSpace(string(data)))
	if !ok {
		return agent.ModeAdvisory
	}
	return mode
}

// forgeCert generates a TLS certificate for the given hostname,
// signed by the proxy's CA.
func (p *GitHubProxy) forgeCert(host string) (tls.Certificate, error) {
	p.certMu.RLock()
	if cached, ok := p.certCache[host]; ok && time.Now().Before(cached.expiresAt) {
		p.certMu.RUnlock()
		return cached.cert, nil
	}
	p.certMu.RUnlock()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}

	caKey, ok := p.caCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return tls.Certificate{}, fmt.Errorf("CA key is not ECDSA")
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, p.caX509, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return cert, err
	}
	// Include the CA cert in the chain so clients that load system/extra
	// certs asynchronously (e.g. GitHub Copilot's bundled undici) can
	// verify the forged leaf without having the CA pre-loaded in their
	// trust store.
	cert.Certificate = append(cert.Certificate, p.caX509.Raw)

	p.certMu.Lock()
	if p.certCache == nil {
		p.certCache = make(map[string]cachedCert)
	}
	p.certCache[host] = cachedCert{cert: cert, expiresAt: time.Now().Add(time.Hour)}
	p.certMu.Unlock()

	return cert, nil
}

// generateCA creates a self-signed CA certificate for MITM.
func generateCA() (tls.Certificate, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Hive ACMM Proxy CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	x509Cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	return tlsCert, x509Cert, nil
}

// loadOrGenerateCA reuses an existing CA key pair from disk if present and
// valid, or generates a fresh one and persists both cert and key to the PVC.
// Reusing the CA across pod restarts means the system trust store (populated
// by entrypoint.sh before the Go binary starts) already contains the right
// CA, so clients that load system certs asynchronously (GitHub Copilot's
// bundled undici) can verify forged certificates immediately.
func loadOrGenerateCA(logger *slog.Logger) (tls.Certificate, *x509.Certificate, error) {
	certPEM, certErr := os.ReadFile(CACertPath)
	keyPEM, keyErr := os.ReadFile(caKeyPath)
	if certErr == nil && keyErr == nil {
		tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err == nil {
			block, _ := pem.Decode(certPEM)
			if block != nil {
				x509Cert, err := x509.ParseCertificate(block.Bytes)
				if err == nil && time.Now().Before(x509Cert.NotAfter) {
					logger.Info("reusing persisted MITM CA", "expires", x509Cert.NotAfter)
					return tlsCert, x509Cert, nil
				}
			}
		}
		logger.Warn("persisted CA unusable, generating fresh one", "err", err)
	}

	tlsCert, x509Cert, err := generateCA()
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: x509Cert.Raw})
	if err := os.WriteFile(CACertPath, caPEM, 0644); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("write CA cert to %s: %w", CACertPath, err)
	}

	caKeyDER, err := x509.MarshalECPrivateKey(tlsCert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("marshal CA key: %w", err)
	}
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER})
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0600); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("write CA key to %s: %w", caKeyPath, err)
	}

	logger.Info("generated and persisted fresh MITM CA", "certPath", CACertPath, "keyPath", caKeyPath)
	return tlsCert, x509Cert, nil
}

func newBufferedConn(c net.Conn) *bufio.Reader {
	return bufio.NewReader(c)
}

func newBufferedReader(c net.Conn) *bufio.Reader {
	return bufio.NewReader(c)
}

// SetInferenceRoute configures an agent to use a self-hosted inference backend.
//
// The route is stored immediately and the caller returns without blocking. The
// model's max context length is queried in the background because this function
// is invoked from callers that hold the agent-manager mutex (agent launch,
// SetModelOverride, SetBackendOverride). A synchronous endpoint probe here
// (up to maxModelLenQueryTimeout) would pin that mutex for its whole duration,
// stalling /api/status and other dashboard handlers — which on a single-replica
// pod reads to the OpenShift router as "Application is not available". The
// max-context-len only feeds token capping and defaults gracefully to 0
// (no cap) until the async probe fills it in.
func (p *GitHubProxy) SetInferenceRoute(agentName string, route *InferenceRoute) {
	p.inference.Set(agentName, route)
	p.logger.Info("inference route set", "agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint, "model", route.Model, "maxContextLen", route.MaxContextLen)

	if route.MaxContextLen == 0 {
		endpoint, model, apiKey, caBundle := route.Endpoint, route.Model, route.APIKey, route.CABundle
		go func() {
			if maxLen := queryMaxModelLen(endpoint, model, apiKey, caBundle); maxLen > 0 {
				if p.inference.UpdateMaxContextLen(agentName, endpoint, model, maxLen) {
					p.logger.Info("inference route max context length resolved", "agent", agentName, "model", model, "maxContextLen", maxLen)
				}
			}
		}()
	}

	// Best-effort: learn the key's entitled model set from a LiteLLM
	// key-info endpoint before any 403 happens, so the dashboard can offer
	// only usable models up front. Only litellm gateways entitlement-filter
	// per key; vllm/llm-d do not. The 403-parse path (recordInferenceError)
	// remains the authoritative fallback when no key-info endpoint responds.
	if route.Backend == "litellm" && route.APIKey != "" {
		p.entitlements.rememberProbe(route.Endpoint, route.APIKey, route.CABundle)
		go p.probeEntitlements(route.Endpoint, route.APIKey, route.CABundle)
	}
}

// probeEntitlements queries a LiteLLM gateway's per-key info endpoints for the
// key's entitled model set and caches it. A nil result (no key-info endpoint,
// or an unrestricted key) leaves any existing 403-derived entry untouched.
func (p *GitHubProxy) probeEntitlements(endpoint, apiKey, caBundle string) {
	if !p.entitlements.beginProbe(endpoint) {
		return // another probe for this endpoint is already in flight
	}
	defer p.entitlements.endProbe(endpoint)
	transport, err := inferenceTransport(caBundle)
	if err != nil {
		transport = nil
	}
	if models := probeEntitledModels(endpoint, apiKey, transport); len(models) > 0 {
		p.entitlements.set(endpoint, apiKey, models, "key-info")
		p.logger.Info("litellm entitled models discovered via key-info",
			"endpoint", endpoint, "count", len(models))
	}
}

// EntitledModels returns the entitled model set known for a LiteLLM endpoint
// (from a key-info probe or a "team not allowed" 403), the signal source, and
// whether the set is known. It is the accessor the dashboard uses to narrow
// model dropdowns to what the configured key can actually use.
//
// A stale entry (older than entitlementTTL) is still returned so the served
// model list stays STABLE for an unchanged key — a filtered list that flapped
// back to the full catalog between probes would make the dashboard's
// auto-heal (see reconcileModelsAfterDiscovery, #1848) see models reappear
// and vanish across refreshes. Staleness instead triggers a bounded
// background re-probe that overwrites the entry only on a fresh signal.
func (p *GitHubProxy) EntitledModels(endpoint string) (models []string, source string, known bool) {
	models, source, known, stale := p.entitlements.get(endpoint)
	if stale {
		if ep, apiKey, caBundle, ok := p.entitlements.probeParams(endpoint); ok {
			go p.probeEntitlements(ep, apiKey, caBundle)
		}
	}
	return models, source, known
}

// recordInferenceError inspects an upstream inference error body for a
// LiteLLM "team not allowed to access model" 403 and, when found, caches the
// entitled model set it lists (keyed by the route's endpoint) so the
// dashboard's model list narrows to the usable set. Only a 403 carrying the
// gateway's team-scope marker is treated as an entitlement signal —
// provider-side failures relayed by the gateway (upstream 404 not-found, 400
// bad-request, 5xx) say nothing about the key's scope and are ignored, as is
// any other 403 (bad key, rate limit).
//
// It deliberately does NOT switch the agent's route itself: automatic model
// switching is governed by the dashboard's reconcile policy (#1848 — unpinned
// + governor suggestion, or confirmed genuine unavailability, at most once
// per list change). Once the entitled set is cached here, the dashboard's
// filtered discovery list drives that heal path for any agent left on a
// non-entitled model.
func (p *GitHubProxy) recordInferenceError(route *InferenceRoute, agentName string, status int, body []byte) {
	if route == nil {
		return
	}

	// Inference-backend AUTH failure (a stale/invalid gateway key returning 401
	// on every call). Tracked for ANY inference backend, not just litellm — a
	// self-hosted vLLM/llm-d fronted by auth can 401 too. Latches only after
	// inferenceAuthFailureThreshold consecutive failures and clears on the next
	// success, so a single transient blip never alarms. The stored cause is
	// log-safe: the gateway's 401 body describes the rejection without echoing
	// the presented token, and inferenceAuthErrorMessage never reads the key.
	if isInferenceAuthStatus(status) && p.inferenceAuth != nil {
		msg := inferenceAuthErrorMessage(route.Backend, status, truncateBytes(body, 200))
		p.inferenceAuth.recordFailure(msg, time.Now())
		p.logger.Warn("inference backend auth failure",
			"agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint, "status", status)
	}

	if route.Backend != "litellm" || status != http.StatusForbidden {
		return
	}
	entitled := parseTeam403Models(string(body))
	if len(entitled) == 0 {
		return
	}
	p.entitlements.set(route.Endpoint, route.APIKey, entitled, "403")
	p.logger.Info("litellm entitled models learned from 403",
		"agent", agentName, "endpoint", route.Endpoint, "count", len(entitled), "model", route.Model)
}

// recordInferenceSuccess records a successful inference call so a previously
// latched auth-failure signal clears — the self-heal for the inference-auth
// health signal. Called from every inference forward path on a 2xx response.
// A nil tracker (impossible after NewGitHubProxy, but cheap to guard) is a
// no-op.
func (p *GitHubProxy) recordInferenceSuccess() {
	if p.inferenceAuth != nil {
		p.inferenceAuth.recordSuccess()
	}
}

// InferenceAuthError reports the current inference-backend auth-failure signal:
// a non-empty, log-safe cause string (and the time it first latched) ONLY while
// the backend has been auth-failing for inferenceAuthFailureThreshold
// consecutive calls, empty otherwise. Wired to the dashboard/heartbeat so the
// hub can flag a hive that is silently 401'ing on every inference call. Safe on
// a nil tracker.
func (p *GitHubProxy) InferenceAuthError() (errMsg string, since time.Time) {
	if p == nil || p.inferenceAuth == nil {
		return "", time.Time{}
	}
	return p.inferenceAuth.snapshot()
}

// ClearInferenceRoute removes an agent's inference backend override.
func (p *GitHubProxy) ClearInferenceRoute(agentName string) {
	p.inference.Clear(agentName)
	p.logger.Info("inference route cleared", "agent", agentName)
}

// StartInferenceTranslator runs a plain HTTP server that accepts Anthropic
// Messages API requests and translates+forwards them to the configured
// inference backend. Agents use ANTHROPIC_BASE_URL=http://127.0.0.1:18444
// to reach this server instead of api.anthropic.com.
func (p *GitHubProxy) StartInferenceTranslator() error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("x-api-key")
		agentName := strings.TrimPrefix(apiKey, "sk-hive-")

		p.logger.Info("inference request",
			"agent", agentName,
			"method", r.Method,
			"path", r.URL.Path,
			"content-type", r.Header.Get("Content-Type"),
			"anthropic-version", r.Header.Get("anthropic-version"),
		)

		route := p.inference.Get(agentName)
		if route == nil {
			p.logger.Warn("inference no route", "agent", agentName)
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"no inference route for agent"}}`, http.StatusBadGateway)
			return
		}

		body, err := io.ReadAll(r.Body)
		if r.Body != nil {
			r.Body.Close()
		}
		if err != nil {
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"failed to read request"}}`, http.StatusBadRequest)
			return
		}

		// Diagnostic: count tools in original Anthropic request.
		var toolDiag struct {
			Tools json.RawMessage `json:"tools"`
		}
		toolCount := -1 // -1 = no tools key
		if json.Unmarshal(body, &toolDiag) == nil && toolDiag.Tools != nil {
			var arr []json.RawMessage
			if json.Unmarshal(toolDiag.Tools, &arr) == nil {
				toolCount = len(arr)
			}
		}
		p.logger.Info("inference request body", "agent", agentName, "len", len(body), "tool_count", toolCount, "preview", truncateBytes(body, 200))

		openaiBody, err := translateAnthropicToOpenAI(body, route.Model, route.MaxContextLen, resolveInferencePreamble(route, agentName))
		if err != nil {
			p.logger.Error("inference translate request failed", "agent", agentName, "error", err)
			http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"translation error: %s"}}`, err.Error()), http.StatusBadGateway)
			return
		}

		upstreamURL := strings.TrimRight(route.Endpoint, "/") + "/v1/chat/completions"
		upstreamReq, err := http.NewRequestWithContext(r.Context(), "POST", upstreamURL, bytes.NewReader(openaiBody))
		if err != nil {
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"failed to create upstream request"}}`, http.StatusBadGateway)
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		applyInferenceAuth(upstreamReq, route)

		p.logger.Info("inference forward",
			"agent", agentName,
			"backend", route.Backend,
			"model", route.Model,
			"url", upstreamURL,
			"openai_body", truncateBytes(openaiBody, 300),
		)

		client, err := inferenceHTTPClient(route)
		if err != nil {
			p.logger.Error("inference client setup failed", "agent", agentName, "error", err)
			http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"inference client setup failed: %s"}}`, err.Error()), http.StatusBadGateway)
			return
		}
		resp, err := client.Do(upstreamReq)
		if err != nil {
			p.logger.Error("inference upstream failed", "agent", agentName, "error", err)
			http.Error(w, fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"inference backend unreachable: %s"}}`, err.Error()), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		p.logger.Info("inference upstream response",
			"agent", agentName,
			"status", resp.StatusCode,
			"content-type", resp.Header.Get("Content-Type"),
		)

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(resp.Body)
			p.logger.Error("inference upstream error",
				"agent", agentName,
				"status", resp.StatusCode,
				"body", truncateBytes(errBody, 500),
			)
			// A LiteLLM "team not allowed to access model" 403 carries the
			// entitled set — cache it so the dashboard's model list narrows
			// and its reconcile policy (#1848) can heal the agent.
			p.recordInferenceError(route, agentName, resp.StatusCode, errBody)
			anthropicErr := fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"inference backend returned %d: %s"}}`,
				resp.StatusCode, truncateBytes(errBody, 200))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write([]byte(anthropicErr))
			return
		}

		// A 2xx means the gateway accepted the key — clear any latched
		// inference-auth failure so a hive whose key was fixed self-heals.
		p.recordInferenceSuccess()

		isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

		if isStreaming {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			in, out, err := translateOpenAISSEToAnthropic(resp.Body, w, route.Model)
			if err != nil {
				p.logger.Error("inference SSE translation failed", "agent", agentName, "error", err)
			}
			if in > 0 || out > 0 {
				p.tokenSink.Record(agentName, route.Model, in, out)
			}
			return
		}

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"failed to read inference response"}}`, http.StatusBadGateway)
			return
		}

		p.logger.Info("inference upstream raw response",
			"agent", agentName,
			"len", len(respBody),
			"preview", truncateBytes(respBody, 500),
		)

		if in, out := extractOpenAIUsage(respBody); in > 0 || out > 0 {
			p.tokenSink.Record(agentName, route.Model, in, out)
		}

		translated, err := translateOpenAIResponseToAnthropic(respBody, route.Model)
		if err != nil {
			p.logger.Error("inference translate response failed", "agent", agentName, "error", err)
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"response translation error"}}`, http.StatusBadGateway)
			return
		}

		p.logger.Info("inference translated response",
			"agent", agentName,
			"len", len(translated),
			"preview", truncateBytes(translated, 500),
		)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(translated)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", InferenceTranslatePort)
	p.logger.Info("inference translation server starting", "addr", addr)
	server := &http.Server{Addr: addr, Handler: mux}
	return server.ListenAndServe()
}

// handleAnthropicReroute performs MITM on an Anthropic API connection and
// reroutes requests to a self-hosted vLLM/llm-d endpoint, translating
// between Anthropic and OpenAI API formats.
func (p *GitHubProxy) handleAnthropicReroute(conn net.Conn, r *http.Request, host, agentName string, route *InferenceRoute) {
	p.logger.Info("inference reroute", "agent", agentName, "backend", route.Backend, "host", host)

	if _, err := fmt.Fprintf(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		p.logger.Error("inference reroute: CONNECT response failed", "error", err)
		return
	}

	tlsCert, err := p.forgeCert(host)
	if err != nil {
		p.logger.Error("inference reroute: forge cert failed", "host", host, "error", err)
		return
	}

	tlsConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
	})
	if err := tlsConn.Handshake(); err != nil {
		p.logger.Warn("inference reroute: TLS handshake failed", "error", err)
		return
	}
	defer tlsConn.Close()

	clientBuf := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(clientBuf)
		if err != nil {
			return
		}

		p.handleInferenceRequest(tlsConn, req, agentName, route)
	}
}

// handleInferenceRequest translates a single Anthropic API request and
// forwards it to the inference backend.
func (p *GitHubProxy) handleInferenceRequest(conn net.Conn, req *http.Request, agentName string, route *InferenceRoute) {
	body, err := io.ReadAll(req.Body)
	if req.Body != nil {
		req.Body.Close()
	}
	if err != nil {
		p.writeHTTPError(conn, http.StatusBadGateway, "failed to read request body")
		return
	}

	openaiBody, err := translateAnthropicToOpenAI(body, route.Model, route.MaxContextLen, resolveInferencePreamble(route, agentName))
	if err != nil {
		p.logger.Error("inference translate request failed", "agent", agentName, "error", err)
		p.writeHTTPError(conn, http.StatusBadGateway, "translation error: "+err.Error())
		return
	}

	upstreamURL := strings.TrimRight(route.Endpoint, "/") + "/v1/chat/completions"
	upstreamReq, err := http.NewRequestWithContext(req.Context(), "POST", upstreamURL, bytes.NewReader(openaiBody))
	if err != nil {
		p.writeHTTPError(conn, http.StatusBadGateway, "failed to create upstream request")
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	applyInferenceAuth(upstreamReq, route)

	p.logger.Info("inference forward", "agent", agentName, "backend", route.Backend, "model", route.Model, "url", upstreamURL)

	client, err := inferenceHTTPClient(route)
	if err != nil {
		p.logger.Error("inference client setup failed", "agent", agentName, "error", err)
		p.writeHTTPError(conn, http.StatusBadGateway, "inference client setup failed: "+err.Error())
		return
	}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		p.logger.Error("inference upstream failed", "agent", agentName, "error", err)
		p.writeHTTPError(conn, http.StatusBadGateway, "inference backend unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		p.logger.Error("inference upstream error", "agent", agentName, "status", resp.StatusCode, "body", truncateBytes(errBody, 500))
		// Same entitlement capture as the HTTP translator path.
		p.recordInferenceError(route, agentName, resp.StatusCode, errBody)
		anthropicErr := &http.Response{
			StatusCode: resp.StatusCode,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"type":"error","error":{"type":"api_error","message":"inference backend returned %d: %s"}}`,
				resp.StatusCode, jsonEscape(truncateBytes(errBody, 200))))),
		}
		anthropicErr.Write(conn)
		return
	}

	// A 2xx means the gateway accepted the key — clear any latched
	// inference-auth failure so a hive whose key was fixed self-heals.
	p.recordInferenceSuccess()

	isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if isStreaming {
		hdr := "HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/event-stream; charset=utf-8\r\n" +
			"Cache-Control: no-cache\r\n" +
			"Connection: keep-alive\r\n\r\n"
		if _, err := conn.Write([]byte(hdr)); err != nil {
			return
		}
		in, out, err := translateOpenAISSEToAnthropic(resp.Body, conn, route.Model)
		if err != nil {
			p.logger.Error("inference SSE translation failed", "agent", agentName, "error", err)
		}
		if in > 0 || out > 0 {
			p.tokenSink.Record(agentName, route.Model, in, out)
		}
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		p.writeHTTPError(conn, http.StatusBadGateway, "failed to read inference response")
		return
	}

	if in, out := extractOpenAIUsage(respBody); in > 0 || out > 0 {
		p.tokenSink.Record(agentName, route.Model, in, out)
	}

	translated, err := translateOpenAIResponseToAnthropic(respBody, route.Model)
	if err != nil {
		p.logger.Error("inference translate response failed", "agent", agentName, "error", err)
		p.writeHTTPError(conn, http.StatusBadGateway, "response translation error")
		return
	}

	httpResp := &http.Response{
		StatusCode: http.StatusOK,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(translated)),
	}
	httpResp.Write(conn)
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
	if err := tlsClientConn.Handshake(); err != nil {
		p.logger.Warn("copilot sniff: client TLS handshake failed", "error", err)
		return
	}
	defer tlsClientConn.Close()

	upstreamConn, err := p.dialCopilotUpstream(host)
	if err != nil {
		p.logger.Error("copilot sniff: upstream dial failed", "host", host, "error", err)
		return
	}
	defer upstreamConn.Close()

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
		req, err := http.ReadRequest(clientBuf)
		if err != nil {
			return // client closed
		}

		isCompletion := isCopilotCompletionsPath(req.URL.Path)
		model := ""

		// For completion requests, buffer the body so we can (a) learn the
		// model id and (b) inject include_usage on streaming requests. Other
		// requests (model listings, etc.) are forwarded without buffering.
		if isCompletion && req.Body != nil {
			body, readErr := io.ReadAll(io.LimitReader(req.Body, copilotSniffBodyLimit))
			req.Body.Close()
			if readErr != nil {
				return
			}
			var streaming bool
			model, streaming = parseCopilotRequest(body)
			if streaming {
				body = ensureStreamUsageRequested(body)
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		}

		if err := req.Write(upstream); err != nil {
			return
		}

		resp, err := http.ReadResponse(upstreamBuf, req)
		if err != nil {
			return
		}

		if isCompletion && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.forwardCopilotResponseWithUsage(client, resp, host, agentName, model)
		} else {
			if err := resp.Write(client); err != nil {
				resp.Body.Close()
				return
			}
			resp.Body.Close()
		}

		if req.Close || resp.Close {
			return
		}
	}
}

// forwardCopilotResponseWithUsage forwards a completion response to the client
// while extracting token usage from a buffered copy. The full body is read into
// memory (completion responses are small), usage is recorded, then the response
// is rewritten to the client verbatim. resp.Body is consumed and closed here.
func (p *GitHubProxy) forwardCopilotResponseWithUsage(client net.Conn, resp *http.Response, host, agentName, model string) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, copilotSniffBodyLimit))
	resp.Body.Close()
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
			p.tokenSink.Record(agentName, resolvedModel, in, out)
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
	if err := resp.Write(client); err != nil {
		p.logger.Warn("copilot sniff: response write to client failed", "agent", agentName, "error", err)
	}
}

func (p *GitHubProxy) writeHTTPError(conn net.Conn, status int, msg string) {
	resp := &http.Response{
		StatusCode: status,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"%s"}}`, msg))),
	}
	resp.Write(conn)
}

func truncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// jsonEscape escapes a string for safe interpolation into a JSON string
// literal (upstream error bodies can contain quotes/backslashes/newlines that
// would otherwise corrupt the hand-built error envelope).
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	// Marshal wraps the value in quotes; strip them since the caller supplies
	// its own surrounding quotes in the format string.
	return string(b[1 : len(b)-1])
}
