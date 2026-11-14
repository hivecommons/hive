package keystore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ProviderEnv is the Name of EnvKeyProvider.
const ProviderEnv = "env"

const (
	// hkdfSalt and hkdfInfo bind the derived data-encryption key to this use.
	// The heartbeat key the default material comes from is an HMAC signing
	// key for hub traffic; deriving through HKDF with a distinct info string
	// means the bytes that encrypt secrets are never the bytes that sign a
	// heartbeat, so neither use can be turned against the other.
	hkdfSalt = "hivecommons.dev/keystore/v1"
	hkdfInfo = "hivecommons.dev/keystore/v1/data-encryption-key"
	// keyIDInfo is hashed with the derived key to name the generation.
	keyIDInfo = "hivecommons.dev/keystore/v1/key-id"

	aesKeySize = 32
	// keyIDBytes of the SHA-256 over the derived key become the generation id
	// (8 hex characters): enough to tell generations apart in a header, far
	// too little to say anything about the key.
	keyIDBytes = 4
	// minMaterialBytes rejects key material too short to be a real secret. A
	// hub-provisioned heartbeat key is 64 hex characters; an operator-chosen
	// HIVE_SECRETS_KEY must be at least this long.
	minMaterialBytes = 16
)

// ErrKeyMaterialTooShort is returned by NewEnvKeyProvider for key material
// under minMaterialBytes.
var ErrKeyMaterialTooShort = fmt.Errorf("keystore: key material must be at least %d bytes", minMaterialBytes)

// EnvKeyProvider is the default provider: AES-256-GCM under a key derived
// from operator- or hub-supplied material with HKDF-SHA256. It holds the
// current generation and any number of previous generations for decryption
// only, so a rotation is absorbed by reading under the old key and re-sealing
// under the new one.
type EnvKeyProvider struct {
	current string
	aeads   map[string]cipher.AEAD
}

// NewEnvKeyProvider derives the current generation from current and
// decrypt-only generations from each of previous. Material equal to the
// current key, or repeated, is folded; empty previous entries are skipped.
func NewEnvKeyProvider(current []byte, previous ...[]byte) (*EnvKeyProvider, error) {
	p := &EnvKeyProvider{aeads: map[string]cipher.AEAD{}}
	kid, err := p.add(current)
	if err != nil {
		return nil, err
	}
	p.current = kid
	for _, prev := range previous {
		if len(strings.TrimSpace(string(prev))) == 0 {
			continue
		}
		if _, err := p.add(prev); err != nil {
			return nil, fmt.Errorf("previous key material: %w", err)
		}
	}
	return p, nil
}

func (p *EnvKeyProvider) add(material []byte) (string, error) {
	material = []byte(strings.TrimSpace(string(material)))
	if len(material) < minMaterialBytes {
		return "", ErrKeyMaterialTooShort
	}
	key, err := hkdf.Key(sha256.New, material, []byte(hkdfSalt), hkdfInfo, aesKeySize)
	if err != nil {
		return "", fmt.Errorf("keystore: derive key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	kid := keyIDFor(key)
	if _, dup := p.aeads[kid]; !dup {
		p.aeads[kid] = aead
	}
	return kid, nil
}

// keyIDFor names a derived key without revealing it: the first keyIDBytes of
// SHA-256(keyIDInfo || key), hex.
func keyIDFor(key []byte) string {
	h := sha256.New()
	h.Write([]byte(keyIDInfo))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil)[:keyIDBytes])
}

// Name implements Provider.
func (p *EnvKeyProvider) Name() string { return ProviderEnv }

// KeyID implements Provider: the current generation.
func (p *EnvKeyProvider) KeyID() string { return p.current }

// Generations returns every generation this provider can decrypt, current
// first. For logs and status, never for envelopes.
func (p *EnvKeyProvider) Generations() []string {
	out := []string{p.current}
	for kid := range p.aeads {
		if kid != p.current {
			out = append(out, kid)
		}
	}
	return out
}

// Encrypt implements Provider: nonce || GCM(plaintext, aad) under the
// current generation.
func (p *EnvKeyProvider) Encrypt(_ context.Context, plaintext, aad []byte) ([]byte, error) {
	aead := p.aeads[p.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("keystore: nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, aad), nil
}

// Decrypt implements Provider. An unknown keyID yields
// ErrUnknownKeyGeneration; a tampered ciphertext or a different aad yields
// ErrAuthentication.
func (p *EnvKeyProvider) Decrypt(_ context.Context, keyID string, ciphertext, aad []byte) ([]byte, error) {
	aead, ok := p.aeads[keyID]
	if !ok {
		return nil, fmt.Errorf("%w (%q; set %s to the retired key material so it can be re-sealed)", ErrUnknownKeyGeneration, keyID, EnvSecretsKeyPrevious)
	}
	ns := aead.NonceSize()
	if len(ciphertext) < ns+aead.Overhead() {
		return nil, fmt.Errorf("%w: ciphertext too short", ErrAuthentication)
	}
	pt, err := aead.Open(nil, ciphertext[:ns], ciphertext[ns:], aad)
	if err != nil {
		return nil, errors.Join(ErrAuthentication, err)
	}
	return pt, nil
}
