package hub

import (
	"testing"
	"time"
)

func TestMintImpersonateCookieValue_ValidInputs(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	val := mintImpersonateCookieValue("secret123", "admin-user", "target-user", now)
	if val == "" {
		t.Fatal("expected non-empty cookie value")
	}

	grant, ok := verifyImpersonateCookieValue("secret123", val, now)
	if !ok {
		t.Fatal("freshly minted cookie should verify")
	}
	if grant.Admin != "admin-user" {
		t.Errorf("Admin = %q, want %q", grant.Admin, "admin-user")
	}
	if grant.Target != "target-user" {
		t.Errorf("Target = %q, want %q", grant.Target, "target-user")
	}
	expectedExp := now.Add(impersonateTTL).Unix()
	if grant.Exp != expectedExp {
		t.Errorf("Exp = %d, want %d", grant.Exp, expectedExp)
	}
}

func TestMintImpersonateCookieValue_EmptySecret(t *testing.T) {
	if v := mintImpersonateCookieValue("", "admin", "target", time.Now()); v != "" {
		t.Errorf("empty secret should return empty, got %q", v)
	}
}

func TestMintImpersonateCookieValue_EmptyAdmin(t *testing.T) {
	if v := mintImpersonateCookieValue("secret", "", "target", time.Now()); v != "" {
		t.Errorf("empty admin should return empty, got %q", v)
	}
}

func TestMintImpersonateCookieValue_EmptyTarget(t *testing.T) {
	if v := mintImpersonateCookieValue("secret", "admin", "", time.Now()); v != "" {
		t.Errorf("empty target should return empty, got %q", v)
	}
}

func TestVerifyImpersonateCookieValue_ExpiredCookie(t *testing.T) {
	mintTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	val := mintImpersonateCookieValue("secret", "admin", "target", mintTime)

	verifyTime := mintTime.Add(impersonateTTL + time.Second)
	_, ok := verifyImpersonateCookieValue("secret", val, verifyTime)
	if ok {
		t.Error("expired cookie should not verify")
	}
}

func TestVerifyImpersonateCookieValue_JustBeforeExpiry(t *testing.T) {
	mintTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	val := mintImpersonateCookieValue("secret", "admin", "target", mintTime)

	verifyTime := mintTime.Add(impersonateTTL - time.Second)
	_, ok := verifyImpersonateCookieValue("secret", val, verifyTime)
	if !ok {
		t.Error("cookie should still be valid 1 second before expiry")
	}
}

func TestVerifyImpersonateCookieValue_WrongSecret(t *testing.T) {
	now := time.Now()
	val := mintImpersonateCookieValue("secret-A", "admin", "target", now)

	_, ok := verifyImpersonateCookieValue("secret-B", val, now)
	if ok {
		t.Error("cookie signed with different secret should not verify")
	}
}

func TestVerifyImpersonateCookieValue_TamperedPayload(t *testing.T) {
	now := time.Now()
	val := mintImpersonateCookieValue("secret", "admin", "target", now)

	tampered := "attacker" + val[5:]
	_, ok := verifyImpersonateCookieValue("secret", tampered, now)
	if ok {
		t.Error("tampered cookie should not verify")
	}
}

func TestVerifyImpersonateCookieValue_EmptyValue(t *testing.T) {
	_, ok := verifyImpersonateCookieValue("secret", "", time.Now())
	if ok {
		t.Error("empty value should not verify")
	}
}

func TestVerifyImpersonateCookieValue_EmptySecret(t *testing.T) {
	now := time.Now()
	val := mintImpersonateCookieValue("secret", "admin", "target", now)
	_, ok := verifyImpersonateCookieValue("", val, now)
	if ok {
		t.Error("empty secret should not verify")
	}
}

func TestVerifyImpersonateCookieValue_NoDotSeparator(t *testing.T) {
	_, ok := verifyImpersonateCookieValue("secret", "no-dot-separator", time.Now())
	if ok {
		t.Error("value without dot separator should not verify")
	}
}

