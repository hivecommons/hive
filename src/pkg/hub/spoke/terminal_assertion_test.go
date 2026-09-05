package spoke

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTerminalAssertionRoundTrip(t *testing.T) {
	secret := "shared-hub-secret-abc123"
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion(secret, "alice", "owner", "hosted-hive-1", now)
	if tok == "" {
		t.Fatal("expected a token")
	}
	user, role, err := VerifyTerminalAssertion(secret, tok, "hosted-hive-1", now)
	if err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	if user != "alice" || role != "owner" {
		t.Fatalf("got user=%q role=%q, want alice/owner", user, role)
	}
}

func TestTerminalAssertionRejectsWrongHive(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion(secret, "alice", "owner", "hive-A", now)
	if _, _, err := VerifyTerminalAssertion(secret, tok, "hive-B", now); err == nil {
		t.Fatal("expected rejection for an assertion minted for a different hive")
	}
}

func TestTerminalAssertionRejectsWrongSecret(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion("secret-1", "alice", "owner", "hive-1", now)
	if _, _, err := VerifyTerminalAssertion("secret-2", tok, "hive-1", now); err == nil {
		t.Fatal("expected rejection under a different secret")
	}
}

func TestTerminalAssertionRejectsExpired(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion(secret, "alice", "owner", "hive-1", now)
	later := now.Add(terminalAssertionTTL + ssoClockSkew + time.Second)
	if _, _, err := VerifyTerminalAssertion(secret, tok, "hive-1", later); err == nil {
		t.Fatal("expected rejection for an expired assertion")
	}
}

func TestTerminalAssertionRejectsNotYetValid(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	// Minted "in the future" relative to the verifier's earlier clock.
	tok := MintTerminalAssertion(secret, "alice", "owner", "hive-1", now)
	earlier := now.Add(-(ssoClockSkew + time.Minute))
	if _, _, err := VerifyTerminalAssertion(secret, tok, "hive-1", earlier); err == nil {
		t.Fatal("expected rejection for a not-yet-valid assertion")
	}
}

func TestTerminalAssertionRejectsTamper(t *testing.T) {
	secret := "s"
	now := time.Unix(1_700_000_000, 0)
	tok := MintTerminalAssertion(secret, "alice", "read-write", "hive-1", now)
	b := []byte(tok)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	if _, _, err := VerifyTerminalAssertion(secret, string(b), "hive-1", now); err == nil {
		t.Fatal("expected rejection for a tampered assertion")
	}
}

func TestTerminalAssertionEmptyInputs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if MintTerminalAssertion("", "alice", "owner", "hive-1", now) != "" {
		t.Fatal("empty secret must not mint an assertion")
	}
	if MintTerminalAssertion("s", "", "owner", "hive-1", now) != "" {
		t.Fatal("empty username must not mint an assertion")
	}
	if MintTerminalAssertion("s", "alice", "owner", "", now) != "" {
		t.Fatal("empty hiveID must not mint an assertion")
	}
	if _, _, err := VerifyTerminalAssertion("", "x.y", "hive-1", now); err == nil {
		t.Fatal("empty secret must not verify")
	}
	if _, _, err := VerifyTerminalAssertion("s", "malformed", "hive-1", now); err == nil {
		t.Fatal("malformed assertion must not verify")
	}
}

// A terminal assertion must NOT verify as an SSO handoff token: the two are
// signed by different keys and carry different version strings, so cross-family
// replay fails. (The reverse — an SSO token verifying as a terminal assertion —
// cannot even be expressed once SSO is Ed25519, since MintSSOToken then takes a
// seed, not the terminal's HMAC key; the version-string mismatch is the durable
// guarantee, exercised by TestTerminalAssertionRejectsForeignVersion.)
func TestTerminalAssertionNotConfusableWithSSO(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// A terminal assertion, verified under the SSO path, must be rejected.
	term := MintTerminalAssertion("terminal-key", "alice", "owner", "hive-1", now)
	// The SSO verifier on this base takes a secret; even if the same string is
	// tried, the version ("hive-terminal-v1") is not an SSO version, so it fails.
	if _, _, err := VerifySSOToken("terminal-key", term, "hive-1", now); err == nil {
		t.Fatal("a terminal assertion must NOT verify as an SSO handoff token")
	}
}

