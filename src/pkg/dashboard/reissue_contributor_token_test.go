package dashboard

import (
	"regexp"
	"testing"
)

// TestReissueContributorToken covers reissueContributorToken (was 0%, #4716):
// the recovery path must rotate to a fresh 256-bit token, store ONLY its
// SHA-256 (never the plaintext), clear any lingering plaintext echo, persist
// the rotation, and thereby invalidate the previous token.
func TestReissueContributorToken(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())

	oldPlain := "old-token-plaintext"
	p := &ContributorProfile{
		GitHubUsername:    "reissue-test-user",
		ContributorID:     "contrib-1",
		TrustTier:         "contributor",
		RegistrationToken: sha256Hex(oldPlain),
		TokenPlain:        oldPlain,
		RegisteredAt:      "2026-08-24T00:00:00Z",
	}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("seeding profile: %v", err)
	}

	newToken := reissueContributorToken(p)

	// 32 random bytes → 64 hex chars of plaintext handed back exactly once.
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(newToken) {
		t.Fatalf("reissued token = %q, want 64 lowercase hex chars (256-bit)", newToken)
	}
	if p.RegistrationToken != sha256Hex(newToken) {
		t.Error("stored RegistrationToken is not the SHA-256 of the returned plaintext")
	}
	if p.RegistrationToken == sha256Hex(oldPlain) {
		t.Error("previous token still validates — reissue must invalidate it")
	}
	if p.TokenPlain != "" {
		t.Errorf("TokenPlain = %q, want cleared — plaintext must never persist", p.TokenPlain)
	}

	// The rotation is persisted: a fresh load sees the new hash, no plaintext.
	onDisk, err := loadContributorProfile("reissue-test-user")
	if err != nil {
		t.Fatalf("reloading profile: %v", err)
	}
	if onDisk.RegistrationToken != sha256Hex(newToken) {
		t.Error("on-disk profile does not carry the rotated token hash")
	}
	if onDisk.TokenPlain != "" {
		t.Errorf("on-disk TokenPlain = %q, want empty", onDisk.TokenPlain)
	}

	// Every reissue mints fresh material.
	if again := reissueContributorToken(p); again == newToken {
		t.Error("two reissues returned the same token — must be fresh randomness")
	}
}
