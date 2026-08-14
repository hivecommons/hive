package hub

import (
	"strings"
	"testing"
	"time"
)

// Tests for follow-on PR #6: SSO / session Ed25519 PUBLIC key plurality.
//
// WHAT MAKES THESE TESTS ABLE TO FAIL. The trap this repo has hit repeatedly is
// a test that is green because the scenario it claims to exercise never
// actually occurs. With ONE generation — the state of every hub today —
// "current key only" and "current plus previous key" are indistinguishable,
// because previousPublicKeys returns nothing and the plural verifier degenerates
// to the singular one. So a test that merely calls the new code with a
// production-shaped set proves nothing at all.
//
// Every test below therefore either (a) installs a genuinely ROTATED set whose
// two generations derive DIFFERENT public keys and asserts on the difference, or
// (b) carries an explicit POSITIVE CONTROL asserting the opposite outcome from
// the same fixture, so that a change which makes verification accept everything
// is caught alongside one that makes it accept nothing.

const (
	pubGenSecretA = "pubkey-generation-test-master-alpha"
	pubGenSecretB = "pubkey-generation-test-master-bravo"
)

// rotatedPubKeySet builds a two-generation set: B current, A previous and live.
func rotatedPubKeySet(t *testing.T, now time.Time) *generationSet {
	t.Helper()
	gs := legacyGenerationSet(pubGenSecretA).rotate(pubGenSecretB, now, defaultVerifyWindow)
	if gs == nil {
		t.Fatal("fixture: rotate returned nil")
	}
	if gs.currentSecret() != pubGenSecretB {
		t.Fatalf("fixture: current secret = %q, want B", "<redacted>")
	}
	if len(gs.acceptableGenerations(now)) != 2 {
		t.Fatalf("fixture: want 2 acceptable generations, got %d", len(gs.acceptableGenerations(now)))
	}
	return gs
}

// TestSSOAndSessionPublicKeysDifferPerGeneration is the FIXTURE VALIDITY check
// every other test in this file depends on.
//
// If generation A and generation B happened to derive the same public key, every
// "verifies under the previous generation" assertion below would pass without
// the previous generation ever being consulted — the classic passes-for-the-
// wrong-reason failure. Asserting the keys are distinct up front means a later
// change that collapses them is reported HERE, as a fixture fault, rather than
// silently hollowing out the rest of the file.
func TestSSOAndSessionPublicKeysDifferPerGeneration(t *testing.T) {
	now := time.Now()
	gs := rotatedPubKeySet(t, now)
	gens := gs.acceptableGenerations(now)

	for _, tc := range []struct {
		name string
		info string
	}{
		{"sso", infoSSOEd25519Seed},
		{"session", infoSessionEd25519Seed},
	} {
		cur := ssoPublicKeyFromSeed(deriveDomainKey(gens[0].Secret, tc.info))
		prev := ssoPublicKeyFromSeed(deriveDomainKey(gens[1].Secret, tc.info))
		if cur == "" || prev == "" {
			t.Fatalf("%s: derived an empty public key (cur empty=%v prev empty=%v)", tc.name, cur == "", prev == "")
		}
		if cur == prev {
			t.Fatalf("%s: current and previous generations derived the SAME public key; every dual-key test in this file would pass vacuously", tc.name)
		}
		if len(cur) != 64 {
			t.Fatalf("%s: public key is %d hex chars, want 64", tc.name, len(cur))
		}
	}
}

// TestPreviousPublicKeysEmptyWithOneGeneration is the NO-OP proof.
//
// This is the single most important assertion in the PR: with one generation —
// production, today, on every hub — there is no previous key, so no _PREV var is
// emitted, so no spoke drifts, so no pod rolls. If this fails, merging this PR
// rolls the fleet.
func TestPreviousPublicKeysEmptyWithOneGeneration(t *testing.T) {
	now := time.Now()
	single := legacyGenerationSet(pubGenSecretA)
	if single == nil {
		t.Fatal("fixture: legacyGenerationSet returned nil")
	}

	for _, tc := range []struct {
		name string
		info string
	}{
		{"sso", infoSSOEd25519Seed},
		{"session", infoSessionEd25519Seed},
	} {
		if got := previousPublicKeys(single, tc.info, now); len(got) != 0 {
			t.Fatalf("%s: previousPublicKeys on a single-generation set returned %d keys, want 0", tc.name, len(got))
		}
	}

	// POSITIVE CONTROL, in the other direction: the same function on a ROTATED
	// set must return exactly one key. Without this, deleting the body of
	// previousPublicKeys and returning nil unconditionally would leave the
	// assertions above green while silently disabling the entire feature.
	rotated := rotatedPubKeySet(t, now)
	for _, tc := range []struct {
		name string
		info string
	}{
		{"sso", infoSSOEd25519Seed},
		{"session", infoSessionEd25519Seed},
	} {
		got := previousPublicKeys(rotated, tc.info, now)
		if len(got) != 1 {
			t.Fatalf("%s: positive control: rotated set returned %d previous keys, want 1", tc.name, len(got))
		}
	}
}