func TestVerifyImpersonateCookieValue_DotAtStart(t *testing.T) {
	_, ok := verifyImpersonateCookieValue("secret", ".signature-only", time.Now())
	if ok {
		t.Error("value with dot at position 0 should not verify")
	}
}

func TestVerifyImpersonateCookieValue_DotAtEnd(t *testing.T) {
	_, ok := verifyImpersonateCookieValue("secret", "payload-only.", time.Now())
	if ok {
		t.Error("value with dot at end (empty sig) should not verify")
	}
}

func TestVerifyImpersonateCookieValue_WrongFieldCount(t *testing.T) {
	now := time.Now()
	payload := "admin" + impersonateSep + "target"
	sig := impersonateSign("secret", payload)
	forged := payload + "." + sig

	_, ok := verifyImpersonateCookieValue("secret", forged, now)
	if ok {
		t.Error("cookie with wrong field count should not verify")
	}
}

func TestVerifyImpersonateCookieValue_InvalidExpiry(t *testing.T) {
	now := time.Now()
	payload := "admin" + impersonateSep + "target" + impersonateSep + "not-a-number"
	sig := impersonateSign("secret", payload)
	forged := payload + "." + sig

	_, ok := verifyImpersonateCookieValue("secret", forged, now)
	if ok {
		t.Error("cookie with non-numeric expiry should not verify")
	}
}

func TestVerifyImpersonateCookieValue_EmptyAdminField(t *testing.T) {
	now := time.Now()
	exp := now.Add(impersonateTTL).Unix()
	payload := impersonatePayload("", "target", exp)
	sig := impersonateSign("secret", payload)
	val := payload + "." + sig

	_, ok := verifyImpersonateCookieValue("secret", val, now)
	if ok {
		t.Error("cookie with empty admin field should not verify")
	}
}

func TestVerifyImpersonateCookieValue_EmptyTargetField(t *testing.T) {
	now := time.Now()
	exp := now.Add(impersonateTTL).Unix()
	payload := impersonatePayload("admin", "", exp)
	sig := impersonateSign("secret", payload)
	val := payload + "." + sig

	_, ok := verifyImpersonateCookieValue("secret", val, now)
	if ok {
		t.Error("cookie with empty target field should not verify")
	}
}

func TestImpersonatePayload_Format(t *testing.T) {
	p := impersonatePayload("alice", "bob", 1700000000)
	want := "alice|bob|1700000000"
	if p != want {
		t.Errorf("impersonatePayload = %q, want %q", p, want)
	}
}

func TestImpersonateSign_Deterministic(t *testing.T) {
	s1 := impersonateSign("key", "data")
	s2 := impersonateSign("key", "data")
	if s1 != s2 {
		t.Error("impersonateSign should be deterministic")
	}
	if s1 == "" {
		t.Error("impersonateSign should not return empty")
	}
}

func TestImpersonateSign_DifferentKeys(t *testing.T) {
	s1 := impersonateSign("key-A", "same-data")
	s2 := impersonateSign("key-B", "same-data")
	if s1 == s2 {
		t.Error("different keys should produce different signatures")
	}
}

func TestImpersonateSign_DifferentPayloads(t *testing.T) {
	s1 := impersonateSign("same-key", "payload-1")
	s2 := impersonateSign("same-key", "payload-2")
	if s1 == s2 {
		t.Error("different payloads should produce different signatures")
	}
}

func TestMintAndVerify_RoundTrip_MultipleUsers(t *testing.T) {
	secret := "hub-secret-2025"
	now := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		admin, target string
	}{
		{"alice", "bob"},
		{"hub-admin", "developer123"},
		{"root-admin", "new-user"},
	}

	for _, tc := range cases {
		val := mintImpersonateCookieValue(secret, tc.admin, tc.target, now)
		grant, ok := verifyImpersonateCookieValue(secret, val, now)
		if !ok {
			t.Errorf("verify failed for admin=%q target=%q", tc.admin, tc.target)
			continue
		}
		if grant.Admin != tc.admin || grant.Target != tc.target {
			t.Errorf("round-trip mismatch: got admin=%q target=%q", grant.Admin, grant.Target)
		}
	}
}
