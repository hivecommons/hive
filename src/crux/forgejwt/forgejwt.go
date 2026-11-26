// Package forgejwt signs the JWTs a Forgejo authorized integration accepts,
// and renders the discovery document and key list Forgejo fetches to verify
// them. It uses the standard library and crux/keystore.
//
// Forgejo verifies a token against the issuer's key list, which an operator
// serves as two static files from an https host Forgejo trusts. The signing
// key stays in a file on the spoke. A key under the data volume is sealed in
// place by the boot keystore sweep, so the key file may hold either a PEM key
// or a keystore envelope around one.
package forgejwt

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/hivecommons/hive/crux/keystore"
)

const (
	// Lifetime is how long a minted token stays valid after it is minted.
	Lifetime = time.Hour
	// Backdate moves iat into the past, so a token from a clock up to 30 s
	// ahead of Forgejo's is not rejected as issued in the future.
	Backdate = time.Minute

	// DiscoveryPath and KeyListPath are where the issuer serves the two
	// files KeyList renders, relative to the issuer URL.
	DiscoveryPath = ".well-known/openid-configuration"
	KeyListPath   = "jwks.json"

	// LaunchGID is the hive-launch group, pinned in src/Dockerfile and used
	// as the pod fsGroup in src/deploy/k8s. Its only member is dev; agent
	// UIDs share group node (GID 1000) instead, so a key that hive-launch
	// can read stays out of their reach.
	LaunchGID = 1002

	alg         = "RS256"
	minKeyBits  = 2048
	maxKeyBytes = 64 << 10

	ownerOnlyMask fs.FileMode = 0o077
	otherMask     fs.FileMode = 0o007
	groupMask     fs.FileMode = 0o070
	groupRead     fs.FileMode = 0o040
)

// ErrKeyFileMode is returned for a signing key file that others can read, or
// that a group other than hive-launch can read, or that any group can write.
var ErrKeyFileMode = errors.New("signing key file is readable beyond its owner and the hive-launch group")

// launchGID is LaunchGID; tests point it at their own GID so a group-read
// file they create can stand in for the shipped Secret mount.
var launchGID uint32 = LaunchGID

// keyModeAllowed reports whether a key file at perm owned by group gid may be
// read. Owner-only modes always pass. Group read (and nothing else for the
// group or for others) passes only when the group is hive-launch, which is
// how the Deployment mounts /secrets (defaultMode 0440, fsGroup 1002). Group
// read under any other group, node above all, would expose the key to every
// agent UID.
func keyModeAllowed(perm fs.FileMode, gid uint32, haveGID bool) bool {
	if perm&ownerOnlyMask == 0 {
		return true
	}
	return perm&otherMask == 0 && perm&groupMask == groupRead && haveGID && gid == launchGID
}

// fileGID returns the owning group of info, when the platform reports one.
func fileGID(info fs.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return st.Gid, true
}

// Issuer is an https URL with no path, query or fragment. It is the iss claim
// and the base URL of the discovery document.
type Issuer string

// ParseIssuer checks s and returns it as an Issuer.
func ParseIssuer(s string) (Issuer, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("forgejwt: issuer %q: %w", s, err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("forgejwt: issuer %q must be https://host[:port] with no path, query or fragment", s)
	}
	return Issuer(s), nil
}

// Audience is the aud value Forgejo generated for one authorized integration.
type Audience string

// Signer mints tokens for one issuer. The key file is read at each Mint, so
// replacing the file rotates the key.
type Signer struct {
	Issuer  Issuer
	KeyFile string
	Now     func() time.Time // nil means time.Now
}

