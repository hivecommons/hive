package forgejwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testIssuer Issuer = "https://hive-keys.example.org"

var testKeys = sync.OnceValue(func() [2]*rsa.PrivateKey {
	var out [2]*rsa.PrivateKey
	for i := range out {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		out[i] = k
	}
	return out
})

func pemBytes(t *testing.T, k any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func writeFile(t *testing.T, data []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func decodeSegment(t *testing.T, seg string, v any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("segment is not base64url: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("segment is not JSON: %v", err)
	}
}

// verify checks token against a rendered key list the way a verifier does:
// pick the key by kid, rebuild it from n and e, check the RS256 signature.
func verify(t *testing.T, token string, jwks []byte) error {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	var h map[string]string
	decodeSegment(t, parts[0], &h)
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &set); err != nil {
		t.Fatal(err)
	}
	for _, k := range set.Keys {
		if k["kid"] != h["kid"] {
			continue
		}
		if k["kty"] != "RSA" || k["alg"] != "RS256" || h["alg"] != "RS256" {
			t.Fatalf("key %v, header %v", k, h)
		}
		n, _ := base64.RawURLEncoding.DecodeString(k["n"])
		e, _ := base64.RawURLEncoding.DecodeString(k["e"])
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig)
	}
	return errors.New("no key with the token's kid")
}

func TestMintClaimSet(t *testing.T) {
	key := testKeys()[0]
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	s := Signer{Issuer: testIssuer, KeyFile: writeFile(t, pemBytes(t, key), 0o600), Now: func() time.Time { return now }}

	token, err := s.Mint("u:7:3f0c", "hive:advisor")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	var h, c map[string]any
	decodeSegment(t, parts[0], &h)
	decodeSegment(t, parts[1], &c)

	wantClaims := map[string]any{
		"iss": "https://hive-keys.example.org",
		"aud": "u:7:3f0c",
		"sub": "hive:advisor",
		"iat": float64(now.Unix() - 60),
		"exp": float64(now.Unix() + 3600),
	}
	if !reflect.DeepEqual(c, wantClaims) {
		t.Errorf("claims = %v, want %v", c, wantClaims)
	}
	_, jwks, err := KeyList(testIssuer, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Keys []struct{ Kid string } `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &set); err != nil {
		t.Fatal(err)
	}
	wantHeader := map[string]any{"alg": "RS256", "typ": "JWT", "kid": set.Keys[0].Kid}
	if !reflect.DeepEqual(h, wantHeader) {
		t.Errorf("header = %v, want %v", h, wantHeader)
	}

	again, err := s.Mint("u:7:3f0c", "hive:advisor")
	if err != nil {
		t.Fatal(err)
	}
	if again != token {
		t.Error("two mints at the same instant differ; the token must be deterministic")
	}
}

func TestMintRejectsEmptyInputs(t *testing.T) {
	s := Signer{Issuer: testIssuer, KeyFile: writeFile(t, pemBytes(t, testKeys()[0]), 0o600)}
	if _, err := s.Mint("", "hive:advisor"); err == nil {
		t.Error("empty audience minted a token")
	}
	if _, err := s.Mint("u:7:3f0c", ""); err == nil {
		t.Error("empty subject minted a token")
	}
}

func TestMintVerifiesAgainstKeyList(t *testing.T) {
	keys := testKeys()
	oldFile := writeFile(t, pemBytes(t, keys[0]), 0o600)
	newFile := writeFile(t, pemBytes(t, keys[1]), 0o400)
	var pubs []*rsa.PublicKey
	for _, f := range []string{oldFile, newFile} {
		p, err := PublicKey(f)
		if err != nil {
			t.Fatal(err)
		}
		pubs = append(pubs, p)
	}
	discovery, both, err := KeyList(testIssuer, pubs...)
	if err != nil {
		t.Fatal(err)
	}
	_, onlyOld, err := KeyList(testIssuer, pubs[0])
	if err != nil {
		t.Fatal(err)
	}

	var d map[string]any
	if err := json.Unmarshal(discovery, &d); err != nil {
		t.Fatal(err)
	}
	wantDiscovery := map[string]any{
		"issuer":                                "https://hive-keys.example.org",
		"jwks_uri":                              "https://hive-keys.example.org/jwks.json",
		"id_token_signing_alg_values_supported": []any{"RS256"},
	}
	if !reflect.DeepEqual(d, wantDiscovery) {
		t.Errorf("discovery = %v, want %v", d, wantDiscovery)
	}

	for _, f := range []string{oldFile, newFile} {
		tok, err := Signer{Issuer: testIssuer, KeyFile: f}.Mint("u:7:3f0c", "hive:advisor")
		if err != nil {
			t.Fatal(err)
		}
		if err := verify(t, tok, both); err != nil {
			t.Errorf("token from %s does not verify against the rotation key list: %v", f, err)
		}
	}
	tok, err := Signer{Issuer: testIssuer, KeyFile: newFile}.Mint("u:7:3f0c", "hive:advisor")
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(t, tok, onlyOld); err == nil {
		t.Error("a token from the new key verified against a list holding only the old key")
	}
	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"hive:contributor"}`)) + "." + parts[2]
	if err := verify(t, tampered, both); err == nil {
		t.Error("a token with altered claims verified")
	}
}

