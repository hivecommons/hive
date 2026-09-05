package hub

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// HIVE_SESSION_KEY, in two stages.
//
// RESIDUAL-2 (2026-08-14) found it derived with deriveDomainKey, which takes
// no hiveID, so every spoke in the fleet held a byte-identical value:
// measured live as 1 distinct value across all 70 spokes, 0 converged, while
// HIVE_TERMINAL_KEY next to it was 70 distinct values. That was not sweep
// drift — uniformity was the intended output of the formula — so it took a
// code change (provisionSessionKey(hiveID)) to make it per-hive, shipped as
// defence-in-depth: nothing verified against the key any more, but a
// fleet-uniform symmetric key is exactly the shape that keeps reappearing in
// these audits.
//
// Issue #3234 (the N2 compatibility-lane follow-up) finishes the job:
// per-hive-and-injected was always a stop on the way to not-injected-at-all,
// not the destination. provisionSessionKey is gone, desiredPerHiveEnv never
// wants HIVE_SESSION_KEY, and it is listed in perHiveEnvRemovedNames so the
// sweep actively strips it from any Deployment that still carries one — see
// TestSessionKeyIsPermanentlyRemoved in perhive_env_reconcile_test.go for the
// behavioural coverage of that removal.
//
// What remains here are the tests specific to THIS var's history: the source
// invariant that guards against a sync merge quietly reintroducing it (the
// failure mode that lost F3, F14 and F18 in this repo), and the positive
// controls that pin why the self-derive fallback staying unchanged is safe
// rather than merely convenient.

const residual2Master = "residual2-test-master-secret"

func residual2ReadSource(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return string(raw)
}

// --- SOURCE INVARIANT --------------------------------------------------------

// TestSessionKeyNeverDerivedInDesiredEnv is source-asserting, not just
// behavioural, for the same reason the rest of this file is: a sync merge
// resolving a conflict in favour of the older side re-adds a line rather than
// changing a value, and a behavioural test alone depends on a fixture a merge
// could rewrite in the same commit.
func TestSessionKeyNeverDerivedInDesiredEnv(t *testing.T) {
	src := residual2ReadSource(t, "perhive_env_reconcile.go")

	if regexp.MustCompile(`want\s*:=\s*map\[string\]string\{[^}]*EnvSessionKey`).MatchString(src) {
		t.Error("desiredPerHiveEnv's `want` map derives EnvSessionKey again — issue #3234 removal regressed; " +
			"it must stay out of `want` and be listed only in perHiveEnvRemovedNames")
	}
	if !regexp.MustCompile(`func perHiveEnvRemovedNames\(\) \[\]string \{[^}]*EnvSessionKey`).MatchString(src) {
		t.Error("perHiveEnvRemovedNames() no longer lists EnvSessionKey — the sweep would stop stripping " +
			"HIVE_SESSION_KEY from Deployments that still carry it (issue #3234)")
	}
	if strings.Contains(src, "provisionSessionKey(") {
		t.Error("perhive_env_reconcile.go still calls provisionSessionKey — that function was removed by " +
			"issue #3234; a caller here means the var is being derived again")
	}

	// Only the LIVE template line is banned — historical prose in comments
	// (explaining what used to be injected here and why) is expected and
	// welcome; strings.Contains on the bare env var name would flag those too.
	provisionSrc := residual2ReadSource(t, "saas_provision.go")
	if regexp.MustCompile(`(?m)^\s*- name: HIVE_SESSION_KEY\s*$`).MatchString(provisionSrc) {
		t.Error("saas_provision.go still renders a HIVE_SESSION_KEY env entry in the Deployment template " +
			"(issue #3234)")
	}
	if strings.Contains(provisionSrc, `"SessionKey"`) {
		t.Error(`saas_provision.go still sets a "SessionKey" template field — the Deployment template no ` +
			"longer has an env entry to consume it (issue #3234)")
	}
	if strings.Contains(provisionSrc, "provisionSessionKey(") {
		t.Error("saas_provision.go still calls provisionSessionKey — that function was removed by issue #3234")
	}
}

// TestSessionKeyPerHiveDerivationRemoved pins that provisionSessionKey itself
// is gone, not merely unreferenced. A stray definition with no callers is the
// kind of dead code that gets called again by a future "helpful" refactor.
func TestSessionKeyPerHiveDerivationRemoved(t *testing.T) {
	src := residual2ReadSource(t, "hub_keys.go")
	if regexp.MustCompile(`func provisionSessionKey\(`).MatchString(src) {
		t.Error("provisionSessionKey still exists in hub_keys.go — issue #3234 removed the last caller; " +
			"a defined-but-unused per-hive derivation for a var the hub no longer injects is a re-introduction " +
			"waiting to happen")
	}
}

// --- POSITIVE CONTROLS -------------------------------------------------------
//
// Without these, "remove everything from desiredPerHiveEnv" would satisfy the
// source invariant above while breaking the fleet in two distinct ways.