// TestPreviousPublicKeysExcludesExpiredGeneration asserts the finiteness
// promise: once VerifyUntil passes, the previous public key stops being offered
// and therefore comes off the fleet.
//
// It also pins the ZERO-VerifyUntil rule, which is the property that stops a
// hand-edited generations file pinning a retired key live forever.
func TestPreviousPublicKeysExcludesExpiredGeneration(t *testing.T) {
	now := time.Now()
	gs := rotatedPubKeySet(t, now)

	// Live: inside the window.
	if got := previousPublicKeys(gs, infoSSOEd25519Seed, now); len(got) != 1 {
		t.Fatalf("inside verify window: got %d previous keys, want 1", len(got))
	}
	// Expired: one second past the window.
	after := now.Add(defaultVerifyWindow).Add(time.Second)
	if got := previousPublicKeys(gs, infoSSOEd25519Seed, after); len(got) != 0 {
		t.Fatalf("past verify window: got %d previous keys, want 0", len(got))
	}

	// VerifyUntil.IsZero() means ALREADY EXPIRED, not "never expires".
	zeroed := &generationSet{
		Current: gs.Current,
		Generations: []keyGeneration{
			gs.Generations[0],
			{ID: gs.Generations[1].ID, Secret: gs.Generations[1].Secret}, // no VerifyUntil
		},
	}
	if got := previousPublicKeys(zeroed, infoSSOEd25519Seed, now); len(got) != 0 {
		t.Fatalf("zero VerifyUntil: got %d previous keys, want 0 (zero must mean already expired)", len(got))
	}
}

// TestSSOTokenVerifiesUnderPreviousGeneration is the CORE fleet-safety property.
//
// A spoke that has not yet been reconciled holds the previous generation's
// public key. It must still be able to verify a token the hub minted under that
// generation, and it must NOT accept a token minted under a generation the hub
// has retired.
func TestSSOTokenVerifiesUnderPreviousGeneration(t *testing.T) {
	now := time.Now()
	gs := rotatedPubKeySet(t, now)
	gens := gs.acceptableGenerations(now)
	curPub := ssoPublicKeyForGeneration(gens[0])
	prevPub := ssoPublicKeyForGeneration(gens[1])

	const hiveID = "hive-alpha"
	// A token minted by the hub under the CURRENT generation.
	curTok := MintSSOToken(deriveDomainKey(gens[0].Secret, infoSSOEd25519Seed), "alice", "owner", hiveID, now)
	// A token minted under the PREVIOUS generation — i.e. minted just before the
	// rotation, still in flight.
	prevTok := MintSSOToken(deriveDomainKey(gens[1].Secret, infoSSOEd25519Seed), "bob", "owner", hiveID, now)
	if curTok == "" || prevTok == "" {
		t.Fatal("fixture: MintSSOToken returned empty")
	}

	keys := []string{curPub, prevPub}

	// Both tokens verify against the two-key spoke.
	if u, _, idx, err := VerifySSOTokenAcrossKeys(keys, curTok, hiveID, now); err != nil || u != "alice" || idx != 0 {
		t.Fatalf("current-generation token: user=%q idx=%d err=%v; want alice/0/nil", u, idx, err)
	}
	if u, _, idx, err := VerifySSOTokenAcrossKeys(keys, prevTok, hiveID, now); err != nil || u != "bob" || idx != 1 {
		t.Fatalf("previous-generation token: user=%q idx=%d err=%v; want bob/1/nil", u, idx, err)
	}

	// POSITIVE CONTROL — the failing direction. A spoke holding ONLY the current
	// key must REJECT the previous-generation token. Without this assertion the
	// two above would pass even if VerifySSOTokenAcrossKeys ignored `keys`
	// entirely and accepted any well-formed token.
	if _, _, _, err := VerifySSOTokenAcrossKeys([]string{curPub}, prevTok, hiveID, now); err == nil {
		t.Fatal("positive control: a spoke holding ONLY the current key accepted a previous-generation token")
	}
	// And the mirror: current key alone still accepts the current token, so the
	// rejection above is about the KEY and not about the token being broken.
	if u, _, _, err := VerifySSOTokenAcrossKeys([]string{curPub}, curTok, hiveID, now); err != nil || u != "alice" {
		t.Fatalf("positive control mirror: current key rejected the current token: user=%q err=%v", u, err)
	}
}

