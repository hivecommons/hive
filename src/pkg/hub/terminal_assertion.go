package hub

import (
	"time"

	"github.com/hivecommons/hive/pkg/terminalassert"
)

// The terminal-assertion implementation (finding C3 follow-up, audit N3) lives
// in the LEAF package pkg/terminalassert: the assertion is a SPOKE-LOCAL
// primitive — the spoke dashboard both mints and verifies it — so the spoke
// side must not have to import the hub server package to reach it. The full
// design rationale (why it does not share the SSO signing path, the N3
// fleet-uniform-lane deletions, rotation posture) is documented there.
//
// pkg/hub keeps these thin aliases because the hub PROVISIONS the per-hive
// terminal key (provisionTerminalKey, perhive_env_reconcile.go) and its tests
// assert provisioning/self-derive coherence against the same symbols.

const (
	// EnvTerminalKey is the dedicated spoke-side env var carrying the PER-HIVE
	// terminal signing key. See terminalassert.EnvTerminalKey.
	EnvTerminalKey = terminalassert.EnvTerminalKey

	// infoTerminalKey is the domain-separation label for the terminal signing
	// sub-key. Provisioning (provisionTerminalKey) must use the SAME label the
	// spoke's self-derive lane uses, so this aliases terminalassert.InfoKey.
	infoTerminalKey = terminalassert.InfoKey
)

// TerminalSigningKey resolves the spoke's terminal signing key.
// See terminalassert.SigningKey for the lane order and the N3 invariant.
func TerminalSigningKey() string {
	return terminalassert.SigningKey()
}

// MintTerminalAssertion creates a short-lived signed terminal assertion.
// See terminalassert.Mint.
func MintTerminalAssertion(key, username, role, hiveID string, now time.Time) string {
	return terminalassert.Mint(key, username, role, hiveID, now)
}

// VerifyTerminalAssertion validates a terminal assertion.
// See terminalassert.Verify.
func VerifyTerminalAssertion(key, token, expectedHiveID string, now time.Time) (username, role string, err error) {
	return terminalassert.Verify(key, token, expectedHiveID, now)
}
