package hub

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SSO handoff: the hub mints a short-lived, HMAC-signed token that lets an
// already-hub-authenticated admin/user open a direct-route spoke (e.g. a
// firewalled vllm-d hive) WITHOUT a second GitHub device-flow login. The spoke
// verifies the token with the SAME shared secret it already uses for heartbeat
// auth (HIVE_HUB_SECRET), confirms the carried user is in its authorized_users
// allowlist, and mints a local session. No secret ever leaves the two trusted
// endpoints; only a signed, expiring, single-hive-scoped assertion travels in
// the redirect URL.
//
// The token is deliberately minimal and stateless (no server-side store): it
// binds {username, role, hiveID, expiry} under HMAC-SHA256. It is NOT a bearer
// credential for arbitrary API calls — the spoke exchanges it exactly once for
// a normal per-user session cookie and never accepts it again for API auth.

const (
	// ssoTokenTTL bounds how long a freshly-minted handoff token is valid. Kept
	// very short: the token is consumed immediately on the redirect, so a tight
	// window minimizes the value of a leaked URL (browser history, referrer).
	ssoTokenTTL = 90 * time.Second

	// ssoClockSkew tolerates minor clock drift between hub and spoke when
	// checking the not-before/expiry bounds.
	ssoClockSkew = 30 * time.Second

	// ssoTokenVersion namespaces the signed payload so the format can evolve
	// without a verifier ever accepting an old/foreign shape.
	ssoTokenVersion = "hive-sso-v1"
)

// ssoClaims is the signed payload of an SSO handoff token.
type ssoClaims struct {
	Version  string `json:"v"`
	Username string `json:"u"`
	Role     string `json:"r"`
	HiveID   string `json:"h"`
	IssuedAt int64  `json:"iat"`
	Expiry   int64  `json:"exp"`
}

// b64 encodes without padding so the token is URL-safe in a query parameter.
var ssoB64 = base64.RawURLEncoding

// MintSSOToken creates a signed handoff token for username/role scoped to a
// single hiveID, valid for ssoTokenTTL. `now` is passed in so callers can use a
// single consistent clock (and tests are deterministic). Returns "" if secret
// is empty — the hub must have a configured HIVE_HUB_SECRET to mint tokens.
func MintSSOToken(secret, username, role, hiveID string, now time.Time) string {
	if secret == "" || username == "" || hiveID == "" {
		return ""
	}
	claims := ssoClaims{
		Version:  ssoTokenVersion,
		Username: username,
		Role:     role,
		HiveID:   hiveID,
		IssuedAt: now.Unix(),
		Expiry:   now.Add(ssoTokenTTL).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return ""
	}
	body := ssoB64.EncodeToString(payload)
	sig := ssoSign(secret, body)
	return body + "." + sig
}

// VerifySSOToken validates a handoff token against secret and the spoke's own
// hiveID, returning the carried username and role. It fails closed on any
// mismatch: bad signature, wrong version, expired/not-yet-valid, or a hiveID
// that does not match THIS spoke (so a token minted for hive A can never open
// hive B). `now` is the verifier's clock.
func VerifySSOToken(secret, token, expectedHiveID string, now time.Time) (username, role string, err error) {
	if secret == "" {
		return "", "", fmt.Errorf("sso: no shared secret configured")
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("sso: malformed token")
	}
	body, sig := parts[0], parts[1]

	// Constant-time signature check BEFORE trusting any payload bytes.
	expected := ssoSign(secret, body)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", "", fmt.Errorf("sso: bad signature")
	}

	raw, err := ssoB64.DecodeString(body)
	if err != nil {
		return "", "", fmt.Errorf("sso: undecodable payload")
	}
	var claims ssoClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", "", fmt.Errorf("sso: unparseable claims")
	}
	if claims.Version != ssoTokenVersion {
		return "", "", fmt.Errorf("sso: unexpected token version")
	}
	if claims.HiveID != expectedHiveID {
		return "", "", fmt.Errorf("sso: token is for a different hive")
	}
	if claims.Username == "" {
		return "", "", fmt.Errorf("sso: empty username")
	}
	nowUnix := now.Unix()
	skew := int64(ssoClockSkew / time.Second)
	if claims.IssuedAt > nowUnix+skew {
		return "", "", fmt.Errorf("sso: token not yet valid")
	}
	if claims.Expiry < nowUnix-skew {
		return "", "", fmt.Errorf("sso: token expired")
	}
	return claims.Username, claims.Role, nil
}

// ssoSign returns the URL-safe base64 HMAC-SHA256 of body under secret.
func ssoSign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return ssoB64.EncodeToString(mac.Sum(nil))
}

// ssoTokenVersionEd25519 namespaces the ASYMMETRIC (Ed25519) handoff payload.
// It is "-v2" (the legacy symmetric token above is "-v1"), so a v4 spoke's
// Ed25519 verifier accepts ONLY this shape and never a legacy symmetric token,
// and an old symmetric verifier never accepts this one. The two schemes cannot
// be confused for each other.
const ssoTokenVersionEd25519 = "hive-sso-v2"

// MintSSOTokenEd25519 mints an ASYMMETRICALLY-signed SSO handoff token that a v4
// spoke (which verifies with an Ed25519 PUBLIC key) can accept. This is the v2
// hub-side backport of the v4 asymmetric-SSO cutover (#2771); it exists ALONGSIDE
// the legacy symmetric MintSSOToken (untouched above) so the hub can hand each
// consuming spoke the scheme it can verify — Ed25519 for a v4 spoke, symmetric
// HMAC for the 33 old spokes.
//
// `seedHex` is the hex-encoded 32-byte Ed25519 SEED (hub-only private material
// from ssoSigningSeed()); the token is signed with the private key it expands to.
// `now` is passed in for a consistent clock / deterministic tests. Returns "" if
// seedHex is empty/not a valid 32-byte seed, or if username/hiveID are empty — so
// a caller holding only a public key (or no key) can never produce a token.
func MintSSOTokenEd25519(seedHex, username, role, hiveID string, now time.Time) string {
	if seedHex == "" || username == "" || hiveID == "" {
		return ""
	}
	seed, err := hex.DecodeString(strings.TrimSpace(seedHex))
	if err != nil || len(seed) != ed25519.SeedSize {
		// Not private-key material (e.g. a public key or garbage was passed):
		// fail closed rather than sign with junk.
		return ""
	}
	priv := ed25519.NewKeyFromSeed(seed)

	claims := ssoClaims{
		Version:  ssoTokenVersionEd25519,
		Username: username,
		Role:     role,
		HiveID:   hiveID,
		IssuedAt: now.Unix(),
		Expiry:   now.Add(ssoTokenTTL).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return ""
	}
	body := ssoB64.EncodeToString(payload)
	sig := ssoB64.EncodeToString(ed25519.Sign(priv, []byte(body)))
	return body + "." + sig
}
