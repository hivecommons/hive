package proxy

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Sandbox GitHub read relay (#18865).
//
// Sandbox Jobs run in their own pods, so they cannot reach the loopback proxy
// the hive's own agents use, and an anonymous Job is capped at GitHub's 60
// requests/hour per source IP, shared by every agent on the host. This relay
// is a second, deliberately narrow listener that a Job points HTTPS_PROXY at.
//
// The invariants, each pinned by a test:
//
//   - READ ONLY. A GitHub credential is attached only to GET/HEAD requests to
//     the read hosts, plus the one POST git itself needs to fetch
//     (git-upload-pack). Every other method, GraphQL included, is refused with
//     a 403 and never reaches GitHub.
//   - NO INJECTION OFF GITHUB. Any other host is an opaque, destination-guarded
//     tunnel: no TLS interception, no header touched, no credential attached.
//   - THE CREDENTIAL STAYS HERE. The Job holds only a per-kick relay key, which
//     authenticates it to this listener and is worth nothing to GitHub. The
//     GitHub token is fetched, attached and discarded in this process; an
//     Authorization header the Job sends is always stripped first.
//   - A READ IDENTITY. The token source is supplied by the host and is expected
//     to be the read-only, repo-scoped tier, never the App write credential.

// SandboxReadProxyUser is the Proxy-Authorization user a Job presents; the
// password is the per-kick relay key.
const SandboxReadProxyUser = "hive-sandbox"

const (
	sandboxReadKeyBytes = 32
	// sandboxReadTokenTTL is how long a fetched read token is reused. GitHub
	// installation tokens live an hour; refreshing at 45 minutes keeps a
	// margin for a request already in flight.
	sandboxReadTokenTTL = 45 * time.Minute
	// sandboxReadTokenRetryAfter is how long a failed mint is remembered, so a
	// token-endpoint outage costs one attempt per interval rather than one
	// (up to 20s, serialized) attempt per request.
	sandboxReadTokenRetryAfter  = 30 * time.Second
	sandboxReadHTTPSPort        = "443"
	sandboxReadRefsSuffix       = "/info/refs"
	sandboxReadUploadPackSuffix = "/git-upload-pack"
	sandboxReadUploadPackName   = "git-upload-pack"
)

// sandboxReadHosts are the hosts whose traffic the relay intercepts. Fixed on
// purpose: not derived from the registered GitHub-family hosts, so enterprise
// hosts added elsewhere never gain relay credential injection.
var sandboxReadHosts = map[string]bool{
	"api.github.com":            true,
	"codeload.github.com":       true,
	"raw.githubusercontent.com": true,
	"github.com":                true,
}

// SandboxReadHost reports whether host is one the relay terminates TLS for.
func SandboxReadHost(host string) bool { return sandboxReadHosts[canonicalHost(host)] }

// SandboxReadDecision is the relay's verdict on one intercepted request.
type SandboxReadDecision struct {
	// Allowed is false for anything the relay refuses to forward.
	Allowed bool
	// Inject is true when a read credential may be attached. A request can be
	// allowed without it (a github.com page GET), and then goes out with no
	// Authorization at all.
	Inject bool
	// Basic selects git smart-HTTP Basic auth over the "token" scheme.
	Basic  bool
	Reason string
}

// SandboxReadPolicy classifies one request to an intercepted host. It is a
// pure function so the read-only invariant is testable without a network.
func SandboxReadPolicy(host, method, path, rawQuery string) SandboxReadDecision {
	host = canonicalHost(host)
	if !sandboxReadHosts[host] {
		return SandboxReadDecision{Reason: "host is not a GitHub read host"}
	}
	method = strings.ToUpper(method)
	isRead := method == http.MethodGet || method == http.MethodHead

	if host == "github.com" {
		switch {
		case isRead && strings.HasSuffix(path, sandboxReadRefsSuffix):
			// Smart-HTTP ref discovery. service=git-receive-pack is push
			// discovery and is refused; no service is a dumb-protocol read.
			service := ""
			if q, err := url.ParseQuery(rawQuery); err == nil {
				service = q.Get("service")
			}
			if service != "" && service != sandboxReadUploadPackName {
				return SandboxReadDecision{Reason: "only git fetch (git-upload-pack) is allowed through the sandbox read relay"}
			}
			return SandboxReadDecision{Allowed: true, Inject: true, Basic: true}
		case method == http.MethodPost && strings.HasSuffix(path, sandboxReadUploadPackSuffix):
			return SandboxReadDecision{Allowed: true, Inject: true, Basic: true}
		case isRead && !strings.HasSuffix(path, "/git-receive-pack"):
			// Archives, release downloads and the like redirect to the
			// download hosts. No credential is needed or attached here.
			return SandboxReadDecision{Allowed: true}
		}
		return SandboxReadDecision{Reason: "the sandbox read relay forwards reads only; writes go through hive's relays"}
	}
	if !isRead {
		return SandboxReadDecision{Reason: "the sandbox read relay forwards GET and HEAD only; writes go through hive's relays"}
	}
	return SandboxReadDecision{Allowed: true, Inject: true}
}

// SandboxReadTokenFunc returns the read-only GitHub credential the relay
// attaches. It must be a read identity (the shared-uid-read tier), never the
// write credential.
type SandboxReadTokenFunc func() (string, error)

// SandboxReadRelayOptions configure NewSandboxReadRelay.
type SandboxReadRelayOptions struct {
	// Endpoint is the http://host:port Jobs use to reach the relay's listener.
	Endpoint string
	// Token supplies the read credential. Nil sends reads unauthenticated.
	Token SandboxReadTokenFunc
	// RootsPEM is the trust bundle handed to a Job alongside the hive CA, so
	// the Job still trusts the real internet. Empty means the hive CA alone.
	RootsPEM []byte
}

// SandboxReadRelay is the Job-facing read relay.
type SandboxReadRelay struct {
	p        *GitHubProxy
	opts     SandboxReadRelayOptions
	endpoint *url.URL

	keysMu sync.RWMutex
	keys   map[[sha256.Size]byte]*sandboxReadKick // sha256(key) -> kick

	tokMu      sync.Mutex
	tok        string
	tokFetched time.Time
	tokFailed  time.Time // last failed mint; zero when the last mint succeeded
	now        func() time.Time
	// dial overrides the upstream TLS dial (tests).
	dial func(host string) (net.Conn, error)
}

// sandboxReadKick is one enrolled kick. released is closed when its key is
// revoked, so connections authenticated at CONNECT stop serving (#18895).
type sandboxReadKick struct {
	agent    string
	released chan struct{}
	once     sync.Once
}

func (k *sandboxReadKick) release() { k.once.Do(func() { close(k.released) }) }

// live reports whether the kick's key has not been released.
func (k *sandboxReadKick) live() bool {
	select {
	case <-k.released:
		return false
	default:
		return true
	}
}

// NewSandboxReadRelay builds the relay on p (which supplies the CA, the
// destination-guarded dialers and the upstream chain).
func NewSandboxReadRelay(p *GitHubProxy, opts SandboxReadRelayOptions) (*SandboxReadRelay, error) {
	if p == nil {
		return nil, errors.New("sandbox read relay: no proxy")
	}
	u, err := url.Parse(strings.TrimSpace(opts.Endpoint))
	if err != nil || u.Hostname() == "" || u.Scheme != "https" || u.User != nil {
		return nil, errors.New("sandbox read relay: endpoint must be an https://host:port URL without credentials")
	}
	return &SandboxReadRelay{p: p, opts: opts, endpoint: u, keys: map[[sha256.Size]byte]*sandboxReadKick{}, now: time.Now}, nil
}

// TLSConfig is the server configuration for the relay's listener. The relay
// is only ever served over TLS: the per-kick key travels in Proxy-Authorization,
// so a plain-HTTP connection is never accepted. The certificate is issued by
// the hive proxy CA (the same CA and forging the GitHub interception uses, no
// new CA and no new key material on disk) for the endpoint's host, as a DNS or
// IP SAN. Jobs already trust that CA through the bundle Enroll returns.
func (r *SandboxReadRelay) TLSConfig() *tls.Config {
	host := canonicalHost(r.endpoint.Hostname())
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := r.p.forgeCert(host)
			if err != nil {
				return nil, fmt.Errorf("sandbox read relay: issuing listener certificate for %q: %w", host, err)
			}
			return &cert, nil
		},
	}
}

