package delegation

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestThirdPartyVerificationAgainstPublishedMaterial is the POSITIVE CONTROL
// for the competitive claim.
//
// It performs exactly what a tenant's own service does, with nothing else in
// scope: take a token, take the published key document, verify. No hive
// credential, no minter, no access to any secret — the test literally never
// touches testSeed() after minting, and the verifying half uses only bytes that
// came out of the published document.
func TestThirdPartyVerificationAgainstPublishedMaterial(t *testing.T) {
	now := time.Now()

	// --- The hive side: mint a chain. ---
	c, err := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	if err != nil {
		t.Fatalf("building chain: %v", err)
	}
	c.Generation = 1
	token := MintToken(testSeed(), c, now)
	if token == "" {
		t.Fatal("MintToken produced nothing")
	}

	// --- The published material, as a third party would fetch it. ---
	doc := BuildKeyDocument(true, 1, testPub(), nil, now)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshalling published document: %v", err)
	}

	// --- The third party: only the token and the fetched JSON. ---
	var fetched KeyDocument
	if err := json.Unmarshal(raw, &fetched); err != nil {
		t.Fatalf("third party could not parse the published document: %v", err)
	}
	if !fetched.Enabled {
		t.Error("published document reports disabled but keys were minted")
	}
	if fetched.ChainVersion != ChainVersion {
		t.Errorf("published chain_version = %q, want %q", fetched.ChainVersion, ChainVersion)
	}
	if len(fetched.Keys) == 0 {
		t.Fatal("published document carries no keys")
	}

	got, keyIdx, err := VerifyTokenAcrossKeys(fetched.PublicKeysFrom(), token, now)
	if err != nil {
		t.Fatalf("third-party verification FAILED against published material: %v", err)
	}
	if keyIdx < 0 {
		t.Error("verification succeeded but reported no accepting key")
	}
	if got.Describe() != c.Describe() {
		t.Errorf("third party read a different chain:\n got %s\nwant %s", got.Describe(), c.Describe())
	}
	if got.HasHumanRoot() {
		t.Error("scheduled work must not report a human root to a third party")
	}

	// The published material must contain NO private key material. This is the
	// secrets-discipline assertion: a document that leaked a seed would still
	// verify correctly, so correctness alone cannot catch it.
	seed := testSeed()
	if strings.Contains(string(raw), seed) {
		t.Fatal("published document contains the PRIVATE signing seed")
	}
	if rawSeed, derr := hex.DecodeString(seed); derr == nil {
		if strings.Contains(string(raw), string(rawSeed)) {
			t.Fatal("published document contains raw private seed bytes")
		}
	}
}

