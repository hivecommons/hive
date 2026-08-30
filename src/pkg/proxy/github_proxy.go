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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/ioscan"
	"github.com/kubestellar/hive/pkg/tokens"
)

const (
	proxyListenPort        = 18443
	InferenceTranslatePort = 18444
	modeFilePrefix         = "/tmp/.hive-mode-"
	// capsFilePrefix holds the per-agent ORTHOGONAL capability set (#4492) — the
	// tokens agent.AgentCapabilities.String() writes. Deliberately a SECOND file
	// rather than a richer format in the mode file: bin/gh-wrapper.sh reads
	// /tmp/.hive-mode-<agent> under `set -e` and expects a bare mode string, so
	// changing that content would break every gh call on every hive.
	// A missing or empty file means no capabilities, which is the default.
	capsFilePrefix         = "/tmp/.hive-caps-"
	maxViolationLog        = 1000
	maxGitHubWriteBodyScan = 2 << 20

	// defaultProxyEgressMark is the fwmark (SO_MARK) the proxy stamps onto its
	// OWN upstream :443 dials so the forced-egress iptables redirect can exempt
	// them (via `-m mark --mark <mark> -j RETURN`) and avoid looping the proxy's
	// traffic back into itself.
	//
	// This replaces the `-m owner --uid-owner` self-exemption, which needs the
	// xt_owner kernel module — absent on OpenShift/OVN nodes. A packet mark works
	// on both OKE and OpenShift. The value is arbitrary but MUST match the mark
	// the entrypoint exempts; entrypoint.sh is the single source of truth and
	// exports HIVE_PROXY_EGRESS_MARK (defaulting to this same value) into the
	// proxy process, which reads it via proxyEgressMark().
	defaultProxyEgressMark = 0x1112

	// proxyEgressMarkEnv is the env var the entrypoint exports to keep the proxy
	// and the iptables rule in sync from one source of truth.
	proxyEgressMarkEnv = "HIVE_PROXY_EGRESS_MARK"
)

// proxyEgressMark returns the fwmark to stamp on the proxy's upstream dials,
// read from HIVE_PROXY_EGRESS_MARK (set by entrypoint.sh) so the Go proxy and
// the iptables exemption stay in lockstep. Accepts hex (0x1112) or decimal.
// Falls back to defaultProxyEgressMark when unset or unparseable — that default
// matches the entrypoint default, so an accidental drift fails safe by staying
// on the agreed value rather than silently disabling the exemption.
func proxyEgressMark() int {
	raw := strings.TrimSpace(os.Getenv(proxyEgressMarkEnv))
	if raw == "" {
		return defaultProxyEgressMark
	}
	// ParseInt with base 0 honours a 0x prefix (hex) and plain decimal.
	if v, err := strconv.ParseInt(raw, 0, 64); err == nil && v > 0 {
		return int(v)
	}
	return defaultProxyEgressMark
}

// markDialer returns a *net.Dialer whose Control hook stamps the proxy egress
// fwmark (SO_MARK) on the outbound socket before connect. On non-Linux builds
// setSockMark is a no-op, so the dialer is portable. The optional timeout is
// applied when non-zero.
//
// SO_MARK is a BEST-EFFORT egress hint, never a hard requirement: it exists so
// the forced-egress iptables redirect can exempt the proxy's own upstream dials
// via `-m mark`. But setsockopt(SO_MARK) needs CAP_NET_ADMIN in the process's
// EFFECTIVE set, and the hive Go process runs as a non-root user (entrypoint
// drops from root to `dev` via gosu, which strips all capabilities). So the mark
// call fails with EPERM ("operation not permitted") on exactly the deployments
// that run non-root — which is all of them. Returning that error from the
// Control hook ABORTS the dial, so every proxy upstream dial fails and agents
// can't reach GitHub at all. That is a regression: on OKE the `-m owner
// --uid-owner` exemption already covers the proxy's uid dials, so an unmarked
// dial succeeds; on OpenShift (no xt_owner) the mark is the only exemption, but
// there too, failing the dial helps nothing. So we log the mark failure ONCE and
// proceed unmarked rather than killing the connection.
func markDialer(timeout time.Duration) *net.Dialer {
	mark := proxyEgressMark()
	return &net.Dialer{
		Timeout: timeout,
		Control: func(_, _ string, c syscall.RawConn) error {
			// A failure to enter the raw-conn control context is a real dial
			// problem (the fd is unusable) — propagate it.
			if err := c.Control(func(fd uintptr) {
				if markErr := setSockMark(fd, mark); markErr != nil {
					warnSockMarkOnce(mark, markErr)
				}
			}); err != nil {
				return err
			}
			// Deliberately do NOT propagate a setSockMark failure: the mark is a
			// best-effort exemption hint, and the owner-UID iptables RETURN
			// already exempts the proxy's own uid on OKE, so an unmarked dial
			// still reaches GitHub.
			return nil
		},
	}
}

// warnSockMarkOnce logs the first SO_MARK failure at WARN and every subsequent
// one at DEBUG, so a non-CAP_NET_ADMIN deployment (the common non-root case)
// does not flood the log with one line per dial while still surfacing the
// condition once.
var sockMarkWarnOnce sync.Once

