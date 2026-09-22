package taskmcp

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLeaseTokenMintVerifyWrongTaskAndExpiry(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	token, claims, err := MintLeaseToken([]byte("secret"), LeaseTokenClaims{
		TaskID: "task-1", Identity: "alice#one", Repo: "owner/repo", Number: 42, ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || claims.ID == "" {
		t.Fatalf("token=%q claims=%#v", token, claims)
	}
	got, err := VerifyLeaseToken([]byte("secret"), token, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Matches("task-1", "OWNER/repo", 42) {
		t.Fatalf("verified claims do not match scope: %#v", got)
	}
	if got.Matches("task-2", "owner/repo", 42) || got.Matches("task-1", "other/repo", 42) {
		t.Fatalf("claims matched wrong task or repo: %#v", got)
	}
	if _, err := VerifyLeaseToken([]byte("secret"), token, now.Add(time.Hour)); !errors.Is(err, ErrLeaseTokenExpired) {
		t.Fatalf("expired verify err = %v, want ErrLeaseTokenExpired", err)
	}
	if _, err := VerifyLeaseToken([]byte("other"), token, now); !errors.Is(err, ErrLeaseTokenInvalid) {
		t.Fatalf("wrong secret err = %v, want ErrLeaseTokenInvalid", err)
	}
	if HashLeaseToken(token) == "" {
		t.Fatal("HashLeaseToken returned empty")
	}
	if err := (RefusalError{Err: ErrForbidden, Data: RefusalData{Reason: "no"}}).Unwrap(); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unwrap = %v", err)
	}
	if got := (RefusalError{Data: RefusalData{Reason: "no"}}).Error(); got != "no" {
		t.Fatalf("refusal Error = %q", got)
	}
	if !errors.Is(LeaseTokenWrongScopeError("task-1", "owner/repo", 1), ErrForbidden) ||
		!errors.Is(LeaseTokenWrongScopeError("task-1", "owner/repo", 0), ErrForbidden) {
		t.Fatal("wrong scope errors must wrap ErrForbidden")
	}
}

func TestLeaseTokenRejectsMalformedInputs(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if _, _, err := MintLeaseToken(nil, LeaseTokenClaims{}, now); !errors.Is(err, ErrLeaseTokenInvalid) {
		t.Fatalf("mint missing fields err = %v", err)
	}
	for _, token := range []string{"", "bad.parts", LeaseTokenPrefix + ".not-base64.sig"} {
		if _, err := VerifyLeaseToken([]byte("secret"), token, now); !errors.Is(err, ErrLeaseTokenInvalid) {
			t.Fatalf("VerifyLeaseToken(%q) err = %v, want invalid", token, err)
		}
	}
	good, _, err := MintLeaseToken([]byte("secret"), LeaseTokenClaims{
		TaskID: "task-1", Identity: "alice", Repo: "owner/repo", ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, ".")
	badPayload := LeaseTokenPrefix + "." + parts[1][:len(parts[1])-1] + "." + parts[2]
	if _, err := VerifyLeaseToken([]byte("secret"), badPayload, now); !errors.Is(err, ErrLeaseTokenInvalid) {
		t.Fatalf("tampered payload err = %v, want invalid", err)
	}
}