func TestKeyListRejectsDuplicateAndEmpty(t *testing.T) {
	pub := &testKeys()[0].PublicKey
	if _, _, err := KeyList(testIssuer, pub, pub); err == nil {
		t.Error("a key listed twice rendered")
	}
	if _, _, err := KeyList(testIssuer); err == nil {
		t.Error("an empty key list rendered")
	}
}

func TestKeyFileModeIsRefused(t *testing.T) {
	data := pemBytes(t, testKeys()[0])
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o601} {
		f := writeFile(t, data, mode)
		if _, err := (Signer{Issuer: testIssuer, KeyFile: f}).Mint("u:7:3f0c", "hive:advisor"); !errors.Is(err, ErrKeyFileMode) {
			t.Errorf("Mint with mode %#o: err = %v, want ErrKeyFileMode", mode, err)
		}
		if _, err := PublicKey(f); !errors.Is(err, ErrKeyFileMode) {
			t.Errorf("PublicKey with mode %#o: err = %v, want ErrKeyFileMode", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if _, err := PublicKey(writeFile(t, data, mode)); err != nil {
			t.Errorf("mode %#o refused: %v", mode, err)
		}
	}
}

func TestErrorsNeverCarryTheKey(t *testing.T) {
	key := testKeys()[0]
	good := pemBytes(t, key)
	block, _ := pem.Decode(good)
	corrupt := append([]byte(nil), block.Bytes...)
	corrupt[len(corrupt)/2] ^= 0xff
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		data []byte
		mode os.FileMode
	}{
		"world readable": {good, 0o644},
		"truncated":      {good[:len(good)/2], 0o600},
		"corrupt der":    {pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: corrupt}), 0o600},
		"pkcs1 label":    {pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: corrupt}), 0o600},
		"bare base64":    {good[len("-----BEGIN PRIVATE KEY-----\n"):], 0o600},
		"1024 bit":       {pemBytes(t, small), 0o600},
		"ec key":         {pemBytes(t, ec), 0o600},
	}
	secrets := []string{key.D.String(), key.D.Text(16), key.Primes[0].String(), key.Primes[0].Text(16)}
	body := strings.ReplaceAll(string(good[len("-----BEGIN PRIVATE KEY-----\n"):len(good)-len("-----END PRIVATE KEY-----\n")]), "\n", "")
	for i := 0; i+12 <= len(body); i += 6 {
		secrets = append(secrets, body[i:i+12])
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := writeFile(t, c.data, c.mode)
			_, mintErr := Signer{Issuer: testIssuer, KeyFile: f}.Mint("u:7:3f0c", "hive:advisor")
			_, pubErr := PublicKey(f)
			for _, err := range []error{mintErr, pubErr} {
				if err == nil {
					t.Fatal("a bad key file loaded")
				}
				for _, s := range secrets {
					if strings.Contains(err.Error(), s) {
						t.Fatalf("error %q contains key material %q", err, s)
					}
				}
			}
		})
	}
}

func TestParseIssuer(t *testing.T) {
	for _, ok := range []string{"https://hive-keys.example.org", "https://keys.example.org:8443"} {
		if _, err := ParseIssuer(ok); err != nil {
			t.Errorf("ParseIssuer(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "http://keys.example.org", "https://", "https://keys.example.org/",
		"https://keys.example.org/hive", "https://keys.example.org?a=b",
		"https://keys.example.org#x", "https://u:p@keys.example.org", "keys.example.org",
	} {
		if _, err := ParseIssuer(bad); err == nil {
			t.Errorf("ParseIssuer(%q) accepted", bad)
		}
	}
}