// TestSSOTokenFromRetiredGenerationIsRejected asserts that expiry actually
// binds end to end: once the previous generation ages out, the reconcile lane
// stops offering its key, and a token signed by it verifies nowhere.
func TestSSOTokenFromRetiredGenerationIsRejected(t *testing.T) {
	now := time.Now()
	gs := rotatedPubKeySet(t, now)
	gens := gs.acceptableGenerations(now)

	const hiveID = "hive-alpha"
	retiredTok := MintSSOToken(deriveDomainKey(gens[1].Secret, infoSSOEd25519Seed), "bob", "owner", hiveID, now)
	if retiredTok == "" {
		t.Fatal("fixture: MintSSOToken returned empty")
	}

	// Well inside the verify window the key IS offered, so the token verifies.
	// This is the control that proves the rejection below is caused by RETIREMENT
	// and not by the token being malformed or expired on its own TTL.
	liveKeys := append(
		[]string{ssoPublicKeyForGeneration(gens[0])},
		previousPublicKeys(gs, infoSSOEd25519Seed, now)...,
	)
	if _, _, _, err := VerifySSOTokenAcrossKeys(liveKeys, retiredTok, hiveID, now); err != nil {
		t.Fatalf("control: token should verify while its generation is live, got %v", err)
	}

	// After the window the previous key is no longer offered. The token is
	// evaluated at the SAME clock as the control above (`now`), so its own
	// 90-second TTL is not what rejects it — the missing key is.
	after := now.Add(defaultVerifyWindow).Add(time.Second)
	retiredKeys := append(
		[]string{ssoPublicKeyForGeneration(gens[0])},
		previousPublicKeys(gs, infoSSOEd25519Seed, after)...,
	)
	if len(retiredKeys) != 1 {
		t.Fatalf("fixture: after retirement want 1 key, got %d", len(retiredKeys))
	}
	if _, _, _, err := VerifySSOTokenAcrossKeys(retiredKeys, retiredTok, hiveID, now); err == nil {
		t.Fatal("a token signed by a RETIRED generation was accepted")
	}
}

// TestMalformedSecondKeyDoesNotBreakTheFirst is the fail-closed invariant.
//
// A garbage HIVE_SSO_PUBLIC_KEY_PREV — truncated, non-hex, or a comma-joined
// list someone tried as an env encoding — must cost one skipped candidate and
// must NOT prevent the primary key from verifying.
func TestMalformedSecondKeyDoesNotBreakTheFirst(t *testing.T) {
	now := time.Now()
	gs := rotatedPubKeySet(t, now)
	gens := gs.acceptableGenerations(now)
	curPub := ssoPublicKeyForGeneration(gens[0])
	prevPub := ssoPublicKeyForGeneration(gens[1])

	const hiveID = "hive-alpha"
	curTok := MintSSOToken(deriveDomainKey(gens[0].Secret, infoSSOEd25519Seed), "alice", "owner", hiveID, now)
	if curTok == "" {
		t.Fatal("fixture: MintSSOToken returned empty")
	}

	malformed := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"not hex", strings.Repeat("z", 64)},
		{"too short", curPub[:62]},
		{"too long", curPub + "ab"},
		// The delimited-list encoding this PR rejected. Go's hex.DecodeString
		// errors on the comma while Node's Buffer.from silently truncates — the
		// disagreement that ruled the encoding out. It must be DROPPED here, not
		// half-accepted.
		{"comma-joined pair", curPub + "," + prevPub},
	}

	for _, m := range malformed {
		t.Run(m.name, func(t *testing.T) {
			u, _, idx, err := VerifySSOTokenAcrossKeys([]string{curPub, m.key}, curTok, hiveID, now)
			if err != nil || u != "alice" {
				t.Fatalf("malformed second key %q broke verification of the first: user=%q err=%v", m.name, u, err)
			}
			if idx != 0 {
				t.Fatalf("expected the PRIMARY key (index 0) to verify, got index %d", idx)
			}
			// The malformed entry must be DROPPED, not merely out-ranked. If it
			// survived validation it would be handed to ed25519.Verify, which
			// PANICS on a wrong-length public key.
			if got := validPublicKeys(curPub, m.key); len(got) != 1 || got[0] != curPub {
				t.Fatalf("validPublicKeys kept a malformed entry: got %d keys", len(got))
			}
		})
	}

	// POSITIVE CONTROL: an entirely malformed key set must verify NOTHING.
	// Without this, validPublicKeys could return everything and these tests
	// would still pass on the strength of the primary key alone.
	if _, _, _, err := VerifySSOTokenAcrossKeys([]string{"zzzz", ""}, curTok, hiveID, now); err == nil {
		t.Fatal("positive control: an all-malformed key set verified a token")
	}
}

