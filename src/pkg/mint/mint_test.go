package mint

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer = "https://hive.example.test/mint"
	testHiveID = "hive-test-1"
	testSub    = "spoke-alpha"
	testAud    = "//iam.googleapis.com/projects/123/locations/global/wif/providers/hive"
)

func newTestMinter(t *testing.T, opts ...Option) (*Minter, *rsa.PrivateKey) {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	base := []Option{WithHiveID(testHiveID)}
	m, err := NewMinter(key, testIssuer, append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return m, key
}

func TestMintVerifyRoundTrip(t *testing.T) {
	m, _ := newTestMinter(t)
	scopes := []string{"registry:pull", "gcs:read"}

	tok, err := m.Mint(testSub, testAud, scopes, 5*time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Issuer != testIssuer {
		t.Errorf("issuer = %q, want %q", claims.Issuer, testIssuer)
	}
	if claims.Subject != testSub {
		t.Errorf("subject = %q, want %q", claims.Subject, testSub)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != testAud {
		t.Errorf("audience = %v, want [%q]", claims.Audience, testAud)
	}
	if claims.HiveID != testHiveID {
		t.Errorf("hive_id = %q, want %q", claims.HiveID, testHiveID)
	}
	if claims.ID == "" {
		t.Error("jti (ID) is empty")
	}
	if claims.ExpiresAt == nil || claims.IssuedAt == nil {
		t.Fatal("exp/iat missing")
	}
}

func TestScopesPreserved(t *testing.T) {
	m, _ := newTestMinter(t)
	scopes := []string{"a", "b", "c"}
	tok, err := m.Mint(testSub, testAud, scopes, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if strings.Join(claims.Scopes, ",") != "a,b,c" {
		t.Errorf("scopes = %v, want [a b c]", claims.Scopes)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	m, key := newTestMinter(t)
	// Hand-craft an already-expired token signed with the same key so we bypass
	// Mint's TTL floor and test Verify's expiry enforcement directly.
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    testIssuer,
			Subject:   testSub,
			Audience:  jwt.ClaimStrings{testAud},
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-1 * time.Hour)),
			ID:        "expired-jti",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := m.Verify(signed); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestWrongIssuerRejected(t *testing.T) {
	m, _ := newTestMinter(t)
	// A second minter with the SAME key but a different issuer.
	other, err := NewMinter(m.key, "https://evil.example/mint", WithHiveID(testHiveID))
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	tok, err := other.Mint(testSub, testAud, nil, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := m.Verify(tok); err == nil {
		t.Fatal("expected wrong-issuer token to be rejected")
	}
}

func TestWrongSignatureRejected(t *testing.T) {
	m, _ := newTestMinter(t)
	// Token minted by a DIFFERENT key but claiming our issuer.
	attacker, _ := newTestMinter(t)
	attacker.issuer = testIssuer
	tok, err := attacker.Mint(testSub, testAud, nil, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := m.Verify(tok); err == nil {
		t.Fatal("expected wrong-signature token to be rejected")
	}
}

func TestAlgNoneRejected(t *testing.T) {
	m, _ := newTestMinter(t)
	// Forge an alg=none token — must be rejected (fail closed).
	claims := Claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   testSub,
		Audience:  jwt.ClaimStrings{testAud},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := m.Verify(signed); err == nil {
		t.Fatal("expected alg=none token to be rejected")
	}
}

func TestTTLCapEnforced(t *testing.T) {
	// Configure a small max; request far above the hard cap.
	m, _ := newTestMinter(t, WithMaxTTL(10*time.Minute))
	before := time.Now()
	tok, err := m.Mint(testSub, testAud, nil, 999*time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, err := m.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	life := claims.ExpiresAt.Sub(before)
	if life > 10*time.Minute+clockSkewLeeway {
		t.Errorf("token life %v exceeds configured max 10m", life)
	}
}

func TestHardCapAlwaysWins(t *testing.T) {
	// Even a misconfigured max above the hard cap is clamped.
	m, _ := newTestMinter(t, WithMaxTTL(999*time.Hour))
	if m.MaxTTL() != HardCapTTL {
		t.Errorf("MaxTTL = %v, want hard cap %v", m.MaxTTL(), HardCapTTL)
	}
	before := time.Now()
	tok, err := m.Mint(testSub, testAud, nil, 999*time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	claims, _ := m.Verify(tok)
	life := claims.ExpiresAt.Sub(before)
	if life > HardCapTTL+clockSkewLeeway {
		t.Errorf("token life %v exceeds hard cap %v", life, HardCapTTL)
	}
}

func TestMintRequiresSubjectAndAudience(t *testing.T) {
	m, _ := newTestMinter(t)
	if _, err := m.Mint("", testAud, nil, time.Minute); err == nil {
		t.Error("expected error for empty subject")
	}
	if _, err := m.Mint(testSub, "", nil, time.Minute); err == nil {
		t.Error("expected error for empty audience")
	}
}

func TestNewMinterValidation(t *testing.T) {
	key, _ := GenerateKey()
	if _, err := NewMinter(nil, testIssuer); err == nil {
		t.Error("expected error for nil key")
	}
	if _, err := NewMinter(key, ""); err == nil {
		t.Error("expected error for empty issuer")
	}
}

func TestJWKSValidatesMintedToken(t *testing.T) {
	m, _ := newTestMinter(t)
	tok, err := m.Mint(testSub, testAud, []string{"x"}, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	jwksBytes, err := m.JWKS()
	if err != nil {
		t.Fatalf("JWKS: %v", err)
	}

	var set struct {
		Keys []struct {
			Kty, Use, Alg, Kid, N, E string
		} `json:"keys"`
	}
	if err := json.Unmarshal(jwksBytes, &set); err != nil {
		t.Fatalf("JWKS parse: %v", err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("JWKS keys = %d, want 1", len(set.Keys))
	}
	k := set.Keys[0]
	if k.Kty != "RSA" || k.Use != "sig" || k.Alg != "RS256" {
		t.Errorf("unexpected JWKS entry: %+v", k)
	}
	if k.Kid == "" || k.N == "" || k.E == "" {
		t.Errorf("JWKS entry missing kid/n/e: %+v", k)
	}

	// Reconstruct the public key from JWKS and verify the token independently.
	pub := rebuildPublicKey(t, k.N, k.E)
	parsed, err := jwt.Parse(tok, func(tk *jwt.Token) (interface{}, error) {
		if _, ok := tk.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil || !parsed.Valid {
		t.Fatalf("token failed JWKS-reconstructed verification: err=%v valid=%v", err, parsed.Valid)
	}

	// kid in JWKS matches token header.
	unverified, _, _ := jwt.NewParser().ParseUnverified(tok, jwt.MapClaims{})
	if kid, _ := unverified.Header["kid"].(string); kid != k.Kid {
		t.Errorf("token kid %q != JWKS kid %q", kid, k.Kid)
	}
}

func rebuildPublicKey(t *testing.T, nB64, eB64 string) *rsa.PublicKey {
	t.Helper()
	nBytes := mustB64URL(t, nB64)
	eBytes := mustB64URL(t, eB64)
	n := new(big.Int).SetBytes(nBytes)
	var e int
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	return &rsa.PublicKey{N: n, E: e}
}

func mustB64URL(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode %q: %v", s, err)
	}
	return b
}

func TestLoadOrCreateKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mint.pem")

	// Create.
	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey (create): %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key perms = %o, want 600", perm)
	}

	// Load existing — must be the same key.
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey (load): %v", err)
	}
	if k1.N.Cmp(k2.N) != 0 {
		t.Error("reloaded key differs from persisted key")
	}

	if _, err := LoadOrCreateKey(""); err == nil {
		t.Error("expected error for empty key path")
	}
}

// --- Additional coverage: key load/generate/persist paths ---

func TestLoadOrCreateKeyCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.pem")
	if err := os.WriteFile(path, []byte("this is not a PEM block at all"), keyFilePerms); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("expected error loading a corrupt key file")
	} else if !strings.Contains(err.Error(), "parsing key") {
		t.Errorf("unexpected error %q, want a parse error", err)
	}
}

func TestLoadOrCreateKeyUnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "noread.pem")
	// A valid-looking file that we make unreadable so ReadFile returns a
	// non-IsNotExist error (the default: branch).
	if err := os.WriteFile(path, []byte("x"), 0o000); err != nil {
		t.Fatalf("seed unreadable file: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, keyFilePerms) })
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("expected error reading an unreadable key file")
	} else if !strings.Contains(err.Error(), "reading key") {
		t.Errorf("unexpected error %q, want a read error", err)
	}
}

func TestLoadOrCreateKeyWriteFailsWhenDirMissing(t *testing.T) {
	// Path whose parent directory does not exist forces writeKeyPEM to fail on
	// the temp WriteFile, exercising its error branch (no partial file left).
	dir := t.TempDir()
	path := filepath.Join(dir, "no-such-subdir", "mint.pem")
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("expected error persisting key into a missing directory")
	} else if !strings.Contains(err.Error(), "writing key") {
		t.Errorf("unexpected error %q, want a write error", err)
	}
	if _, statErr := os.Stat(path + ".tmp"); statErr == nil {
		t.Error("a partial temp file was left behind")
	}
}

func TestParsePEMKeyPKCS1RoundTrip(t *testing.T) {
	// A key written in PKCS#1 (as other tooling might) must load via the
	// fallback path in parsePEMKey.
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})

	dir := t.TempDir()
	path := filepath.Join(dir, "pkcs1.pem")
	if err := os.WriteFile(path, pemBytes, keyFilePerms); err != nil {
		t.Fatalf("write PKCS1 pem: %v", err)
	}
	loaded, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey (PKCS1): %v", err)
	}
	if loaded.N.Cmp(key.N) != 0 {
		t.Error("PKCS1-loaded key differs from original")
	}
}

func TestParsePEMKeyEmptyBlock(t *testing.T) {
	if _, err := parsePEMKey([]byte("garbage without pem")); err == nil {
		t.Fatal("expected error for missing PEM block")
	} else if !strings.Contains(err.Error(), "no PEM block") {
		t.Errorf("unexpected error %q", err)
	}
}

func TestParsePEMKeyNonRSAPKCS8(t *testing.T) {
	// A valid PKCS#8 key that is NOT RSA (an EC key) must be rejected.
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ec keygen: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatalf("marshal ec pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := parsePEMKey(pemBytes); err == nil {
		t.Fatal("expected error for non-RSA PKCS8 key")
	} else if !strings.Contains(err.Error(), "not RSA") {
		t.Errorf("unexpected error %q", err)
	}
}

func TestParsePEMKeyBadDER(t *testing.T) {
	// A PEM block whose bytes are neither valid PKCS8 nor PKCS1.
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{0x00, 0x01, 0x02, 0x03}})
	if _, err := parsePEMKey(pemBytes); err == nil {
		t.Fatal("expected error for undecodable DER")
	} else if !strings.Contains(err.Error(), "PKCS8 and PKCS1 both failed") {
		t.Errorf("unexpected error %q", err)
	}
}