// TestThirdPartyVerificationFailsOnTamperedChain is the NEGATIVE CONTROL.
//
// A verification test that only proves valid things verify has proven nothing —
// a function returning nil unconditionally would pass it. Each case here
// tampers with one aspect and requires rejection. The claim-swap cases matter
// most: they are what an attacker would actually attempt, i.e. keep a valid
// signature's shape while changing WHO the chain says authorized the action.
func TestThirdPartyVerificationFailsOnTamperedChain(t *testing.T) {
	now := time.Now()
	// Start from a MACHINE-rooted chain (scheduled work). Starting from a
	// human-rooted one would make the "promote a machine root to a human one"
	// case below a no-op that re-serialized to byte-identical JSON and
	// "verified" for the wrong reason — the test would pass while proving
	// nothing. The tamper cases must each change the signed bytes.
	c, err := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	if err != nil {
		t.Fatalf("building chain: %v", err)
	}
	token := MintToken(testSeed(), c, now)
	if token == "" {
		t.Fatal("MintToken produced nothing")
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected token shape %q", token)
	}
	body, sig := parts[0], parts[1]

	// Re-encode a chain with a swapped root, keeping the original signature.
	forge := func(mutate func(*Chain)) string {
		var m Chain
		rawBody, derr := chainB64.DecodeString(body)
		if derr != nil {
			t.Fatalf("decoding body: %v", derr)
		}
		if uerr := json.Unmarshal(rawBody, &m); uerr != nil {
			t.Fatalf("unmarshalling body: %v", uerr)
		}
		mutate(&m)
		out, merr := json.Marshal(m)
		if merr != nil {
			t.Fatalf("marshalling forged body: %v", merr)
		}
		forged := chainB64.EncodeToString(out)
		// GUARD: a mutation that produced identical bytes is not a tamper case
		// at all — it would "fail to verify as tampered" only because nothing
		// was tampered, and the assertion below would pass vacuously. Catching
		// this here is what stopped an earlier version of this test from
		// silently proving nothing.
		if forged == body {
			t.Fatalf("mutation produced byte-identical claims; this case tampers with nothing")
		}
		return forged + "." + sig
	}

	cases := []struct {
		name  string
		token string
	}{
		{
			name:  "escalate the root to a different human",
			token: forge(func(m *Chain) { m.Subject.ID = "attacker" }),
		},
		{
			name: "promote a machine root to a human one",
			token: forge(func(m *Chain) {
				m.Subject.Type = PrincipalUser
				m.Subject.ID = "clubanderson"
				m.Actors = nil
			}),
		},
		{
			name:  "retarget the chain at another tenant's hive",
			token: forge(func(m *Chain) { m.HiveID = "other-tenant" }),
		},
		{
			name:  "claim a different key generation",
			token: forge(func(m *Chain) { m.Generation = 99 }),
		},
		{
			name:  "extend the validity window",
			token: forge(func(m *Chain) { m.Expiry = time.Now().Add(100 * 24 * time.Hour).Unix() }),
		},
		{
			name:  "corrupt the signature",
			token: body + "." + flipSignatureByte(t, sig),
		},
		{
			name:  "strip the signature entirely",
			token: body + ".",
		},
		{
			name:  "no separator",
			token: body,
		},
		{
			name:  "empty token",
			token: "",
		},
	}

	doc := BuildKeyDocument(true, 1, testPub(), nil, now)
	keys := doc.PublicKeysFrom()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := VerifyTokenAcrossKeys(keys, tc.token, now); err == nil {
				t.Fatal("a TAMPERED chain verified; the signature is not binding what it must bind")
			}
			// The single-key path must reject identically — a verifier that
			// used VerifyToken directly must not be more permissive.
			if _, err := VerifyToken(testPub(), tc.token, now); err == nil {
				t.Fatal("VerifyToken accepted a tampered chain")
			}
		})
	}
}

// flipSignatureByte corrupts a base64url signature by flipping a bit in the
// DECODED bytes and re-encoding.
//
// Mutating the base64 TEXT directly is not reliable: base64's final character
// carries padding bits, so altering the last character can produce a different
// string that decodes to identical bytes — a "corruption" that corrupts
// nothing. An earlier version of this test did exactly that and passed
// vacuously. Flipping a decoded byte and re-encoding guarantees the signature
// really differs.
func flipSignatureByte(t *testing.T, sigB64 string) string {
	t.Helper()
	raw, err := chainB64.DecodeString(sigB64)
	if err != nil || len(raw) == 0 {
		t.Fatalf("decoding signature: %v", err)
	}
	raw[0] ^= 0x01
	out := chainB64.EncodeToString(raw)
	if out == sigB64 {
		t.Fatal("signature corruption produced an identical value")
	}
	return out
}

