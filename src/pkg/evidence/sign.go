package evidence

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Hash returns the hex SHA-256 of the canonical encoding of b with the Hash
// and Signature fields excluded. The Signed flag is covered, so flipping it
// changes the hash.
func Hash(b *Bundle) (string, error) {
	if b == nil {
		return "", errors.New("evidence: nil bundle")
	}
	c := *b
	c.Hash, c.Signature = "", ""
	canon, err := Canonical(&c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// Seal fills in b.Hash. When priv is non-nil it also signs the bundle;
// when priv is nil the bundle is left unsigned with Signed=false.
func Seal(b *Bundle, priv ed25519.PrivateKey) error {
	if len(priv) == 0 {
		if b == nil {
			return errors.New("evidence: nil bundle")
		}
		b.Signed, b.Signature = false, ""
		h, err := Hash(b)
		if err != nil {
			return err
		}
		b.Hash = h
		return nil
	}
	return Sign(b, priv)
}

// Sign sets Signed, Hash and a detached base64 Ed25519 Signature over the
// hash bytes.
func Sign(b *Bundle, priv ed25519.PrivateKey) error {
	if b == nil {
		return errors.New("evidence: nil bundle")
	}
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("evidence: private key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	b.Signed = true
	h, err := Hash(b)
	if err != nil {
		return err
	}
	b.Hash = h
	b.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(h)))
	return nil
}

// Verify checks that b.Hash matches the bundle content and that b.Signature
// is a valid Ed25519 signature of it under pub. Any mutation of a covered
// field fails verification.
func Verify(b *Bundle, pub ed25519.PublicKey) error {
	if b == nil {
		return errors.New("evidence: nil bundle")
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("evidence: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	if !b.Signed || b.Signature == "" {
		return errors.New("evidence: bundle is not signed")
	}
	h, err := Hash(b)
	if err != nil {
		return err
	}
	if h != b.Hash {
		return errors.New("evidence: hash mismatch: bundle content changed after sealing")
	}
	sig, err := base64.StdEncoding.DecodeString(b.Signature)
	if err != nil {
		return fmt.Errorf("evidence: decode signature: %w", err)
	}
	if !ed25519.Verify(pub, []byte(h), sig) {
		return errors.New("evidence: signature does not verify")
	}
	return nil
}

// LoadSigningKey reads an Ed25519 private key from path. The file holds a
// 32-byte seed or 64-byte private key, hex or base64 encoded. An empty or
// absent path returns (nil, nil): the caller then produces unsigned bundles.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("evidence: read signing key: %w", err)
	}
	text := strings.TrimSpace(string(data))
	raw, err := hex.DecodeString(text)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, errors.New("evidence: signing key is neither hex nor base64")
		}
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	}
	return nil, fmt.Errorf("evidence: signing key is %d bytes, want %d or %d", len(raw), ed25519.SeedSize, ed25519.PrivateKeySize)
}
