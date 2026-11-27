package spoke

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SSO handoff: the hub mints a short-lived, ASYMMETRICALLY-signed token that lets
// an already-hub-authenticated admin/user open a direct-route spoke (e.g. a
// firewalled heartbeat-only-cluster hive) WITHOUT a second GitHub device-flow login. The hub signs
// the token with an Ed25519 PRIVATE key that never leaves the hub; the spoke
// verifies it with only the corresponding PUBLIC key. The spoke confirms the
// carried user is in its authorized_users allowlist and mints a local session.
// Only a signed, expiring, single-hive-scoped assertion travels in the redirect
// URL — and, unlike the earlier symmetric HMAC design, the verifying spoke holds
// NO material that could mint a token.
//
// C2 follow-up — why asymmetric (residual of #2758): #2758's HKDF domain
// separation stopped cross-domain forgery and kept the master off spokes, but the
// SSO sub-key was still SYMMETRIC — the same derived key both minted (hub) and
// verified (spoke). A spoke operator who read HIVE_SSO_KEY from the pod env could
// therefore mint an SSO-as-any-owner token for its OWN hive. Ed25519 closes that:
// the spoke is injected the PUBLIC key only, so "verify hub-minted tokens" no
// longer implies "can mint tokens". The signing seed is still derived
// deterministically from the existing master (see ssoSigningSeed), so no new
// secret is provisioned and the keypair is stable across restarts — there is no
// key rotation, only a change of algorithm.
//
// The token is deliberately minimal and stateless (no server-side store): it
// binds {username, role, hiveID, expiry} under an Ed25519 signature. It is NOT a
// bearer credential for arbitrary API calls — the spoke exchanges it exactly once
// for a normal per-user session cookie and never accepts it again for API auth.

const (
	// ssoTokenTTL bounds how long a freshly-minted handoff token is valid. Kept
	// very short: the token is consumed immediately on the redirect, so a tight
	// window minimizes the value of a leaked URL (browser history, referrer).
	ssoTokenTTL = 90 * time.Second

	// ssoClockSkew tolerates minor clock drift between hub and spoke when
	// checking the not-before/expiry bounds.
	ssoClockSkew = 30 * time.Second

	// ssoTokenVersion namespaces the signed payload so the format can evolve
	// without a verifier ever accepting an old/foreign shape. Bumped to -v2 for the
	// Ed25519 cutover: a -v1 (symmetric HMAC) token can never be replayed against a
	// -v2 verifier and vice versa.
	ssoTokenVersion = "hive-sso-v2"
)

// ssoClaims is the signed payload of an SSO handoff token.
type ssoClaims struct {
	Version  string `json:"v"`
	Username string `json:"u"`
	Role     string `json:"r"`
	HiveID   string `json:"h"`
	IssuedAt int64  `json:"iat"`
	Expiry   int64  `json:"exp"`
	// GitHubLogin is the GitHub login a non-GitHub sign-in proved control of
	// through the hub's link step, and GitHubLinkedAt when (unix seconds)
	// (hivecommons/hive#19992). Both are omitted when there is no proven link,
	// so the token is byte-identical to the earlier shape, and a spoke that
	// does not know the fields ignores them.
	GitHubLogin    string `json:"g,omitempty"`
	GitHubLinkedAt int64  `json:"ga,omitempty"`
}

// SSOGitHubLink is a proven GitHub login link carried by an SSO token.
type SSOGitHubLink struct {
	Login    string
	LinkedAt time.Time
}

// SSOIdentity is what a verified SSO token asserts.
type SSOIdentity struct {
	Username   string
	Role       string
	GitHubLink SSOGitHubLink
}

// b64 encodes without padding so the token is URL-safe in a query parameter.
var ssoB64 = base64.RawURLEncoding

// MintSSOToken creates a signed handoff token for username/role scoped to a
// single hiveID, valid for ssoTokenTTL. `seedHex` is the hex-encoded 32-byte
// Ed25519 SEED (the hub-only private-key material from ssoSigningSeed); the token
// is signed with the private key it expands to. `now` is passed in so callers can
// use a single consistent clock (and tests are deterministic). Returns "" if
// seedHex is empty/not a valid 32-byte seed, or if username/hiveID are empty — a
// caller holding only a PUBLIC key (or no key) therefore cannot produce a token.
func MintSSOToken(seedHex, username, role, hiveID string, now time.Time) string {
	return MintSSOTokenWithGitHubLink(seedHex, username, role, hiveID, SSOGitHubLink{}, now)
}