// TestResidual2PublicKeysStayFleetWide is the substantive control. The two
// Ed25519 PUBLIC keys must NOT become per-hive: the hub holds ONE private seed
// and mints ONE cookie for a user, so a spoke handed a per-hive "public key"
// would verify nothing the hub ever signed and every hosted login would break.
func TestResidual2PublicKeysStayFleetWide(t *testing.T) {
	withTestMaster(t, residual2Master)

	a := desiredPerHiveEnv("hive-alpha")
	b := desiredPerHiveEnv("hive-beta")
	if a == nil || b == nil {
		t.Fatal("desiredPerHiveEnv returned nil")
	}
	for _, name := range []string{EnvSSOPublicKey, envSessionPublicKey} {
		if a[name] == "" {
			t.Fatalf("%s derived empty; test setup is wrong", name)
		}
		if a[name] != b[name] {
			t.Errorf("%s differs between hives — it is the PUBLIC half of a key the HUB alone holds "+
				"the seed for. Per-hive public keys verify nothing the hub signed and would break "+
				"every hosted login. Fleet-wide is CORRECT here.", name)
		}
	}
}

// TestResidual2FailsClosedWithoutIdentity asserts derivePerHiveKey's fail-closed
// guard on the empty-hiveID case, using infoSessionKey as the probe — the same
// primitive heartbeat/terminal/invite derivation shares, so this still exercises
// live production code even though HIVE_SESSION_KEY itself is no longer
// derived through desiredPerHiveEnv.
func TestResidual2FailsClosedWithoutIdentity(t *testing.T) {
	withTestMaster(t, residual2Master)

	if got := desiredPerHiveEnv(""); got != nil {
		t.Errorf("desiredPerHiveEnv(\"\") returned %v, want nil — a hive whose ID does not resolve must "+
			"be SKIPPED, never patched with empty values", got)
	}
	if derivePerHiveKey(residual2Master, infoSessionKey, "") != "" {
		t.Error("derivePerHiveKey must return \"\" for an empty hiveID (fail closed)")
	}
}

// --- THE SELF-DERIVE FALLBACK ------------------------------------------------

// TestResidual2SessionKeyIsNotAVerificationComparand documents, as an
// executable claim, why removing HIVE_SESSION_KEY from provisioning outright
// does not break a fleet.
//
// Every spoke self-derives HIVE_SESSION_KEY when the env var is absent — after
// issue #3234, that is EVERY spoke, permanently, not just those mid-roll — and
// both spoke-side resolvers (Go spokeDomainKey, the proxy's deriveSessionKey)
// derive the FLEET-WIDE value from HIVE_HUB_SECRET, because neither mixes in
// the hive ID. That is safe only because nothing verifies with this key: F1
// deleted the Go legacy symmetric cookie lane, N3 stopped the terminal key
// falling through to it, and F23 deleted the Node proxy's copy. A value no
// party ever compares has no observable effect no matter how many spokes
// disagree about it.
//
// This test pins the premise, not the conclusion: it asserts the value is not
// used as a comparand. If some future change makes it one again, the
// self-derive disagreement becomes a live fleet-breaking bug and this test
// fails first.
func TestResidual2SessionKeyIsNotAVerificationComparand(t *testing.T) {
	// Go side: the legacy symmetric lane must stay deleted.
	cookieSrc := residual2ReadSource(t, "hub_cookie.go")
	if !strings.Contains(cookieSrc, "_ = legacySecret") {
		t.Error("hub_cookie.go no longer discards legacySecret — if the symmetric session lane came " +
			"back, spokes self-deriving the fleet-wide key would disagree with the hub and hosted auth " +
			"would break (RESIDUAL-2 / F1 / #3234)")
	}

	// SpokeSessionKey must remain callable-but-unused on the Go side. A new
	// caller is the tripwire: it would be verifying with a value the hub no
	// longer ever ships.
	for _, f := range []string{"hub_cookie.go", "hub_session_revocation.go", "spoke/terminal_assertion.go"} {
		src := residual2ReadSource(t, f)
		if strings.Contains(src, "SpokeSessionKey()") {
			t.Errorf("%s calls SpokeSessionKey() — the hub no longer injects HIVE_SESSION_KEY at all, so "+
				"a caller here would verify with the fleet-wide self-derived fallback (#3234)", f)
		}
	}

	// The spoke-side resolver is deliberately left fleet-wide. Making it
	// per-hive would be dead code (the hub never ships a per-hive value to
	// disagree with) and would hand a WRONG answer to a spoke with no
	// HIVE_ID, flipping the proxy's IS_HOSTED signal to false and silently
	// converting a hosted hive to the self-hosted identity model.
	keysSrc := residual2ReadSource(t, "hub_keys.go")
	if !strings.Contains(keysSrc, "func SpokeSessionKey() string { return spokeDomainKey(EnvSessionKey, infoSessionKey) }") {
		t.Error("SpokeSessionKey changed shape — it must keep self-deriving the fleet-wide value; a " +
			"self-hosted operator who never receives HIVE_SESSION_KEY at all legitimately depends on this (#3234)")
	}
}
