// Package mint implements an OIDC-style token "mint" service for Hive.
//
// It issues short-lived, scoped OIDC (JWT) tokens that agents and spokes can
// exchange for cloud/registry access via a Workload Identity Federation (WIF)
// broker. This is the standards-based short-lived-credential path: instead of
// distributing long-lived cloud keys, a caller proves its identity to the mint
// (see server.go) and receives a narrowly-scoped, quickly-expiring JWT signed
// by this hive. A downstream WIF provider (GCP/AWS/Azure/registry) is
// configured to trust this hive's issuer + JWKS and swaps the JWT for a cloud
// credential.
//
// This package ships the crypto core: signing, standard + scoped claims,
// bounded TTL, verification (fail-closed), and a `.well-known/jwks.json` public
// key set. The actual cloud-side WIF exchange is out of scope for the
// foundation and lives on the provider (see the package README/TODOs).
package mint

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// DefaultMaxTTL is the default upper bound on a minted token's lifetime.
	// Callers may request less; requests above this are clamped down.
	DefaultMaxTTL = 15 * time.Minute

	// HardCapTTL is the absolute ceiling on token lifetime regardless of the
	// configured max_ttl. No minted token may ever live longer than this — it
	// bounds the blast radius of a leaked short-lived credential.
	HardCapTTL = 1 * time.Hour

	// MinTTL is the smallest usable lifetime. A non-positive requested TTL
	// falls back to the configured max.
	MinTTL = 1 * time.Minute

	// signingAlg is the JWT signing algorithm. RS256 matches the RSA key used
	// elsewhere in hive (pkg/github/app.go) and is universally accepted by WIF
	// providers.
	signingAlg = "RS256"

	// jwtKeyType / jwtUse describe the JWKS entry for an RSA signature key.
	jwtKeyType = "RSA"
	jwtUse     = "sig"

	// clockSkewLeeway tolerates small clock differences between the mint and
	// verifiers when checking exp/iat/nbf.
	clockSkewLeeway = 30 * time.Second

	// generatedKeyBits is the modulus size for keys this package generates.
	generatedKeyBits = 2048

	// keyFilePerms restricts a persisted private key to owner read/write only.
	keyFilePerms = 0o600

	// ScopesClaim is the custom JWT claim carrying the granted scope strings.
	ScopesClaim = "scopes"
	// HiveIDClaim carries the id of the hive that minted the token.
	HiveIDClaim = "hive_id"
)

// Claims is the decoded payload of a minted token: the standard registered
// claims (iss, sub, aud, exp, iat, jti) plus hive-specific scopes and hive id.
type Claims struct {
	jwt.RegisteredClaims
	Scopes []string `json:"scopes,omitempty"`
	HiveID string   `json:"hive_id,omitempty"`
}

// Minter signs and verifies scoped short-lived JWTs for a single issuer.
type Minter struct {
	key    *rsa.PrivateKey
	issuer string
	hiveID string
	keyID  string
	maxTTL time.Duration
}

// Option configures a Minter.
type Option func(*Minter)

// WithHiveID sets the hive id embedded in the hive_id claim.
func WithHiveID(id string) Option { return func(m *Minter) { m.hiveID = id } }

// WithMaxTTL sets the configured max TTL. It is silently clamped into
// [MinTTL, HardCapTTL] so a misconfiguration can never widen the hard cap.
func WithMaxTTL(d time.Duration) Option {
	return func(m *Minter) {
		if d <= 0 {
			d = DefaultMaxTTL
		}
		if d < MinTTL {
			d = MinTTL
		}
		if d > HardCapTTL {
			d = HardCapTTL
		}
		m.maxTTL = d
	}
}

// NewMinter builds a Minter from an existing RSA private key. issuer must be
// non-empty (it is verified on every token and configured into WIF providers).
func NewMinter(key *rsa.PrivateKey, issuer string, opts ...Option) (*Minter, error) {
	if key == nil {
		return nil, fmt.Errorf("mint: nil signing key")
	}
	if issuer == "" {
		return nil, fmt.Errorf("mint: issuer is required")
	}
	m := &Minter{
		key:    key,
		issuer: issuer,
		maxTTL: DefaultMaxTTL,
		keyID:  keyIDFor(&key.PublicKey),
	}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Mint issues a signed JWT for subject with the given audience and scopes.
// The requested ttl is clamped into [MinTTL, min(maxTTL, HardCapTTL)]: a
// non-positive ttl uses the configured max, and any ttl above the cap is
// reduced to the cap (never denied — but never honored above the ceiling).
func (m *Minter) Mint(subject, audience string, scopes []string, ttl time.Duration) (string, error) {
	if subject == "" {
		return "", fmt.Errorf("mint: subject is required")
	}
	if audience == "" {
		return "", fmt.Errorf("mint: audience is required")
	}

	ttl = m.clampTTL(ttl)

	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   subject,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-clockSkewLeeway)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        uuid.NewString(),
		},
		Scopes: append([]string(nil), scopes...),
		HiveID: m.hiveID,
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = m.keyID
	signed, err := tok.SignedString(m.key)
	if err != nil {
		return "", fmt.Errorf("mint: signing token: %w", err)
	}
	return signed, nil
}

