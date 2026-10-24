package openrouter

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestChallengeForS256 asserts the exact PKCE S256 transform:
// code_challenge == base64url-no-pad( sha256( verifier ) ).
func TestChallengeForS256(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := ChallengeFor(verifier); got != want {
		t.Fatalf("ChallengeFor mismatch:\n got=%s\nwant=%s", got, want)
	}
}

// TestGeneratePKCE checks the verifier length bounds (RFC 7636: 43–128 chars)
// and that GeneratePKCE's challenge matches ChallengeFor(verifier).
func TestGeneratePKCE(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier length %d out of RFC 7636 range 43..128", len(verifier))
	}
	if strings.ContainsAny(verifier, "+/=") {
		t.Fatalf("verifier is not url-safe (has +/=): %q", verifier)
	}
	if challenge != ChallengeFor(verifier) {
		t.Fatalf("GeneratePKCE challenge does not match ChallengeFor(verifier)")
	}
	// Two calls must differ (randomness).
	v2, _, _ := GeneratePKCE()
	if verifier == v2 {
		t.Fatalf("two GeneratePKCE calls produced identical verifiers")
	}
}

// TestBuildAuthorizeURL asserts the authorize URL carries the required PKCE
// params and that state is bound to the callback_url (round-trips).
func TestBuildAuthorizeURL(t *testing.T) {
	authURL, err := BuildAuthorizeURL("https://h.example.com/openrouter/callback", "CHAL", "STATE123")
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	if !strings.HasPrefix(authURL, AuthURL+"?") {
		t.Fatalf("authorize URL does not start with %s: %s", AuthURL, authURL)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	q := u.Query()
	if q.Get("code_challenge") != "CHAL" {
		t.Errorf("code_challenge = %q, want CHAL", q.Get("code_challenge"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	// callback_url must embed the state so it round-trips even if OpenRouter
	// only echoes callback_url.
	cbRaw := q.Get("callback_url")
	cb, err := url.Parse(cbRaw)
	if err != nil {
		t.Fatalf("parse callback_url: %v", err)
	}
	if cb.Query().Get("state") != "STATE123" {
		t.Errorf("callback_url state = %q, want STATE123 (state not bound to callback)", cb.Query().Get("state"))
	}
}

// TestStateStoreSingleUse verifies that a state can be consumed exactly once:
// the first Consume succeeds and returns the bound flow; a replay fails.
func TestStateStoreSingleUse(t *testing.T) {
	s := NewStateStore(StateTTL)
	state, err := s.Create("verifier-abc", "hive-1", "deepseek/deepseek-chat")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if state == "" {
		t.Fatal("Create returned empty state")
	}

	f, ok := s.Consume(state)
	if !ok {
		t.Fatal("first Consume failed, want success")
	}
	if f.Verifier != "verifier-abc" || f.HiveID != "hive-1" || f.Model != "deepseek/deepseek-chat" {
		t.Fatalf("consumed flow mismatch: %+v", f)
	}

	// Replay must fail — single-use.
	if _, ok := s.Consume(state); ok {
		t.Fatal("second Consume succeeded, want single-use rejection (replay)")
	}
}

// TestStateStoreUnknown rejects a state that was never created.
func TestStateStoreUnknown(t *testing.T) {
	s := NewStateStore(StateTTL)
	if _, ok := s.Consume("never-created"); ok {
		t.Fatal("Consume of unknown state succeeded, want rejection")
	}
}

// TestStateStoreExpiry verifies an expired flow is rejected, using an injected
// clock so the test does not sleep.
func TestStateStoreExpiry(t *testing.T) {
	s := NewStateStore(10 * time.Minute)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	s.SetClock(func() time.Time { return now })

	state, err := s.Create("v", "hive-1", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance past the TTL.
	now = base.Add(11 * time.Minute)
	if _, ok := s.Consume(state); ok {
		t.Fatal("Consume of expired state succeeded, want expiry rejection")
	}
}

// TestStateStoreWithinTTL confirms a flow just inside the TTL still consumes.
func TestStateStoreWithinTTL(t *testing.T) {
	s := NewStateStore(10 * time.Minute)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	s.SetClock(func() time.Time { return now })

	state, _ := s.Create("v", "hive-1", "")
	now = base.Add(9 * time.Minute)
	if _, ok := s.Consume(state); !ok {
		t.Fatal("Consume within TTL failed, want success")
	}
}

// TestQRPNG checks the QR encoder returns a valid PNG (magic header) for a URL.
func TestQRPNG(t *testing.T) {
	png, err := QRPNG("https://openrouter.ai/auth?callback_url=x&code_challenge=y")
	if err != nil {
		t.Fatalf("QRPNG: %v", err)
	}
	if len(png) < 8 {
		t.Fatal("QRPNG returned too few bytes")
	}
	// PNG signature: 89 50 4E 47 0D 0A 1A 0A
	sig := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if string(png[:8]) != string(sig) {
		t.Fatalf("QRPNG output is not a PNG (bad signature)")
	}
	if _, err := QRPNG(""); err == nil {
		t.Fatal("QRPNG(\"\") should error")
	}
}