// --- Additional coverage: TTL policy edges ---

func TestWithMaxTTLClamps(t *testing.T) {
	// Non-positive falls back to DefaultMaxTTL.
	m1, _ := newTestMinter(t, WithMaxTTL(0))
	if m1.MaxTTL() != DefaultMaxTTL {
		t.Errorf("MaxTTL(0) = %v, want default %v", m1.MaxTTL(), DefaultMaxTTL)
	}
	m1n, _ := newTestMinter(t, WithMaxTTL(-5*time.Minute))
	if m1n.MaxTTL() != DefaultMaxTTL {
		t.Errorf("MaxTTL(negative) = %v, want default %v", m1n.MaxTTL(), DefaultMaxTTL)
	}
	// Below MinTTL floors up to MinTTL.
	m2, _ := newTestMinter(t, WithMaxTTL(1*time.Second))
	if m2.MaxTTL() != MinTTL {
		t.Errorf("MaxTTL(1s) = %v, want floor %v", m2.MaxTTL(), MinTTL)
	}
	// A normal value in range is preserved and returned by MaxTTL directly.
	m3, _ := newTestMinter(t, WithMaxTTL(7*time.Minute))
	if m3.MaxTTL() != 7*time.Minute {
		t.Errorf("MaxTTL(7m) = %v, want 7m", m3.MaxTTL())
	}
}

