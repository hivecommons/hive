package hub

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// deriveDomainKey
// ---------------------------------------------------------------------------

func TestDeriveDomainKey_EmptyMaster(t *testing.T) {
	if got := deriveDomainKey("", "hive-heartbeat-v1"); got != "" {
		t.Fatalf("deriveDomainKey with empty master should return \"\", got %q", got)
	}
}

func TestDeriveDomainKey_Deterministic(t *testing.T) {
	a := deriveDomainKey("master-secret", infoHeartbeatKey)
	b := deriveDomainKey("master-secret", infoHeartbeatKey)
	if a == "" {
		t.Fatal("deriveDomainKey returned empty for non-empty master")
	}
	if a != b {
		t.Fatalf("deriveDomainKey is not deterministic: %q != %q", a, b)
	}
}

func TestDeriveDomainKey_DifferentInfoProducesDifferentKeys(t *testing.T) {
	master := "shared-master"
	keys := map[string]string{
		infoHeartbeatKey:       deriveDomainKey(master, infoHeartbeatKey),
		infoSessionKey:         deriveDomainKey(master, infoSessionKey),
		infoSSOKey:             deriveDomainKey(master, infoSSOKey),
		infoSSOEd25519Seed:     deriveDomainKey(master, infoSSOEd25519Seed),
		infoSessionEd25519Seed: deriveDomainKey(master, infoSessionEd25519Seed),
	}
	seen := make(map[string]string)
	for info, key := range keys {
		if key == "" {
			t.Fatalf("deriveDomainKey(%q, %q) is empty", master, info)
		}
		if prev, dup := seen[key]; dup {
			t.Fatalf("info labels %q and %q produced identical key %q", prev, info, key)
		}
		seen[key] = info
	}
}

func TestDeriveDomainKey_DifferentMasterProducesDifferentKeys(t *testing.T) {
	a := deriveDomainKey("master-1", infoHeartbeatKey)
	b := deriveDomainKey("master-2", infoHeartbeatKey)
	if a == b {
		t.Fatal("different masters should produce different keys")
	}
}

func TestDeriveDomainKey_IsLowercaseHex(t *testing.T) {
	key := deriveDomainKey("some-secret", infoHeartbeatKey)
	if _, err := hex.DecodeString(key); err != nil {
		t.Fatalf("key is not valid hex: %v", err)
	}
	if key != strings.ToLower(key) {
		t.Fatalf("key is not lowercase hex: %q", key)
	}
}