// TestRotationChainMintedUnderPreviousGenerationStillVerifies is the rotation
// story.
//
// A chain minted under generation N must keep verifying after the hub rotates
// to N+1, for as long as N remains inside its dual-acceptance window. This is
// the property that makes rotation survivable rather than a flag day, and it is
// the same guarantee pkg/hub's generations machinery gives every other signed
// artifact.
//
// The test deliberately mints BEFORE the rotation and verifies AFTER, against
// the post-rotation published document — the exact sequence a tenant hits when
// they verify yesterday's chains today.
func TestRotationChainMintedUnderPreviousGenerationStillVerifies(t *testing.T) {
	now := time.Now()

	const genOneMaster = "master-generation-one"
	const genTwoMaster = "master-generation-two"

	// Sanity: the two generations must derive DIFFERENT keys, or this test
	// would pass for the wrong reason (verifying under a key that never
	// changed).
	if PublicKeyFromSeed(SeedFromMaster(genOneMaster)) == PublicKeyFromSeed(SeedFromMaster(genTwoMaster)) {
		t.Fatal("two generations derived the same key; the test cannot prove anything")
	}

	// --- Before rotation: mint under generation 1. ---
	c, err := HostedSpokeAgentChain("acme", "scanner", "kubestellar-hive[bot]", 1, 2)
	if err != nil {
		t.Fatalf("building chain: %v", err)
	}
	c.Generation = 1
	tokenGen1 := MintToken(SeedFromMaster(genOneMaster), c, now)
	if tokenGen1 == "" {
		t.Fatal("MintToken produced nothing under generation 1")
	}

	// --- Rotate: generation 2 is current, 1 is verify-only. ---
	after := now.Add(time.Hour)
	doc := BuildKeyDocument(true, 2,
		PublicKeyFromSeed(SeedFromMaster(genTwoMaster)),
		[]PublishedKey{{Generation: 1, PublicKey: PublicKeyFromSeed(SeedFromMaster(genOneMaster))}},
		after)

	if len(doc.Keys) != 2 {
		t.Fatalf("post-rotation document should publish 2 keys, got %d", len(doc.Keys))
	}
	// Exactly one key is current, and it is the new one.
	currents := 0
	for _, k := range doc.Keys {
		if k.Current {
			currents++
			if k.Generation != 2 {
				t.Errorf("current key is generation %d, want 2", k.Generation)
			}
		}
	}
	if currents != 1 {
		t.Errorf("published document marks %d keys current, want exactly 1", currents)
	}

	// --- After rotation: the OLD chain still verifies. ---
	got, _, err := VerifyTokenAcrossKeys(doc.PublicKeysFrom(), tokenGen1, after)
	if err != nil {
		t.Fatalf("a chain minted under generation 1 stopped verifying after rotation: %v", err)
	}
	if got.Generation != 1 {
		t.Errorf("verified chain reports generation %d, want 1", got.Generation)
	}
	if got.Describe() != c.Describe() {
		t.Errorf("rotation changed the chain:\n got %s\nwant %s", got.Describe(), c.Describe())
	}

	// A chain minted under the NEW generation verifies too — otherwise the
	// rotation succeeded at preserving the past by breaking the present.
	c2, _ := ScheduledWorkChain("acme", "reviewer", "cadence:reviewer")
	c2.Generation = 2
	tokenGen2 := MintToken(SeedFromMaster(genTwoMaster), c2, after)
	if _, _, err := VerifyTokenAcrossKeys(doc.PublicKeysFrom(), tokenGen2, after); err != nil {
		t.Fatalf("a chain minted under the CURRENT generation failed to verify: %v", err)
	}

	// --- Window closed: generation 1 leaves the document. ---
	// The hub drops an expired generation from acceptableGenerations, so the
	// published set shrinks and the old chain stops verifying. That expiry is
	// the finiteness property that stops dual acceptance rotting into a
	// permanent compat lane.
	expired := BuildKeyDocument(true, 2, PublicKeyFromSeed(SeedFromMaster(genTwoMaster)), nil, after)
	if _, _, err := VerifyTokenAcrossKeys(expired.PublicKeysFrom(), tokenGen1, after); err == nil {
		t.Fatal("a generation-1 chain still verified after generation 1 left the published set")
	}
}

