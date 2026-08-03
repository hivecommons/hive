package hub

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadSaaSUserBackfillsLoginCount covers the read-time backfill: a record
// that predates the login counter (a real LastLogin but LoginCount 0) must load
// as at least 1 login, so the stats card never shows the contradictory
// "0 logins (last <date>)". A genuine count is never lowered, and a user who has
// never logged in (no LastLogin) stays at 0.
func TestLoadSaaSUserBackfillsLoginCount(t *testing.T) {
	orig := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	if err := os.MkdirAll(saasUsersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { saasUsersDir = orig })

	cases := []struct {
		name      string
		user      SaaSUser
		wantCount int
	}{
		{
			name:      "pre-counter user: last_login set, count 0 -> 1",
			user:      SaaSUser{GitHubUsername: "gee4vee", LastLogin: "2026-07-30T18:10:00Z"},
			wantCount: 1,
		},
		{
			name:      "genuine count is preserved, never lowered",
			user:      SaaSUser{GitHubUsername: "active", LastLogin: "2026-07-30T18:10:00Z", LoginCount: 42},
			wantCount: 42,
		},
		{
			name:      "never-logged-in user stays at 0",
			user:      SaaSUser{GitHubUsername: "newbie"},
			wantCount: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := saveSaaSUser(&tc.user); err != nil {
				t.Fatal(err)
			}
			got := loadSaaSUser(tc.user.GitHubUsername)
			if got == nil {
				t.Fatal("loadSaaSUser returned nil")
			}
			if got.LoginCount != tc.wantCount {
				t.Errorf("LoginCount = %d, want %d", got.LoginCount, tc.wantCount)
			}
		})
	}
}
