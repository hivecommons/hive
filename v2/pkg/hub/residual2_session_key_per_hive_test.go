package hub

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Audit RESIDUAL-2 (2026-08-14). HIVE_SESSION_KEY was derived with
// deriveDomainKey, which takes no hiveID, so every spoke in the fleet held a
// byte-identical value: measured live as 1 distinct value across all 70 spokes,
// 0 converged, while HIVE_TERMINAL_KEY next to it was 70 distinct values.
//
// That was not sweep drift. Uniformity was the intended output of the formula,
// which is why no rotation could ever have fixed it — a rotation moves all 70
// in lockstep. It required a code change: provisionSessionKey(hiveID).
//
// These tests are source-asserting as well as behavioural because the failure
// mode for this class in this repo is a sync merge resolving a conflict in
// favour of the older side (F3, F14 and F18 were all lost that way), and the
// old expression differs from the new one by a single call.

const residual2Master = "residual2-test-master-secret"

// --- THE INVARIANT -----------------------------------------------------------

// TestResidual2SessionKeyIsPerHive is the core regression: two hives must not
// receive the same HIVE_SESSION_KEY.
func TestResidual2SessionKeyIsPerHive(t *testing.T) {
	withTestMaster(t, residual2Master)

	a := desiredPerHiveEnv("hive-alpha")
	b := desiredPerHiveEnv("hive-beta")
	if a == nil || b == nil {
		t.Fatal("desiredPerHiveEnv returned nil for a valid master + hive ID")
	}

	if a[EnvSessionKey] == "" || b[EnvSessionKey] == "" {
		t.Fatal("HIVE_SESSION_KEY derived empty — an empty var is WORSE than absent: it makes the " +
			"readiness count report the hive as converged when it is not")
	}
	if a[EnvSessionKey] == b[EnvSessionKey] {
		t.Error("two hives received a byte-identical HIVE_SESSION_KEY — RESIDUAL-2 regressed; " +
			"this is the fleet-uniform symmetric-key shape that keeps reappearing in these audits")
	}

	// Pin it against the exact pre-fix expression, so a revert is named rather
	// than merely differing from a fixture.
	if a[EnvSessionKey] == deriveDomainKey(provisionCurrentSecret(), infoSessionKey) {
		t.Error("HIVE_SESSION_KEY equals deriveDomainKey(master, infoSessionKey) — the pre-fix " +
			"fleet-wide derivation is back (RESIDUAL-2)")
	}
	if want := derivePerHiveKey(provisionCurrentSecret(), infoSessionKey, "hive-alpha"); a[EnvSessionKey] != want {
		t.Error("HIVE_SESSION_KEY is not HMAC(master, infoSessionKey || 0x00 || hiveID); the " +
			"derivation no longer matches derivePerHiveKey")
	}
}

// TestResidual2SessionKeyStaysReconciled guards the other half: being per-hive
// is worthless if the sweep stops shipping the var. perHiveEnvNames() is what
// the reconcile actually walks.
func TestResidual2SessionKeyStaysReconciled(t *testing.T) {
	found := false
	for _, n := range perHiveEnvNames() {
		if n == EnvSessionKey {
			found = true
		}
	}
	if !found {
		t.Error("EnvSessionKey is absent from perHiveEnvNames() — the sweep would never converge it, " +
			"so the fleet would keep the fleet-uniform value it already holds (RESIDUAL-2)")
	}
}

// TestResidual2ProvisioningAndReconcileAgree pins the constraint that makes
// this change safe to ship. If provisioning emitted the fleet-wide value while
// the sweep wanted the per-hive one, perHiveEnvDrift would see drift on every
// pass and roll every pod every cycle, forever.
func TestResidual2ProvisioningAndReconcileAgree(t *testing.T) {
	withTestMaster(t, residual2Master)
	const hiveID = "hive-alpha"

	got := desiredPerHiveEnv(hiveID)
	if got == nil {
		t.Fatal("desiredPerHiveEnv returned nil")
	}
	// provisionSessionKey is the single helper both sides call.
	if got[EnvSessionKey] != provisionSessionKey(hiveID) {
		t.Error("the reconcile sweep and provisionSessionKey disagree — provisioning and reconcile " +
			"MUST derive identically or they fight and roll a pod every cycle forever")
	}

	// The provisioning template must call the same helper, not re-implement the
	// formula. Asserted in source because the template data map is built in a
	// function too large to exercise here.
	src := residual2ReadSource(t, "saas_provision.go")
	if !strings.Contains(src, `"SessionKey":   provisionSessionKey(h.ID)`) {
		t.Error(`saas_provision.go no longer sets "SessionKey" from provisionSessionKey(h.ID) — if it ` +
			`reverted to deriveDomainKey, provisioning emits the fleet-wide value while the sweep ` +
			`wants the per-hive one, and every newly provisioned spoke rolls forever (RESIDUAL-2)`)
	}
}

// --- SOURCE INVARIANT --------------------------------------------------------

func residual2ReadSource(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return string(raw)
}

