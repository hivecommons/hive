package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Sandbox inference listener (#19619).
//
// A sandbox.runtime: job kick runs in its own pod and cannot reach the
// loopback translator on 127.0.0.1:18444, and the translator's caller
// identity there is the socket owner's UID, which a remote pod does not have.
// This listener serves the same translator over TLS to Job pods and names the
// caller from a per-kick key instead:
//
//   - THE GATEWAY KEY STAYS HERE. A Job holds only its kick key, which this
//     listener alone accepts. The translator attaches the upstream gateway
//     credential, exactly as it does for an agent that is not sandboxed.
//   - ONE KEY PER KICK. Enroll mints a fresh random key for one kick of one
//     agent; release revokes it when the kick ends. Only a digest is kept.
//   - NEVER LOGGED. Neither the key nor its digest is written to a log line;
//     a refusal records only whether a key was presented.
//   - THE LOOPBACK LISTENER IS UNCHANGED. Its UID check keeps applying there;
//     a kick key is meaningless on 127.0.0.1:18444.

// sandboxInferenceKeyPrefix marks a kick key, so a loopback "sk-hive-<agent>"
// name is never looked up as one.
const sandboxInferenceKeyPrefix = "sk-hive-kick-"

const sandboxInferenceKeyBytes = 32

// SandboxInferenceRelayOptions configure NewSandboxInferenceRelay.
type SandboxInferenceRelayOptions struct {
	// Endpoint is the https://host:port Jobs use to reach the listener.
	Endpoint string
	// RootsPEM is the trust bundle handed to a Job alongside the hive CA.
	// Empty means the hive CA alone.
	RootsPEM []byte
}

// SandboxInferenceRelay is the Job-facing inference listener.
type SandboxInferenceRelay struct {
	p        *GitHubProxy
	opts     SandboxInferenceRelayOptions
	endpoint *url.URL

	mu   sync.RWMutex
	keys map[[sha256.Size]byte]string // sha256(key) -> agent
}

// NewSandboxInferenceRelay builds the listener on p, which supplies the CA,
// the inference routes and the translator.
func NewSandboxInferenceRelay(p *GitHubProxy, opts SandboxInferenceRelayOptions) (*SandboxInferenceRelay, error) {
	if p == nil {
		return nil, errors.New("sandbox inference listener: no proxy")
	}
	u, err := url.Parse(strings.TrimSpace(opts.Endpoint))
	if err != nil || u.Hostname() == "" || u.Scheme != "https" || u.User != nil {
		return nil, errors.New("sandbox inference listener: endpoint must be an https://host:port URL without credentials")
	}
	return &SandboxInferenceRelay{p: p, opts: opts, endpoint: u, keys: map[[sha256.Size]byte]string{}}, nil
}

// TLSConfig is the server configuration for the listener. It is only ever
// served over TLS, because the kick key travels in a request header. The
// certificate is issued by the hive proxy CA for the endpoint's host.
func (r *SandboxInferenceRelay) TLSConfig() *tls.Config {
	host := canonicalHost(r.endpoint.Hostname())
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert, err := r.p.forgeCert(host)
			if err != nil {
				return nil, fmt.Errorf("sandbox inference listener: issuing certificate for %q: %w", host, err)
			}
			return &cert, nil
		},
	}
}

// Enroll admits one kick of agentName. It returns the base URL the Job sets
// as ANTHROPIC_BASE_URL, the kick key it sends as ANTHROPIC_API_KEY, the CA
// bundle it must trust and a release func that revokes the key.
func (r *SandboxInferenceRelay) Enroll(agentName string) (baseURL, apiKey string, caBundlePEM []byte, release func(), err error) {
	if strings.TrimSpace(agentName) == "" {
		return "", "", nil, nil, errors.New("sandbox inference listener: no agent name")
	}
	raw := make([]byte, sandboxInferenceKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", nil, nil, fmt.Errorf("sandbox inference listener: minting kick key: %w", err)
	}
	key := sandboxInferenceKeyPrefix + hex.EncodeToString(raw)
	digest := sha256.Sum256([]byte(key))
	r.mu.Lock()
	r.keys[digest] = agentName
	r.mu.Unlock()

	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.p.caX509.Raw})
	bundle := append(append([]byte{}, r.opts.RootsPEM...), ca...)
	var once sync.Once
	return strings.TrimRight(r.endpoint.String(), "/"), key, bundle, func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.keys, digest)
			r.mu.Unlock()
		})
	}, nil
}

// presentedKey is the key a request carries: x-api-key, which the Claude CLI
// sends for ANTHROPIC_API_KEY, or a Bearer Authorization.
func presentedKey(req *http.Request) string {
	if k := strings.TrimSpace(req.Header.Get("x-api-key")); k != "" {
		return k
	}
	if auth := req.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// identify names the agent a live kick key belongs to.
func (r *SandboxInferenceRelay) identify(req *http.Request) (string, bool) {
	key := presentedKey(req)
	if !strings.HasPrefix(key, sandboxInferenceKeyPrefix) {
		return "", false
	}
	digest := sha256.Sum256([]byte(key))
	r.mu.RLock()
	agentName, ok := r.keys[digest]
	r.mu.RUnlock()
	return agentName, ok
}

// refusalAttrs describes a refused request without the key it presented.
func (r *SandboxInferenceRelay) refusalAttrs(req *http.Request) []any {
	return []any{"listener", "sandbox", "remote", req.RemoteAddr, "method", req.Method, "path", req.URL.Path,
		"reason", "unknown_kick_key", "has_api_key", presentedKey(req) != ""}
}

// Handler is the translator, identifying callers by kick key.
func (r *SandboxInferenceRelay) Handler() http.Handler {
	return r.p.inferenceTranslatorHandlerFor(r.identify, r.refusalAttrs)
}

// Serve serves the translator on ln, which the caller wraps in TLS with
// TLSConfig. It returns only when the listener fails.
func (r *SandboxInferenceRelay) Serve(ln net.Listener) error {
	server := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: httpReadTimeout}
	return server.Serve(ln)
}