// TestSpokeSSOPublicKeysLegacySingleValueConfig asserts the backward-compatible
// case that describes all 65 spokes today: HIVE_SSO_PUBLIC_KEY set,
// HIVE_SSO_PUBLIC_KEY_PREV absent.
func TestSpokeSSOPublicKeysLegacySingleValueConfig(t *testing.T) {
	now := time.Now()
	gs := rotatedPubKeySet(t, now)
	gens := gs.acceptableGenerations(now)
	curPub := ssoPublicKeyForGeneration(gens[0])
	prevPub := ssoPublicKeyForGeneration(gens[1])

	// Legacy: primary only, no _PREV. This is production today.
	t.Setenv(EnvSSOPublicKey, curPub)
	t.Setenv(EnvSSOPublicKeyPrevious, "")
	got := SpokeSSOPublicKeys()
	if len(got) != 1 || got[0] != curPub {
		t.Fatalf("legacy single-value config: got %d keys, want exactly the primary", len(got))
	}
	// It must be byte-identical to what the singular accessor returns, so a
	// spoke on today's env verifies exactly what it verifies now.
	if single := SpokeSSOPublicKey(); single != got[0] {
		t.Fatal("plural accessor disagreed with SpokeSSOPublicKey on the legacy config")
	}

	// Rotated: both vars set.
	t.Setenv(EnvSSOPublicKeyPrevious, prevPub)
	got = SpokeSSOPublicKeys()
	if len(got) != 2 || got[0] != curPub || got[1] != prevPub {
		t.Fatalf("rotated config: got %d keys, want [current, previous]", len(got))
	}

	// A _PREV equal to the primary is de-duplicated rather than doubling the
	// verification work on the hottest hosted path.
	t.Setenv(EnvSSOPublicKeyPrevious, curPub)
	if got = SpokeSSOPublicKeys(); len(got) != 1 {
		t.Fatalf("duplicate _PREV: got %d keys, want 1", len(got))
	}

	// A malformed _PREV degrades to the legacy single-key config rather than
	// disabling SSO.
	t.Setenv(EnvSSOPublicKeyPrevious, "not-a-key")
	if got = SpokeSSOPublicKeys(); len(got) != 1 || got[0] != curPub {
		t.Fatalf("malformed _PREV: got %d keys, want exactly the primary", len(got))
	}
}

// TestDesiredPerHiveEnvPrevVarsFollowGenerations ties the verifier to the
// ROLLOUT: the _PREV vars appear only after a rotation and disappear again once
// the previous generation expires.
//
// The absent case is the one that decides whether merging this PR rolls the
// fleet, so it is asserted first and asserted by NAME rather than by count.
func TestDesiredPerHiveEnvPrevVarsFollowGenerations(t *testing.T) {
	withTestMaster(t, pubGenSecretA)
	const hiveID = "hive-alpha"

	// ONE GENERATION — production today. Neither _PREV var may be present.
	withProvisionGenerations(t, legacyGenerationSet(pubGenSecretA))
	got := desiredPerHiveEnv(hiveID)
	if got == nil {
		t.Fatal("desiredPerHiveEnv returned nil on a single-generation set")
	}
	for _, name := range perHiveEnvOptionalNames() {
		if v, ok := got[name]; ok {
			t.Fatalf("single generation: %s present (value len %d); merging this PR would drift and roll every spoke", name, len(v))
		}
	}
	// And with no _PREV wanted, a spoke that has neither var is CONVERGED —
	// no patch, no pod roll. This is the assertion that says "no-op".
	live := make([]deploymentEnvVar, 0, len(got))
	for _, name := range perHiveEnvNames() {
		live = append(live, deploymentEnvVar{Name: name, Value: got[name]})
	}
	if drift := perHiveEnvDrift(live, got); len(drift) != 0 {
		t.Fatalf("single generation: a spoke with today's env shape drifted on %v", drift)
	}

	// ROTATED — both _PREV vars must appear, carrying the PREVIOUS generation's
	// public keys.
	now := time.Now()
	rotated := rotatedPubKeySet(t, now)
	withProvisionGenerations(t, rotated)
	got = desiredPerHiveEnv(hiveID)
	if got == nil {
		t.Fatal("desiredPerHiveEnv returned nil on a rotated set")
	}
	gens := rotated.acceptableGenerations(now)
	wantSSOPrev := ssoPublicKeyForGeneration(gens[1])
	wantSessPrev := ssoPublicKeyFromSeed(deriveDomainKey(gens[1].Secret, infoSessionEd25519Seed))
	if got[EnvSSOPublicKeyPrevious] != wantSSOPrev {
		t.Fatalf("rotated: %s is not the previous generation's SSO public key", EnvSSOPublicKeyPrevious)
	}
	if got[envSessionPublicKeyPrevious] != wantSessPrev {
		t.Fatalf("rotated: %s is not the previous generation's session public key", envSessionPublicKeyPrevious)
	}
	// The PRIMARY vars must still carry the CURRENT generation — a _PREV that
	// overwrote the primary would break every already-converged spoke.
	if got[EnvSSOPublicKey] != ssoPublicKeyForGeneration(gens[0]) {
		t.Fatal("rotated: primary SSO public key is not the CURRENT generation's")
	}
	if got[EnvSSOPublicKey] == got[EnvSSOPublicKeyPrevious] {
		t.Fatal("rotated: primary and previous SSO public keys are identical")
	}

	// A spoke still carrying only the five mandatory vars must now DRIFT on the
	// missing _PREV — this is what makes the reconcile lane roll the fleet onto
	// the new key set.
	stale := make([]deploymentEnvVar, 0, len(perHiveEnvNames()))
	for _, name := range perHiveEnvNames() {
		stale = append(stale, deploymentEnvVar{Name: name, Value: got[name]})
	}
	drift := perHiveEnvDrift(stale, got)
	if len(drift) != 2 {
		t.Fatalf("rotated: a spoke missing both _PREV vars drifted on %v, want both", drift)
	}
}

