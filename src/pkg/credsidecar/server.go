package credsidecar

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Server is the sidecar's HTTP handler: verify, attach, forward.
type Server struct {
	key          []byte
	maxBodyBytes int64
	allowedHosts map[string]bool
	tokens       *tokenCache
	nonces       *nonceCache
	upstream     *http.Client
	logger       *slog.Logger
	now          func() time.Time
	// upstreamScheme is "https" in production; tests point the forwarder at a
	// plain httptest server through it.
	upstreamScheme string
}

// Upstream timeouts for the sidecar's own leg to GitHub. The dial bound covers
// connect plus TLS; the header bound is how long GitHub may take to START
// answering (git upload-pack negotiates before it streams). The body itself is
// not time-boxed, so a long clone keeps flowing.
const (
	upstreamDialTimeout           = 15 * time.Second
	upstreamTLSHandshakeTimeout   = 15 * time.Second
	upstreamResponseHeaderTimeout = 60 * time.Second
	upstreamIdleConnTimeout       = 90 * time.Second
	upstreamMaxIdleConnsPerHost   = 16
)

// gitBasicUser is the username git expects alongside an App installation token
// in HTTP Basic auth - the same shape git-credential-hive.sh and the in-process
// injection (pkg/proxy gitInjectBasicUser) produce.
const gitBasicUser = "x-access-token"

// loginPathPrefix marks GitHub's OAuth/device-flow endpoints. They
// authenticate the flow by their form body; a token is never attached there.
const loginPathPrefix = "/login/"

// hopByHopHeaders are connection-scoped and never forwarded in either
// direction (RFC 9110 section 7.6.1), plus the proxy credential headers.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// ServerOption customizes a Server.
type ServerOption func(*Server)

// WithDialContext sets the dialer the sidecar uses for its GitHub leg. The
// hive wires the proxy's egress-mark dialer here, so the sidecar's own :443
// dials are exempt from the pod's forced-egress redirect on OpenShift (no
// xt_owner) as well as OKE.
func WithDialContext(dial func(ctx context.Context, network, addr string) (net.Conn, error)) ServerOption {
	return func(s *Server) {
		if t, ok := s.upstream.Transport.(*http.Transport); ok && dial != nil {
			t.DialContext = dial
		}
	}
}

// withClock and withUpstream are test seams.
func withClock(now func() time.Time) ServerOption {
	return func(s *Server) { s.now = now; s.tokens.now = now }
}

func withUpstream(client *http.Client, scheme string) ServerOption {
	return func(s *Server) { s.upstream = client; s.upstreamScheme = scheme }
}