func TestClampTTLFloorApplied(t *testing.T) {
	// With a max of exactly MinTTL, requesting a tiny positive ttl clamps up to
	// MinTTL (exercises the ttl < MinTTL branch on an in-range request).
	m, _ := newTestMinter(t, WithMaxTTL(MinTTL))
	got := m.clampTTL(1 * time.Second)
	if got != MinTTL {
		t.Errorf("clampTTL(1s) = %v, want %v", got, MinTTL)
	}
	// A request equal to the ceiling is honored unchanged.
	m2, _ := newTestMinter(t, WithMaxTTL(10*time.Minute))
	if got := m2.clampTTL(5 * time.Minute); got != 5*time.Minute {
		t.Errorf("clampTTL(5m) = %v, want 5m", got)
	}
}

func TestMintJTIUnique(t *testing.T) {
	m, _ := newTestMinter(t)
	t1, err := m.Mint(testSub, testAud, nil, time.Minute)
	if err != nil {
		t.Fatalf("Mint 1: %v", err)
	}
	t2, err := m.Mint(testSub, testAud, nil, time.Minute)
	if err != nil {
		t.Fatalf("Mint 2: %v", err)
	}
	c1, err := m.Verify(t1)
	if err != nil {
		t.Fatalf("Verify 1: %v", err)
	}
	c2, err := m.Verify(t2)
	if err != nil {
		t.Fatalf("Verify 2: %v", err)
	}
	if c1.ID == "" || c2.ID == "" {
		t.Fatal("jti empty")
	}
	if c1.ID == c2.ID {
		t.Errorf("jti not unique across mints: %q", c1.ID)
	}
}