// A token carrying any non-terminal version string is rejected even when signed
// with the correct terminal key — the version namespace is a hard gate.
func TestTerminalAssertionRejectsForeignVersion(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	key := "terminal-key"
	// Hand-mint a well-signed token whose version is an SSO version.
	forged := forgeTerminalToken(key, ssoClaims{
		Version: "hive-sso-v2", Username: "alice", Role: "owner", HiveID: "hive-1",
		IssuedAt: now.Unix(), Expiry: now.Add(terminalAssertionTTL).Unix(),
	})
	if _, _, err := VerifyTerminalAssertion(key, forged, "hive-1", now); err == nil {
		t.Fatal("a correctly-signed token with a foreign version must be rejected")
	}
}

// The terminal signing key is a DEDICATED, PER-HIVE symmetric sub-key: distinct
// from the master, from the SSO material, and — the N3 property — from every
// other hive's terminal key.
func TestDeriveTerminalKeyIsDomainSeparatedAndPerHive(t *testing.T) {
	master := "master-secret"
	got := derivePerHiveKey(master, infoTerminalKey, "hive-1")
	if got == "" {
		t.Fatal("expected a derived key")
	}
	if got == master {
		t.Fatal("derived key must not equal the master")
	}
	// Deterministic and equal to the documented formula:
	// HMAC-SHA256(master, info || 0x00 || hiveID) hex.
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(infoTerminalKey))
	mac.Write([]byte{0})
	mac.Write([]byte("hive-1"))
	if want := hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("derived key = %q, want %q", got, want)
	}
	// N3: a DIFFERENT hive under the SAME master must get a different key.
	if other := derivePerHiveKey(master, infoTerminalKey, "hive-2"); other == got {
		t.Fatal("terminal key must differ per hive — this is audit N3")
	}
	// Fail closed on either missing input.
	if derivePerHiveKey("", infoTerminalKey, "hive-1") != "" {
		t.Fatal("empty master must derive to empty (fail closed)")
	}
	if derivePerHiveKey(master, infoTerminalKey, "") != "" {
		t.Fatal("empty hive ID must derive to empty (fail closed) — never a shared key")
	}
}

// TerminalSigningKey resolution order: HIVE_TERMINAL_KEY > self-derived per-hive
// key from HIVE_HUB_SECRET + HIVE_ID. Every lane is per-hive; there is no lane
// that yields a fleet-uniform value.
func TestTerminalSigningKeyResolutionOrder(t *testing.T) {
	t.Setenv(EnvTerminalKey, "")
	t.Setenv(envHubSecret, "")
	t.Setenv(EnvHiveID, "")
	if TerminalSigningKey() != "" {
		t.Fatal("no key sources → empty (fail closed)")
	}

	// Master present but NO identity: must stay empty rather than fall back to
	// any shared value.
	t.Setenv(envHubSecret, "master")
	if got := TerminalSigningKey(); got != "" {
		t.Fatalf("master without HIVE_ID must not resolve a key, got %q", got)
	}

	t.Setenv(EnvHiveID, "hive-1")
	if got, want := TerminalSigningKey(), derivePerHiveKey("master", infoTerminalKey, "hive-1"); got != want {
		t.Fatalf("self-derive lane: got %q want per-hive %q", got, want)
	}

	t.Setenv(EnvTerminalKey, "dedicated-terminal-key")
	if TerminalSigningKey() != "dedicated-terminal-key" {
		t.Fatal("dedicated terminal key must win over all")
	}
}