// TestPerHiveEnvDriftRemovesRetiredPrevVars asserts the RETIREMENT direction:
// a spoke still carrying a _PREV var after the previous generation expired must
// drift, and the patch must actually DROP the var rather than leave it.
//
// Without this the retired generation's public key would sit on all 65 spokes
// forever — the "unversioned, permanent compat lane" the generations design
// exists to prevent, reintroduced as a stale env var.
func TestPerHiveEnvDriftRemovesRetiredPrevVars(t *testing.T) {
	withTestMaster(t, pubGenSecretA)
	withProvisionGenerations(t, legacyGenerationSet(pubGenSecretA))
	const hiveID = "hive-alpha"

	want := desiredPerHiveEnv(hiveID)
	if want == nil {
		t.Fatal("desiredPerHiveEnv returned nil")
	}
	if _, ok := want[EnvSSOPublicKeyPrevious]; ok {
		t.Fatal("fixture: single-generation set unexpectedly wants a _PREV var")
	}

	// A spoke carrying a leftover _PREV from a rotation whose window has closed.
	live := make([]deploymentEnvVar, 0, len(perHiveEnvNames())+1)
	for _, name := range perHiveEnvNames() {
		live = append(live, deploymentEnvVar{Name: name, Value: want[name]})
	}
	const stale = "aa" + "bb" + "cc" + "dd"
	live = append(live, deploymentEnvVar{Name: EnvSSOPublicKeyPrevious, Value: stale})

	drift := perHiveEnvDrift(live, want)
	if len(drift) != 1 || drift[0] != EnvSSOPublicKeyPrevious {
		t.Fatalf("a leftover retired _PREV did not drift: got %v", drift)
	}

	patch, err := perHiveEnvPatchJSON(live, want)
	if err != nil {
		t.Fatalf("perHiveEnvPatchJSON: %v", err)
	}
	if strings.Contains(patch, EnvSSOPublicKeyPrevious) {
		t.Fatal("the patch retained the retired _PREV var instead of dropping it")
	}
	// POSITIVE CONTROL: the patch must still carry the mandatory vars. A patch
	// that dropped EVERYTHING would also pass the assertion above.
	for _, name := range perHiveEnvNames() {
		if !strings.Contains(patch, name) {
			t.Fatalf("the patch dropped mandatory var %s", name)
		}
	}
}

// TestPerHiveEnvPatchIsStableWhenConverged guards the pod-roll-forever hazard:
// once a rotated spoke has both _PREV vars, the next sweep must see NO drift.
//
// perHiveEnvPatchJSON appends optional vars after the mandatory ones, so a
// converged Deployment must produce a byte-identical array. If it did not, the
// sweep would patch every hive every cycle and roll all 65 pods every 15
// minutes indefinitely.
func TestPerHiveEnvPatchIsStableWhenConverged(t *testing.T) {
	withTestMaster(t, pubGenSecretA)
	now := time.Now()
	withProvisionGenerations(t, rotatedPubKeySet(t, now))
	const hiveID = "hive-alpha"

	want := desiredPerHiveEnv(hiveID)
	if want == nil {
		t.Fatal("desiredPerHiveEnv returned nil")
	}
	if len(want) != len(perHiveEnvNames())+len(perHiveEnvOptionalNames()) {
		t.Fatalf("rotated: want %d vars, got %d", len(perHiveEnvNames())+len(perHiveEnvOptionalNames()), len(want))
	}

	// Start from a drifted spoke, apply the patch shape, then re-check drift.
	live := []deploymentEnvVar{{Name: "HIVE_ID", Value: hiveID}}
	patched := applyPerHiveEnvPatchShape(live, want)
	if drift := perHiveEnvDrift(patched, want); len(drift) != 0 {
		t.Fatalf("a freshly patched spoke still drifts on %v — the sweep would roll its pod every cycle", drift)
	}
	// An unrelated var must survive untouched.
	if patched[0].Name != "HIVE_ID" || patched[0].Value != hiveID {
		t.Fatal("the patch clobbered an unrelated env var")
	}
}