// clampTTL enforces the TTL policy. min(maxTTL, HardCapTTL) is the effective
// ceiling; MinTTL is the floor; a non-positive request falls back to the max.
func (m *Minter) clampTTL(ttl time.Duration) time.Duration {
	ceiling := m.maxTTL
	if ceiling > HardCapTTL {
		ceiling = HardCapTTL
	}
	if ttl <= 0 || ttl > ceiling {
		ttl = ceiling
	}
	if ttl < MinTTL {
		ttl = MinTTL
	}
	return ttl
}

// MaxTTL returns the effective ceiling this minter will honor.
func (m *Minter) MaxTTL() time.Duration {
	if m.maxTTL > HardCapTTL {
		return HardCapTTL
	}
	return m.maxTTL
}

// Verify validates a token's signature, algorithm, expiry, and issuer, and
// returns the decoded claims. It fails closed: any parse/validation error, an
// unexpected signing method, or an issuer mismatch yields a nil claims and a
// non-nil error.
func (m *Minter) Verify(token string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims,
		func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("mint: unexpected signing method %q", t.Header["alg"])
			}
			return &m.key.PublicKey, nil
		},
		jwt.WithValidMethods([]string{signingAlg}),
		jwt.WithIssuer(m.issuer),
		jwt.WithLeeway(clockSkewLeeway),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("mint: verifying token: %w", err)
	}
	if !parsed.Valid {
		return nil, fmt.Errorf("mint: token invalid")
	}
	return claims, nil
}

// jwk is a single RSA public key in JSON Web Key form.
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// JWKS returns the public key set in standard `.well-known/jwks.json` shape so
// verifiers (and WIF providers) can validate tokens this minter issues.
func (m *Minter) JWKS() ([]byte, error) {
	pub := &m.key.PublicKey
	set := jwkSet{Keys: []jwk{{
		Kty: jwtKeyType,
		Use: jwtUse,
		Alg: signingAlg,
		Kid: m.keyID,
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	b, err := json.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("mint: marshaling JWKS: %w", err)
	}
	return b, nil
}

// keyIDFor derives a stable, deterministic key id from the public modulus.
// A base64url SHA-256-free thumbprint is overkill for the foundation; we use a
// short hash of the modulus so redeploys with the same key keep the same kid.
func keyIDFor(pub *rsa.PublicKey) string {
	sum := pub.N.Bytes()
	const kidBytes = 8
	if len(sum) > kidBytes {
		sum = sum[:kidBytes]
	}
	return base64.RawURLEncoding.EncodeToString(sum)
}

// GenerateKey creates a fresh RSA signing key of generatedKeyBits.
func GenerateKey() (*rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, generatedKeyBits)
	if err != nil {
		return nil, fmt.Errorf("mint: generating key: %w", err)
	}
	return key, nil
}

// LoadOrCreateKey reads a PKCS#8 PEM private key from path. If the file does
// not exist it generates one, writes it with keyFilePerms (0600), and returns
// it. This is the persistence helper for the mint's signing key. path must be
// non-empty; secrets are never hardcoded — the caller supplies the path from
// config/env.
func LoadOrCreateKey(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		return nil, fmt.Errorf("mint: key path is required")
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, perr := parsePEMKey(data)
		if perr != nil {
			return nil, fmt.Errorf("mint: parsing key %s: %w", path, perr)
		}
		return key, nil
	case os.IsNotExist(err):
		// fall through to generate + persist
	default:
		return nil, fmt.Errorf("mint: reading key %s: %w", path, err)
	}

	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	if err := writeKeyPEM(path, key); err != nil {
		return nil, err
	}
	return key, nil
}

func parsePEMKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key is not RSA")
		}
		return rsaKey, nil
	}
	// Fall back to PKCS1 for keys written by other tooling.
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key (PKCS8 and PKCS1 both failed): %w", err)
	}
	return key, nil
}

// writeKeyPEM writes key as a PKCS#8 PEM file with 0600 perms, atomically via a
// temp file + rename so a partial write is never left in place.
func writeKeyPEM(path string, key *rsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("mint: marshaling key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pemBytes, keyFilePerms); err != nil {
		return fmt.Errorf("mint: writing key %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, keyFilePerms); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the chmod error is what's returned
		return fmt.Errorf("mint: chmod key %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the rename error is what's returned
		return fmt.Errorf("mint: renaming key into place: %w", err)
	}
	return nil
}