// MintSSOTokenWithGitHubLink is MintSSOToken carrying a proven GitHub login
// link. A link with an empty login or zero time is left out.
func MintSSOTokenWithGitHubLink(seedHex, username, role, hiveID string, link SSOGitHubLink, now time.Time) string {
	if seedHex == "" || username == "" || hiveID == "" {
		return ""
	}
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != ed25519.SeedSize {
		// Not private-key material (e.g. a public key was passed by mistake, or a
		// truncated/garbage value): fail closed rather than sign with junk.
		return ""
	}
	priv := ed25519.NewKeyFromSeed(seed)

	claims := ssoClaims{
		Version:  ssoTokenVersion,
		Username: username,
		Role:     role,
		HiveID:   hiveID,
		IssuedAt: now.Unix(),
		Expiry:   now.Add(ssoTokenTTL).Unix(),
	}
	if login := strings.TrimSpace(link.Login); login != "" && !link.LinkedAt.IsZero() {
		claims.GitHubLogin = login
		claims.GitHubLinkedAt = link.LinkedAt.Unix()
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return ""
	}
	body := ssoB64.EncodeToString(payload)
	sig := ssoB64.EncodeToString(ed25519.Sign(priv, []byte(body)))
	return body + "." + sig
}

// VerifySSOToken validates a handoff token against an Ed25519 PUBLIC key and the
// spoke's own hiveID, returning the carried username and role. `pubHex` is the
// hex-encoded 32-byte Ed25519 public key (from SpokeSSOPublicKey). It fails closed
// on any mismatch: bad signature, wrong version, expired/not-yet-valid, or a
// hiveID that does not match THIS spoke (so a token minted for hive A can never
// open hive B). Because verification needs only the public key, a spoke that holds
// pubHex cannot mint a token. `now` is the verifier's clock.
func VerifySSOToken(pubHex, token, expectedHiveID string, now time.Time) (username, role string, err error) {
	id, err := VerifySSOTokenIdentity(pubHex, token, expectedHiveID, now)
	if err != nil {
		return "", "", err
	}
	return id.Username, id.Role, nil
}

// VerifySSOTokenIdentity is VerifySSOToken returning everything the token
// asserts, including a proven GitHub login link.
func VerifySSOTokenIdentity(pubHex, token, expectedHiveID string, now time.Time) (SSOIdentity, error) {
	if pubHex == "" {
		return SSOIdentity{}, fmt.Errorf("sso: no verification key configured")
	}
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return SSOIdentity{}, fmt.Errorf("sso: invalid verification key")
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return SSOIdentity{}, fmt.Errorf("sso: malformed token")
	}
	body, sigB64 := parts[0], parts[1]

	// Verify the Ed25519 signature BEFORE trusting any payload bytes. ed25519.Verify
	// is constant-time and safe on attacker-controlled input.
	sig, err := ssoB64.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return SSOIdentity{}, fmt.Errorf("sso: bad signature")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(body), sig) {
		return SSOIdentity{}, fmt.Errorf("sso: bad signature")
	}

	raw, err := ssoB64.DecodeString(body)
	if err != nil {
		return SSOIdentity{}, fmt.Errorf("sso: undecodable payload")
	}
	var claims ssoClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return SSOIdentity{}, fmt.Errorf("sso: unparseable claims")
	}
	if claims.Version != ssoTokenVersion {
		return SSOIdentity{}, fmt.Errorf("sso: unexpected token version")
	}
	if claims.HiveID != expectedHiveID {
		return SSOIdentity{}, fmt.Errorf("sso: token is for a different hive")
	}
	if claims.Username == "" {
		return SSOIdentity{}, fmt.Errorf("sso: empty username")
	}
	nowUnix := now.Unix()
	skew := int64(ssoClockSkew / time.Second)
	if claims.IssuedAt > nowUnix+skew {
		return SSOIdentity{}, fmt.Errorf("sso: token not yet valid")
	}
	if claims.Expiry < nowUnix-skew {
		return SSOIdentity{}, fmt.Errorf("sso: token expired")
	}
	id := SSOIdentity{Username: claims.Username, Role: claims.Role}
	if claims.GitHubLogin != "" && claims.GitHubLinkedAt > 0 {
		id.GitHubLink = SSOGitHubLink{Login: claims.GitHubLogin, LinkedAt: time.Unix(claims.GitHubLinkedAt, 0).UTC()}
	}
	return id, nil
}