// applyPerHiveEnvPatchShape mirrors perHiveEnvPatchJSON's merge, so the test
// above exercises the real ordering rather than assuming one. It is written as
// a separate helper rather than by unmarshalling the patch JSON so that a
// failure points at the merge rule rather than at JSON plumbing.
func applyPerHiveEnvPatchShape(live []deploymentEnvVar, want map[string]string) []deploymentEnvVar {
	removable := map[string]bool{}
	for _, name := range perHiveEnvOptionalNames() {
		if _, wanted := want[name]; !wanted {
			removable[name] = true
		}
	}
	out := make([]deploymentEnvVar, 0, len(live)+len(want))
	seen := map[string]bool{}
	for _, e := range live {
		if removable[e.Name] {
			continue
		}
		if v, ok := want[e.Name]; ok {
			e.Value = v
			seen[e.Name] = true
		}
		out = append(out, e)
	}
	for _, name := range perHiveEnvNames() {
		if !seen[name] {
			out = append(out, deploymentEnvVar{Name: name, Value: want[name]})
		}
	}
	for _, name := range perHiveEnvOptionalNames() {
		if v, wanted := want[name]; wanted && !seen[name] {
			out = append(out, deploymentEnvVar{Name: name, Value: v})
		}
	}
	return out
}

// TestPrivateSeedsNeverLeaveTheHub is the non-negotiable invariant made
// mechanical: nothing this PR adds to a spoke's env may be private material.
//
// The check is not "does the code look right" but "is the value the spoke gets
// derivable into the seed". Every _PREV value must be a PUBLIC key: equal to
// the public half of its generation's seed, and NOT equal to the seed itself.
func TestPrivateSeedsNeverLeaveTheHub(t *testing.T) {
	withTestMaster(t, pubGenSecretA)
	now := time.Now()
	gs := rotatedPubKeySet(t, now)
	withProvisionGenerations(t, gs)

	got := desiredPerHiveEnv("hive-alpha")
	if got == nil {
		t.Fatal("desiredPerHiveEnv returned nil")
	}

	gens := gs.acceptableGenerations(now)
	var seeds []string
	for _, g := range gens {
		seeds = append(seeds,
			deriveDomainKey(g.Secret, infoSSOEd25519Seed),
			deriveDomainKey(g.Secret, infoSessionEd25519Seed),
		)
		// The masters themselves must never appear either.
		seeds = append(seeds, g.Secret)
	}

	for name, value := range got {
		for _, seed := range seeds {
			if seed != "" && value == seed {
				t.Fatalf("env var %s carries PRIVATE material (a signing seed or a master)", name)
			}
		}
	}

	// And specifically: each _PREV var IS the public half of its generation's
	// seed. This is the assertion that would fail if someone "simplified" the
	// derivation by dropping ssoPublicKeyFromSeed and shipping the seed.
	if got[EnvSSOPublicKeyPrevious] != ssoPublicKeyFromSeed(deriveDomainKey(gens[1].Secret, infoSSOEd25519Seed)) {
		t.Fatalf("%s is not the public half of the previous generation's SSO seed", EnvSSOPublicKeyPrevious)
	}
	if got[envSessionPublicKeyPrevious] != ssoPublicKeyFromSeed(deriveDomainKey(gens[1].Secret, infoSessionEd25519Seed)) {
		t.Fatalf("%s is not the public half of the previous generation's session seed", envSessionPublicKeyPrevious)
	}
}

// ---------------------------------------------------------------------------
// Direct unit tests for the internal helpers (#3784).
//
// The tests above exercise these helpers only THROUGH the exported surface
// (SpokeSSOPublicKeys, VerifySSOTokenAcrossKeys, desiredPerHiveEnv). That
// indirection means a helper's contract could drift — say, validPublicKeys
// starting to return malformed entries that a caller happens to re-filter — and
// nothing here would notice until the second caller appeared. These helpers sit
// on the key-rotation path, so each one gets its contract pinned directly, with
// the same positive-control discipline as the rest of the file: every rejection
// assertion is paired with an acceptance assertion on the same fixture.
//
// All key material below is derived from the fixed test masters at the top of
// this file; nothing is a production value.
// ---------------------------------------------------------------------------

// malformedPublicKeys is the shared rejection corpus for validPublicKeys and
// appendDistinctPublicKey. Each entry must be DROPPED by validation: an
// unvalidated wrong-length key would reach ed25519.Verify, which PANICS.
func malformedPublicKeys(t *testing.T, curPub, prevPub string) []struct{ name, key string } {
	t.Helper()
	if len(curPub) != 64 || len(prevPub) != 64 {
		t.Fatalf("fixture: want 64-hex-char keys, got %d and %d", len(curPub), len(prevPub))
	}
	return []struct{ name, key string }{
		{"empty", ""},
		{"whitespace only", " \t "},
		{"not hex", strings.Repeat("z", 64)},
		{"odd length", curPub[:63]},
		{"too short", curPub[:62]},
		{"too long", curPub + "ab"},
		{"0x prefix", "0x" + curPub[:62]},
		// The delimited-list encoding the PR rejected: must be dropped whole,
		// never truncated to its first key the way Node's Buffer.from would.
		{"comma-joined pair", curPub + "," + prevPub},
	}
}