func TestDeriveDomainKey_MatchesManualHMAC(t *testing.T) {
	master := "reference-master"
	info := infoHeartbeatKey
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	want := hex.EncodeToString(mac.Sum(nil))
	got := deriveDomainKey(master, info)
	if got != want {
		t.Fatalf("deriveDomainKey mismatch:\n  got  %q\n  want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// derivePerHiveKey
// ---------------------------------------------------------------------------

func TestDerivePerHiveKey_EmptyMaster(t *testing.T) {
	if got := derivePerHiveKey("", infoHeartbeatKey, "hive-1"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestDerivePerHiveKey_EmptyHiveID(t *testing.T) {
	if got := derivePerHiveKey("master", infoHeartbeatKey, ""); got != "" {
		t.Fatalf("expected empty for empty hiveID, got %q", got)
	}
}

func TestDerivePerHiveKey_Deterministic(t *testing.T) {
	a := derivePerHiveKey("master", infoHeartbeatKey, "hive-1")
	b := derivePerHiveKey("master", infoHeartbeatKey, "hive-1")
	if a == "" {
		t.Fatal("returned empty for valid inputs")
	}
	if a != b {
		t.Fatalf("not deterministic: %q != %q", a, b)
	}
}

func TestDerivePerHiveKey_DifferentHiveIDsProduceDifferentKeys(t *testing.T) {
	a := derivePerHiveKey("master", infoHeartbeatKey, "hive-a")
	b := derivePerHiveKey("master", infoHeartbeatKey, "hive-b")
	if a == b {
		t.Fatal("different hive IDs should produce different keys")
	}
}

func TestDerivePerHiveKey_DiffersFromFleetWide(t *testing.T) {
	master := "master"
	fleet := deriveDomainKey(master, infoHeartbeatKey)
	perHive := derivePerHiveKey(master, infoHeartbeatKey, "hive-x")
	if fleet == perHive {
		t.Fatal("per-hive key must differ from fleet-wide key")
	}
}

func TestDerivePerHiveKey_NULSeparatorPreventsAmbiguity(t *testing.T) {
	// The NUL separator prevents ambiguity between info and hiveID boundaries.
	// Without the 0x00, ("info-a", "b") and ("info-a" + "b", "") would collide.
	// With the 0x00: HMAC("info-a" + 0x00 + "b") != HMAC("info-ab" + 0x00 + "x")
	a := derivePerHiveKey("master", "info-a", "b")
	b := derivePerHiveKey("master", "info-ab", "x")
	if a == b {
		t.Fatal("different info/hiveID splits should produce different keys")
	}
	// Also verify that concatenation-only (no separator) WOULD be ambiguous:
	// "info-a" + "b" == "info-ab" if there were no separator. With the NUL
	// byte: "info-a\x00b" != "info-ab\x00x".
	c := derivePerHiveKey("master", "info", "ab")
	d := derivePerHiveKey("master", "infoa", "b")
	if c == d {
		t.Fatal("NUL separator should prevent info+hiveID concatenation ambiguity")
	}
}

func TestDerivePerHiveKey_MatchesManualHMAC(t *testing.T) {
	master := "ref-master"
	info := infoHeartbeatKey
	hiveID := "hive-42"
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write([]byte(info))
	mac.Write([]byte{0})
	mac.Write([]byte(hiveID))
	want := hex.EncodeToString(mac.Sum(nil))
	got := derivePerHiveKey(master, info, hiveID)
	if got != want {
		t.Fatalf("mismatch:\n  got  %q\n  want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// HubServer key accessors
// ---------------------------------------------------------------------------

func TestHubServer_HeartbeatKeyFor(t *testing.T) {
	s := &HubServer{hubSecret: "test-secret"}
	key := s.heartbeatKeyFor("hive-1")
	if key == "" {
		t.Fatal("heartbeatKeyFor returned empty for valid inputs")
	}
	// Must match derivePerHiveKey directly
	want := derivePerHiveKey("test-secret", infoHeartbeatKey, "hive-1")
	if key != want {
		t.Fatalf("heartbeatKeyFor mismatch: got %q, want %q", key, want)
	}
}

func TestHubServer_HeartbeatKeyFor_EmptySecret(t *testing.T) {
	s := &HubServer{hubSecret: ""}
	if got := s.heartbeatKeyFor("hive-1"); got != "" {
		t.Fatalf("expected empty with no secret, got %q", got)
	}
}

func TestHubServer_HeartbeatBearerIsPerHive(t *testing.T) {
	s := &HubServer{hubSecret: "test-secret"}
	perHive := s.heartbeatKeyFor("hive-1")
	if !s.heartbeatBearerIsPerHive(perHive, "hive-1") {
		t.Fatal("should recognize its own per-hive key")
	}
	if s.heartbeatBearerIsPerHive(perHive, "hive-2") {
		t.Fatal("per-hive key for hive-1 should not match hive-2")
	}
	if s.heartbeatBearerIsPerHive("wrong-key", "hive-1") {
		t.Fatal("should reject wrong key")
	}
}

func TestHubServer_HeartbeatBearerIsPerHive_EmptySecret(t *testing.T) {
	s := &HubServer{hubSecret: ""}
	if s.heartbeatBearerIsPerHive("anything", "hive-1") {
		t.Fatal("should return false with empty secret")
	}
}

func TestHubServer_HeartbeatKey(t *testing.T) {
	s := &HubServer{hubSecret: "test-secret"}
	key := s.heartbeatKey()
	want := deriveDomainKey("test-secret", infoHeartbeatKey)
	if key != want {
		t.Fatalf("heartbeatKey mismatch: got %q, want %q", key, want)
	}
}

func TestHubServer_SessionCookieKey(t *testing.T) {
	s := &HubServer{hubSecret: "test-secret"}
	key := s.sessionCookieKey()
	want := deriveDomainKey("test-secret", infoSessionKey)
	if key != want {
		t.Fatalf("sessionCookieKey mismatch: got %q, want %q", key, want)
	}
}

func TestHubServer_SessionSigningSeed(t *testing.T) {
	s := &HubServer{hubSecret: "test-secret"}
	seed := s.sessionSigningSeed()
	if seed == "" {
		t.Fatal("sessionSigningSeed returned empty")
	}
	want := deriveDomainKey("test-secret", infoSessionEd25519Seed)
	if seed != want {
		t.Fatalf("sessionSigningSeed mismatch: got %q, want %q", seed, want)
	}
}

func TestHubServer_SessionPublicKey(t *testing.T) {
	s := &HubServer{hubSecret: "test-secret"}
	pub := s.sessionPublicKey()
	if pub == "" {
		t.Fatal("sessionPublicKey returned empty")
	}
	// Verify it is a valid 32-byte Ed25519 public key
	pubBytes, err := hex.DecodeString(pub)
	if err != nil {
		t.Fatalf("sessionPublicKey is not valid hex: %v", err)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		t.Fatalf("public key is %d bytes, want %d", len(pubBytes), ed25519.PublicKeySize)
	}
}

func TestHubServer_SessionPublicKey_EmptySecret(t *testing.T) {
	s := &HubServer{hubSecret: ""}
	if got := s.sessionPublicKey(); got != "" {
		t.Fatalf("expected empty with no secret, got %q", got)
	}
}

func TestHubServer_SSOSigningSeed(t *testing.T) {
	s := &HubServer{hubSecret: "test-secret"}
	seed := s.ssoSigningSeed()
	want := deriveDomainKey("test-secret", infoSSOEd25519Seed)
	if seed != want {
		t.Fatalf("ssoSigningSeed mismatch: got %q, want %q", seed, want)
	}
}

// ---------------------------------------------------------------------------
// ssoPublicKeyFromSeed
// ---------------------------------------------------------------------------

func TestSSOPublicKeyFromSeed_ValidSeed(t *testing.T) {
	seed := deriveDomainKey("master", infoSSOEd25519Seed)
	pub := ssoPublicKeyFromSeed(seed)
	if pub == "" {
		t.Fatal("returned empty for valid seed")
	}
	pubBytes, err := hex.DecodeString(pub)
	if err != nil {
		t.Fatalf("not valid hex: %v", err)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		t.Fatalf("public key is %d bytes, want %d", len(pubBytes), ed25519.PublicKeySize)
	}
}

func TestSSOPublicKeyFromSeed_EmptyInput(t *testing.T) {
	if got := ssoPublicKeyFromSeed(""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestSSOPublicKeyFromSeed_InvalidHex(t *testing.T) {
	if got := ssoPublicKeyFromSeed("not-hex-at-all"); got != "" {
		t.Fatalf("expected empty for invalid hex, got %q", got)
	}
}

func TestSSOPublicKeyFromSeed_WrongLength(t *testing.T) {
	if got := ssoPublicKeyFromSeed("abcd"); got != "" {
		t.Fatalf("expected empty for short seed, got %q", got)
	}
}

func TestSSOPublicKeyFromSeed_Deterministic(t *testing.T) {
	seed := deriveDomainKey("master", infoSSOEd25519Seed)
	a := ssoPublicKeyFromSeed(seed)
	b := ssoPublicKeyFromSeed(seed)
	if a != b {
		t.Fatalf("not deterministic: %q != %q", a, b)
	}
}

func TestSSOPublicKeyFromSeed_MatchesStdLib(t *testing.T) {
	seed := deriveDomainKey("master", infoSSOEd25519Seed)
	seedBytes, _ := hex.DecodeString(seed)
	priv := ed25519.NewKeyFromSeed(seedBytes)
	wantPub := hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	got := ssoPublicKeyFromSeed(seed)
	if got != wantPub {
		t.Fatalf("public key mismatch with stdlib:\n  got  %q\n  want %q", got, wantPub)
	}
}

func TestSSOPublicKeyFromSeed_WhitespaceHandling(t *testing.T) {
	seed := deriveDomainKey("master", infoSSOEd25519Seed)
	padded := "  " + seed + "\n"
	pub := ssoPublicKeyFromSeed(padded)
	if pub == "" {
		t.Fatal("should handle whitespace-padded seed")
	}
	if pub != ssoPublicKeyFromSeed(seed) {
		t.Fatal("whitespace-padded result should match clean seed")
	}
}

// ---------------------------------------------------------------------------
// SSOSigningSeedFromMaster
// ---------------------------------------------------------------------------

func TestSSOSigningSeedFromMaster_EmptyMaster(t *testing.T) {
	if got := SSOSigningSeedFromMaster(""); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestSSOSigningSeedFromMaster_MatchesSSOSigningSeed(t *testing.T) {
	master := "test-master"
	s := &HubServer{hubSecret: master}
	if got := SSOSigningSeedFromMaster(master); got != s.ssoSigningSeed() {
		t.Fatalf("SSOSigningSeedFromMaster and ssoSigningSeed disagree: %q != %q", got, s.ssoSigningSeed())
	}
}

// ---------------------------------------------------------------------------
// hiveWantsEd25519SSO
// ---------------------------------------------------------------------------

func TestHiveWantsEd25519SSO_AllowlistedHive(t *testing.T) {
	if !hiveWantsEd25519SSO("hosted-kubestellar-console-4vkt", nil) {
		t.Fatal("allowlisted hive should return true even with nil entry")
	}
}

func TestHiveWantsEd25519SSO_UnknownHiveNilEntry(t *testing.T) {
	if hiveWantsEd25519SSO("unknown-hive", nil) {
		t.Fatal("unknown hive with nil entry should return false")
	}
}

func TestHiveWantsEd25519SSO_UnknownHiveEmptyEntry(t *testing.T) {
	if hiveWantsEd25519SSO("unknown-hive", &RegistryEntry{}) {
		t.Fatal("unknown hive with empty entry should return false")
	}
}

func TestHiveWantsEd25519SSO_MatchingGitHash(t *testing.T) {
	entry := &RegistryEntry{GitHash: "12c34e5"}
	if !hiveWantsEd25519SSO("some-hive", entry) {
		t.Fatal("should match on target commit hash")
	}
}

func TestHiveWantsEd25519SSO_GitHashPrefix(t *testing.T) {
	entry := &RegistryEntry{GitHash: "12c34e5abc"}
	if !hiveWantsEd25519SSO("some-hive", entry) {
		t.Fatal("should match when git hash has target as prefix")
	}
}

func TestHiveWantsEd25519SSO_GitHashShortPrefix(t *testing.T) {
	entry := &RegistryEntry{GitHash: "12c34"}
	if !hiveWantsEd25519SSO("some-hive", entry) {
		t.Fatal("should match when stored hash is prefix of target")
	}
}

func TestHiveWantsEd25519SSO_ImageRefContainsCommit(t *testing.T) {
	entry := &RegistryEntry{ImageRef: "ghcr.io/kubestellar/hive:12c34e5"}
	if !hiveWantsEd25519SSO("some-hive", entry) {
		t.Fatal("should match when image ref contains target commit")
	}
}

func TestHiveWantsEd25519SSO_CaseInsensitive(t *testing.T) {
	entry := &RegistryEntry{GitHash: "12C34E5"}
	if !hiveWantsEd25519SSO("some-hive", entry) {
		t.Fatal("git hash comparison should be case-insensitive")
	}
}

func TestHiveWantsEd25519SSO_NonMatchingHash(t *testing.T) {
	entry := &RegistryEntry{GitHash: "deadbeef"}
	if hiveWantsEd25519SSO("some-hive", entry) {
		t.Fatal("should not match a different commit hash")
	}
}

func TestHiveWantsEd25519SSO_NonMatchingImageRef(t *testing.T) {
	entry := &RegistryEntry{ImageRef: "ghcr.io/kubestellar/hive:v2.0.0"}
	if hiveWantsEd25519SSO("some-hive", entry) {
		t.Fatal("should not match a non-matching image ref")
	}
}

// ---------------------------------------------------------------------------
// Cross-domain isolation: keys from different domains must never collide
// ---------------------------------------------------------------------------

func TestDomainIsolation_AllInfoLabelsProduceUniqueKeys(t *testing.T) {
	master := "isolation-test-master"
	labels := []string{
		infoHeartbeatKey,
		infoSessionKey,
		infoSSOKey,
		infoSSOEd25519Seed,
		infoSessionEd25519Seed,
	}
	seen := make(map[string]string)
	for _, label := range labels {
		key := deriveDomainKey(master, label)
		if prev, dup := seen[key]; dup {
			t.Fatalf("labels %q and %q derived to same key", prev, label)
		}
		seen[key] = label
	}
}

func TestDomainIsolation_PerHiveKeysAcrossDomainsNeverCollide(t *testing.T) {
	master := "isolation-test-master"
	hiveID := "hive-1"
	labels := []string{infoHeartbeatKey, infoSessionKey, infoSSOKey}
	seen := make(map[string]string)
	for _, label := range labels {
		key := derivePerHiveKey(master, label, hiveID)
		if prev, dup := seen[key]; dup {
			t.Fatalf("per-hive labels %q and %q derived to same key", prev, label)
		}
		seen[key] = label
	}
}

// ---------------------------------------------------------------------------
// verifyHubUserDual (integration-level: exercises the full chain)
// ---------------------------------------------------------------------------

func TestVerifyHubUserDual_EmptySecret(t *testing.T) {
	s := &HubServer{hubSecret: ""}
	if _, ok := s.verifyHubUserDual("anything"); ok {
		t.Fatal("should not verify with empty secret")
	}
}

func TestVerifyHubUserDual_LegacyCookie(t *testing.T) {
	secret := "dual-test-secret"
	s := &HubServer{hubSecret: secret}
	// Mint a cookie with the raw master (legacy path)
	legacy := mintHubUserCookieValue(secret, "admin")
	user, ok := s.verifyHubUserDual(legacy)
	if !ok || user != "admin" {
		t.Fatalf("verifyHubUserDual legacy = (%q, %v), want (admin, true)", user, ok)
	}
}

func TestVerifyHubUserDual_DerivedSessionKeyCookie(t *testing.T) {
	secret := "dual-test-secret"
	s := &HubServer{hubSecret: secret}
	// Mint a cookie with the derived session key
	derived := mintHubUserCookieValue(s.sessionCookieKey(), "admin")
	user, ok := s.verifyHubUserDual(derived)
	if !ok || user != "admin" {
		t.Fatalf("verifyHubUserDual derived = (%q, %v), want (admin, true)", user, ok)
	}
}

func TestVerifyHubUserDual_InvalidCookie(t *testing.T) {
	s := &HubServer{hubSecret: "secret"}
	if _, ok := s.verifyHubUserDual("forged.cookie"); ok {
		t.Fatal("should reject forged cookie")
	}
}