// TestUnverifiedGenerationIsHintOnly pins that the unauthenticated `g` reader
// cannot be used to make a chain acceptable. A caller may use it to pick a key;
// it must never be able to make verification succeed on its own.
func TestUnverifiedGenerationIsHintOnly(t *testing.T) {
	now := time.Now()
	c, _ := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	c.Generation = 7
	token := MintToken(testSeed(), c, now)

	if got := UnverifiedGeneration(token); got != 7 {
		t.Errorf("UnverifiedGeneration = %d, want 7", got)
	}
	// Garbage in, zero out — never a panic and never a wrong key selection
	// that a caller might treat as authoritative.
	for _, bad := range []string{"", "no-dot", "!!!.???", "."} {
		if got := UnverifiedGeneration(bad); got != 0 {
			t.Errorf("UnverifiedGeneration(%q) = %d, want 0", bad, got)
		}
	}
	// And the hint cannot substitute for a signature: a token whose claimed
	// generation matches a published key still fails under the wrong key.
	other := PublicKeyFromSeed(SeedFromMaster("some-other-master"))
	if _, err := VerifyToken(other, token, now); err == nil {
		t.Fatal("a chain verified under an unrelated key")
	}
}

// TestVerifyRejectsMalformedKeysWithoutPanicking pins the crash guard.
// ed25519.Verify PANICS on a wrong-length public key, so a malformed entry in a
// published document must be dropped rather than passed through — otherwise a
// bad key in the document crashes every third-party verifier that fetched it.
func TestVerifyRejectsMalformedKeysWithoutPanicking(t *testing.T) {
	now := time.Now()
	c, _ := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	token := MintToken(testSeed(), c, now)

	malformed := []string{"", "zzzz", "abcd", strings.Repeat("a", 63), strings.Repeat("a", 65)}
	for _, k := range malformed {
		if _, err := VerifyToken(k, token, now); err == nil {
			t.Errorf("malformed key %q was accepted", k)
		}
	}
	// A malformed key alongside a good one must not prevent the good one from
	// working — a garbage entry costs one wasted candidate, nothing more.
	keys := append([]string{"zzzz", ""}, testPub())
	if _, _, err := VerifyTokenAcrossKeys(keys, token, now); err != nil {
		t.Fatalf("a malformed key disabled a valid one: %v", err)
	}
}

// TestChainOutsideValidityWindowIsRejected pins the clock bounds, including
// that skew tolerance does not become an unbounded window.
func TestChainOutsideValidityWindowIsRejected(t *testing.T) {
	now := time.Now()
	c, _ := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	token := MintToken(testSeed(), c, now)

	if _, err := VerifyToken(testPub(), token, now.Add(ChainTokenTTL+2*ChainClockSkew)); err == nil {
		t.Error("an expired chain verified")
	}
	if _, err := VerifyToken(testPub(), token, now.Add(-2*ChainClockSkew-time.Minute)); err == nil {
		t.Error("a not-yet-valid chain verified")
	}
	// Within skew, both edges still work — a third party's clock is not ours.
	if _, err := VerifyToken(testPub(), token, now.Add(-ChainClockSkew/2)); err != nil {
		t.Errorf("a chain within clock skew was rejected: %v", err)
	}
}

// TestNestedRenderingMatchesRFC8693Ordering pins that the external nested form
// puts the most immediate actor outermost and the root innermost, which is what
// an RFC 8693 reader expects. Getting this backwards would invert every chain a
// third party reads while still verifying correctly.
func TestNestedRenderingMatchesRFC8693Ordering(t *testing.T) {
	c, err := HostedSpokeAgentChain("acme", "scanner", "kubestellar-hive[bot]", 1, 2)
	if err != nil {
		t.Fatalf("building chain: %v", err)
	}
	n := c.Nested()
	if n == nil {
		t.Fatal("a three-link chain rendered no nesting")
	}
	// Outermost act is the App (the immediate delegator to the agent).
	if n.Type != PrincipalApp {
		t.Errorf("outermost act type = %q, want %q", n.Type, PrincipalApp)
	}
	// Innermost act is the root.
	if n.Act == nil {
		t.Fatal("nesting stopped before the root")
	}
	if n.Act.Type != PrincipalHiveAuthority {
		t.Errorf("innermost act type = %q, want %q", n.Act.Type, PrincipalHiveAuthority)
	}
	if n.Act.Act != nil {
		t.Error("nesting continued past the root")
	}
	// A one-link chain has no nesting at all.
	u, _ := ContributePlaneUserChain("acme", "clubanderson")
	if u.Nested() != nil {
		t.Error("a one-link chain rendered a nesting")
	}
}
