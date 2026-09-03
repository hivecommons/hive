package main

import "testing"

// bareRepoName must strip everything up to the last slash and pass a bare
// name through untouched — the audit-trail attribution map keys off the bare
// form, so a fully-qualified and a bare spelling of the same repo must
// collapse to the same key.
func TestBareRepoName(t *testing.T) {
	cases := []struct {
		name string
		repo string
		want string
	}{
		{"qualified", "hivecommons/hive", "hive"},
		{"bare passthrough", "hive", "hive"},
		{"nested path keeps last segment", "ghe.example.com/org/hive", "hive"},
		{"trailing slash yields empty", "kubestellar/", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bareRepoName(tc.repo); got != tc.want {
				t.Errorf("bareRepoName(%q) = %q, want %q", tc.repo, got, tc.want)
			}
		})
	}
}

// fullRepoName must qualify a bare repo with the org, and must NEVER rewrite
// a repo that already names an owner — re-qualifying "otherorg/repo" with the
// project org would silently point maintainer-approval and merge checks at
// the wrong repository.
func TestFullRepoName(t *testing.T) {
	cases := []struct {
		name string
		repo string
		org  string
		want string
	}{
		{"bare gets org", "hive", "hivecommons", "hivecommons/hive"},
		{"qualified untouched", "hivecommons/hive", "hivecommons", "hivecommons/hive"},
		{"foreign owner untouched", "otherorg/hive", "kubestellar", "otherorg/hive"},
		{"empty org leaves bare", "hive", "", "hive"},
		{"empty repo stays empty", "", "hivecommons", "hivecommons/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fullRepoName(tc.repo, tc.org); got != tc.want {
				t.Errorf("fullRepoName(%q, %q) = %q, want %q", tc.repo, tc.org, got, tc.want)
			}
		})
	}
}

// describeKeySource is banner copy for the App-key resolution trail: an empty
// or whitespace-only source must render as the explicit "(unset)" marker so an
// operator can tell "not configured" from a path that merely failed to load.
func TestDescribeKeySource(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "(unset)"},
		{"whitespace only", "   \t", "(unset)"},
		{"path passthrough", "/data/github-app.pem", "/data/github-app.pem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeKeySource(tc.in); got != tc.want {
				t.Errorf("describeKeySource(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
