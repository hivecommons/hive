package commands

import (
	"reflect"
	"testing"
)

// withoutGitHubToken is the scrub that keeps the hive's ambient GITHUB_TOKEN /
// GH_TOKEN out of the gh subprocess during enroll, so gh uses the operator's
// own login instead of a hive credential. It had 0% coverage — a regression
// here silently leaks the hive token into every enroll gh call.
func TestWithoutGitHubTokenScrubsOnlyTokenVars(t *testing.T) {
	in := []string{
		"HOME=/home/op",
		"GITHUB_TOKEN=ghp_secret",
		"PATH=/usr/bin",
		"GH_TOKEN=gho_secret",
		"GH_HOST=github.example.com",
		"MY_GITHUB_TOKEN_BACKUP=keepme",
	}
	got := withoutGitHubToken(in)
	want := []string{
		"HOME=/home/op",
		"PATH=/usr/bin",
		"GH_HOST=github.example.com",
		"MY_GITHUB_TOKEN_BACKUP=keepme",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("withoutGitHubToken = %v, want %v", got, want)
	}
}

func TestWithoutGitHubTokenEmptyEnv(t *testing.T) {
	if got := withoutGitHubToken(nil); len(got) != 0 {
		t.Fatalf("withoutGitHubToken(nil) = %v, want empty", got)
	}
}

// numberString renders the enrollment response's installation id, which
// arrives as float64 from JSON but may be int64 or string from other callers.
func TestNumberStringVariants(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"float64", float64(12345), "12345"},
		{"float64 zero", float64(0), ""},
		{"int64", int64(67890), "67890"},
		{"int64 zero", int64(0), ""},
		{"string", "42", "42"},
		{"unsupported type", []int{1}, ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		if got := numberString(tc.in); got != tc.want {
			t.Errorf("%s: numberString(%v) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
