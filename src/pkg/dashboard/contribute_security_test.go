package dashboard

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestVerifyInviteTokenRejectsReservedUsername(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-for-unit-tests")
	resetInviteSecretForTest(t)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	token := mintInviteToken("admin", now)
	if got := verifyInviteToken(token, now); got != "" {
		t.Fatalf("reserved inviter verified to %q, want empty", got)
	}
}

func TestIsPrivateURLBlocksDNSRebindingAndFailures(t *testing.T) {
	origResolver := privateURLResolver
	t.Cleanup(func() { privateURLResolver = origResolver })
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "public.example":
			return []string{"93.184.216.34"}, nil
		case "rebind.example":
			return []string{"10.0.0.1"}, nil
		default:
			return nil, context.DeadlineExceeded
		}
	}

	ctx := context.Background()
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "literal localhost", raw: "http://localhost:8080/path", want: true},
		{name: "literal metadata", raw: "http://169.254.169.254/latest", want: true},
		{name: "public dns", raw: "https://public.example/path", want: false},
		{name: "dns rebind", raw: "https://rebind.example/path", want: true},
		{name: "dns failure", raw: "https://missing.example/path", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPrivateURL(ctx, tc.raw); got != tc.want {
				t.Fatalf("isPrivateURL(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestPruneQueueHoldReasonsKeepsOnlyLiveNonBlankReasons(t *testing.T) {
	got := pruneQueueHoldReasons(map[string]string{
		"budget": "over limit",
		"manual": "user requested",
		"stale":  "old reason",
		"empty":  "  ",
	}, []string{"budget", "manual", "empty", "missing"})

	if len(got) != 2 {
		t.Fatalf("len(pruned reasons) = %d, want 2: %#v", len(got), got)
	}
	if got["budget"] != "over limit" || got["manual"] != "user requested" {
		t.Fatalf("kept reasons = %#v, want budget and manual reasons", got)
	}
	if _, ok := got["stale"]; ok {
		t.Fatal("stale issue reason should be pruned")
	}
	if _, ok := got["empty"]; ok {
		t.Fatal("blank reason should be pruned")
	}
}

func TestIsValidUsernameRejectsReservedNames(t *testing.T) {
	for _, username := range []string{"admin", "ROOT", "null"} {
		if isValidUsername(username) {
			t.Fatalf("isValidUsername(%q) = true, want false for reserved username", username)
		}
	}
}

func resetInviteSecretForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		inviteSecretOnce = sync.Once{}
		inviteSecretCache = nil
	}
	reset()
	t.Cleanup(reset)
}
