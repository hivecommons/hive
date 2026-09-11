package hub

// Tests for the pkg/hub terminal-assertion ALIASES (terminal_assertion.go).
//
// The alias file's own contract is that pkg/hub keeps these thin wrappers so
// hub tests can assert provisioning/self-derive COHERENCE against the same
// symbols: the hub provisions HIVE_TERMINAL_KEY (provisionTerminalKey) and the
// spoke resolves it (TerminalSigningKey → terminalassert.SigningKey). Nothing
// previously exercised TerminalSigningKey() or stated the coherence invariant
// end-to-end, so a drift between the hub's provisioning derivation and the
// spoke's lane-2 self-derive — the exact silent-drift hazard the shared leaf
// package exists to prevent — would not have failed any test in this package.
//
// terminal_key_per_hive_test.go covers the N3 per-hive DERIVATION properties;
// this file covers the RESOLVER and the hub↔spoke agreement on its lanes.

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/terminalassert"
)

const aliasTestMaster = "alias-coherence-test-master"

// TestTerminalSigningKeyPrefersInjectedKey pins lane 1: when the hub has
// provisioned HIVE_TERMINAL_KEY into the spoke env, TerminalSigningKey must
// return exactly that value — never a re-derivation — and must agree with the
// leaf resolver it aliases.
func TestTerminalSigningKeyPrefersInjectedKey(t *testing.T) {
	t.Setenv(EnvTerminalKey, "injected-per-hive-key")
	// A master and identity are ALSO present; lane 1 must still win, or a
	// provisioned spoke would silently ignore its injected per-hive key.
	t.Setenv("HIVE_HUB_SECRET", aliasTestMaster)
	t.Setenv("HIVE_ID", "hive-alias")

	got := TerminalSigningKey()
	if got != "injected-per-hive-key" {
		t.Fatalf("TerminalSigningKey() = %q, want the injected HIVE_TERMINAL_KEY value", got)
	}
	if leaf := terminalassert.SigningKey(); got != leaf {
		t.Fatalf("hub alias resolved %q but terminalassert.SigningKey resolved %q — the alias drifted", got, leaf)
	}
}

// TestProvisionAndSelfDeriveLanesAgree is the coherence invariant itself: the
// key the hub would inject for a hive (provisionTerminalKey, hub derivation
// path) must be byte-identical to what that hive self-derives on lane 2 when
// the injected var is absent (TerminalSigningKey with only HIVE_HUB_SECRET +
// HIVE_ID). If these ever diverge, a re-provisioned spoke and a self-derived
// spoke mint terminal assertions under DIFFERENT keys for the same hive, and
// every in-flight terminal session breaks unrecoverably rather than healing
// within the assertion TTL.
func TestProvisionAndSelfDeriveLanesAgree(t *testing.T) {
	const hiveID = "hive-coherence"

	// Hub side: what provisioning would inject as HIVE_TERMINAL_KEY.
	t.Setenv("HIVE_HUB_SECRET", aliasTestMaster)
	provisioned := provisionTerminalKey(hiveID)
	if provisioned == "" {
		t.Fatal("provisionTerminalKey returned empty for a valid master+hiveID")
	}

	// Spoke side: injected var ABSENT, so the resolver takes lane 2 and
	// self-derives from the master the spoke already holds plus its identity.
	t.Setenv(EnvTerminalKey, "")
	t.Setenv("HIVE_ID", hiveID)
	selfDerived := TerminalSigningKey()

	if selfDerived != provisioned {
		t.Fatalf("self-derived key %q != provisioned key %q — the hub provisioning lane and the "+
			"spoke lane-2 self-derive have drifted apart; the leaf package exists to make this impossible",
			selfDerived, provisioned)
	}

	// End-to-end through the aliases: an assertion minted under the
	// provisioned key must verify under the self-derived key (they are the
	// same bytes, and the mint/verify pair must agree with the leaf package).
	now := time.Now()
	token := MintTerminalAssertion(provisioned, "alice", "owner", hiveID, now)
	if token == "" {
		t.Fatal("MintTerminalAssertion returned empty for valid inputs")
	}
	user, role, err := VerifyTerminalAssertion(selfDerived, token, hiveID, now)
	if err != nil || user != "alice" || role != "owner" {
		t.Fatalf("assertion minted under the provisioned key failed to verify under the "+
			"self-derived key: user=%q role=%q err=%v", user, role, err)
	}
}

// TestHubAliasConstantsMatchLeaf pins the alias constants to their leaf
// definitions at runtime. The aliases are consts, so a drift would require an
// edit — this test makes that edit fail loudly with the reason, instead of
// silently splitting the env var name or the domain-separation label between
// the provisioning side and the resolving side.
func TestHubAliasConstantsMatchLeaf(t *testing.T) {
	if EnvTerminalKey != terminalassert.EnvTerminalKey {
		t.Errorf("hub EnvTerminalKey %q != terminalassert.EnvTerminalKey %q — provisioning would "+
			"patch one env var while the spoke reads another", EnvTerminalKey, terminalassert.EnvTerminalKey)
	}
	if infoTerminalKey != terminalassert.InfoKey {
		t.Errorf("hub infoTerminalKey %q != terminalassert.InfoKey %q — provisioning and self-derive "+
			"would derive under different domain-separation labels", infoTerminalKey, terminalassert.InfoKey)
	}
}
