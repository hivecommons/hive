package spoke

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"
)

// testResponse mirrors the shape of hub.HeartbeatResponse for the fields this
// package's verifier cares about, so the tests can construct a signed body
// without importing pkg/hub (which imports pkg/spoke). The sig_* json tags MUST
// match; hub.TestHeartbeatSigTagsMatch pins hub.HeartbeatResponse against
// spoke.sigEnvelope, and this local struct uses the same tags.
type testResponse struct {
	OK              bool   `json:"ok"`
	GitHubAppConfig string `json:"github_app_config,omitempty"` // stand-in for delivered credentials
	SigHiveID       string `json:"sig_hive_id,omitempty"`
	SigSeq          int64  `json:"sig_seq,omitempty"`
	SigSignedAt     int64  `json:"sig_ts,omitempty"`
	SigVersion      int    `json:"sig_v,omitempty"`
}

// signer is a test hub: a fresh Ed25519 keypair with the seed rendered as hex
// exactly like hub.ssoSigningSeed() would be.
type signer struct {
	seedHex string
	pubHex  string
}

func newSigner(t *testing.T) signer {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return signer{seedHex: hex.EncodeToString(seed), pubHex: hex.EncodeToString(pub)}
}

// signed builds a response body bound to hiveID/seq and returns the body bytes
// and the detached signature header value, exactly as the hub would.
func (s signer) signed(t *testing.T, hiveID string, seq int64, creds string) (body []byte, sig string) {
	t.Helper()
	resp := testResponse{
		OK:              true,
		GitHubAppConfig: creds,
		SigHiveID:       hiveID,
		SigSeq:          seq,
		SigSignedAt:     time.Now().Unix(),
		SigVersion:      SigVersion,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b, SignBody(s.seedHex, b)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- Required cases ---------------------------------------------------------

// Valid signature, correct hive_id, increasing seq -> accepted, in both modes.
func TestValidSignedResponseAccepted(t *testing.T) {
	s := newSigner(t)
	for _, mode := range []Mode{ModeLogOnly, ModeEnforce} {
		v := NewVerifier(mode, quietLogger())
		body, sig := s.signed(t, "hive-a", 1, "creds")
		got := v.Verify([]string{s.pubHex}, "hive-a", body, sig, time.Now())
		if !got.Accepted || !got.Signed || got.Reason != ReasonOK {
			t.Fatalf("mode %v: want accepted+signed ok, got %+v", mode, got)
		}
		// A second, higher seq keeps being accepted.
		body2, sig2 := s.signed(t, "hive-a", 2, "creds")
		if got := v.Verify([]string{s.pubHex}, "hive-a", body2, sig2, time.Now()); !got.Accepted || !got.Signed {
			t.Fatalf("mode %v: increasing seq should be accepted, got %+v", mode, got)
		}
	}
}

// Signature invalid / body tampered after signing -> rejected when enforcing,
// logged-and-accepted when log-only.
func TestTamperedBodyRejectedWhenEnforcing(t *testing.T) {
	s := newSigner(t)

	// Establish trust first with a genuine signed response (seq 1), then tamper.
	tamper := func(body []byte) []byte {
		var r testResponse
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		r.GitHubAppConfig = "attacker-injected-credentials" // change a signed byte
		out, _ := json.Marshal(r)
		return out
	}

	// Enforce: tampered body rejected.
	{
		v := NewVerifier(ModeEnforce, quietLogger())
		b1, sig1 := s.signed(t, "hive-a", 1, "creds")
		if got := v.Verify([]string{s.pubHex}, "hive-a", b1, sig1, time.Now()); !got.Accepted {
			t.Fatalf("setup signed response should be accepted, got %+v", got)
		}
		b2, sig2 := s.signed(t, "hive-a", 2, "creds")
		bad := tamper(b2)
		got := v.Verify([]string{s.pubHex}, "hive-a", bad, sig2, time.Now())
		if got.Accepted || got.Reason != ReasonBadSignature {
			t.Fatalf("enforce: tampered body must be rejected as bad-signature, got %+v", got)
		}
	}

	// Log-only: tampered body logged but accepted.
	{
		v := NewVerifier(ModeLogOnly, quietLogger())
		b1, sig1 := s.signed(t, "hive-a", 1, "creds")
		v.Verify([]string{s.pubHex}, "hive-a", b1, sig1, time.Now())
		b2, sig2 := s.signed(t, "hive-a", 2, "creds")
		bad := tamper(b2)
		got := v.Verify([]string{s.pubHex}, "hive-a", bad, sig2, time.Now())
		if !got.Accepted || got.Reason != ReasonBadSignature {
			t.Fatalf("log-only: tampered body must be accepted-with-log, got %+v", got)
		}
	}
}

// Response signed for hive A presented to hive B -> rejected when enforcing.
// This is the cross-hive replay case.
func TestCrossHiveReplayRejectedWhenEnforcing(t *testing.T) {
	s := newSigner(t)
	body, sig := s.signed(t, "hive-a", 1, "creds") // genuine hub signature, bound to hive-a

	// Spoke B has established trust with its own genuine responses first, so it
	// is past the pre-trust window and will actually enforce.
	v := NewVerifier(ModeEnforce, quietLogger())
	bB, sigB := s.signed(t, "hive-b", 1, "creds")
	if got := v.Verify([]string{s.pubHex}, "hive-b", bB, sigB, time.Now()); !got.Accepted {
		t.Fatalf("setup: hive-b's own signed response should be accepted, got %+v", got)
	}

	// Now the captured hive-a response (seq 1, higher than nothing; use a fresh
	// verifier check by seq 2 to rule out the seq guard masking the hive check).
	bodyA2, sigA2 := s.signed(t, "hive-a", 2, "creds")
	_ = body
	_ = sig
	got := v.Verify([]string{s.pubHex}, "hive-b", bodyA2, sigA2, time.Now())
	if got.Accepted || got.Reason != ReasonHiveMismatch {
		t.Fatalf("cross-hive replay must be rejected as hive-mismatch, got %+v", got)
	}
}

// Stale/replayed seq (equal or lower than last seen) -> rejected when enforcing.
func TestStaleSeqRejectedWhenEnforcing(t *testing.T) {
	s := newSigner(t)
	v := NewVerifier(ModeEnforce, quietLogger())

	// Accept seq 5 to set the floor.
	b5, sig5 := s.signed(t, "hive-a", 5, "creds")
	if got := v.Verify([]string{s.pubHex}, "hive-a", b5, sig5, time.Now()); !got.Accepted {
		t.Fatalf("seq 5 should be accepted, got %+v", got)
	}

	// Replay seq 5 (equal) -> stale.
	if got := v.Verify([]string{s.pubHex}, "hive-a", b5, sig5, time.Now()); got.Accepted || got.Reason != ReasonStaleSeq {
		t.Fatalf("equal seq must be rejected as stale, got %+v", got)
	}
	// Older seq 3 -> stale.
	b3, sig3 := s.signed(t, "hive-a", 3, "creds")
	if got := v.Verify([]string{s.pubHex}, "hive-a", b3, sig3, time.Now()); got.Accepted || got.Reason != ReasonStaleSeq {
		t.Fatalf("lower seq must be rejected as stale, got %+v", got)
	}
}

// Unsigned response from an un-upgraded hub, spoke has never seen a signed one
// -> accepted (no bricking) — in BOTH modes, including enforce.
func TestUnsignedAcceptedBeforeTrust(t *testing.T) {
	s := newSigner(t)
	for _, mode := range []Mode{ModeLogOnly, ModeEnforce} {
		v := NewVerifier(mode, quietLogger())
		body, _ := s.signed(t, "hive-a", 1, "creds")
		// Present the body with NO signature header (un-upgraded hub).
		got := v.Verify([]string{s.pubHex}, "hive-a", body, "", time.Now())
		if !got.Accepted || got.Signed || got.Reason != ReasonUnsignedUntrusted {
			t.Fatalf("mode %v: unsigned before trust must be accepted (no bricking), got %+v", mode, got)
		}
	}
}

// Unsigned response AFTER the spoke has seen a valid signed one -> rejected when
// enforcing (downgrade attack); logged-and-accepted when log-only.
func TestUnsignedAfterTrustIsDowngrade(t *testing.T) {
	s := newSigner(t)

	// Enforce: downgrade rejected.
	{
		v := NewVerifier(ModeEnforce, quietLogger())
		b1, sig1 := s.signed(t, "hive-a", 1, "creds")
		if got := v.Verify([]string{s.pubHex}, "hive-a", b1, sig1, time.Now()); !got.Accepted || !got.Signed {
			t.Fatalf("setup signed response should establish trust, got %+v", got)
		}
		b2, _ := s.signed(t, "hive-a", 2, "creds")
		got := v.Verify([]string{s.pubHex}, "hive-a", b2, "", time.Now()) // no sig now
		if got.Accepted || got.Reason != ReasonDowngrade {
			t.Fatalf("enforce: unsigned after trust must be rejected as downgrade, got %+v", got)
		}
	}

	// Log-only: downgrade logged but accepted.
	{
		v := NewVerifier(ModeLogOnly, quietLogger())
		b1, sig1 := s.signed(t, "hive-a", 1, "creds")
		v.Verify([]string{s.pubHex}, "hive-a", b1, sig1, time.Now())
		b2, _ := s.signed(t, "hive-a", 2, "creds")
		got := v.Verify([]string{s.pubHex}, "hive-a", b2, "", time.Now())
		if !got.Accepted || got.Reason != ReasonDowngrade {
			t.Fatalf("log-only: unsigned after trust must be accepted-with-log, got %+v", got)
		}
	}
}

// --- Supporting behaviour ---------------------------------------------------

func TestModeFromString(t *testing.T) {
	cases := map[string]Mode{
		"":         ModeLogOnly,
		"log":      ModeLogOnly,
		"log-only": ModeLogOnly,
		"garbage":  ModeLogOnly,
		"enforce":  ModeEnforce,
		"ENFORCE":  ModeEnforce,
		"off":      ModeOff,
		"disabled": ModeOff,
	}
	for in, want := range cases {
		if got := ModeFromString(in); got != want {
			t.Errorf("ModeFromString(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestModeOffAcceptsEverything(t *testing.T) {
	s := newSigner(t)
	v := NewVerifier(ModeOff, quietLogger())
	// Even a tampered, unsigned, wrong-hive body is accepted when off.
	got := v.Verify([]string{s.pubHex}, "hive-b", []byte(`{"ok":true,"sig_hive_id":"hive-a"}`), "", time.Now())
	if !got.Accepted || got.Reason != ReasonDisabled {
		t.Fatalf("mode off must accept unconditionally, got %+v", got)
	}
}

func TestWrongVersionRejectedWhenEnforcing(t *testing.T) {
	s := newSigner(t)
	v := NewVerifier(ModeEnforce, quietLogger())
	// Establish trust.
	b1, sig1 := s.signed(t, "hive-a", 1, "creds")
	v.Verify([]string{s.pubHex}, "hive-a", b1, sig1, time.Now())
	// Build a validly-signed body with an unknown version.
	resp := testResponse{OK: true, SigHiveID: "hive-a", SigSeq: 2, SigVersion: 9999}
	body, _ := json.Marshal(resp)
	sig := SignBody(s.seedHex, body)
	got := v.Verify([]string{s.pubHex}, "hive-a", body, sig, time.Now())
	if got.Accepted || got.Reason != ReasonWrongVersion {
		t.Fatalf("unknown version must be rejected, got %+v", got)
	}
}

func TestWrongKeyIsBadSignature(t *testing.T) {
	s := newSigner(t)
	other := newSigner(t)
	v := NewVerifier(ModeEnforce, quietLogger())
	// Establish trust with the real key.
	b1, sig1 := s.signed(t, "hive-a", 1, "creds")
	v.Verify([]string{s.pubHex}, "hive-a", b1, sig1, time.Now())
	// A response signed by a DIFFERENT hub key.
	b2, sig2 := other.signed(t, "hive-a", 2, "creds")
	got := v.Verify([]string{s.pubHex}, "hive-a", b2, sig2, time.Now())
	if got.Accepted || got.Reason != ReasonBadSignature {
		t.Fatalf("wrong signing key must be bad-signature, got %+v", got)
	}
}

// The previous-generation key still verifies during a rotation window.
func TestVerifiesAcrossPublishedKeys(t *testing.T) {
	prev := newSigner(t)
	cur := newSigner(t)
	v := NewVerifier(ModeEnforce, quietLogger())
	// Body signed by the PREVIOUS key; spoke holds current-then-previous.
	body, sig := prev.signed(t, "hive-a", 1, "creds")
	got := v.Verify([]string{cur.pubHex, prev.pubHex}, "hive-a", body, sig, time.Now())
	if !got.Accepted || !got.Signed {
		t.Fatalf("a response signed by a published (previous) key must verify, got %+v", got)
	}
}

func TestCountersTrack(t *testing.T) {
	s := newSigner(t)
	v := NewVerifier(ModeLogOnly, quietLogger())
	b1, sig1 := s.signed(t, "hive-a", 1, "creds")
	v.Verify([]string{s.pubHex}, "hive-a", b1, sig1, time.Now()) // signed accept
	v.Verify([]string{s.pubHex}, "hive-a", b1, sig1, time.Now()) // stale -> logged failure
	c := v.Counters()
	if c.SignedAccepted != 1 {
		t.Errorf("SignedAccepted = %d, want 1", c.SignedAccepted)
	}
	if c.LoggedFailures != 1 {
		t.Errorf("LoggedFailures = %d, want 1", c.LoggedFailures)
	}
}

// Sanity: SignBody is deterministic and verifies with the matching public key.
func TestSignBodyRoundTrip(t *testing.T) {
	s := newSigner(t)
	body := []byte(`{"ok":true}`)
	sig := SignBody(s.seedHex, body)
	if sig == "" {
		t.Fatal("SignBody returned empty for a valid seed")
	}
	if !verifyBodySignature(s.pubHex, sig, body) {
		t.Fatal("round-trip verification failed")
	}
	// A public key passed where a seed is expected must not sign.
	if SignBody(s.pubHex, body) != "" {
		// pubHex is 32 bytes too, so it "works" as a seed but produces a
		// signature that will not verify against pubHex — that is acceptable
		// fail-closed behaviour, but we assert the more important direction:
		if verifyBodySignature(s.pubHex, SignBody(s.pubHex, body), body) {
			t.Fatal("a signature made with the public key as a seed must not verify against the public key")
		}
	}
	if SignBody("", body) != "" || SignBody("nothex", body) != "" {
		t.Fatal("SignBody must fail closed on an empty/invalid seed")
	}
}

// Guard that the signature really covers the whole body, not just a prefix.
func TestSignatureCoversEntireBody(t *testing.T) {
	s := newSigner(t)
	body, sig := s.signed(t, "hive-a", 1, "creds")
	flipped := bytes.Repeat([]byte{0}, len(body))
	copy(flipped, body)
	flipped[len(flipped)-2] ^= 0xff
	if verifyBodySignature(s.pubHex, sig, flipped) {
		t.Fatal("signature verified over a modified body")
	}
	_ = io.Discard
}
