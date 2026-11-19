// Package keystore encrypts the spoke's secrets at rest behind one small
// provider interface (hivecommons/hive#18214).
//
// Every secret value a hive collects at runtime — the LiteLLM, Bob and
// per-gateway API keys, the backup key, the owner's GitHub token, the Copilot
// device-flow login, the PAT in the persisted runtime config — used to sit in
// plaintext on the data volume. A copy of the volume alone yielded all of
// them. With a Provider active, those values are stored as an Envelope: a
// versioned text header naming the provider and the key generation, followed
// by the ciphertext. The envelope is a single line of printable ASCII, so the
// same encoding serves as a file body and as a YAML scalar.
//
// The package is deliberately stdlib-only and imports nothing else in the
// module, so the configuration leaf (crux/config) and the agent manager can
// read secrets through it without an import cycle. Providers that need a
// network client (OpenBao Transit, hive#18216) live in their own package and
// register through Register.
//
// Three states, chosen by HIVE_KEYSTORE_PROVIDER:
//
//   - "env" (default): AES-256-GCM under a key derived from HIVE_SECRETS_KEY,
//     HIVE_SECRETS_KEY_FILE or the hub-provisioned HIVE_HEARTBEAT_KEY. With no
//     key material at all the hive keeps today's plaintext behaviour and says
//     so once at WARN — it never silently encrypts with a key stored beside
//     the data.
//   - a registered provider name (such as "openbao"): built by its Factory;
//     a factory error refuses boot rather than falling back to plaintext.
//   - "none": explicit legacy plaintext.
package keystore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Provider seals and opens secret values. Implementations must be safe for
// concurrent use.
type Provider interface {
	// Name identifies the provider in envelopes and logs ("env", "openbao").
	Name() string
	// KeyID identifies the key generation new envelopes are sealed under. It
	// is recorded in the envelope header so a later reader can tell a stale
	// generation apart and re-seal it.
	KeyID() string
	// Encrypt seals plaintext bound to aad (additional authenticated data).
	// The same aad must be supplied to Decrypt.
	Encrypt(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	// Decrypt opens ciphertext sealed under key generation keyID with aad.
	Decrypt(ctx context.Context, keyID string, ciphertext, aad []byte) ([]byte, error)
}

// Envelope wire format: Prefix + Version + ":" + provider + ":" + keyID + ":" +
// base64(ciphertext). Fields never contain ':'; the ciphertext is standard
// base64 with padding so it survives YAML and shell quoting unchanged.
const (
	// Prefix opens every envelope. IsEnvelope keys on it alone, so a legacy
	// plaintext secret that happens to start with it is unrepresentable — no
	// real credential format begins with "hive-enc:".
	Prefix = "hive-enc:"
	// Version is the envelope format version this package writes.
	Version = "v1"

	envelopeFields = 5
)

var (
	// ErrNotEnvelope is returned by ParseEnvelope for a value without the
	// Prefix. Callers treat such a value as legacy plaintext.
	ErrNotEnvelope = errors.New("keystore: not an envelope")
	// ErrMalformedEnvelope is returned for a value with the Prefix whose
	// remainder does not parse. Unlike ErrNotEnvelope it is never treated as
	// plaintext: a truncated envelope is a corrupted secret, not a credential.
	ErrMalformedEnvelope = errors.New("keystore: malformed envelope")
	// ErrNoProvider is returned when an envelope must be opened and no
	// provider is active.
	ErrNoProvider = errors.New("keystore: encrypted secret found but no keystore provider is active")
	// ErrProviderMismatch is returned when an envelope names a provider other
	// than the active one.
	ErrProviderMismatch = errors.New("keystore: envelope was sealed by a different provider")
	// ErrUnknownKeyGeneration is returned by a provider that holds no key for
	// the generation an envelope names.
	ErrUnknownKeyGeneration = errors.New("keystore: no key for this generation")
	// ErrAuthentication is returned when a ciphertext fails to authenticate:
	// tampered bytes, or the wrong additional authenticated data.
	ErrAuthentication = errors.New("keystore: ciphertext failed to authenticate")
)

// Envelope is a parsed sealed value.
type Envelope struct {
	Version    string
	Provider   string
	KeyID      string
	Ciphertext []byte
}

// String renders the envelope in wire format.
func (e Envelope) String() string {
	return Prefix + e.Version + ":" + e.Provider + ":" + e.KeyID + ":" + base64.StdEncoding.EncodeToString(e.Ciphertext)
}

// IsEnvelope reports whether b (trimmed) carries the envelope Prefix.
func IsEnvelope(b []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(b)), Prefix)
}

// IsEnvelopeString is IsEnvelope for a string value.
func IsEnvelopeString(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), Prefix)
}

// ParseEnvelope decodes a wire-format envelope. A value without the Prefix
// yields ErrNotEnvelope; one with the Prefix that does not parse yields
// ErrMalformedEnvelope.
func ParseEnvelope(s string) (Envelope, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, Prefix) {
		return Envelope{}, ErrNotEnvelope
	}
	parts := strings.SplitN(s, ":", envelopeFields)
	if len(parts) != envelopeFields {
		return Envelope{}, fmt.Errorf("%w: expected %d fields", ErrMalformedEnvelope, envelopeFields)
	}
	e := Envelope{Version: parts[1], Provider: parts[2], KeyID: parts[3]}
	if e.Version != Version {
		return Envelope{}, fmt.Errorf("%w: unsupported version %q", ErrMalformedEnvelope, e.Version)
	}
	if e.Provider == "" || e.KeyID == "" {
		return Envelope{}, fmt.Errorf("%w: empty provider or key id", ErrMalformedEnvelope)
	}
	ct, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil || len(ct) == 0 {
		return Envelope{}, fmt.Errorf("%w: ciphertext is not base64", ErrMalformedEnvelope)
	}
	e.Ciphertext = ct
	return e, nil
}

// Seal encrypts plaintext with p bound to aad and returns the wire-format
// envelope. A nil provider is a programming error and is reported as
// ErrNoProvider rather than returning the plaintext.
func Seal(ctx context.Context, p Provider, plaintext, aad []byte) (string, error) {
	if p == nil {
		return "", ErrNoProvider
	}
	ct, err := p.Encrypt(ctx, plaintext, aad)
	if err != nil {
		return "", fmt.Errorf("keystore: %s encrypt: %w", p.Name(), err)
	}
	return Envelope{Version: Version, Provider: p.Name(), KeyID: p.KeyID(), Ciphertext: ct}.String(), nil
}

// Open decrypts a wire-format envelope with p. stale reports that the
// envelope was sealed under a key generation other than p's current one, so
// the caller can re-seal it. A value that is not an envelope is reported as
// ErrNotEnvelope; callers that accept legacy plaintext check for it.
func Open(ctx context.Context, p Provider, envelope string, aad []byte) (plaintext []byte, stale bool, err error) {
	e, err := ParseEnvelope(envelope)
	if err != nil {
		return nil, false, err
	}
	if p == nil {
		return nil, false, fmt.Errorf("%w (envelope names provider %q, key %q)", ErrNoProvider, e.Provider, e.KeyID)
	}
	if e.Provider != p.Name() {
		return nil, false, fmt.Errorf("%w: envelope %q, active %q", ErrProviderMismatch, e.Provider, p.Name())
	}
	pt, err := p.Decrypt(ctx, e.KeyID, e.Ciphertext, aad)
	if err != nil {
		return nil, false, fmt.Errorf("keystore: %s decrypt (key %s): %w", p.Name(), e.KeyID, err)
	}
	return pt, e.KeyID != p.KeyID(), nil
}
