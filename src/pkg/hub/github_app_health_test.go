package hub

import (
	"testing"
	"time"
)

func TestGitHubAppHealthBuckets(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	stamp := func(age time.Duration) string { return now.Add(-age).Format(time.RFC3339) }
	cases := []struct {
		name string
		in   RegistryEntry
		want string
	}{
		{
			name: "ok token cache",
			in: RegistryEntry{
				GitHubAppTokenStatus:     GitHubAppTokenStatusOK,
				GitHubAppTokenLastMintAt: stamp(time.Minute),
			},
			want: ghAppBucketOK,
		},
		{
			name: "ok status with old mint is degraded",
			in: RegistryEntry{
				GitHubAppTokenStatus:     GitHubAppTokenStatusOK,
				GitHubAppTokenLastMintAt: stamp(GitHubAppTokenStaleAfter + time.Second),
			},
			want: ghAppBucketDegraded,
		},
		{
			name: "explicit stale token cache",
			in:   RegistryEntry{GitHubAppTokenStatus: GitHubAppTokenStatusStale},
			want: ghAppBucketDegraded,
		},
		{
			name: "missing token cache is broken",
			in:   RegistryEntry{GitHubAppTokenStatus: GitHubAppTokenStatusMissing},
			want: ghAppBucketBroken,
		},
		{
			name: "token error is broken",
			in:   RegistryEntry{GitHubAppTokenStatus: GitHubAppTokenStatusError},
			want: ghAppBucketBroken,
		},
		{
			name: "app required is broken",
			in:   RegistryEntry{GitHubAppRequired: true, GitHubAppState: "not-installed"},
			want: ghAppBucketBroken,
		},
		{
			name: "app state ok is ok",
			in:   RegistryEntry{GitHubAppState: GitHubAppTokenStatusOK},
			want: ghAppBucketOK,
		},
		{
			name: "unparseable mint time stays ok",
			in: RegistryEntry{
				GitHubAppTokenStatus:     GitHubAppTokenStatusOK,
				GitHubAppTokenLastMintAt: "not-a-time",
			},
			want: ghAppBucketOK,
		},
		{
			name: "no signal is unknown",
			in:   RegistryEntry{},
			want: ghAppBucketUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := githubAppHealthFor(tc.in, now)
			if got.Bucket != tc.want {
				t.Fatalf("bucket = %q, want %q (health=%+v)", got.Bucket, tc.want, got)
			}
		})
	}
}

func TestGitHubAppHealthDetail(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	type healthCheck struct {
		Name   string
		Status string
		Detail string
	}
	cases := []struct {
		name string
		in   RegistryEntry
		want string
	}{
		{
			name: "perm issue wins",
			in: RegistryEntry{
				GitHubAppRequired:   true,
				GitHubAppPermIssue:  "install expired",
				GitHubAppTokenError: "cache missing",
			},
			want: "install expired",
		},
		{
			name: "token error detail",
			in:   RegistryEntry{GitHubAppTokenStatus: GitHubAppTokenStatusError, GitHubAppTokenError: "connection refused"},
			want: "connection refused",
		},
		{
			name: "map health detail",
			in: RegistryEntry{Health: map[string]any{"checks": []any{
				map[string]any{"name": "ready", "status": "pass"},
				map[string]any{"name": "github_auth", "status": "fail", "detail": "token error"},
			}}},
			want: "token error",
		},
		{
			name: "struct health detail",
			in: RegistryEntry{Health: map[string]any{"checks": []healthCheck{
				{Name: "github_auth", Status: "fail", Detail: "no auth"},
			}}},
			want: "no auth",
		},
		{
			name: "non failing health ignored",
			in: RegistryEntry{Health: map[string]any{"checks": []any{
				map[string]any{"name": "github_auth", "status": "pass", "detail": "ok"},
			}}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := githubAppHealthFor(tc.in, now)
			if got.Detail != tc.want {
				t.Fatalf("detail = %q, want %q (health=%+v)", got.Detail, tc.want, got)
			}
		})
	}
}

func TestGitHubAppHealthStructuredDetails(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   RegistryEntry
		want string
	}{
		{
			name: "installation token 404 names identity",
			in: RegistryEntry{
				GitHubAppRequired:    true,
				GitHubAppState:       "not-installed",
				GitHubHost:           "github.ibm.com",
				GitHubAppID:          123,
				GitHubInstallationID: 456,
				GitHubAppErrorClass:  "installation-not-found",
				GitHubAppHTTPStatus:  404,
				GitHubAppTokenError:  "creating installation token: 404",
			},
			want: "GitHub App 123 on github.ibm.com: installation 456 not found (404) — reinstall or fix app_id/installation_id",
		},
		{
			name: "invalid key",
			in: RegistryEntry{
				GitHubAppRequired:   true,
				GitHubAppState:      "key-invalid",
				GitHubHost:          "github.com",
				GitHubAppID:         123,
				GitHubAppErrorClass: "key-invalid",
			},
			want: "GitHub App 123 on github.com: private key rejected — fix app_id or the installed private key",
		},
		{
			name: "wrong installation",
			in: RegistryEntry{
				GitHubAppRequired:    true,
				GitHubAppState:       "wrong-installation",
				GitHubHost:           "github.ibm.com",
				GitHubAppID:          123,
				GitHubInstallationID: 456,
				GitHubAppErrorClass:  "wrong-installation",
			},
			want: "GitHub App 123 on github.ibm.com: installation 456 belongs to a different account — fix installation_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := githubAppHealthFor(tc.in, now)
			if got.Detail != tc.want {
				t.Fatalf("detail = %q, want %q", got.Detail, tc.want)
			}
		})
	}
}