func warnSockMarkOnce(mark int, err error) {
	sockMarkWarnOnce.Do(func() {
		slog.Warn("proxy egress SO_MARK unavailable — dialing unmarked (owner-UID exemption still applies on OKE); this is expected when the hive runs non-root without CAP_NET_ADMIN in its effective set",
			"mark", mark, "error", err)
	})
}

// CACertPath and caKeyPath are the PVC locations of the persisted MITM CA.
// The certificate is intentionally in /data because agents need to trust it.
// The private key is kept in a dedicated owner-only directory: /data contains
// agent-readable state and must not be treated as a suitable secret directory.
// They are vars rather than consts solely so tests can redirect the CA
// read/write to a temporary directory; production never reassigns them.
var (
	CACertPath = "/data/proxy-ca.pem"
	caKeyPath  = "/data/.hive/proxy-ca-key.pem"
)

// GitHubProxy is an HTTP CONNECT proxy that performs MITM TLS
// inspection on GitHub API traffic and enforces ACMM mode rules.
type GitHubProxy struct {
	// localInferencePaths records CLI telemetry paths already logged once by
	// localInferenceResponse, so per-path DEBUG lines do not repeat.
	localInferencePaths sync.Map
	listenAddr          string
	caCert              tls.Certificate
	caX509              *x509.Certificate
	logger              *slog.Logger
	uidMap              *agent.UIDMap
	allowedRepos        map[string]bool

	// proxyAdvisoryOK mirrors entrypoint.sh's HIVE_PROXY_ADVISORY_OK — the SAME
	// explicit, operator-set escape hatch that already governs whether a failed
	// forced-egress iptables redirect is fatal. Read once at construction (it
	// is a boot-time deployment choice, not something that changes mid-process).
	// See identifyAgentFromReq/identifyAgentFromConn (N7, #3841): it gates
	// whether the proxy may EVER trust the self-asserted Proxy-Authorization
	// header as an agent's identity.
	proxyAdvisoryOK bool

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
	// inferenceBudget tracks PROVIDER spending-limit refusals (a LiteLLM key
	// past its daily dollar cap, a project out of quota). Distinct from
	// inferenceAuth (a rejected key) and from the hive's own token budget in
	// pkg/governor (denominated in tokens, blind to what the gateway charges).
	// See inference_budget.go (kubestellar/hive#4294).
	inferenceBudget *inferenceBudgetState

	// tokenSink records per-agent token usage for bare-mode inference
	// agents, whose usage the file-scanning token collector cannot see
	// otherwise. May be nil (Record is a safe no-op on a nil sink).
	tokenSink *tokens.InferenceSink

	// copilotDial, when set, overrides how the Copilot-sniff MITM path dials the
	// upstream Copilot host (default: tls.Dial to host:443). It exists solely as
	// a test seam so the CONNECT handler can be driven against a local fake
	// upstream; production leaves it nil.
	copilotDial func(host string) (net.Conn, error)

	canariesEnabled  bool
	canaryFailClosed bool
	canaryRegistry   *ioscan.CanaryRegistry
	canaryLeakFunc   func(ioscan.CanaryLeak)
}

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

// SetTokenSink wires the inference token sink so the translator can record
// per-agent usage from upstream OpenAI responses into the shared token store.
func (p *GitHubProxy) SetTokenSink(sink *tokens.InferenceSink) {
	p.tokenSink = sink
}

