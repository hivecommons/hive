// Package credsidecar is the isolated GitHub credential holder of #9586 phase
// 2: a small server, run as `hive credsidecar` in its OWN container, that
// holds the GitHub App key, mints the per-tier scoped installation tokens, and
// attaches them to requests the hive's MITM proxy relays for agents.
//
// Why a separate process. With proxy-side injection (#1861) on but no sidecar,
// every agent's real scoped token lives in the hive process's in-memory
// registry (pkg/github agentProxyTokens), in the same container as the agents.
// Moving the token holder into a sidecar container takes those tokens out of
// the process the agents share a filesystem, a PID namespace and a UID-less
// memory boundary with: the hive process then holds, per agent, only which
// TIER the agent is entitled to, and a key that lets it SIGN a request.
//
// The protocol. The proxy sends each GitHub request to the sidecar over the
// pod-local loopback, with an HMAC-SHA256 signature (see canonicalString) over
// the method, the target GitHub host, the request URI, the agent name, the
// agent's tier, a timestamp, a single-use nonce and the SHA-256 of the body,
// keyed by a per-spoke secret only the hive process and the sidecar can read.
// The sidecar refuses anything unsigned, badly signed, outside the replay
// window, replayed, or bound for a host that is not GitHub, then strips every
// caller-supplied credential, attaches the tier's token, and forwards the
// request to GitHub itself. Agents share the pod's network namespace, so they
// CAN reach the sidecar's port - the signature is what keeps them out.
//
// This package is a stdlib-only leaf on purpose: pkg/github (the tier
// registry) and pkg/config (the startup guard) both import it, and
// pkg/github deliberately takes no dependency on the application config.
package credsidecar

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request headers of the signing protocol. Every header with HeaderPrefix is
// protocol metadata: the sidecar strips all of them before forwarding, so none
// ever reaches GitHub.
const (
	HeaderPrefix     = "X-Hive-Sidecar-"
	HeaderAgent      = HeaderPrefix + "Agent"
	HeaderTier       = HeaderPrefix + "Tier"
	HeaderHost       = HeaderPrefix + "Host"
	HeaderTimestamp  = HeaderPrefix + "Timestamp"
	HeaderNonce      = HeaderPrefix + "Nonce"
	HeaderBodySHA256 = HeaderPrefix + "Body-Sha256"
	HeaderSignature  = HeaderPrefix + "Signature"

	// HeaderRefused marks a response the SIDECAR produced (a refusal), as
	// opposed to one relayed from GitHub, so an agent or operator reading a
	// 401 can tell "the sidecar rejected the proxy" from "GitHub rejected the
	// token". The value is the refusal reason.
	HeaderRefused = "X-Hive-Cred-Sidecar-Refused"
)

// signatureVersion is the first line of the signed canonical string. Bumping it
// invalidates every signature minted under the old layout, so a proxy and a
// sidecar that disagree on the layout fail closed instead of misreading fields.
const signatureVersion = "hive-credsidecar-v1"

// ReplayWindow is how far a request's signed timestamp may sit from the
// sidecar's clock, in either direction, before it is refused as expired. Both
// processes run in the same pod, on the same node clock, and the proxy signs
// immediately before sending, so a few seconds would do; the window is wider to
// absorb a slow body upload, but short enough that a captured signed request is
// useless well within the lifetime of the nonce cache that also guards it.
const ReplayWindow = 30 * time.Second

// nonceBytes is the random length of a nonce (hex-encoded on the wire).
const nonceBytes = 16

// maxNonceLen bounds the nonce header the sidecar accepts, so a caller cannot
// make the replay cache store arbitrarily long keys.
const maxNonceLen = 2 * nonceBytes

// minNonceLen is the shortest nonce accepted. A short nonce collides, and a
// collision reads as a replay; requiring the full length keeps that impossible
// in practice.
const minNonceLen = 2 * nonceBytes

// MinKeyBytes is the shortest HMAC key either side accepts. 32 bytes matches
// the SHA-256 block security level; a shorter key is refused at startup rather
// than silently weakening every signature.
const MinKeyBytes = 32

// Errors the verifier returns. Each is a distinct refusal so the sidecar can
// say which check failed (in the HeaderRefused response header, never with any
// secret material) and tests can assert on the exact reason.
var (
	ErrUnsigned        = errors.New("request is not signed")
	ErrBadSignature    = errors.New("signature does not match")
	ErrExpired         = errors.New("signed timestamp is outside the replay window")
	ErrReplayed        = errors.New("nonce was already used")
	ErrBodyDigest      = errors.New("body digest does not match the body")
	ErrMalformed       = errors.New("signing header is malformed")
	ErrNonceCacheFull  = errors.New("replay cache is full")
	ErrHostNotAllowed  = errors.New("host is not a GitHub host this sidecar serves")
	ErrKeyTooShort     = fmt.Errorf("HMAC key is shorter than %d bytes", MinKeyBytes)
	ErrFieldHasNewline = errors.New("signed field contains a line break")
	ErrBodyTooLarge    = errors.New("request body exceeds the sidecar body limit")
	ErrNotConfigured   = errors.New("credential sidecar is not configured")
)

