package config

import (
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"strings"
)

// GitHub App private-key fingerprinting.
//
// A spoke must be able to tell the hub WHICH App key it is running without ever
// sending the key itself. The fingerprint below is derived from the PUBLIC half
// of the key, so it is safe to put in a heartbeat payload, a log line, or an
// operator-facing API response — it reveals nothing that GitHub does not already
// publish, and it cannot be inverted back into signing material.
//
// The digest is taken over the DER-encoded PKIX *public* key rather than over
// the PEM file bytes, so two byte-different encodings of the SAME key
// (PKCS#1 "BEGIN RSA PRIVATE KEY" vs PKCS#8 "BEGIN PRIVATE KEY", CRLF vs LF,
// trailing newline present or absent) produce the SAME fingerprint. A file-bytes
// digest would report a spurious mismatch on all of those and make the hub
// re-push a key that was already correct on every single heartbeat.

// AppKeyFingerprintLen is the number of hex characters kept from the SHA-256
// digest of the public key. 32 hex chars = 128 bits, far beyond what is needed
// to distinguish the handful of App keys in a fleet while keeping log lines and
// JSON payloads readable. Never truncate below this: shorter prefixes invite
// accidental collisions that would suppress a needed key push.
const AppKeyFingerprintLen = 32

// appKeyFingerprintPrefix labels the digest with its scheme so a future change
// of algorithm is self-describing on the wire and in logs, and so an old
// fingerprint can never be silently compared against a new-scheme one.
const appKeyFingerprintPrefix = "sha256:"

// ErrNoAppKey is returned when there is no key material to fingerprint at all
// (empty string, or a key file that does not exist). Callers treat this as
// "spoke has no key", which is a legitimate, expected state — not an error to
// log loudly.
var ErrNoAppKey = errors.New("no github app private key")

// AppKeyFingerprint returns a non-secret, stable identifier for a GitHub App
// private key in PEM form. The returned value is "sha256:<hex>" and NEVER
// contains key material.
//
// It returns ErrNoAppKey for empty input so callers can distinguish "no key" from
// "malformed key" — the two demand different operator responses.
func AppKeyFingerprint(pemData string) (string, error) {
	trimmed := strings.TrimSpace(pemData)
	if trimmed == "" {
		return "", ErrNoAppKey
	}

	block, _ := pem.Decode([]byte(trimmed))
	if block == nil {
		return "", errors.New("github app key is not valid PEM")
	}

	pub, err := publicKeyFromPrivateDER(block.Bytes)
	if err != nil {
		return "", err
	}

	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(der)
	full := hex.EncodeToString(sum[:])
	return appKeyFingerprintPrefix + full[:AppKeyFingerprintLen], nil
}

// publicKeyFromPrivateDER extracts the public half of a private key from DER
// bytes, accepting every encoding GitHub and GitHub Enterprise hand out:
// PKCS#1 (the classic "BEGIN RSA PRIVATE KEY" download), PKCS#8, and SEC1 EC.
// Trying each in turn is deliberate — rejecting a valid key because it arrived
// in the other standard encoding would make the hub re-push forever.
func publicKeyFromPrivateDER(der []byte) (any, error) {
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return &k.PublicKey, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if pk, ok := k.(interface{ Public() crypto.PublicKey }); ok {
			return pk.Public(), nil
		}
		return nil, errors.New("github app key has no derivable public key")
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return &k.PublicKey, nil
	}
	return nil, errors.New("github app key is not a recognized private key encoding")
}

// AppKeyFingerprintFromFile fingerprints the key stored at path. A missing or
// empty file yields ErrNoAppKey — the normal "this hive was provisioned without
// a key" state, which the hub repairs rather than reports as a failure.
func AppKeyFingerprintFromFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", ErrNoAppKey
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoAppKey
		}
		return "", err
	}
	return AppKeyFingerprint(string(data))
}