func (p *GitHubProxy) SetCanaryScanner(enabled, failClosed bool, reg *ioscan.CanaryRegistry, onLeak func(ioscan.CanaryLeak)) {
	p.canariesEnabled = enabled
	p.canaryFailClosed = failClosed
	p.canaryRegistry = reg
	p.canaryLeakFunc = onLeak
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

	// N7 (#3841): the same explicit opt-in entrypoint.sh already requires for a
	// degraded forced-egress gate. See proxyAdvisoryOK's doc comment.
	advisoryOK := strings.TrimSpace(os.Getenv("HIVE_PROXY_ADVISORY_OK")) == "true"
	if uidMap == nil {
		if advisoryOK {
			logger.Warn("proxy: no UID map — agent identity falls back to the self-asserted Proxy-Authorization header (HIVE_PROXY_ADVISORY_OK=true)")
		} else {
			logger.Warn("proxy: no UID map — agents will be treated as unidentified (ADVISORY, writes blocked) rather than trusting a self-asserted identity; set HIVE_PROXY_ADVISORY_OK=true to accept the header instead (N7, #3841)")
		}
	}

	allowed := make(map[string]bool, len(repos))
	for _, repo := range repos {
		key := org + "/" + repo
		allowed[key] = true
	}

	p := &GitHubProxy{
		listenAddr:      fmt.Sprintf("127.0.0.1:%d", proxyListenPort),
		caCert:          caCert,
		caX509:          caX509,
		logger:          logger,
		uidMap:          uidMap,
		proxyAdvisoryOK: advisoryOK,
		allowedRepos:    allowed,
		violations:      make(map[string]int),
		certCache:       make(map[string]cachedCert),
		inference:       newInferenceRouter(),
		entitlements:    newEntitlementStore(),
		inferenceAuth:   &inferenceAuthState{},
		inferenceBudget: &inferenceBudgetState{},
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
				// The hive's own control plane is not an agent, so the lookup
				// above returns "". Under v4's forced-proxy egress that made
				// every hive-originated write — notably the App installation
				// token mint — look like an unattributable agent, which the
				// mode gate blocks. Name it explicitly so it is attributed
				// rather than mistaken for a UID-attribution failure.
				if agentName == "" && p.uidMap.IsInternalUID(uid) {
					agentName = internalCallerName
				}
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

	if !NeedsInspection(host) {
		// Non-inspected host: tunnel directly. SO_MARK the socket
		// so the forced-egress redirect exempts this proxy-originated dial.
		upstream, err := markDialer(transparentProxyTimeout).Dial("tcp", host+":443")
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
		p.logger.Warn("transparent proxy TLS handshake failed", "error", handshakeErr)
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

// identifyAgentFromReq determines the agent name for a request. It always
// tries UID-based identification first (unforgeable, works for any client
// including native binaries that don't send Proxy-Authorization); see
// fallbackAgentName for what happens when that fails.
func (p *GitHubProxy) identifyAgentFromReq(r *http.Request) string {
	if p.uidMap != nil {
		if name := p.identifyAgentByUID(r.RemoteAddr); name != "" {
			return name
		}
	}
	return p.fallbackAgentName(r)
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
	return p.fallbackAgentName(r)
}

// fallbackAgentName is reached whenever unforgeable UID-based identification
// did NOT succeed — either p.uidMap is nil (never loaded: no entrypoint root
// phase ran to write uid-map.json, e.g. local dev, a native/systemd install,
// or a genuine boot-ordering failure) or this specific connection's UID has
// no entry in a map that DID load (N7, #3841).
//
// Before this fix, both cases fell through to extractAgentName — the
// Proxy-Authorization header, which the CALLER sets. Any process that can
// reach the proxy's loopback port can send `Proxy-Authorization: hive
// <any-name>` and be identified as that name: a spoke-agent process claiming
// to be a MORE privileged agent bypasses the whole capability model this
// proxy exists to enforce, and #1861 (inject GitHub auth at the proxy so
// agents never hold tokens) would turn that spoofed identity directly into a
// minted token for the impersonated agent. That is strictly worse than
// simply refusing to identify the caller.
//
// The safe default is therefore to NOT trust the header: return "", which
// readAgentMode already treats as ModeAdvisory (the most restrictive mode —
// writes blocked, per the existing "agent could not be identified" WARN a few
// lines below in proxyHTTP). This is not new plumbing; it is the same path an
// unidentified caller already hits today, now reached deliberately instead of
// only when the header happens to be absent.
//
// The one deliberate exception is proxyAdvisoryOK (HIVE_PROXY_ADVISORY_OK=true)
// — the SAME explicit, operator-set escape hatch entrypoint.sh already uses
// for "this deployment does not enforce the full forced-egress gate" (local
// dev, a native/systemd install with no per-agent UID separation at all,
// platforms without NET_ADMIN). An operator who has already accepted a
// degraded capability model there is not newly exposed by also accepting a
// self-asserted identity within it — and without this exception, those
// deployments would lose agent identification (and therefore write access)
// entirely, which would be a functional regression, not a security fix.
func (p *GitHubProxy) fallbackAgentName(r *http.Request) string {
	if !p.proxyAdvisoryOK {
		return ""
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

	// Hosts we do not inspect: tunnel without interception. github.com is in
	// this set — its OAuth device flow and git smart HTTP are handled by CLI
	// --deny-tool flags. api.github.com and api.linear.app are not: both need
	// request-level inspection for ACMM enforcement.
	if !NeedsInspection(host) {
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
		p.logger.Warn("proxy client TLS handshake failed", "error", handshakeErr)
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

func (p *GitHubProxy) logTimeout(msg string, err error, attrs ...any) {
	if !isTimeoutError(err) {
		return
	}
	attrs = append(attrs, "error", err)
	p.logger.Warn(msg, attrs...)
}

// proxyHTTP relays a MITM'd GitHub connection with ACMM enforcement applied.
// It is proxyHTTPHost with the GitHub host implied, which is what every
// pre-existing caller means.
func (p *GitHubProxy) proxyHTTP(client net.Conn, upstream net.Conn, agentName string, mode agent.AgentMode, caps agent.AgentCapabilities) {
	p.proxyHTTPHost(client, upstream, "", agentName, mode, caps)
}

// proxyHTTPHost is proxyHTTP with the terminated host carried through, so the
// request loop can pick the right rule engine (#4492 F).
//
// The host is threaded rather than inferred from the request because a MITM'd
// request line is a path, not an absolute URL — by this point the only record
// of which host the TLS session was forged for is the CONNECT that opened it.
// An empty host means GitHub, preserving every existing call site verbatim.
func (p *GitHubProxy) proxyHTTPHost(client net.Conn, upstream net.Conn, host string, agentName string, mode agent.AgentMode, caps agent.AgentCapabilities) {
	isLinear := IsLinearHost(host)
	clientBuf := newBufferedConn(client)

	for {
		// Bound every phase of this request/response exchange. Before this,
		// the relay had NO deadlines anywhere after the initial CONNECT read:
		// a client or upstream that stopped mid-exchange (stalled response,
		// half-written body, frozen keep-alive) blocked forever — the
		// multi-hour "egress wedge" where the agent's request never reached
		// GitHub and its session froze. Each phase gets its own deadline;
		// they are cleared once the phase completes so healthy keep-alive
		// connections are unaffected.
		_ = client.SetReadDeadline(time.Now().Add(httpReadTimeout))
		req, err := http.ReadRequest(clientBuf)
		if err != nil {
			_ = client.SetReadDeadline(time.Time{})
			p.logTimeout("proxy client request read timed out", err, "agent", agentName)
			return
		}

		blocked := false
		blockReason := ""

		if isLinear && agentName == internalCallerName {
			// The hive's OWN Linear traffic — the read path #4178 shipped
			// (backlog enumeration in pkg/worksource/linear.go) and, later,
			// the OAuth/webhook plumbing. ACMM governs AGENT autonomy, so the
			// control plane is exempt here exactly as it is for GitHub below.
			// This branch must precede the enforcement one: without it, adding
			// Linear to NeedsMITM would newly gate hive-originated reads that
			// have always been ungated, breaking enumeration rather than
			// restricting an agent.
		} else if isLinear {
			// Linear is a single POST /graphql for everything, so the
			// (method, path) rule table cannot classify it and the GitHub
			// GraphQL classifier is the wrong posture — it ends in a
			// permissive default, backed by the REST table. Linear has no
			// table underneath. See linear_rules.go.
			blocked, blockReason = p.enforceLinear(req, agentName, mode, caps)
		} else if req.Method == "POST" && IsGraphQLPath(req.URL.Path) {
			body, readErr := io.ReadAll(io.LimitReader(req.Body, graphQLBodyLimit))
			if req.Body != nil {
				_ = req.Body.Close()
			}
			if readErr != nil {
				_ = client.SetReadDeadline(time.Time{})
				p.logTimeout("proxy GraphQL request body read timed out", readErr, "agent", agentName, "path", req.URL.Path)
				return
			}
			allowed, isMutation := GraphQLAllowedCaps(mode, caps, body)
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
		} else if agentName == internalCallerName {
			// The hive itself has no ACMM mode to enforce — ACMM governs AGENT
			// autonomy. Its control-plane calls (App token mint, heartbeat)
			// must not be gated as if they were agent writes. Repo-filter and
			// canary-egress checks below still apply.
		} else if !AllowedByModeCaps(mode, caps, req.Method, req.URL.Path) {
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
		if reason, deny, ok := p.inspectCanaryEgress(agentName, req); ok {
			if deny {
				blocked = true
				if blockReason == "" {
					blockReason = reason
				}
			}
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
			_ = client.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
			writeErr := resp.Write(client)
			_ = client.SetWriteDeadline(time.Time{})
			if writeErr != nil {
				p.logTimeout("proxy blocked response write timed out", writeErr, "agent", agentName)
				return
			}

			if req.Body != nil {
				if _, drainErr := io.Copy(io.Discard, req.Body); drainErr != nil {
					p.logTimeout("proxy blocked request body drain timed out", drainErr, "agent", agentName, "path", req.URL.Path)
				}
				_ = req.Body.Close()
			}
			_ = client.SetReadDeadline(time.Time{})
			continue
		}

		// Git smart HTTP uses chunked streaming that http.ReadResponse
		// can't handle reliably. After the ACMM check passes, forward
		// the request and switch to raw bidirectional streaming.
		if isGitPath(req.URL.Path) {
			_ = upstream.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
			if err := req.Write(upstream); err != nil {
				_ = upstream.SetWriteDeadline(time.Time{})
				_ = client.SetReadDeadline(time.Time{})
				p.logTimeout("proxy git request relay timed out", err, "agent", agentName, "path", req.URL.Path)
				return
			}
			_ = upstream.SetWriteDeadline(time.Time{})
			_ = client.SetReadDeadline(time.Time{})
			// Use relayTunnel, NOT an inline copy pair: this branch used to run
			// its own unbounded io.Copy pair, which was the same wedge #3872
			// fixed in relayTunnel but left here. When GitHub FIN'd after the
			// response while the client kept its side open (keep-alive), the
			// client→upstream copy stayed blocked in client.Read() forever and
			// `<-done` never returned — the handler's deferred Closes never ran,
			// parking the upstream socket in CLOSE_WAIT for the life of the
			// process (issue #3875's FD pile). relayTunnel bounds each direction
			// once the other finishes.
			relayTunnel(client, upstream)
			return
		}

		// Forward to upstream. The client read deadline stays in force until
		// the body is fully drained (req.Write pipes req.Body from the client),
		// so a client stalling mid-body cannot hang the relay.
		_ = upstream.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
		if err := req.Write(upstream); err != nil {
			_ = upstream.SetWriteDeadline(time.Time{})
			_ = client.SetReadDeadline(time.Time{})
			p.logTimeout("proxy request relay timed out", err, "agent", agentName, "path", req.URL.Path)
			return
		}
		_ = upstream.SetWriteDeadline(time.Time{})
		_ = client.SetReadDeadline(time.Time{})

		_ = upstream.SetReadDeadline(time.Now().Add(httpReadTimeout))
		resp, err := http.ReadResponse(newBufferedReader(upstream), req)
		_ = upstream.SetReadDeadline(time.Time{})
		if err != nil {
			p.logTimeout("proxy upstream response read timed out", err, "agent", agentName, "path", req.URL.Path)
			return
		}

		// Bound the BODY relay too, not just the header read above. The
		// deadline was cleared once headers arrived, so a "reachable but slow"
		// GitHub — TLS and headers complete, body never arrives (the measured
		// #3875 signature: time_total=12s, 0 body bytes) — parked resp.Write
		// in upstream.Read() with no deadline. The client had long since given
		// up (10s collect budget) and FIN'd its side, but this goroutine never
		// noticed: it holds all three sockets of the exchange forever, and the
		// retry loop stacks a fresh parked handler on every attempt (~1k
		// FDs/min measured live). The wrapper re-arms a rolling per-Read
		// deadline, so a body that keeps flowing is never cut while a stall
		// longer than responseBodyStallTimeout errors out and lets the
		// handler's deferred Closes reclaim the sockets.
		resp.Body = &stallBoundedBody{body: resp.Body, conn: upstream, idle: responseBodyStallTimeout}

		_ = client.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
		if err := resp.Write(client); err != nil {
			_ = resp.Body.Close()
			_ = client.SetWriteDeadline(time.Time{})
			p.logTimeout("proxy response relay timed out", err, "agent", agentName, "path", req.URL.Path)
			return
		}
		_ = client.SetWriteDeadline(time.Time{})
		_ = resp.Body.Close()
	}
}

func (p *GitHubProxy) inspectCanaryEgress(agentName string, req *http.Request) (reason string, deny bool, detected bool) {
	if p == nil || !p.canariesEnabled || p.canaryRegistry == nil || req == nil || req.Body == nil {
		return "", false, false
	}
	if isGitReceivePack(req.URL.Path) && p.canaryFailClosed {
		return "ioscan canary scan unavailable for git push", true, true
	}
	if !writeMethods[req.Method] || isGitPath(req.URL.Path) || !githubWriteBodyMayLeak(req.Method, req.URL.Path) {
		return "", false, false
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxGitHubWriteBodyScan+1))
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if err != nil {
		return "", false, false
	}
	if len(body) > maxGitHubWriteBodyScan {
		return "ioscan canary scan body too large", true, true
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	if leak, ok := p.canaryRegistry.Scan(agentName, string(body), "github:"+req.Method+" "+req.URL.Path); ok {
		if p.canaryLeakFunc != nil {
			p.canaryLeakFunc(leak)
		}
		p.logger.Warn("ioscan canary leak detected", "agent", leak.Agent, "source", leak.Source)
		return "ioscan canary leak", p.canaryFailClosed, true
	}
	return "", false, false
}

// enforceLinear applies ACMM to one request on a MITM'd api.linear.app
// connection and returns whether it is blocked, with an agent-facing reason.
//
// The request body is read (bounded), inspected, and restored so an allowed
// request forwards byte-identically.
//
// NOTHING from the body is logged or surfaced in the 403. Linear documents
// carry issue and comment content in the query and, in variables, potentially
// tokens; the operation NAMES are schema identifiers rather than user content,
// so those are what makes a denial debuggable without leaking the payload.
func (p *GitHubProxy) enforceLinear(req *http.Request, agentName string, mode agent.AgentMode, caps agent.AgentCapabilities) (bool, string) {
	// Linear exposes exactly one endpoint. Anything else on this host is not a
	// shape this gate understands, so it is refused rather than forwarded.
	if req.Method != http.MethodPost || !IsLinearGraphQLPath(req.URL.Path) {
		return true, "linear: only POST " + linearGraphQLPath + " is permitted"
	}

	// Read one byte past the limit so hitting the cap is detectable: a body
	// that exactly fills the buffer is indistinguishable from a truncated one
	// otherwise, and truncation must deny.
	body, readErr := io.ReadAll(io.LimitReader(req.Body, linearBodyLimit+1))
	if req.Body != nil {
		_ = req.Body.Close()
	}
	truncated := len(body) > linearBodyLimit
	if truncated {
		body = body[:linearBodyLimit]
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))

	if readErr != nil {
		// Fail closed: a body we could not fully read is a body we could not
		// classify.
		p.logger.Warn("linear: request body read failed — denying", "agent", agentName)
		return true, "linear: request body unreadable"
	}

	decision := LinearGraphQLAllowed(mode, caps, body, truncated)
	if decision.Allowed {
		return false, ""
	}

	// Operations may be empty when the document could not be classified at
	// all, which is itself the useful signal.
	p.logger.Warn("linear: blocked GraphQL request",
		"agent", agentName,
		"mode", mode.String(),
		"operations", strings.Join(decision.Operations, ","),
		"reason", decision.Reason,
		"mutation", decision.IsMutation)

	reason := "linear: " + decision.Reason
	if len(decision.Operations) > 0 {
		reason += " (" + strings.Join(decision.Operations, ",") + ")"
	}
	return true, reason
}

func githubWriteBodyMayLeak(method, path string) bool {
	if method != http.MethodPost && method != http.MethodPatch && method != http.MethodPut {
		return false
	}
	if !strings.HasPrefix(path, "/repos/") {
		return IsGraphQLPath(path)
	}
	return strings.Contains(path, "/issues") ||
		strings.Contains(path, "/pulls") ||
		strings.Contains(path, "/comments") ||
		strings.Contains(path, "/reviews") ||
		strings.Contains(path, "/git/")
}

func isGitReceivePack(path string) bool {
	return strings.HasSuffix(path, "/git-receive-pack")
}

// internalCallerName attributes a connection that originated from the hive's
// OWN control plane rather than from a sandboxed agent. It is deliberately not
// a valid agent name (agents come from the UID map) so it can never collide
// with one, and it is what distinguishes "the hive called GitHub" from the
// genuine failure mode the mode gate warns about: "an agent's UID could not be
// attributed". See UIDMap.IsInternalUID for the security argument.
const internalCallerName = "hive-internal"

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

// responseBodyStallTimeout bounds how long the MITM'd response-BODY relay may
// go without a single byte of progress from the upstream. It is a rolling
// per-Read bound (re-armed on every read by stallBoundedBody), not a cap on
// total transfer time, so a large-but-flowing response is never cut while an
// upstream that goes silent mid-body releases the handler — and with it the
// three sockets of the exchange — within one stall window. A var, not a
// const, so tests can shorten it (same pattern as tunnelHalfCloseDrain).
// Matches httpReadTimeout: the same patience already extended to the header
// phase.
var responseBodyStallTimeout = httpReadTimeout

// stallBoundedBody wraps a proxied response body so every underlying Read is
// preceded by re-arming a read deadline on the upstream conn. Close clears the
// deadline (for the next keep-alive exchange on the same conn) and closes the
// wrapped body. See responseBodyStallTimeout for why this exists (#3875).
type stallBoundedBody struct {
	body io.ReadCloser
	conn net.Conn
	idle time.Duration
}

func (b *stallBoundedBody) Read(p []byte) (int, error) {
	_ = b.conn.SetReadDeadline(time.Now().Add(b.idle))
	return b.body.Read(p)
}

func (b *stallBoundedBody) Close() error {
	// Close the wrapped body while a stall deadline is armed: net/http body
	// Close paths may still read from the conn (drain-to-EOF), and an unarmed
	// read there would reintroduce the very stall this type exists to bound.
	// The deadline is cleared afterwards so the next keep-alive exchange on
	// this conn starts unencumbered.
	_ = b.conn.SetReadDeadline(time.Now().Add(b.idle))
	err := b.body.Close()
	_ = b.conn.SetReadDeadline(time.Time{})
	return err
}

// tunnelHalfCloseDrain bounds how long one direction of an opaque tunnel may
// keep waiting for bytes AFTER the other direction has already finished. It is
// a var, not a const, for the same reason as waitForReadyPollInterval in
// pkg/hub: tests need a relay that unwedges in milliseconds. Production keeps
// the default.
//
// 30s is deliberately generous: the only legitimate traffic still in flight
// once one side has hung up is a response tail already on the wire, which
// arrives in RTTs, not tens of seconds.
var tunnelHalfCloseDrain = 30 * time.Second

// relayTunnel shuttles bytes both ways between the proxied client conn and the
// upstream until BOTH directions have finished, bounding each direction once
// the other has ended.
//
// WHY THE BOUNDS EXIST (measured live, 2026-08-14): the previous inline copies
// waited on each other unboundedly. On spokes whose forge host
// (github.ibm.com) sits behind a SYN-accepting firewall, the upstream TCP
// connect succeeds but no byte ever comes back and no FIN is ever sent. The
// client (the daemon's own JWT mints, agents' CLIs) gives up at its TLS
// handshake timeout and closes — the client→upstream copy finishes and
// half-closes the upstream — but io.Copy(conn, upstream) stayed blocked in
// upstream.Read() FOREVER. Every attempt leaked two sockets (the FIN_WAIT2 /
// CLOSE_WAIT piles), the goroutine, and the splice pipe pair io.Copy holds for
// a TCP↔TCP copy on Linux: ~300k fds and ~11 burned cores per affected spoke.
//
// SetReadDeadline is documented to interrupt Reads that are ALREADY blocked,
// which is exactly what arms the escape hatch here: each side arms the OTHER
// side's deadline only at the moment its own direction completes, so a
// healthy long-lived tunnel (git smart HTTP, device-flow polling) is never
// cut while both directions are still live.
func relayTunnel(conn, upstream net.Conn) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, conn)
		if tc, ok := upstream.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		// Client side is done (EOF or error — a dead client looks the same).
		// A live upstream answers or closes within RTTs; a blackholed one
		// never would, so bound the remaining upstream→client read.
		_ = upstream.SetReadDeadline(time.Now().Add(tunnelHalfCloseDrain))
		close(done)
	}()
	_, _ = io.Copy(conn, upstream)
	// Mirror image: upstream finished (or errored) but the client may sit
	// half-open without ever sending EOF; bound the client-side read so
	// <-done cannot wedge this handler.
	_ = conn.SetReadDeadline(time.Now().Add(tunnelHalfCloseDrain))
	<-done
}

func transfer(dst, src net.Conn) {
	defer func() { _ = dst.Close() }()
	defer func() { _ = src.Close() }()
	_, _ = io.Copy(dst, src)
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
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\nupstream request failed\n")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_ = resp.Write(conn)
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
// readAgentCaps loads the agent's orthogonal capability set (#4492) from
// /tmp/.hive-caps-<agent>. Every failure path returns the zero value — no
// capabilities — so an unreadable, missing or malformed file degrades toward
// the tier check alone, which is the deny direction.
func readAgentCaps(agentName string) agent.AgentCapabilities {
	if agentName == "" {
		return agent.AgentCapabilities{}
	}
	data, err := os.ReadFile(capsFilePrefix + agentName)
	if err != nil {
		return agent.AgentCapabilities{}
	}
	return agent.ParseCapabilities(strings.TrimSpace(string(data)))
}

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
	// The parent directory is created with owner-only permissions before the
	// key is written. /data itself is intentionally group-writable for agent
	// workspaces, so 0600 on a file directly under /data is not sufficient:
	// an agent could replace or race the path before the proxy opens it.
	if err := os.MkdirAll(filepath.Dir(caKeyPath), 0700); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create CA key directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(caKeyPath), 0700); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("restrict CA key directory: %w", err)
	}
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

	// PROVIDER SPEND REBUFF (#4294). Checked before the 403/entitlement branch
	// below because that branch returns early for every non-403, which would
	// skip the 429/400 statuses a budget refusal actually arrives on.
	//
	// Requires the body to name spend, so an ordinary 429 throttle keeps
	// flowing to the existing transient-retry path untouched.
	if p.inferenceBudget != nil && isInferenceBudgetRebuff(status, body) {
		msg := inferenceBudgetMessage(route.Backend, status, truncateBytes(body, 200))
		p.inferenceBudget.recordRebuff(msg, time.Now())
		p.logger.Error("inference backend refused on a spending limit",
			"agent", agentName, "backend", route.Backend, "endpoint", route.Endpoint,
			"status", status, "cause", msg)
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

// routeWithEntitledModel returns the route to actually forward with: when the
// gateway's entitled model set is known (key-info probe or a prior team-scope
// 403) and the route's stored model differs from an entitled id only by
// provider-prefix / dot-hyphen spelling drift, a COPY of the route carrying
// the exact entitled id is returned. LiteLLM matches the request model string
// verbatim, so without this a stored copilot-spelled id (e.g.
// "claude-sonnet-4.6" vs entitled "aws/claude-sonnet-4-6") 403s on every call
// forever (#4400) — while the dashboard's tolerant preselect matcher shows the
// agent on a perfectly valid model. Self-healing: the first 403 teaches the
// entitled set (recordInferenceError), so the next request resolves.
//
// The stored route is never mutated: the rewrite is per-request, so a
// key/team change that re-entitles the verbatim id takes effect immediately,
// and concurrent requests race on nothing. Unknown entitlements, verbatim
// matches, and ambiguous candidates all pass the route through unchanged.
func (p *GitHubProxy) routeWithEntitledModel(route *InferenceRoute, agentName string) *InferenceRoute {
	if route == nil || route.Backend != "litellm" || p.entitlements == nil {
		return route
	}
	entitled, _, known := p.EntitledModels(route.Endpoint)
	if !known {
		return route
	}
	resolved, ok := resolveEntitledModelID(entitled, route.Model)
	if !ok {
		return route
	}
	p.logger.Info("inference model resolved to entitled gateway id",
		"agent", agentName, "endpoint", route.Endpoint,
		"configured", route.Model, "resolved", resolved)
	rewritten := *route
	rewritten.Model = resolved
	return &rewritten
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
	// Self-heal for the spend rebuff (#4294): whenever the provider's window
	// resets — hive neither knows nor assumes the schedule; the field report's
	// gateway rolled over at the key's creation time of day, not at midnight
	// (see #4294) — the first call that succeeds takes the hive out of the
	// suppressed state with no operator action.
	if p.inferenceBudget != nil {
		p.inferenceBudget.recordSuccess()
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

// InferenceBudgetExceeded reports the current provider spending-limit signal:
// a non-empty log-safe cause, when it first latched, when the most recent
// rebuff arrived, and how many rebuffs have been observed since — all
// zero-valued while the provider is serving normally.
//
// Callers use a non-empty cause as "this hive cannot buy inference right now",
// which is a reason to stop kicking agents and to tell the operator, NOT a
// reason to retry in a few minutes. lastRebuff is what bounds that: because
// withholding kicks also withholds the calls that would clear the latch,
// callers suppress only while lastRebuff is fresh and send a probe once it is
// stale (see inference_budget.go). Safe on a nil receiver/tracker.
func (p *GitHubProxy) InferenceBudgetExceeded() (errMsg string, since, lastRebuff time.Time, rebuffs int) {
	if p == nil || p.inferenceBudget == nil {
		return "", time.Time{}, time.Time{}, 0
	}
	return p.inferenceBudget.snapshot()
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
func (p *GitHubProxy) inferenceTranslatorHandler() http.Handler {
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
		route = p.routeWithEntitledModel(route, agentName)

		body, err := io.ReadAll(r.Body)
		if r.Body != nil {
			_ = r.Body.Close()
		}
		if err != nil {
			http.Error(w, `{"type":"error","error":{"type":"api_error","message":"failed to read request"}}`, http.StatusBadRequest)
			return
		}

		// Same path gate as the MITM reroute: only POST /v1/messages is
		// translated and forwarded; CLI housekeeping is answered here.
		if kind := classifyInferencePath(r.Method, r.URL.Path); kind != inferencePathMessages {
			status, respBody := p.localInferenceResponse(kind, r.Method, r.URL.Path, body, agentName)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, respBody)
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
		defer func() { _ = resp.Body.Close() }()

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
			_, _ = w.Write([]byte(anthropicErr))
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
		_, _ = w.Write(translated)
	})

	return mux
}

// StartInferenceTranslator runs the Anthropic-compatible inference translator
// on the fixed local port used by agents.
func (p *GitHubProxy) StartInferenceTranslator() error {
	addr := fmt.Sprintf("127.0.0.1:%d", InferenceTranslatePort)
	p.logger.Info("inference translation server starting", "addr", addr)
	server := &http.Server{Addr: addr, Handler: p.inferenceTranslatorHandler()}
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
	_ = conn.SetDeadline(time.Now().Add(clientTLSHandshakeTimeout))
	handshakeErr := tlsConn.Handshake()
	_ = conn.SetDeadline(time.Time{})
	if handshakeErr != nil {
		p.logger.Warn("inference reroute: TLS handshake failed", "error", handshakeErr)
		return
	}
	defer func() { _ = tlsConn.Close() }()

	clientBuf := bufio.NewReader(tlsConn)
	for {
		_ = tlsConn.SetReadDeadline(time.Now().Add(httpReadTimeout))
		req, err := http.ReadRequest(clientBuf)
		_ = tlsConn.SetReadDeadline(time.Time{})
		if err != nil {
			p.logTimeout("inference reroute: client request read timed out", err, "agent", agentName)
			return
		}

		p.handleInferenceRequest(tlsConn, req, agentName, route)
	}
}

// handleInferenceRequest translates a single Anthropic API request and
// forwards it to the inference backend.
func (p *GitHubProxy) handleInferenceRequest(conn net.Conn, req *http.Request, agentName string, route *InferenceRoute) {
	route = p.routeWithEntitledModel(route, agentName)
	_ = conn.SetReadDeadline(time.Now().Add(httpReadTimeout))
	body, err := io.ReadAll(req.Body)
	_ = conn.SetReadDeadline(time.Time{})
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if err != nil {
		p.logTimeout("inference reroute: request body read timed out", err, "agent", agentName)
		p.writeHTTPError(conn, http.StatusBadGateway, "failed to read request body")
		return
	}

	// Only POST /v1/messages is an inference call. The Claude Code CLI also
	// sends telemetry batches, error reports, and count_tokens to this host;
	// translating those bodies produced {"messages": null} and a gateway 400
	// per call, charged against the provider's request rate limit.
	if kind := classifyInferencePath(req.Method, req.URL.Path); kind != inferencePathMessages {
		status, respBody := p.localInferenceResponse(kind, req.Method, req.URL.Path, body, agentName)
		writeLocalInferenceResponse(conn, status, respBody)
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
	defer func() { _ = resp.Body.Close() }()

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
		_ = anthropicErr.Write(conn)
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
	_ = httpResp.Write(conn)
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
		p.logger.Warn("copilot sniff: client TLS handshake failed", "error", handshakeErr)
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
			p.logTimeout("copilot sniff: client request read timed out", err, "agent", agentName, "host", host)
			return
		}

		isCompletion := isCopilotCompletionsPath(req.URL.Path)
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
			if streaming {
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

		if isCompletion && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.forwardCopilotResponseWithUsage(client, resp, host, agentName, model)
		} else {
			_ = client.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
			if err := resp.Write(client); err != nil {
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
	_ = client.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
	if err := resp.Write(client); err != nil {
		p.logger.Warn("copilot sniff: response write to client failed", "agent", agentName, "error", err)
	}
	_ = client.SetWriteDeadline(time.Time{})
}

func (p *GitHubProxy) writeHTTPError(conn net.Conn, status int, msg string) {
	resp := &http.Response{
		StatusCode: status,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":"%s"}}`, msg))),
	}
	_ = resp.Write(conn)
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