// SignedFields are the values a signature covers. Host is the GitHub host the
// request is for (the sidecar forwards to https://Host + RequestURI), Agent the
// proxy-identified agent ("" when the proxy could not identify the caller), and
// Tier the token tier to attach ("" means forward with no credential).
type SignedFields struct {
	Method     string
	Host       string
	RequestURI string
	Agent      string
	Tier       string
	Timestamp  int64
	Nonce      string
	BodySHA256 string
}

// canonicalString is the exact byte string that is signed. One field per line,
// in a fixed order, behind a version line. No field may contain a line break
// (checkFields), so no two distinct field tuples can produce the same string.
func canonicalString(f SignedFields) string {
	return strings.Join([]string{
		signatureVersion,
		f.Method,
		f.Host,
		f.RequestURI,
		f.Agent,
		f.Tier,
		strconv.FormatInt(f.Timestamp, 10),
		f.Nonce,
		f.BodySHA256,
	}, "\n")
}

// checkFields refuses a field tuple that could make canonicalString ambiguous.
func checkFields(f SignedFields) error {
	for _, v := range []string{f.Method, f.Host, f.RequestURI, f.Agent, f.Tier, f.Nonce, f.BodySHA256} {
		if strings.ContainsAny(v, "\r\n") {
			return ErrFieldHasNewline
		}
	}
	return nil
}

// Sign returns the hex HMAC-SHA256 of the canonical string under key.
func Sign(key []byte, f SignedFields) (string, error) {
	if len(key) < MinKeyBytes {
		return "", ErrKeyTooShort
	}
	if err := checkFields(f); err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(canonicalString(f)))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// verifySignature reports whether sigHex is the signature of f under key, in
// constant time.
func verifySignature(key []byte, f SignedFields, sigHex string) error {
	want, err := Sign(key, f)
	if err != nil {
		return err
	}
	got, decErr := hex.DecodeString(sigHex)
	if decErr != nil {
		return ErrBadSignature
	}
	wantRaw, _ := hex.DecodeString(want)
	if !hmac.Equal(got, wantRaw) {
		return ErrBadSignature
	}
	return nil
}

// BodyDigest is the hex SHA-256 of a request body, the value signed as
// SignedFields.BodySHA256.
func BodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// newNonce returns a fresh random nonce, hex-encoded.
func newNonce() (string, error) {
	buf := make([]byte, nonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// checkTimestamp refuses a signed timestamp further than ReplayWindow from now.
func checkTimestamp(ts int64, now time.Time) error {
	signed := time.Unix(ts, 0)
	delta := now.Sub(signed)
	if delta < 0 {
		delta = -delta
	}
	if delta > ReplayWindow {
		return ErrExpired
	}
	return nil
}

// nonceCache remembers every nonce seen within the replay window, so a signed
// request captured on the loopback cannot be sent a second time. An entry only
// has to outlive the window: after that the timestamp check refuses the replay
// on its own. The cache is bounded (maxEntries); when it is full of live
// entries the sidecar refuses new requests rather than evicting a live nonce,
// because evicting would re-open exactly the replay the cache exists to stop.
type nonceCache struct {
	mu         sync.Mutex
	seen       map[string]time.Time
	maxEntries int
}

// defaultMaxNonceEntries bounds the replay cache. At the proxy's request rate
// (a spoke's whole fleet shares it) this is several orders of magnitude more
// than one replay window holds, while capping the memory an abusive signer
// could make the sidecar spend at a few megabytes.
const defaultMaxNonceEntries = 1 << 16

func newNonceCache(maxEntries int) *nonceCache {
	return &nonceCache{seen: make(map[string]time.Time), maxEntries: maxEntries}
}

// checkAndStore records nonce and reports ErrReplayed when it was already seen
// inside the window. Only called AFTER the signature verified, so an unsigned
// caller cannot fill the cache.
func (c *nonceCache) checkAndStore(nonce string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seenAt, ok := c.seen[nonce]; ok && now.Sub(seenAt) <= 2*ReplayWindow {
		return ErrReplayed
	}
	if len(c.seen) >= c.maxEntries {
		c.pruneLocked(now)
		if len(c.seen) >= c.maxEntries {
			return ErrNonceCacheFull
		}
	}
	c.seen[nonce] = now
	return nil
}

// pruneLocked drops entries old enough that the timestamp check alone refuses
// any replay of them. The margin is two windows: a request may be signed up to
// one window in the future and arrive up to one window late.
func (c *nonceCache) pruneLocked(now time.Time) {
	for n, at := range c.seen {
		if now.Sub(at) > 2*ReplayWindow {
			delete(c.seen, n)
		}
	}
}

// size is the number of cached nonces (tests).
func (c *nonceCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}
