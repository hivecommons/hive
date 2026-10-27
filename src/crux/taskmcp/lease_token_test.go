package taskmcp

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLeaseTokenMintVerifyAndScope(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	claims := LeaseTokenClaims{TaskID: "t1", Identity: "alice", Repo: "owner/repo", Number: 42, Stage: "spec", ExpiresAt: now.Add(time.Hour), Contributor: "alice"}
	token, minted, err := MintLeaseToken([]byte("secret"), claims, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, LeaseTokenPrefix+".") || minted.ID == "" || minted.IssuedAt.IsZero() {
		t.Fatalf("token=%q claims=%#v", token, minted)
	}
	verified, err := VerifyLeaseToken([]byte("secret"), token, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Matches("t1", "OWNER/REPO", 42) || !verified.Matches("t1", "owner/repo", 0) || verified.Matches("t1", "owner/repo", 7) {
		t.Fatalf("scope matching failed: %#v", verified)
	}
	if verified.Stage != "spec" {
		t.Fatalf("stage = %q, want spec", verified.Stage)
	}
	if HashLeaseToken(token) == HashLeaseToken(token+"x") {
		t.Fatal("hash should change when token changes")
	}
}

func TestLeaseTokenRejectsInvalidAndExpired(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if _, _, err := MintLeaseToken(nil, LeaseTokenClaims{}, now); !errors.Is(err, ErrLeaseTokenInvalid) {
		t.Fatalf("mint invalid err = %v", err)
	}
	token, _, err := MintLeaseToken([]byte("secret"), LeaseTokenClaims{TaskID: "t1", Identity: "alice", Repo: "owner/repo", ExpiresAt: now.Add(time.Minute)}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "wrong.parts", strings.Replace(token, LeaseTokenPrefix, "bad", 1), token + "x"} {
		if _, err := VerifyLeaseToken([]byte("secret"), bad, now); !errors.Is(err, ErrLeaseTokenInvalid) {
			t.Fatalf("VerifyLeaseToken(%q) err = %v", bad, err)
		}
	}
	if _, err := VerifyLeaseToken([]byte("secret"), token, now.Add(time.Hour)); !errors.Is(err, ErrLeaseTokenExpired) {
		t.Fatalf("expired err = %v", err)
	}
	if err := LeaseTokenWrongScopeError("t1", "owner/repo", 42); !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "#42") {
		t.Fatalf("wrong scope err = %v", err)
	}
}
