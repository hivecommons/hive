package hub

import (
	"os"
	"testing"
)

// TestResolveHubAdminUsername verifies the fleet-superuser login is
// configurable via HIVE_HUB_ADMIN_USERNAME with a backward-compatible default
// (audit F12, CWE-798).
func TestResolveHubAdminUsername(t *testing.T) {
	cases := []struct {
		name  string
		unset bool
		env   string
		want  string
	}{
		{name: "unset falls back to default", unset: true, want: defaultHubAdminUsername},
		{name: "blank falls back to default", env: "", want: defaultHubAdminUsername},
		{name: "whitespace-only falls back to default", env: "   ", want: defaultHubAdminUsername},
		{name: "override is honored", env: "fleet-admin", want: "fleet-admin"},
		{name: "override is trimmed", env: "  padded-admin  ", want: "padded-admin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv registers cleanup that restores the prior value after
			// the subtest, so mutating the env here is safe.
			t.Setenv("HIVE_HUB_ADMIN_USERNAME", tc.env)
			if tc.unset {
				if err := os.Unsetenv("HIVE_HUB_ADMIN_USERNAME"); err != nil {
					t.Fatalf("unset env: %v", err)
				}
			}
			if got := resolveHubAdminUsername(); got != tc.want {
				t.Fatalf("resolveHubAdminUsername() = %q, want %q", got, tc.want)
			}
		})
	}
}