// Enroll admits one kick: it mints a per-kick relay key and returns the proxy
// URL the Job should use, the CA bundle it must trust and a release func that
// revokes the key. The key authenticates the Job to this listener only.
func (r *SandboxReadRelay) Enroll(agentName string) (proxyURL string, caBundlePEM []byte, release func(), err error) {
	raw := make([]byte, sandboxReadKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, nil, fmt.Errorf("sandbox read relay: minting kick key: %w", err)
	}
	key := hex.EncodeToString(raw)
	digest := sha256.Sum256([]byte(key))
	kick := &sandboxReadKick{agent: agentName, released: make(chan struct{})}
	r.keysMu.Lock()
	r.keys[digest] = kick
	r.keysMu.Unlock()

	u := *r.endpoint
	u.User = url.UserPassword(SandboxReadProxyUser, key)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.p.caX509.Raw})
	bundle := append(append([]byte{}, r.opts.RootsPEM...), ca...)
	return u.String(), bundle, func() {
		r.keysMu.Lock()
		delete(r.keys, digest)
		r.keysMu.Unlock()
		kick.release()
	}, nil
}

// authenticate returns the kick a Proxy-Authorization header belongs to.
func (r *SandboxReadRelay) authenticate(req *http.Request) (*sandboxReadKick, bool) {
	auth := req.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(auth, "Basic ") {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		return nil, false
	}
	user, key, ok := strings.Cut(string(decoded), ":")
	if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(SandboxReadProxyUser)) != 1 {
		return nil, false
	}
	digest := sha256.Sum256([]byte(key))
	r.keysMu.RLock()
	kick, found := r.keys[digest]
	r.keysMu.RUnlock()
	return kick, found
}

