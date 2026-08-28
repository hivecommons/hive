package delegation

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Token wire format and crypto.
//
// The construction is copied deliberately from pkg/hub/sso.go's MintSSOToken /
// VerifySSOToken rather than reinvented:
//
//	<base64url(claims JSON)> "." <base64url(Ed25519 signature over the body STRING)>
//
// Note "over the body STRING" — the signature covers the base64 text, not the
// raw JSON bytes. That is what sso.go and hub_cookie.go both do, and matching
// them matters more than any aesthetic preference: a verifier written against
// one of hive's signed artifacts should not have to discover that this one
// canonicalizes differently. It also removes a whole class of bug, since the
// verifier never has to re-encode JSON identically to verify a signature.
//
// SIGNATURE IS CHECKED BEFORE ANY PAYLOAD BYTE IS TRUSTED. Same ordering as
// VerifySSOToken: decode the key, decode the signature, ed25519.Verify, and
// ONLY then base64-decode and unmarshal the claims. Parsing attacker-controlled
// JSON before authenticating it is how a signed-token implementation grows a
// pre-auth attack surface.

const (
	// ChainTokenTTL bounds a minted chain's validity.
	//
	// Much longer than ssoTokenTTL (90s) because these are different kinds of
	// artifact. An SSO token is a credential consumed once on a redirect, so a
	// tight window limits the damage of a leaked URL. A delegation chain is
	// EVIDENCE about an action that already happened, attached to a record a
	// tenant may audit later — it authorizes nothing (observe-only), so its
	// window bounds how long it is useful as fresh evidence, not how long a
	// stolen copy is dangerous. 24h lets a tenant's nightly reconciliation job
	// verify the day's chains against material fetched once.
	ChainTokenTTL = 24 * time.Hour

	// ChainClockSkew tolerates drift between the minting hive and a
	// third-party verifier. Same 30s as ssoClockSkew; a tenant's verifier is
	// on infrastructure whose clock we do not control, so some tolerance is
	// mandatory, and 30s is already hive's established answer.
	ChainClockSkew = 30 * time.Second

	// InfoChainEd25519Seed is the domain-separation label for the delegation
	// chain signing key, in the same namespace as hub_keys.go's
	// infoSSOEd25519Seed / infoSessionEd25519Seed.
	//
	// A DISTINCT label is the whole point of domain separation: a chain
	// signature must never verify as an SSO handoff or a session cookie, and
	// vice versa. Because deriveDomainKey is HMAC-SHA256(master, label), a
	// distinct label yields an independent keypair with no new secret to store,
	// escrow, or rotate — rotation comes free from the existing generations
	// machinery. The "-v1" suffix versions the DOMAIN, not the key; see the
	// note in hub_keys.go.
	InfoChainEd25519Seed = "hive-delegation-ed25519-v1"
)

// chainB64 encodes without padding so a token is safe in a URL, a header, and a
// JSON string without escaping. Matches ssoB64.
var chainB64 = base64.RawURLEncoding

// SeedFromMaster derives the hex Ed25519 SEED that signs delegation chains for
// a given master secret (one key generation's secret).
//
// PRIVATE MATERIAL. This is the signing half and it must never leave the
// minting process — not into a spoke env var, not into a log, not into an API
// response. Publishing happens through PublicKeyFromSeed, which discards the
// private half internally. The same rule is stated on hub_keys.go's
// ssoSigningSeed and sessionSigningSeed and it applies here verbatim.
//
// Returns "" for an empty master so a keyless caller fails closed — with no
// key, MintToken returns "" and the caller emits no chain, which is exactly the
// right behavior for a hive that has no secret configured.
func SeedFromMaster(master string) string {
	return deriveDomainKey(master, InfoChainEd25519Seed)
}