// TestFirstOrEmpty pins the accessor both provision helpers stand on. If it
// ever returned a later element, the _PREV vars would silently carry the wrong
// generation's key.
func TestFirstOrEmpty(t *testing.T) {
	if got := firstOrEmpty(nil); got != "" {
		t.Fatalf("nil slice: got %q, want empty", got)
	}
	if got := firstOrEmpty([]string{}); got != "" {
		t.Fatalf("empty slice: got %q, want empty", got)
	}
	// POSITIVE CONTROL: a populated slice yields its FIRST element — the
	// most-recent previous generation, per previousPublicKeys' ordering.
	if got := firstOrEmpty([]string{"first"}); got != "first" {
		t.Fatalf("one-element slice: got %q, want first", got)
	}
	if got := firstOrEmpty([]string{"first", "second"}); got != "first" {
		t.Fatalf("two-element slice: got %q, want the FIRST element", got)
	}
}

// TestValidPublicKeysFiltering pins validPublicKeys' contract directly: keep
// well-formed 32-byte hex Ed25519 keys in order, trim whitespace, drop
// everything else.
func TestValidPublicKeysFiltering(t *testing.T) {
	now := time.Now()
	gs := rotatedPubKeySet(t, now)
	gens := gs.acceptableGenerations(now)
	curPub := ssoPublicKeyForGeneration(gens[0])
	prevPub := ssoPublicKeyForGeneration(gens[1])

	// POSITIVE CONTROL first: well-formed keys pass through unchanged, in
	// order. Without this, a filter that drops EVERYTHING would pass every
	// rejection case below.
	if got := validPublicKeys(curPub, prevPub); len(got) != 2 || got[0] != curPub || got[1] != prevPub {
		t.Fatalf("two valid keys: got %v-element result, want [current, previous] in order", len(got))
	}
	// Zero input yields zero output, not nil-dereference or a phantom entry.
	if got := validPublicKeys(); len(got) != 0 {
		t.Fatalf("no input: got %d keys, want 0", len(got))
	}
	// Surrounding whitespace is TRIMMED, not rejected: the env var is operator-
	// written, and the trimmed form is what callers compare against.
	if got := validPublicKeys("  " + curPub + "\t"); len(got) != 1 || got[0] != curPub {
		t.Fatalf("whitespace-wrapped key: got %d keys, want exactly the trimmed key", len(got))
	}

	for _, m := range malformedPublicKeys(t, curPub, prevPub) {
		t.Run(m.name, func(t *testing.T) {
			if got := validPublicKeys(m.key); len(got) != 0 {
				t.Fatalf("malformed key %q survived validation (%d keys)", m.name, len(got))
			}
			// POSITIVE CONTROL on the same fixture: the malformed entry is
			// dropped while its valid neighbours survive, in order — so the
			// rejection above cannot be the work of a filter that rejects all.
			got := validPublicKeys(curPub, m.key, prevPub)
			if len(got) != 2 || got[0] != curPub || got[1] != prevPub {
				t.Fatalf("mixed list with %q: got %d keys, want the two valid neighbours in order", m.name, len(got))
			}
		})
	}
}

// TestAppendDistinctPublicKey pins the dedup helper SpokeSSOPublicKeys uses to
// merge _PREV into the key list: append when well-formed and new, no-op when
// duplicate or malformed.
func TestAppendDistinctPublicKey(t *testing.T) {
	now := time.Now()
	gs := rotatedPubKeySet(t, now)
	gens := gs.acceptableGenerations(now)
	curPub := ssoPublicKeyForGeneration(gens[0])
	prevPub := ssoPublicKeyForGeneration(gens[1])

	// POSITIVE CONTROL: a distinct well-formed candidate is appended AFTER the
	// existing keys — current-then-previous is the trial-verification order.
	if got := appendDistinctPublicKey([]string{curPub}, prevPub); len(got) != 2 || got[0] != curPub || got[1] != prevPub {
		t.Fatalf("distinct candidate: got %d keys, want [current, previous]", len(got))
	}
	// Appending to an empty list works: this is what SpokeSSOPublicKeys hits
	// when the primary var is unset but _PREV is present.
	if got := appendDistinctPublicKey(nil, prevPub); len(got) != 1 || got[0] != prevPub {
		t.Fatalf("append to nil: got %d keys, want exactly the candidate", len(got))
	}

	// A duplicate is NOT re-appended — the state right after a rotation writes
	// the var but before the generations actually differ. Doubling here would
	// double the Ed25519 work on the hottest hosted path.
	if got := appendDistinctPublicKey([]string{curPub}, curPub); len(got) != 1 || got[0] != curPub {
		t.Fatalf("duplicate candidate: got %d keys, want 1", len(got))
	}
	// Dedup must compare the VALIDATED (trimmed) form, or a space-padded
	// duplicate would sneak past the string compare.
	if got := appendDistinctPublicKey([]string{curPub}, "  "+curPub+" "); len(got) != 1 {
		t.Fatalf("whitespace-wrapped duplicate: got %d keys, want 1", len(got))
	}

	// Malformed candidates leave the list UNTOUCHED — a garbage _PREV must cost
	// nothing, not corrupt the primary. The acceptance control for each of
	// these is the distinct-candidate append at the top of the test.
	for _, m := range malformedPublicKeys(t, curPub, prevPub) {
		t.Run(m.name, func(t *testing.T) {
			got := appendDistinctPublicKey([]string{curPub}, m.key)
			if len(got) != 1 || got[0] != curPub {
				t.Fatalf("malformed candidate %q changed the list: got %d keys", m.name, len(got))
			}
		})
	}
}

