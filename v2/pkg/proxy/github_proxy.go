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
)

const (
	proxyListenPort         = 18443
	InferenceTranslatePort  = 18444
	modeFilePrefix          = "/tmp/.hive-mode-"
	maxViolationLog         = 1000
	CACertPath              = "/data/proxy-ca.pem"
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
	caCert, caX509, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("generate CA: %w", err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caX509.Raw})
	if err := os.WriteFile(CACertPath, caPEM, 0644); err != nil {
		return nil, fmt.Errorf("write CA cert to %s: %w", CACertPath, err)
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
		listenAddr:   fmt.Sprintf("127.0.0.1:%d", proxyListenPort),
		caCert:       caCert,
		caX509:       caX509,
		logger:       logger,
		uidMap:       uidMap,
		allowedRepos: allowed,
		violations:   make(map[string]int),
		certCache:    make(map[string]cachedCert),
		inference:    newInferenceRouter(),
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
)

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
	if p.uidMap != nil && p.uidMap.IptablesActive {
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

	if !IsGitHubHost(host) {
		// Non-GitHub: tunnel directly to the intended host.
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

	upstreamConn, err := tls.Dial("tcp", host+":443", &tls.Config{ServerName: host})
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


// identifyAgentFromReq determines the agent name for a request. It prefers UID-based
// identification (unforgeable) when iptables is active, falling back to
// Proxy-Authorization headers for non-iptables deployments.
func (p *GitHubProxy) identifyAgentFromReq(r *http.Request) string {
	if p.uidMap != nil && p.uidMap.IptablesActive {
		if name := p.identifyAgentByUID(r.RemoteAddr); name != "" {
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

	agentName := p.identifyAgentFromReq(r)

	// Anthropic hosts with an inference route: reroute to self-hosted backend.
	if IsAnthropicHost(host) {
		if route := p.inference.Get(agentName); route != nil {
			p.handleAnthropicReroute(conn, r, host, agentName, route)
			return
		}
	}

	// Non-GitHub hosts: tunnel without inspection.
	if !IsGitHubHost(host) {
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
	upstreamConn, err := tls.Dial("tcp", r.Host, &tls.Config{
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

func newBufferedConn(c net.Conn) *bufio.Reader {
	return bufio.NewReader(c)
}

func newBufferedReader(c net.Conn) *bufio.Reader {
	return bufio.NewReader(c)
}

// SetInferenceRoute configures an agent to use a self-hosted inference backend.
func (p *GitHubProxy) SetInferenceRoute(agentName string, route *InferenceRoute) {
	p.inference.Set(agentName, route)
	p.logger.Info("inference route set", "agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint, "model", route.Model)
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

		p.logger.Info("inference request body", "agent", agentName, "len", len(body), "preview", truncateBytes(body, 200))

		openaiBody, err := translateAnthropicToOpenAI(body, route.Model)
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

		p.logger.Info("inference forward",
			"agent", agentName,
			"backend", route.Backend,
			"model", route.Model,
			"url", upstreamURL,
			"openai_body", truncateBytes(openaiBody, 300),
		)

		resp, err := http.DefaultClient.Do(upstreamReq)
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

		isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

		if isStreaming {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if err := translateOpenAISSEToAnthropic(resp.Body, w, route.Model); err != nil {
				p.logger.Error("inference SSE translation failed", "agent", agentName, "error", err)
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

	openaiBody, err := translateAnthropicToOpenAI(body, route.Model)
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

	p.logger.Info("inference forward", "agent", agentName, "backend", route.Backend, "model", route.Model, "url", upstreamURL)

	resp, err := http.DefaultClient.Do(upstreamReq)
	if err != nil {
		p.logger.Error("inference upstream failed", "agent", agentName, "error", err)
		p.writeHTTPError(conn, http.StatusBadGateway, "inference backend unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	isStreaming := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

	if isStreaming {
		hdr := "HTTP/1.1 200 OK\r\n" +
			"Content-Type: text/event-stream; charset=utf-8\r\n" +
			"Cache-Control: no-cache\r\n" +
			"Connection: keep-alive\r\n\r\n"
		if _, err := conn.Write([]byte(hdr)); err != nil {
			return
		}
		if err := translateOpenAISSEToAnthropic(resp.Body, conn, route.Model); err != nil {
			p.logger.Error("inference SSE translation failed", "agent", agentName, "error", err)
		}
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		p.writeHTTPError(conn, http.StatusBadGateway, "failed to read inference response")
		return
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
