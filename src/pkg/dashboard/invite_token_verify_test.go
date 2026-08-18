package dashboard

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestVerifyInviteToken_RoundTrip confirms that a freshly minted token
// verifies back to the original inviter.
func TestVerifyInviteToken_RoundTrip(t *testing.T) {
	setupContributeEnv(t)
	now := time.Now()
	tok := mintInviteToken("alice", now)
	if got := verifyInviteToken(tok, now); got != "alice" {
		t.Fatalf("round-trip failed: got %q, want %q", got, "alice")
	}
}

// TestVerifyInviteToken_TamperedSignature rejects a token whose HMAC has been
// altered (simulates an attacker forging attribution).
func TestVerifyInviteToken_TamperedSignature(t *testing.T) {
	setupContributeEnv(t)
	tok := mintInviteToken("bob", time.Now())
	parts := strings.Split(tok, ".")
	// Flip a character in the signature.
	sig := []byte(parts[2])
	if sig[0] == 'A' {
		sig[0] = 'B'
	} else {
		sig[0] = 'A'
	}
	tampered := parts[0] + "." + parts[1] + "." + string(sig)
	if got := verifyInviteToken(tampered, time.Now()); got != "" {
		t.Fatalf("tampered signature verified to %q, want empty", got)
	}
}

// TestVerifyInviteToken_TamperedExpiry rejects a token whose expiry was
// altered to extend its validity (forward-date attack).
func TestVerifyInviteToken_TamperedExpiry(t *testing.T) {
	setupContributeEnv(t)
	tok := mintInviteToken("carol", time.Now())
	parts := strings.Split(tok, ".")
	// Replace expiry with a far-future value.
	parts[1] = strconv.FormatInt(time.Now().Add(365*24*time.Hour).Unix(), 10)
	tampered := strings.Join(parts, ".")
	if got := verifyInviteToken(tampered, time.Now()); got != "" {
		t.Fatalf("forward-dated expiry verified to %q, want empty", got)
	}
}

// TestVerifyInviteToken_TamperedInviter rejects a token where the inviter
// field was swapped (impersonation).
func TestVerifyInviteToken_TamperedInviter(t *testing.T) {
	setupContributeEnv(t)
	tok := mintInviteToken("dave", time.Now())
	parts := strings.Split(tok, ".")
	// Replace inviter with a different username.
	parts[0] = base64.RawURLEncoding.EncodeToString([]byte("evil"))
	tampered := strings.Join(parts, ".")
	if got := verifyInviteToken(tampered, time.Now()); got != "" {
		t.Fatalf("swapped inviter verified to %q, want empty", got)
	}
}

// TestVerifyInviteToken_EmptyString rejects an empty token.
func TestVerifyInviteToken_EmptyString(t *testing.T) {
	setupContributeEnv(t)
	if got := verifyInviteToken("", time.Now()); got != "" {
		t.Fatalf("empty token verified to %q, want empty", got)
	}
}

// TestVerifyInviteToken_TwoParts rejects a token with only two dot-separated
// segments (missing signature).
func TestVerifyInviteToken_TwoParts(t *testing.T) {
	setupContributeEnv(t)
	if got := verifyInviteToken("foo.bar", time.Now()); got != "" {
		t.Fatalf("two-part token verified to %q, want empty", got)
	}
}

// TestVerifyInviteToken_FourParts rejects a token with extra segments.
func TestVerifyInviteToken_FourParts(t *testing.T) {
	setupContributeEnv(t)
	if got := verifyInviteToken("a.b.c.d", time.Now()); got != "" {
		t.Fatalf("four-part token verified to %q, want empty", got)
	}
}

// TestVerifyInviteToken_InvalidBase64Inviter rejects a token where the inviter
// segment is not valid base64.
func TestVerifyInviteToken_InvalidBase64Inviter(t *testing.T) {
	setupContributeEnv(t)
	// Build a token whose signature is technically valid for the literal
	// corrupted content — we only test that the decode step rejects it.
	// Since we cannot produce a valid HMAC for garbage content, a token
	// with invalid base64 will fail at HMAC check first. Confirm empty.
	tok := "%%%invalid%%%.12345.fakesig"
	if got := verifyInviteToken(tok, time.Now()); got != "" {
		t.Fatalf("invalid base64 inviter verified to %q, want empty", got)
	}
}

// TestVerifyInviteToken_NonNumericExpiry rejects a token with non-numeric expiry.
func TestVerifyInviteToken_NonNumericExpiry(t *testing.T) {
	setupContributeEnv(t)
	tok := mintInviteToken("eve", time.Now())
	parts := strings.Split(tok, ".")
	parts[1] = "notanumber"
	tampered := strings.Join(parts, ".")
	if got := verifyInviteToken(tampered, time.Now()); got != "" {
		t.Fatalf("non-numeric expiry verified to %q, want empty", got)
	}
}

// TestVerifyInviteToken_JustExpired ensures a token minted exactly at the TTL
// boundary is rejected one second later.
func TestVerifyInviteToken_JustExpired(t *testing.T) {
	setupContributeEnv(t)
	mintTime := time.Now().Add(-inviteTokenTTL)
	tok := mintInviteToken("frank", mintTime)
	// Verify 1 second after expiry.
	if got := verifyInviteToken(tok, time.Now().Add(time.Second)); got != "" {
		t.Fatalf("just-expired token verified to %q, want empty", got)
	}
}

// TestVerifyInviteToken_StillValid ensures a token minted recently verifies
// until its TTL.
func TestVerifyInviteToken_StillValid(t *testing.T) {
	setupContributeEnv(t)
	mintTime := time.Now()
	tok := mintInviteToken("grace", mintTime)
	// Verify 1 hour later — well within the 14-day TTL.
	if got := verifyInviteToken(tok, mintTime.Add(time.Hour)); got != "grace" {
		t.Fatalf("valid token returned %q, want %q", got, "grace")
	}
}

// TestVerifyInviteToken_WhitespaceHandling confirms leading/trailing whitespace
// is tolerated (copy-paste from chat often adds spaces).
func TestVerifyInviteToken_WhitespaceHandling(t *testing.T) {
	setupContributeEnv(t)
	tok := mintInviteToken("hank", time.Now())
	padded := "  " + tok + "\n"
	if got := verifyInviteToken(padded, time.Now()); got != "hank" {
		t.Fatalf("whitespace-padded token returned %q, want %q", got, "hank")
	}
}