// NewServer builds the sidecar handler.
func NewServer(cfg ServerConfig, minter Minter, logger *slog.Logger, opts ...ServerOption) (*Server, error) {
	if len(cfg.Key) < MinKeyBytes {
		return nil, ErrKeyTooShort
	}
	if minter == nil {
		return nil, errors.New("credential sidecar needs a token minter")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	hosts := cfg.AllowedHosts
	if len(hosts) == 0 {
		hosts = allowedHosts("")
	}
	s := &Server{
		key:            cfg.Key,
		maxBodyBytes:   maxBody,
		allowedHosts:   hosts,
		tokens:         newTokenCache(minter, time.Now),
		nonces:         newNonceCache(defaultMaxNonceEntries),
		upstream:       newUpstreamClient(),
		logger:         logger,
		now:            time.Now,
		upstreamScheme: "https",
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// newUpstreamClient is the sidecar's GitHub client. Proxy is explicitly nil:
// a token-bearing request must go straight to GitHub, never through whatever
// HTTPS_PROXY the environment names. Redirects are returned, not followed, so
// a token is never re-sent anywhere the original request did not name.
// Compression is left to the caller so bodies pass through byte-identical.
func newUpstreamClient() *http.Client {
	dialer := &net.Dialer{Timeout: upstreamDialTimeout}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   upstreamTLSHandshakeTimeout,
			ResponseHeaderTimeout: upstreamResponseHeaderTimeout,
			IdleConnTimeout:       upstreamIdleConnTimeout,
			MaxIdleConnsPerHost:   upstreamMaxIdleConnsPerHost,
			DisableCompression:    true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ServeHTTP verifies one signed request and, when it passes, forwards it to
// GitHub with the tier's token attached. Refusals never echo request content
// and never mention token material.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fields, sig, err := s.readSignedFields(r)
	if err != nil {
		s.refuse(w, http.StatusUnauthorized, err, fields)
		return
	}
	if err := checkTimestamp(fields.Timestamp, s.now()); err != nil {
		s.refuse(w, http.StatusUnauthorized, err, fields)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.maxBodyBytes+1))
	if err != nil {
		s.refuse(w, http.StatusBadRequest, fmt.Errorf("%w: reading body: %v", ErrMalformed, err), fields)
		return
	}
	if int64(len(body)) > s.maxBodyBytes {
		s.refuse(w, http.StatusRequestEntityTooLarge, ErrBodyTooLarge, fields)
		return
	}
	if BodyDigest(body) != fields.BodySHA256 {
		s.refuse(w, http.StatusUnauthorized, ErrBodyDigest, fields)
		return
	}
	if err := verifySignature(s.key, fields, sig); err != nil {
		s.refuse(w, http.StatusUnauthorized, err, fields)
		return
	}
	// Only a correctly signed request reaches the replay cache, so an unsigned
	// caller cannot fill it.
	if err := s.nonces.checkAndStore(fields.Nonce, s.now()); err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrNonceCacheFull) {
			status = http.StatusServiceUnavailable
		}
		s.refuse(w, status, err, fields)
		return
	}
	if !s.allowedHosts[strings.ToLower(fields.Host)] {
		s.refuse(w, http.StatusForbidden, ErrHostNotAllowed, fields)
		return
	}

	out, err := s.buildUpstreamRequest(r, fields, body)
	if err != nil {
		s.refuse(w, http.StatusBadGateway, err, fields)
		return
	}
	resp, err := s.upstream.Do(out)
	if err != nil {
		s.refuse(w, http.StatusBadGateway, fmt.Errorf("forwarding to %s: %w", fields.Host, err), fields)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	stripHopByHop(w.Header())
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(flushWriter{w}, resp.Body); err != nil {
		s.logger.Warn("credential sidecar: response relay ended early", "agent", fields.Agent, "host", fields.Host, "error", err)
	}
}

// readSignedFields pulls the protocol headers off r. A request with no
// signature at all is ErrUnsigned; a present-but-unparseable field is
// ErrMalformed.
func (s *Server) readSignedFields(r *http.Request) (SignedFields, string, error) {
	f := SignedFields{
		Method:     r.Method,
		Host:       r.Header.Get(HeaderHost),
		RequestURI: r.RequestURI,
		Agent:      r.Header.Get(HeaderAgent),
		Tier:       r.Header.Get(HeaderTier),
		Nonce:      r.Header.Get(HeaderNonce),
		BodySHA256: r.Header.Get(HeaderBodySHA256),
	}
	sig := r.Header.Get(HeaderSignature)
	tsRaw := r.Header.Get(HeaderTimestamp)
	if sig == "" {
		return f, "", ErrUnsigned
	}
	if f.Host == "" || tsRaw == "" || f.BodySHA256 == "" || f.RequestURI == "" || !strings.HasPrefix(f.RequestURI, "/") {
		return f, "", ErrMalformed
	}
	if n := len(f.Nonce); n < minNonceLen || n > maxNonceLen {
		return f, "", ErrMalformed
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return f, "", ErrMalformed
	}
	f.Timestamp = ts
	return f, sig, nil
}

// buildUpstreamRequest is the request that leaves for GitHub: the caller's
// headers minus every credential, hop-by-hop and protocol header, plus the
// token for the signed tier.
func (s *Server) buildUpstreamRequest(r *http.Request, f SignedFields, body []byte) (*http.Request, error) {
	target, err := url.Parse(s.upstreamScheme + "://" + f.Host + f.RequestURI)
	if err != nil {
		return nil, fmt.Errorf("%w: request URI: %v", ErrMalformed, err)
	}
	var reader io.Reader = http.NoBody
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(r.Context(), f.Method, target.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	out.ContentLength = int64(len(body))
	for k, vv := range r.Header {
		for _, v := range vv {
			out.Header.Add(k, v)
		}
	}
	stripHopByHop(out.Header)
	stripProtocolHeaders(out.Header)
	// The sidecar, never the caller, decides the credential.
	out.Header.Del("Authorization")
	out.Host = f.Host

	if f.Tier == "" || strings.HasPrefix(target.Path, loginPathPrefix) {
		return out, nil
	}
	tok, err := s.tokens.token(r.Context(), f.Tier)
	if err != nil {
		return nil, err
	}
	if isGitPath(target.Path) {
		out.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(gitBasicUser+":"+tok)))
	} else {
		out.Header.Set("Authorization", "token "+tok)
	}
	return out, nil
}

// refuse answers a request the sidecar will not forward. The reason goes in
// HeaderRefused and the body; the log line carries the agent and host (both
// already known to the proxy) and never the signature, nonce or body.
func (s *Server) refuse(w http.ResponseWriter, status int, reason error, f SignedFields) {
	s.logger.Warn("credential sidecar refused a request", "status", status, "reason", reason.Error(), "agent", f.Agent, "host", f.Host, "method", f.Method)
	w.Header().Set(HeaderRefused, reason.Error())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "hive credential sidecar refused the request: %s\n", reason.Error())
}

// ListenAndServe serves on the configured loopback address until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("credential sidecar listen %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

// sidecarReadHeaderTimeout bounds how long a connection may take to send its
// request headers, so an idle connection from anything sharing the pod network
// cannot pin a server goroutine.
const sidecarReadHeaderTimeout = 10 * time.Second

// sidecarShutdownTimeout bounds the graceful drain on shutdown.
const sidecarShutdownTimeout = 10 * time.Second

// Serve serves on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: sidecarReadHeaderTimeout}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	s.logger.Info("credential sidecar listening", "addr", ln.Addr().String(), "hosts", len(s.allowedHosts))
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), sidecarShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	}
}

// isGitPath mirrors pkg/proxy isGitPath: git smart-HTTP endpoints, which
// authenticate with Basic x-access-token:<token>.
func isGitPath(path string) bool {
	return strings.HasSuffix(path, "/git-receive-pack") ||
		strings.HasSuffix(path, "/git-upload-pack") ||
		strings.HasSuffix(path, "/info/refs")
}

// stripHopByHop removes connection-scoped headers, including any named in
// Connection.
func stripHopByHop(h http.Header) {
	for _, c := range h.Values("Connection") {
		for _, name := range strings.Split(c, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// stripProtocolHeaders removes every signing-protocol header.
func stripProtocolHeaders(h http.Header) {
	for name := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), http.CanonicalHeaderKey(HeaderPrefix)) {
			h.Del(name)
		}
	}
}

// flushWriter flushes after every write so a streamed response (git clone)
// reaches the proxy as it arrives rather than when a buffer fills.
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