// readToken returns the cached read credential, fetching a fresh one when it
// is older than sandboxReadTokenTTL. An error or empty result means "send the
// request unauthenticated": the relay never substitutes another credential.
func (r *SandboxReadRelay) readToken() string {
	if r.opts.Token == nil {
		return ""
	}
	r.tokMu.Lock()
	defer r.tokMu.Unlock()
	if r.tok != "" && r.now().Sub(r.tokFetched) < sandboxReadTokenTTL {
		return r.tok
	}
	if r.tok == "" && !r.tokFailed.IsZero() && r.now().Sub(r.tokFailed) < sandboxReadTokenRetryAfter {
		return ""
	}
	tok, err := r.opts.Token()
	if err != nil || tok == "" {
		reason := "empty token"
		if err != nil {
			reason = err.Error()
		}
		r.p.logger.Warn("sandbox read relay: no read credential available; forwarding reads unauthenticated", "reason", reason)
		r.tok, r.tokFailed = "", r.now()
		return ""
	}
	r.tok, r.tokFetched, r.tokFailed = tok, r.now(), time.Time{}
	return tok
}

// applySandboxReadAuth rewrites a request's Authorization per decision d. Any
// credential the Job supplied is removed unconditionally; the read token is
// attached only when d allows injection.
func applySandboxReadAuth(req *http.Request, d SandboxReadDecision, token string) {
	req.Header.Del("Authorization")
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Expect")
	if !d.Allowed || !d.Inject || token == "" {
		return
	}
	if d.Basic {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(gitInjectBasicUser+":"+token)))
		return
	}
	req.Header.Set("Authorization", "token "+token)
}

// Serve accepts Job connections on ln until it is closed.
func (r *SandboxReadRelay) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("sandbox read relay accept: %w", err)
		}
		go r.handleConn(conn)
	}
}

func (r *SandboxReadRelay) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(httpReadTimeout))
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}
	kick, ok := r.authenticate(req)
	if !ok {
		writeRelayStatus(conn, http.StatusProxyAuthRequired, "proxy authentication required", "Proxy-Authenticate: Basic realm=\"hive-sandbox\"\r\n")
		return
	}
	if req.Method != http.MethodConnect {
		// Plain-HTTP proxying is not offered: every GitHub read host is HTTPS.
		writeRelayStatus(conn, http.StatusForbidden, "the sandbox read relay carries HTTPS CONNECT only", "")
		return
	}
	host, port, err := net.SplitHostPort(req.Host)
	if err != nil {
		host, port = req.Host, sandboxReadHTTPSPort
	}
	if port != sandboxReadHTTPSPort {
		writeRelayStatus(conn, http.StatusForbidden, "the sandbox read relay carries port 443 only", "")
		return
	}
	if !SandboxReadHost(host) {
		r.tunnel(conn, host, kick)
		return
	}
	r.intercept(conn, host, kick)
}

// tunnel is the opaque path for every non-GitHub host: no interception, no
// header touched, no credential. The destination guard keeps it from reaching
// loopback, link-local metadata or the private estate.
// The tunnel is torn down when the kick's key is released.
func (r *SandboxReadRelay) tunnel(conn net.Conn, host string, kick *sandboxReadKick) {
	agentName := kick.agent
	const dialTimeout = 10 * time.Second
	upstream, err := r.p.dialUpstreamRaw(net.JoinHostPort(host, sandboxReadHTTPSPort), dialTimeout)
	if err != nil {
		r.p.logger.Debug("sandbox read relay: tunnel refused", "agent", agentName, "host", host, "error", err)
		writeRelayStatus(conn, http.StatusBadGateway, "connection failed", "")
		return
	}
	defer func() { _ = upstream.Close() }()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-kick.released:
			r.p.logger.Info("sandbox read relay: closing tunnel, key released", "agent", agentName, "host", host)
			_ = conn.Close()
			_ = upstream.Close()
		case <-finished:
		}
	}()
	_, _ = fmt.Fprint(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	relayTunnel(conn, upstream)
}