// TestResidual2NoFleetWideSessionDerivationInReconcile asserts the shape at the
// source level. The behavioural tests above would catch a revert today, but
// they depend on fixtures that a merge could rewrite in the same commit that
// reverts the fix; this does not.
func TestResidual2NoFleetWideSessionDerivationInReconcile(t *testing.T) {
	src := residual2ReadSource(t, "perhive_env_reconcile.go")

	if regexp.MustCompile(`EnvSessionKey:\s*deriveDomainKey\(`).MatchString(src) {
		t.Error("perhive_env_reconcile.go derives EnvSessionKey with deriveDomainKey — that takes no " +
			"hiveID and is fleet-uniform by construction (RESIDUAL-2). Use provisionSessionKey(hiveID).")
	}
	if !regexp.MustCompile(`EnvSessionKey:\s*provisionSessionKey\(hiveID\)`).MatchString(src) {
		t.Error("perhive_env_reconcile.go no longer derives EnvSessionKey with provisionSessionKey(hiveID) " +
			"(RESIDUAL-2)")
	}
}

// TestResidual2PerHiveVarsCountFloor is the blunt instrument that survives
// renames: if the number of per-hive derivations in the desired-env map drops,
// something went back to a fleet-wide formula even if the names still look
// right. Exactly the signal a sync merge trips.
func TestResidual2PerHiveVarsCountFloor(t *testing.T) {
	src := residual2ReadSource(t, "perhive_env_reconcile.go")

	// After RESIDUAL-2 the desired-env map has four hive-bound derivations:
	// heartbeat, terminal, invite (each provision*Key(hiveID)) and session.
	// The two public keys are legitimately fleet-wide — they are PUBLIC halves
	// of hub-held Ed25519 seeds, so binding them per-hive would be meaningless.
	perHive := regexp.MustCompile(`provision(?:Heartbeat|Terminal|Invite|Session)Key\(hiveID\)`)
	if got := len(perHive.FindAllString(src, -1)); got < 4 {
		t.Errorf("perhive_env_reconcile.go has %d per-hive key derivations, want at least 4 "+
			"(heartbeat, terminal, invite, session) — one reverted to a fleet-wide formula (RESIDUAL-2)", got)
	}
}

// --- POSITIVE CONTROLS -------------------------------------------------------
//
// Without these, "make everything per-hive" would satisfy every assertion above
// while breaking the fleet in two distinct ways.

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

// TestResidual2FailsClosedWithoutIdentity asserts the guard that makes the
// per-hive formula safe. derivePerHiveKey returns "" for an empty hiveID, and
// desiredPerHiveEnv must SKIP such a hive rather than ship HIVE_SESSION_KEY=""
// — an empty var falls back exactly like an absent one on the spoke, but it
// also makes the readiness counter report the hive as converged when it is not.
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

// TestResidual2SelfDeriveDisagreementIsHarmless documents, as an executable
// claim, the single most likely way a change like this breaks a fleet.
//
// Spokes self-derive HIVE_SESSION_KEY when the env var is absent, and both
// spoke-side resolvers — Go spokeDomainKey and the proxy's deriveSessionKey —
// derive the FLEET-WIDE value from HIVE_HUB_SECRET, because neither mixes in
// the hive ID. So once the hub injects a per-hive value the two DISAGREE by
// construction, and during the 66-spoke roll the fleet holds a mix of both.
//
// That is safe here, and only here, because nothing verifies with this key:
// F1 deleted the Go legacy symmetric cookie lane, N3 stopped the terminal key
// falling through to it, and F23 deleted the Node proxy's copy. A disagreement
// over a value that no party ever compares has no observable effect.
//
// This test pins the premise, not the conclusion: it asserts the value is not
// used as a comparand. If some future change makes it one again, the
// disagreement becomes a live fleet-breaking bug and this test fails first.
func TestResidual2SessionKeyIsNotAVerificationComparand(t *testing.T) {
	// Go side: the legacy symmetric lane must stay deleted.
	cookieSrc := residual2ReadSource(t, "hub_cookie.go")
	if !strings.Contains(cookieSrc, "_ = legacySecret") {
		t.Error("hub_cookie.go no longer discards legacySecret — if the symmetric session lane came " +
			"back, spokes self-deriving the fleet-wide key would disagree with the hub's per-hive " +
			"value and hosted auth would break during the roll (RESIDUAL-2 / F1)")
	}

	// SpokeSessionKey must remain callable-but-unused on the Go side. A new
	// caller is the tripwire: it would be verifying with a value the hub now
	// derives differently.
	for _, f := range []string{"hub_cookie.go", "hub_session_revocation.go", "terminal_assertion.go"} {
		src := residual2ReadSource(t, f)
		if strings.Contains(src, "SpokeSessionKey()") {
			t.Errorf("%s calls SpokeSessionKey() — it had zero Go callers when HIVE_SESSION_KEY became "+
				"per-hive, and a spoke self-deriving the fleet-wide value would not match the hub "+
				"(RESIDUAL-2)", f)
		}
	}

	// The spoke-side resolver is deliberately left fleet-wide. Making it
	// per-hive would be dead code today and would hand a WRONG answer to a
	// spoke with no HIVE_ID, flipping the proxy's IS_HOSTED signal to false and
	// silently converting a hosted hive to the self-hosted identity model.
	keysSrc := residual2ReadSource(t, "hub_keys.go")
	if !strings.Contains(keysSrc, "func SpokeSessionKey() string { return spokeDomainKey(EnvSessionKey, infoSessionKey) }") {
		t.Error("SpokeSessionKey changed shape — it must keep reading the injected var first and " +
			"self-deriving the fleet-wide value only as a fallback; see provisionSessionKey for why " +
			"(RESIDUAL-2)")
	}
}
