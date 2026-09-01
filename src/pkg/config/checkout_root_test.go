package config

import (
	"path/filepath"
	"testing"
)

// TestCheckoutRootFor pins ProjectConfig.CheckoutRootFor (config.go), which
// maps a monitored repo to its host-local checkout root (kubestellar/hive#5227)
// and was previously untested at 0% coverage. The traversal guard matters:
// the result is a filesystem path built from config strings, and a name of
// ".." or one carrying a separator must never escape CheckoutsDir.
func TestCheckoutRootFor(t *testing.T) {
	cases := []struct {
		name string
		dir  string
		repo string
		want string
	}{
		{"bare repo name", "/data/checkouts", "hive", filepath.Join("/data/checkouts", "hive")},
		{"org-qualified slug uses name only", "/data/checkouts", "kubestellar/hive", filepath.Join("/data/checkouts", "hive")},
		{"deep slug uses last segment", "/data/checkouts", "gitlab.com/group/sub/repo", filepath.Join("/data/checkouts", "repo")},
		{"empty checkouts dir is a no-op", "", "hive", ""},
		{"whitespace-only checkouts dir is a no-op", "   ", "hive", ""},
		{"empty repo is a no-op", "/data/checkouts", "", ""},
		{"slug with trailing slash has no name", "/data/checkouts", "kubestellar/", ""},
		{"dot name refused", "/data/checkouts", ".", ""},
		{"dotdot name refused", "/data/checkouts", "..", ""},
		{"org-qualified dotdot refused", "/data/checkouts", "kubestellar/..", ""},
		{"backslash in name refused", "/data/checkouts", `evil\name`, ""},
		{"org-qualified backslash refused", "/data/checkouts", `org/..\evil`, ""},
		{"surrounding whitespace trimmed", "  /data/checkouts  ", "  hive  ", filepath.Join("/data/checkouts", "hive")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &ProjectConfig{CheckoutsDir: tc.dir}
			if got := p.CheckoutRootFor(tc.repo); got != tc.want {
				t.Fatalf("CheckoutRootFor(%q) with CheckoutsDir=%q = %q, want %q", tc.repo, tc.dir, got, tc.want)
			}
		})
	}
}

// A refused or unconfigured lookup must return exactly "" — the documented
// no-op sentinel — never a partial path the caller might join or stat.
func TestCheckoutRootFor_NoOpIsEmptyString(t *testing.T) {
	p := &ProjectConfig{}
	if got := p.CheckoutRootFor("kubestellar/hive"); got != "" {
		t.Fatalf("unconfigured CheckoutRootFor = %q, want empty string", got)
	}
}
