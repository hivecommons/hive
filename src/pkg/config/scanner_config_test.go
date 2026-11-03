package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The open-PR dispatch guard is on unless
// `scanner.skip_issues_with_open_pr: false` (hivecommons/hive#14799).
func TestScannerSkipIssuesWithOpenPRDefaultsOn(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"project:\n  org: acme\n", true},
		{"scanner:\n  skip_issues_with_open_pr: true\n", true},
		{"scanner:\n  skip_issues_with_open_pr: false\n", false},
	}
	for _, tc := range cases {
		var cfg Config
		if err := yaml.Unmarshal([]byte(tc.src), &cfg); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.src, err)
		}
		if got := cfg.Scanner.IsSkipIssuesWithOpenPR(); got != tc.want {
			t.Errorf("%q: IsSkipIssuesWithOpenPR = %v, want %v", tc.src, got, tc.want)
		}
	}
}
