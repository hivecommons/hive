package hub

import (
	"strings"
	"testing"
)

func TestAuthorizedUsersForHive_OwnerOnly(t *testing.T) {
	saasUsersDir = t.TempDir()
	h := &SaaSHive{ID: "abc123", Owner: "clubanderson"}
	got := authorizedUsersForHive(h)
	if got != "clubanderson:owner" {
		t.Fatalf("expected owner-only list, got %q", got)
	}
}

func TestAuthorizedUsersForHive_IncludesGrantedViewers(t *testing.T) {
	saasUsersDir = t.TempDir()
	// Owner record.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "clubanderson", Hives: map[string]string{"abc123": "owner"}}); err != nil {
		t.Fatal(err)
	}
	// A granted viewer.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "kellyaa", Hives: map[string]string{"abc123": "read"}}); err != nil {
		t.Fatal(err)
	}
	// A user with no access to this hive — must be excluded.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "stranger", Hives: map[string]string{"other": "read"}}); err != nil {
		t.Fatal(err)
	}

	h := &SaaSHive{ID: "abc123", Owner: "clubanderson"}
	got := authorizedUsersForHive(h)

	if !strings.HasPrefix(got, "clubanderson:owner") {
		t.Fatalf("owner must be first with owner role, got %q", got)
	}
	if !strings.Contains(got, "kellyaa:read") {
		t.Fatalf("granted viewer must be included as read, got %q", got)
	}
	if strings.Contains(got, "stranger") {
		t.Fatalf("user without access to this hive must be excluded, got %q", got)
	}
}

func TestAuthorizedUsersForHive_OwnerNotDuplicated(t *testing.T) {
	saasUsersDir = t.TempDir()
	// Owner also has a user record granting owner on the hive.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "clubanderson", Hives: map[string]string{"abc123": "owner"}}); err != nil {
		t.Fatal(err)
	}
	h := &SaaSHive{ID: "abc123", Owner: "clubanderson"}
	got := authorizedUsersForHive(h)
	if strings.Count(got, "clubanderson") != 1 {
		t.Fatalf("owner must appear exactly once, got %q", got)
	}
}

func TestAuthorizedUsersForHive_GrantedUserGetsReadNotWrite(t *testing.T) {
	saasUsersDir = t.TempDir()
	// A non-owner user whose stored role is "owner" for the hive must still be
	// downgraded to read on the direct route (only the provisioned owner writes).
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "escalate", Hives: map[string]string{"abc123": "owner"}}); err != nil {
		t.Fatal(err)
	}
	h := &SaaSHive{ID: "abc123", Owner: "clubanderson"}
	got := authorizedUsersForHive(h)
	if !strings.Contains(got, "escalate:read") {
		t.Fatalf("non-owner grant must be downgraded to read, got %q", got)
	}
	if strings.Contains(got, "escalate:owner") {
		t.Fatalf("non-owner must not get owner role, got %q", got)
	}
}