// TestProvisionPublicKeyPreviousFollowsGenerations pins the two provisioning
// entry points directly: empty with one generation (so desiredPerHiveEnv omits
// the vars and no spoke rolls), the previous generation's DOMAIN-SEPARATED
// public key after a rotation, and empty again once the window closes.
func TestProvisionPublicKeyPreviousFollowsGenerations(t *testing.T) {
	now := time.Now()

	// SINGLE GENERATION — production today. Both helpers must return "" so the
	// caller omits the vars entirely rather than shipping value: "".
	withProvisionGenerations(t, legacyGenerationSet(pubGenSecretA))
	if got := provisionSSOPublicKeyPrevious(); got != "" {
		t.Fatalf("single generation: SSO _PREV = %d chars, want empty", len(got))
	}
	if got := provisionSessionPublicKeyPrevious(); got != "" {
		t.Fatalf("single generation: session _PREV = %d chars, want empty", len(got))
	}

	// ROTATED — POSITIVE CONTROL for the empties above: each helper now returns
	// exactly the PREVIOUS generation's public key for ITS domain.
	rotated := rotatedPubKeySet(t, now)
	withProvisionGenerations(t, rotated)
	gens := rotated.acceptableGenerations(now)
	gotSSO := provisionSSOPublicKeyPrevious()
	gotSess := provisionSessionPublicKeyPrevious()
	if want := ssoPublicKeyFromSeed(deriveDomainKey(gens[1].Secret, infoSSOEd25519Seed)); gotSSO != want {
		t.Fatal("rotated: SSO _PREV is not the previous generation's SSO public key")
	}
	if want := ssoPublicKeyFromSeed(deriveDomainKey(gens[1].Secret, infoSessionEd25519Seed)); gotSess != want {
		t.Fatal("rotated: session _PREV is not the previous generation's session public key")
	}
	if gotSSO == ssoPublicKeyForGeneration(gens[0]) {
		t.Fatal("rotated: SSO _PREV equals the CURRENT generation's key — provisioning would offer no previous key at all")
	}
	if gotSSO == gotSess {
		t.Fatal("rotated: SSO and session _PREV values are identical — domain separation lost")
	}

	// WIRE NAMES: desiredPerHiveEnv must publish exactly these helper values
	// under exactly these var names — the spelling the spoke resolvers read.
	withTestMaster(t, pubGenSecretA)
	env := desiredPerHiveEnv("hive-alpha")
	if env == nil {
		t.Fatal("desiredPerHiveEnv returned nil on a rotated set")
	}
	if env[EnvSSOPublicKeyPrevious] != gotSSO {
		t.Fatalf("%s does not carry provisionSSOPublicKeyPrevious()'s value", EnvSSOPublicKeyPrevious)
	}
	if env[envSessionPublicKeyPrevious] != gotSess {
		t.Fatalf("%s does not carry provisionSessionPublicKeyPrevious()'s value", envSessionPublicKeyPrevious)
	}

	// GENERATION BOUNDARY: a previous generation with a ZERO VerifyUntil is
	// ALREADY EXPIRED, so both helpers return "" again — the self-clearing
	// property, exercised through the provision entry points themselves. (The
	// helpers read time.Now() internally, so the boundary is driven with the
	// zero-VerifyUntil rule rather than an advanced clock; the wall-clock
	// expiry path is covered by TestPreviousPublicKeysExcludesExpiredGeneration.)
	zeroed := &generationSet{
		Current: rotated.Current,
		Generations: []keyGeneration{
			rotated.Generations[0],
			{ID: rotated.Generations[1].ID, Secret: rotated.Generations[1].Secret}, // no VerifyUntil
		},
	}
	withProvisionGenerations(t, zeroed)
	if got := provisionSSOPublicKeyPrevious(); got != "" {
		t.Fatalf("zero VerifyUntil: SSO _PREV = %d chars, want empty (zero must mean already expired)", len(got))
	}
	if got := provisionSessionPublicKeyPrevious(); got != "" {
		t.Fatalf("zero VerifyUntil: session _PREV = %d chars, want empty (zero must mean already expired)", len(got))
	}
}