// --- Additional coverage: Verify failure branches ---

func TestVerifyMalformedToken(t *testing.T) {
	m, _ := newTestMinter(t)
	for _, tok := range []string{"", "not.a.jwt", "aaa.bbb.ccc", "onlyonesegment"} {
		if _, err := m.Verify(tok); err == nil {
			t.Errorf("expected malformed token %q to be rejected", tok)
		}
	}
}

func TestVerifyTamperedSignature(t *testing.T) {
	m, _ := newTestMinter(t)
	tok, err := m.Mint(testSub, testAud, nil, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape")
	}
	// Decode the signature, flip a byte, and re-encode so the change survives
	// base64 canonicalization (flipping a raw base64 char can decode to the
	// same bytes and leave the signature unchanged).
	rawSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	rawSig[0] ^= 0xFF
	tampered := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(rawSig)
	if _, err := m.Verify(tampered); err == nil {
		t.Fatal("expected tampered-signature token to be rejected")
	}
}

func TestVerifyWrongKeyRejected(t *testing.T) {
	m, _ := newTestMinter(t)
	// A verifier with a different key but the same issuer must reject.
	otherKey, _ := GenerateKey()
	verifier, err := NewMinter(otherKey, testIssuer, WithHiveID(testHiveID))
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	tok, err := m.Mint(testSub, testAud, nil, time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := verifier.Verify(tok); err == nil {
		t.Fatal("expected token verified with wrong key to be rejected")
	}
}

func TestVerifyUnexpectedSigningMethodHMAC(t *testing.T) {
	m, _ := newTestMinter(t)
	// Forge an HMAC (HS256) token — Verify must reject the non-RSA method.
	claims := Claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer:    testIssuer,
		Subject:   testSub,
		Audience:  jwt.ClaimStrings{testAud},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString([]byte("attacker-hmac-secret"))
	if err != nil {
		t.Fatalf("sign hs256: %v", err)
	}
	if _, err := m.Verify(signed); err == nil {
		t.Fatal("expected HS256 token to be rejected by RSA verifier")
	}
}

func TestMaxTTLReportsEffectiveCeiling(t *testing.T) {
	m, _ := newTestMinter(t)
	if got := m.MaxTTL(); got != DefaultMaxTTL {
		t.Errorf("MaxTTL() = %v, want default %v", got, DefaultMaxTTL)
	}
	// A maxTTL beyond the hard cap (only reachable by construction bugs —
	// WithMaxTTL clamps) must still report the hard cap, never above it.
	m.maxTTL = 2 * HardCapTTL
	if got := m.MaxTTL(); got != HardCapTTL {
		t.Errorf("MaxTTL() with oversized maxTTL = %v, want %v", got, HardCapTTL)
	}
}

func TestClampTTLFloorsAtMinTTL(t *testing.T) {
	m, _ := newTestMinter(t)
	if got := m.clampTTL(time.Second); got != MinTTL {
		t.Errorf("clampTTL(1s) = %v, want floor %v", got, MinTTL)
	}
}

func TestLoadOrCreateKeyReadErrorFailsClosed(t *testing.T) {
	// A directory at the key path yields a read error that is not
	// IsNotExist — LoadOrCreateKey must fail rather than overwrite.
	dir := t.TempDir()
	if _, err := LoadOrCreateKey(dir); err == nil {
		t.Fatal("expected error when key path is a directory")
	}
}

func TestClampTTLCeilingNeverExceedsHardCap(t *testing.T) {
	m, _ := newTestMinter(t)
	// Only reachable by construction bugs — WithMaxTTL clamps — but clampTTL
	// must still cap at HardCapTTL on its own.
	m.maxTTL = 2 * HardCapTTL
	if got := m.clampTTL(0); got != HardCapTTL {
		t.Errorf("clampTTL(0) with oversized maxTTL = %v, want %v", got, HardCapTTL)
	}
}

func TestWriteKeyPEMRenameFailsOntoDirectory(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	// The destination is an existing directory: the temp write succeeds but
	// the final rename cannot land, exercising the rename error branch.
	dir := t.TempDir()
	if err := writeKeyPEM(dir, key); err == nil {
		t.Fatal("expected rename onto a directory to fail")
	} else if !strings.Contains(err.Error(), "renaming key") {
		t.Errorf("unexpected error %q, want a rename error", err)
	}
	if _, statErr := os.Stat(dir + ".tmp"); statErr == nil {
		t.Error("temp file was not cleaned up after rename failure")
	}
}