// !! AUDIT N3 REGRESSION GATE !!
//
// The terminal key must NEVER resolve to HIVE_SESSION_KEY. That var is
// byte-identical across all 65 spokes (verified live), so any lane that returns
// it re-creates a fleet-wide forgery lane: an assertion minted on one tenant's
// spoke would verify on every other tenant's spoke.
//
// This asserts the invariant TWICE — behaviourally (the resolver ignores the var
// entirely) and structurally (the source does not mention it) — because a
// behavioural test alone passes if some future refactor reads the var under a
// condition this test does not happen to hit.
func TestN3_TerminalKeyNeverFallsThroughToSessionKey(t *testing.T) {
	const fleetUniform = "fleet-uniform-session-key"

	// Behavioural: HIVE_SESSION_KEY set and nothing else — must NOT be adopted.
	t.Setenv(EnvTerminalKey, "")
	t.Setenv(envHubSecret, "")
	t.Setenv(EnvHiveID, "")
	t.Setenv("HIVE_SESSION_KEY", fleetUniform)
	if got := TerminalSigningKey(); got != "" {
		t.Fatalf("N3 REGRESSION: HIVE_SESSION_KEY was adopted as the terminal key (got %q)", got)
	}

	// Behavioural: even alongside a valid self-derive, the session key must not win.
	t.Setenv(envHubSecret, "master")
	t.Setenv(EnvHiveID, "hive-1")
	got := TerminalSigningKey()
	if got == fleetUniform {
		t.Fatal("N3 REGRESSION: HIVE_SESSION_KEY won over the per-hive derivation")
	}
	if want := derivePerHiveKey("master", infoTerminalKey, "hive-1"); got != want {
		t.Fatalf("expected per-hive key %q, got %q", want, got)
	}

	// Structural: the resolver's source must not reference the session key at
	// all. Positive control below proves this assertion can fail.
	src, err := os.ReadFile("terminal_assertion.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	fn := terminalSigningKeyFuncBody(t, string(src))
	if strings.Contains(fn, "HIVE_SESSION_KEY") || strings.Contains(fn, "envSessionKey") {
		t.Fatal("N3 REGRESSION: TerminalSigningKey's body references the fleet-uniform session key")
	}
	// Positive control: the extracted body is really the function we think it
	// is, so the check above is not vacuously passing on an empty string.
	if !strings.Contains(fn, "EnvTerminalKey") || !strings.Contains(fn, "derivePerHiveKey") {
		t.Fatalf("positive control failed: extracted body is not TerminalSigningKey:\n%s", fn)
	}
}

// terminalSigningKeyFuncBody extracts the source text of TerminalSigningKey so
// the structural assertion above inspects the RESOLVER rather than the whole
// file (whose comments legitimately discuss the deleted lane by name).
func terminalSigningKeyFuncBody(t *testing.T, src string) string {
	t.Helper()
	const sig = "func TerminalSigningKey() string {"
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatal("TerminalSigningKey not found in source")
	}
	rest := src[i:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatal("could not find end of TerminalSigningKey")
	}
	return rest[:end]
}

// N3 identity isolation (the F2-style property): a terminal assertion minted by
// hive A must NOT verify on hive B, under any key-resolution lane. This is the
// property that the fleet-uniform lanes destroyed.
func TestN3_TerminalAssertionDoesNotCrossHives(t *testing.T) {
	const master = "shared-fleet-master"
	now := time.Now()

	keyA := derivePerHiveKey(master, infoTerminalKey, "hive-a")
	keyB := derivePerHiveKey(master, infoTerminalKey, "hive-b")
	if keyA == "" || keyB == "" {
		t.Fatal("expected derived keys")
	}
	if keyA == keyB {
		t.Fatal("N3: two hives under one master must not share a terminal key")
	}

	tok := MintTerminalAssertion(keyA, "alice", "admin", "hive-a", now)
	if tok == "" {
		t.Fatal("expected a minted assertion")
	}
	// Positive control: it DOES verify on its own hive with its own key.
	if _, _, err := VerifyTerminalAssertion(keyA, tok, "hive-a", now); err != nil {
		t.Fatalf("positive control failed: assertion must verify on its own hive: %v", err)
	}
	// The property: hive B's key rejects it.
	if _, _, err := VerifyTerminalAssertion(keyB, tok, "hive-b", now); err == nil {
		t.Fatal("N3 REGRESSION: hive A's assertion verified under hive B's key")
	}
}

// forgeTerminalToken signs an arbitrary claims payload with the terminal key —
// a test-only helper to construct otherwise-valid tokens with a bad version.
func forgeTerminalToken(key string, claims ssoClaims) string {
	payload, _ := json.Marshal(claims)
	body := ssoB64.EncodeToString(payload)
	return body + "." + terminalSign(key, body)
}
