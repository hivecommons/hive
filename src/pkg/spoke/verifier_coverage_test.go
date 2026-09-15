package spoke

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestModeAccessorReportsConfiguredMode covers the Mode accessor, which had no
// test at all. It is the only way a readiness surface can report whether a
// spoke is actually enforcing, so a wrong answer here would misreport a spoke
// as protected while it silently accepts unsigned responses.
func TestModeAccessorReportsConfiguredMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode Mode
	}{
		{"log-only", ModeLogOnly},
		{"enforce", ModeEnforce},
		{"off", ModeOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewVerifier(tc.mode, nil).Mode(); got != tc.mode {
				t.Errorf("Mode() = %v, want %v", got, tc.mode)
			}
		})
	}
}

// TestModeNameCoversEveryMode pins the operator-facing mode label. It appears
// in the structured logs an operator reads to confirm enforcement, so an
// unknown mode must fall back to the conservative "log-only" label rather than
// an empty string.
func TestModeNameCoversEveryMode(t *testing.T) {
	for _, tc := range []struct {
		mode Mode
		want string
	}{
		{ModeLogOnly, "log-only"},
		{ModeEnforce, "enforce"},
		{ModeOff, "off"},
		{Mode(99), "log-only"},
	} {
		if got := (&Verifier{mode: tc.mode}).modeName(); got != tc.want {
			t.Errorf("modeName(%v) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// TestDefaultStatePathEmptyDirStaysMemoryOnly pins the empty-dir contract.
// Returning a path for an empty dir would make the verifier persist to a
// relative file in the process's working directory instead of staying
// memory-only, which is how a test or a dirless deployment would silently
// acquire surprise on-disk state.
func TestDefaultStatePathEmptyDirStaysMemoryOnly(t *testing.T) {
	for _, dir := range []string{"", "   ", "\t\n"} {
		if got := DefaultStatePath(dir); got != "" {
			t.Errorf("DefaultStatePath(%q) = %q, want \"\" (memory-only)", dir, got)
		}
	}
	if got, want := DefaultStatePath("/data"), filepath.Join("/data", "heartbeat-sig-state.json"); got != want {
		t.Errorf("DefaultStatePath(\"/data\") = %q, want %q", got, want)
	}
}

// TestPersistStateSurvivesUnwritablePath covers the write-failure branch of
// persistStateLocked. Persistence is best-effort hardening: a beat that has
// already verified must never be failed because the state file could not be
// written, so an unwritable path must degrade silently rather than panic or
// reject.
func TestPersistStateSurvivesUnwritablePath(t *testing.T) {
	// A path under a file (not a directory) cannot be created.
	f := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	v := NewPersistentVerifier(ModeEnforce, nil, filepath.Join(f, "state.json"))
	v.mu.Lock()
	v.seenSigned = true
	v.lastSeq = 42
	v.persistStateLocked() // must not panic
	v.mu.Unlock()

	// In-memory state is authoritative and unaffected by the failed write.
	if !v.seenSigned || v.lastSeq != 42 {
		t.Errorf("in-memory state lost after a failed persist: seenSigned=%v lastSeq=%d", v.seenSigned, v.lastSeq)
	}
}

// TestLoadVerifierStateFailsOpen pins the fail-open load contract. A corrupt or
// unreadable state file must yield the same fresh state a new spoke has rather
// than bricking the spoke — at worst it degrades to the pre-#7121 behaviour for
// one process lifetime.
func TestLoadVerifierStateFailsOpen(t *testing.T) {
	dir := t.TempDir()

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, ok := loadVerifierState(corrupt); ok {
		t.Error("loadVerifierState accepted corrupt contents, want fail-open")
	}
	if _, ok := loadVerifierState(filepath.Join(dir, "missing.json")); ok {
		t.Error("loadVerifierState accepted a missing file, want fail-open")
	}
	if _, ok := loadVerifierState(""); ok {
		t.Error("loadVerifierState accepted an empty path, want memory-only")
	}

	// A verifier built on corrupt state starts fresh, not trusted.
	v := NewPersistentVerifier(ModeEnforce, nil, corrupt)
	if v.seenSigned || v.lastSeq != 0 {
		t.Errorf("corrupt state leaked into the verifier: seenSigned=%v lastSeq=%d", v.seenSigned, v.lastSeq)
	}
}

// TestVerifyBodySignatureRejectsMalformedKeysAndSignatures covers the guards in
// front of ed25519.Verify. These are not cosmetic: ed25519.Verify PANICS on a
// public key of the wrong size, and the key material arrives from configuration
// a spoke does not control, so each guard is what keeps a malformed key from
// crashing the spoke instead of simply failing verification.
func TestVerifyBodySignatureRejectsMalformedKeysAndSignatures(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	body := []byte(`{"hive_id":"h1","seq":1}`)
	goodSig := sigB64.EncodeToString(ed25519.Sign(priv, body))
	goodKey := hex.EncodeToString(pub)

	if !verifyBodySignature(goodKey, goodSig, body) {
		t.Fatal("valid key and signature rejected")
	}
	// Whitespace around the encoded forms is tolerated.
	if !verifyBodySignature("  "+goodKey+"\n", " "+goodSig+" ", body) {
		t.Error("padded-but-valid key/signature rejected")
	}

	shortKey := hex.EncodeToString(pub[:ed25519.PublicKeySize-1])
	longKey := hex.EncodeToString(append(append([]byte{}, pub...), 0x00))
	for _, tc := range []struct {
		name string
		key  string
		sig  string
	}{
		{"empty key", "", goodSig},
		{"whitespace key", "   ", goodSig},
		{"non-hex key", "zzzz", goodSig},
		{"undersized key (would panic ed25519.Verify)", shortKey, goodSig},
		{"oversized key (would panic ed25519.Verify)", longKey, goodSig},
		{"non-base64 signature", goodKey, "!!!not-base64!!!"},
		{"empty signature", goodKey, ""},
		{"wrong-size signature", goodKey, sigB64.EncodeToString([]byte("short"))},
		{"signature over different body", goodKey, sigB64.EncodeToString(ed25519.Sign(priv, []byte("other")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if verifyBodySignature(tc.key, tc.sig, body) {
				t.Errorf("verifyBodySignature accepted %s", tc.name)
			}
		})
	}
}

// TestVerifyBodyAcrossKeysTriesEveryPublishedKey pins rotation behaviour: a
// malformed or stale key must cost one wasted candidate rather than prevent a
// good key later in the list from verifying.
func TestVerifyBodyAcrossKeysTriesEveryPublishedKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	body := []byte(`{"hive_id":"h1"}`)
	sig := sigB64.EncodeToString(ed25519.Sign(priv, body))
	good := hex.EncodeToString(pub)

	if !verifyBodyAcrossKeys([]string{"zzzz", good}, sig, body) {
		t.Error("a malformed leading key prevented a valid trailing key from verifying")
	}
	if verifyBodyAcrossKeys([]string{"zzzz", strings.Repeat("ab", ed25519.PublicKeySize)}, sig, body) {
		t.Error("verification succeeded with no matching key")
	}
	if verifyBodyAcrossKeys(nil, sig, body) {
		t.Error("verification succeeded with no keys at all")
	}
}

// TestNowOrDefaultSubstitutesRealClock covers the clock-injection helper: a
// zero time means "use the real clock", and any supplied time must pass through
// unchanged so tests can pin staleness boundaries deterministically.
func TestNowOrDefaultSubstitutesRealClock(t *testing.T) {
	fixed := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if got := nowOrDefault(fixed); !got.Equal(fixed) {
		t.Errorf("nowOrDefault(fixed) = %v, want %v", got, fixed)
	}
	before := time.Now()
	got := nowOrDefault(time.Time{})
	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("nowOrDefault(zero) = %v, want a time within the call window", got)
	}
}