// intercept terminates TLS for a GitHub read host and relays each request
// through SandboxReadPolicy. The kick's key is re-checked before each
// request, so a tunnel opened while the key was live stops serving once the
// key is released.
func (r *SandboxReadRelay) intercept(conn net.Conn, host string, kick *sandboxReadKick) {
	agentName := kick.agent
	cert, err := r.p.forgeCert(host)
	if err != nil {
		r.p.logger.Error("sandbox read relay: forge cert failed", "host", host, "error", err)
		writeRelayStatus(conn, http.StatusBadGateway, "certificate error", "")
		return
	}
	if _, err := fmt.Fprint(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	client := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
	_ = conn.SetDeadline(time.Now().Add(clientTLSHandshakeTimeout))
	err = client.Handshake()
	_ = conn.SetDeadline(time.Time{})
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()

	// The upstream leg is dialed on the first ALLOWED request, so a refused
	// request never opens a connection to GitHub.
	var upstream net.Conn
	var upstreamBuf *bufio.Reader
	defer func() {
		if upstream != nil {
			_ = upstream.Close()
		}
	}()

	clientBuf := bufio.NewReader(client)
	for {
		_ = client.SetReadDeadline(time.Now().Add(httpReadTimeout))
		req, err := http.ReadRequest(clientBuf)
		if err != nil {
			return
		}
		if !kick.live() {
			r.p.logger.Info("sandbox read relay: request refused, key released", "agent", agentName, "host", host, "method", req.Method)
			writeRelayRefusal(client, "the per-kick relay key has been released")
			return
		}
		d := SandboxReadPolicy(host, req.Method, req.URL.Path, req.URL.RawQuery)
		if d.Allowed && !sandboxReadRequestTargetsHost(req, host) {
			// The policy and the credential decision were made for the CONNECT
			// host; a request naming a different Host or an absolute URL for
			// another host must not ride that decision.
			d = SandboxReadDecision{Reason: "the request names a different host than the CONNECT target"}
		}
		if !d.Allowed {
			r.p.logger.Info("sandbox read relay: request refused", "agent", agentName, "host", host, "method", req.Method)
			writeRelayRefusal(client, d.Reason)
			return
		}
		if upstream == nil {
			up, derr := r.dialUpstream(host)
			if derr != nil {
				r.p.logger.Warn("sandbox read relay: upstream dial failed", "host", host, "error", derr)
				return
			}
			upstream, upstreamBuf = up, bufio.NewReader(up)
		}
		token := ""
		if d.Inject {
			token = r.readToken()
		}
		applySandboxReadAuth(req, d, token)
		// Log the decision, never the credential.
		r.p.logger.Debug("sandbox read relay: request", "agent", agentName, "host", host, "method", req.Method, "injected", token != "")

		_ = upstream.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
		if err := req.Write(upstream); err != nil {
			return
		}
		_ = upstream.SetWriteDeadline(time.Time{})
		_ = client.SetReadDeadline(time.Time{})
		_ = upstream.SetReadDeadline(time.Now().Add(httpReadTimeout))
		resp, err := http.ReadResponse(upstreamBuf, req)
		_ = upstream.SetReadDeadline(time.Time{})
		if err != nil {
			return
		}
		resp.Body = &stallBoundedBody{body: resp.Body, conn: upstream, idle: r.p.bodyStallTimeoutDuration()}
		werr := resp.Write(&stallBoundedWriter{conn: client, idle: httpWriteTimeout})
		_ = resp.Body.Close()
		_ = client.SetWriteDeadline(time.Time{})
		if werr != nil || resp.Close || req.Close {
			return
		}
	}
}

// sandboxReadRequestTargetsHost reports whether the Host header and any
// absolute request URL name the host the tunnel was opened for.
func sandboxReadRequestTargetsHost(req *http.Request, host string) bool {
	want := canonicalHost(host)
	for _, got := range []string{req.Host, req.URL.Host} {
		if got == "" {
			continue
		}
		if h, _, err := net.SplitHostPort(got); err == nil {
			got = h
		}
		if canonicalHost(got) != want {
			return false
		}
	}
	return true
}

// dialUpstream opens the TLS leg to a read host. It is a method so
// tests can drive the relay without network access by replacing r.dial.
func (r *SandboxReadRelay) dialUpstream(host string) (net.Conn, error) {
	if r.dial != nil {
		return r.dial(host)
	}
	return r.p.dialUpstreamTLS(net.JoinHostPort(host, sandboxReadHTTPSPort), host, upstreamDialTimeout)
}

func writeRelayStatus(conn net.Conn, status int, msg, extraHeaders string) {
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n%sContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, http.StatusText(status), extraHeaders, len(msg), msg)
}

func writeRelayRefusal(w net.Conn, reason string) {
	body, _ := json.Marshal(map[string]string{"message": "blocked by hive: " + reason})
	_, _ = fmt.Fprintf(w, "HTTP/1.1 403 Forbidden\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}
