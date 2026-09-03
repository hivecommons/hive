package delegation

import (
	"encoding/json"
	"net/http"
	"time"
)

// PUBLISHED VERIFICATION MATERIAL — the JWKS-equivalent.
//
// THE COMPETITIVE POINT LIVES HERE. A chain a tenant can only verify by asking
// the hub is not evidence about the hub — it is the hub's opinion, restated.
// The material is therefore published at a stable, documented, ANONYMOUSLY
// fetchable path so a tenant's own services can:
//
//  1. fetch it once (or on a cache schedule of their choosing),
//  2. verify every chain they hold OFFLINE, with no hive credentials, and
//  3. keep verifying during a hub outage or after leaving the platform.
//
// Point 3 is the one that makes it real. If verification required a request-time
// call to the hub, the operator could silently stop answering — or answer
// differently — for a tenant they were in dispute with, which is precisely the
// scenario a tenant wants cryptographic evidence for.
//
// WHY NOT LITERAL RFC 7517 JWKS. A JWK for Ed25519 is OKP/Ed25519 with an
// x-coordinate in base64url (RFC 8037), which is a fine format but a different
// encoding from the hex Ed25519 public keys hive already publishes into every
// spoke's env (HIVE_SSO_PUBLIC_KEY and friends). Shipping a second encoding of
// the same key type would mean two places to get the decoding wrong for no
// gain. This document is JWKS-SHAPED — a `keys` array of objects with a key id,
// an algorithm, and the public key — so it maps onto JWKS mechanically, and the
// docs give the one-line conversion for tooling that insists on RFC 7517.
//
// NOTHING SECRET IS SERVED. Every field below is a public key, a generation
// number, or a timestamp. The generation ID is explicitly non-secret in this
// codebase: it names a key and is not a key.

const (
	// KeysPath is the stable, documented, anonymously-fetchable path.
	//
	// STABLE IS A PROMISE, not a convenience. Third parties pin this path in
	// their own verification jobs, so changing it breaks verifiers we do not
	// operate and cannot notify. It sits under /api/hub/ alongside the other
	// unauthenticated read-only endpoints (/api/hub/version, /api/hub/stats).
	KeysPath = "/api/hub/delegation-keys"

	// KeyAlgorithm labels the signature algorithm in the published document,
	// using the JOSE name for Ed25519 (RFC 8037) so the value is meaningful to
	// generic tooling even though the surrounding document is not literal JWKS.
	KeyAlgorithm = "EdDSA"

	// KeyCurve is the JOSE curve name, for the same reason.
	KeyCurve = "Ed25519"
)

// PublishedKey is one verification key.
type PublishedKey struct {
	// Generation is the key id. A verifier matches it against a chain's `g`
	// claim to select this key directly instead of trial-verifying.
	Generation int `json:"generation"`

	// PublicKey is the hex-encoded 32-byte Ed25519 public key — the same
	// encoding as every other public key hive publishes.
	PublicKey string `json:"public_key"`

	// Algorithm and Curve are constant, but emitted explicitly so a consumer
	// never has to infer the algorithm from the key length, and so a future
	// second algorithm would be a visible document change rather than a silent
	// reinterpretation.
	Algorithm string `json:"algorithm"`
	Curve     string `json:"curve"`

	// Current marks the generation that is currently MINTING. Exactly one key
	// is current; the others are accepted for verification only, during their
	// dual-acceptance window.
	//
	// Published so a verifier can distinguish "this chain is fresh" from "this
	// chain was minted under a generation we are still honouring" — a
	// distinction a tenant auditing a rotation genuinely wants, and one they
	// could not reconstruct from the key list alone.
	Current bool `json:"current"`
}

// KeyDocument is the published verification material.
type KeyDocument struct {
	// Version namespaces the DOCUMENT format, independently of ChainVersion
	// which namespaces the token format. They can evolve separately: adding a
	// field here must not invalidate outstanding chains.
	Version string `json:"version"`

	// Enabled reports whether this hive is currently minting chains.
	//
	// Published so a tenant can tell "no chains exist because the feature is
	// off" from "no chains exist because nothing happened" — without it, an
	// absence of chains is unreadable, and a tenant might conclude actions were
	// unattributed when in fact observation was simply not switched on.
	Enabled bool `json:"enabled"`

	// Keys is the verification set: the current generation plus every previous
	// generation still inside its acceptance window. EMPTY when disabled.
	Keys []PublishedKey `json:"keys"`

	// GeneratedAt is when this document was rendered. Advisory only — it is NOT
	// a freshness guarantee and a verifier must not treat a stale document as
	// invalid. Keys leave the set on their own schedule (VerifyUntil), so a
	// cached document is correct until a rotation, not until a timestamp.
	GeneratedAt string `json:"generated_at"`
}

// BuildKeyDocument renders the published material.
//
// `current` is the currently-minting (generation, public key). `previous` are
// still-acceptable non-current generations. Malformed or empty keys are DROPPED
// rather than emitted: an empty-string key in a published document is strictly
// worse than an absent one, because a consumer that stores it will treat itself
// as configured while verifying nothing — the exact failure
// pkg/hub/desiredPerHiveEnv's empty-var fail-safe exists to prevent.
//
// Returns an empty key list when disabled, so the flag's OFF state is visible
// in the document rather than inferred from silence.
func BuildKeyDocument(enabled bool, currentGen int, currentPub string, previous []PublishedKey, now time.Time) KeyDocument {
	doc := KeyDocument{
		Version:     "hive-delegation-keys-v1",
		Enabled:     enabled,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Keys:        []PublishedKey{},
	}
	if !enabled {
		return doc
	}
	if len(ValidPublicKeys(currentPub)) == 1 {
		doc.Keys = append(doc.Keys, PublishedKey{
			Generation: currentGen,
			PublicKey:  currentPub,
			Algorithm:  KeyAlgorithm,
			Curve:      KeyCurve,
			Current:    true,
		})
	}
	for _, p := range previous {
		if len(ValidPublicKeys(p.PublicKey)) != 1 {
			continue
		}
		if p.Generation == currentGen {
			// Before the first rotation the previous set can equal the current
			// one. De-duplicating keeps "how many keys is a verifier trying"
			// an honest number, matching appendDistinctPublicKey's reasoning.
			continue
		}
		p.Algorithm, p.Curve, p.Current = KeyAlgorithm, KeyCurve, false
		doc.Keys = append(doc.Keys, p)
	}
	return doc
}

// ServeKeys writes a KeyDocument as JSON.
//
// UNAUTHENTICATED BY DESIGN — registered without requireAuth — because a tenant
// must be able to verify without holding a hive credential, and a third party
// they delegate verification to (an auditor, a compliance system) will hold no
// credential at all. Requiring auth here would defeat the entire purpose while
// protecting nothing: the response is public keys.
//
// Cache-Control permits caching: the document changes only on a rotation, and a
// verifier re-fetching per chain would be pointless load on a path we want
// tenants to use freely.
func ServeKeys(w http.ResponseWriter, doc KeyDocument) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	// Cross-origin reads are allowed: a tenant's browser-based tooling verifying
	// chains against this document is a first-class use, and the content is
	// public keys with no credential attached, so there is nothing for an
	// origin to be trusted with.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(doc)
}