// Mint returns a compact RS256 JWT for one integration. The header carries
// alg, typ and kid, the RFC 7638 thumbprint of the public key. The claims
// are iss, aud, sub, iat = now - Backdate and exp = now + Lifetime.
func (s Signer) Mint(aud Audience, subject string) (string, error) {
	if aud == "" {
		return "", errors.New("forgejwt: audience is empty")
	}
	if subject == "" {
		return "", errors.New("forgejwt: subject is empty")
	}
	if _, err := ParseIssuer(string(s.Issuer)); err != nil {
		return "", fmt.Errorf("forgejwt: issuer is not a valid https origin: %w", err)
	}
	key, err := readKey(s.KeyFile)
	if err != nil {
		return "", err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	t := now()
	return sign(key, claims{
		Iss: s.Issuer,
		Aud: aud,
		Sub: subject,
		Iat: t.Add(-Backdate).Unix(),
		Exp: t.Add(Lifetime).Unix(),
	})
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type claims struct {
	Iss Issuer   `json:"iss"`
	Aud Audience `json:"aud"`
	Sub string   `json:"sub"`
	Iat int64    `json:"iat"`
	Exp int64    `json:"exp"`
}

func sign(key *rsa.PrivateKey, payload any) (string, error) {
	h, err := json.Marshal(header{Alg: alg, Kid: keyID(&key.PublicKey), Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("forgejwt: encoding header: %w", err)
	}
	p, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("forgejwt: encoding claims: %w", err)
	}
	signingInput := b64(h) + "." + b64(p)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("forgejwt: signing failed")
	}
	return signingInput + "." + b64(sig), nil
}

// PublicKey reads the signing key file at path, with the same checks as
// Mint, and returns its public half.
func PublicKey(path string) (*rsa.PublicKey, error) {
	key, err := readKey(path)
	if err != nil {
		return nil, err
	}
	return &key.PublicKey, nil
}

// CheckKeyFile runs the checks Mint runs on the signing key file at path,
// without signing: the file exists and can be read, its mode is allowed, and
// it holds an RSA key of at least the minimum size. A file the boot keystore
// sweep sealed in place passes once it opens, and passes unopened when this
// process has no active keystore, because only the booted hive can open it.
// `hive validate` calls it so a bad key_file fails there and not at the first
// clone or push (hivecommons/hive#19919).
func CheckKeyFile(path string) error {
	data, err := readKeyFile(path)
	if err != nil {
		return err
	}
	if keystore.IsEnvelope(data) && keystore.Active() == nil {
		return nil
	}
	_, err = parseKey(path, data)
	return err
}

// readKey loads a PEM RSA private key, opening it first when the boot
// keystore sweep has sealed the file in place. Its errors name the path and
// never carry file contents or parser detail.
func readKey(path string) (*rsa.PrivateKey, error) {
	data, err := readKeyFile(path)
	if err != nil {
		return nil, err
	}
	return parseKey(path, data)
}

// readKeyFile reads the raw key file. The mode check and the read use the
// same open file, so the checked file is the one read.
func readKeyFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("forgejwt: signing key: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("forgejwt: signing key: %w", err)
	}
	gid, haveGID := fileGID(info)
	if perm := info.Mode().Perm(); !keyModeAllowed(perm, gid, haveGID) {
		return nil, fmt.Errorf("forgejwt: %s has mode %#o: %w; chmod it to 0600, or to 0440 with group hive-launch (GID %d)", path, perm, ErrKeyFileMode, LaunchGID)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyBytes))
	if err != nil {
		return nil, fmt.Errorf("forgejwt: signing key: %w", err)
	}
	return data, nil
}

// parseKey opens a sealed key and parses the PEM RSA private key in data.
func parseKey(path string, data []byte) (*rsa.PrivateKey, error) {
	if keystore.IsEnvelope(data) {
		pt, _, err := keystore.Open(context.Background(), keystore.Active(), string(data), keystore.FileAAD(path))
		if err != nil {
			return nil, fmt.Errorf("forgejwt: %s is a sealed signing key that could not be opened: %w", path, err)
		}
		data = pt
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("forgejwt: %s is not a PEM file", path)
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, _ = k.(*rsa.PrivateKey)
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	}
	if key == nil {
		return nil, fmt.Errorf("forgejwt: %s is not an RSA private key in PKCS #8 or PKCS #1 form", path)
	}
	if key.N.BitLen() < minKeyBits {
		return nil, fmt.Errorf("forgejwt: %s is a %d-bit RSA key; use at least %d bits", path, key.N.BitLen(), minKeyBits)
	}
	return key, nil
}

type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// KeyList renders the discovery document and the key list (JWKS) for iss.
// List both the old and the new key during a rotation.
func KeyList(iss Issuer, keys ...*rsa.PublicKey) (discovery, jwks []byte, err error) {
	if len(keys) == 0 {
		return nil, nil, errors.New("forgejwt: key list needs at least one key")
	}
	set := struct {
		Keys []jwk `json:"keys"`
	}{}
	seen := map[string]bool{}
	for _, k := range keys {
		kid := keyID(k)
		if seen[kid] {
			return nil, nil, fmt.Errorf("forgejwt: key %s is listed twice", kid)
		}
		seen[kid] = true
		set.Keys = append(set.Keys, jwk{Kty: "RSA", Use: "sig", Alg: alg, Kid: kid, N: b64(k.N.Bytes()), E: b64(big.NewInt(int64(k.E)).Bytes())})
	}
	jwks, err = json.MarshalIndent(set, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("forgejwt: encoding key list: %w", err)
	}
	discovery, err = json.MarshalIndent(struct {
		Issuer  Issuer   `json:"issuer"`
		JWKSURI string   `json:"jwks_uri"`
		Algs    []string `json:"id_token_signing_alg_values_supported"`
	}{iss, string(iss) + "/" + KeyListPath, []string{alg}}, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("forgejwt: encoding discovery document: %w", err)
	}
	return append(discovery, '\n'), append(jwks, '\n'), nil
}

// keyID is the RFC 7638 JWK thumbprint of k.
func keyID(k *rsa.PublicKey) string {
	sum := sha256.Sum256(fmt.Appendf(nil, `{"e":%q,"kty":"RSA","n":%q}`, b64(big.NewInt(int64(k.E)).Bytes()), b64(k.N.Bytes())))
	return b64(sum[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