// PublicKeyFromSeed expands a hex Ed25519 seed and returns ONLY the hex public
// half. Returns "" for anything that is not a valid 32-byte seed.
//
// Mirrors pkg/hub/ssoPublicKeyFromSeed. The private key is a local that goes
// out of scope; nothing in this function can return it.
func PublicKeyFromSeed(seedHex string) string {
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != ed25519.SeedSize {
		return ""
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return hex.EncodeToString(pub)
}

// MintToken signs a chain and returns the wire token.
//
// Returns "" — never a partial or unsigned token — for an empty/invalid seed or
// a chain that does not Validate. A caller that gets "" emits no chain. There
// is deliberately no "unsigned chain" representation on the wire: an
// unverifiable chain is not evidence, and offering one would invite a consumer
// to read it as though it were.
//
// `now` is passed in so callers share one clock and tests are deterministic,
// matching MintSSOToken's signature.
func MintToken(seedHex string, c Chain, now time.Time) string {
	if strings.TrimSpace(seedHex) == "" {
		return ""
	}
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != ed25519.SeedSize {
		// Not private-key material — e.g. a PUBLIC key was passed by mistake,
		// which is a plausible wiring error given both are 64 hex chars. Fail
		// closed rather than sign with junk that would never verify.
		return ""
	}
	c.Version = ChainVersion
	if c.IssuedAt == 0 {
		c.IssuedAt = now.Unix()
	}
	if c.Expiry == 0 {
		c.Expiry = now.Add(ChainTokenTTL).Unix()
	}
	if err := c.Validate(); err != nil {
		return ""
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	priv := ed25519.NewKeyFromSeed(seed)
	body := chainB64.EncodeToString(payload)
	sig := chainB64.EncodeToString(ed25519.Sign(priv, []byte(body)))
	return body + "." + sig
}

// VerifyToken checks a token against ONE hex Ed25519 public key and returns the
// carried chain.
//
// Fails closed on every mismatch: unusable key, malformed token, bad signature,
// wrong version, structurally invalid chain, or outside the validity window.
// Because verification needs only the public key, a party that can verify
// CANNOT mint — the asymmetry that makes third-party verification meaningful,
// and the same reason SSO was moved off symmetric HMAC (see sso.go's C2 note).
func VerifyToken(pubHex, token string, now time.Time) (Chain, error) {
	if strings.TrimSpace(pubHex) == "" {
		return Chain{}, fmt.Errorf("delegation: no verification key configured")
	}
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		// Length is checked BEFORE use: ed25519.Verify PANICS on a public key
		// of the wrong size, so an unvalidated key turns a malformed input into
		// a crash of the verifying process. pkg/hub/validPublicKeys exists for
		// exactly this reason and states it explicitly.
		return Chain{}, fmt.Errorf("delegation: invalid verification key")
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Chain{}, fmt.Errorf("delegation: malformed token")
	}
	body, sigB64 := parts[0], parts[1]

	sig, err := chainB64.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Chain{}, fmt.Errorf("delegation: bad signature")
	}
	// Authenticate before parsing. Nothing below this line existed as trusted
	// data above it.
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(body), sig) {
		return Chain{}, fmt.Errorf("delegation: bad signature")
	}

	raw, err := chainB64.DecodeString(body)
	if err != nil {
		return Chain{}, fmt.Errorf("delegation: undecodable payload")
	}
	var c Chain
	if err := json.Unmarshal(raw, &c); err != nil {
		return Chain{}, fmt.Errorf("delegation: unparseable chain")
	}
	if err := c.Validate(); err != nil {
		return Chain{}, err
	}
	if c.Expired(now) {
		return Chain{}, fmt.Errorf("delegation: chain outside validity window")
	}
	return c, nil
}

// VerifyTokenAcrossKeys is bounded trial verification against every published
// key, for a verifier holding a key SET rather than a single key.
//
// This is what a third party calls after fetching the published material: the
// set contains the current generation plus any generation still inside its
// dual-acceptance window, so a chain minted just before a rotation keeps
// verifying afterwards without the verifier knowing anything about rotation.
//
// NO EARLY RETURN. The loop evaluates every candidate and records the first
// that verified, so the number of Ed25519 operations does not depend on WHICH
// key matched. pkg/hub's VerifySSOTokenAcrossKeys declines the same early exit
// and documents why: returning early leaks, through latency, which generation
// accepted — an oracle for rotation state offered on an unauthenticated path.
// The cost is bounded by maxLiveGenerations (2 on the hub today).
//
// A malformed key in the set costs one wasted candidate and cannot prevent a
// well-formed one from verifying.
func VerifyTokenAcrossKeys(pubHexes []string, token string, now time.Time) (Chain, int, error) {
	valid := ValidPublicKeys(pubHexes...)
	if len(valid) == 0 {
		return Chain{}, -1, fmt.Errorf("delegation: no verification key configured")
	}
	matched := -1
	var matchedChain Chain
	for i, k := range valid {
		c, err := VerifyToken(k, token, now)
		if err == nil && matched == -1 {
			matched, matchedChain = i, c
		}
	}
	if matched == -1 {
		return Chain{}, -1, fmt.Errorf("delegation: chain rejected by every published key")
	}
	return matchedChain, matched, nil
}

// ValidPublicKeys filters candidates down to well-formed hex Ed25519 public
// keys, dropping empty/malformed entries rather than passing them to Verify.
//
// Mirrors pkg/hub/validPublicKeys, including its reason: ed25519.Verify panics
// on a wrong-length key, so filtering is a crash guard and not just hygiene.
func ValidPublicKeys(keys ...string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		raw, err := hex.DecodeString(k)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		out = append(out, k)
	}
	return out
}

// UnverifiedGeneration reads the `g` claim WITHOUT checking the signature, so a
// verifier can pick the right published key before doing the expensive check.
//
// THE RESULT IS A HINT AND NOTHING ELSE. It is attacker-steerable — anyone can
// write any number there — so it may be used only to ORDER or SELECT candidate
// keys, never to decide that a chain is acceptable. The authenticated copy of
// the same value is inside the verified payload, and that is the one any
// decision must read. hub_cookie.go's hubCookieClaimedGeneration carries the
// identical caveat, and hub_cookie_generations.go shows the correct use:
// a claimed generation may narrow the search, and a chain naming an
// unacceptable generation falls back to trying all of them rather than failing,
// so a bogus marker can never turn a verifiable chain into a rejected one.
func UnverifiedGeneration(token string) int {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return 0
	}
	raw, err := chainB64.DecodeString(parts[0])
	if err != nil {
		return 0
	}
	var c Chain
	if json.Unmarshal(raw, &c) != nil {
		return 0
	}
	return c.Generation
}
