package commands

import (
	"strings"
	"testing"
)

// withoutGitHubToken guards the enroll flow's `gh` subprocess from inheriting
// the hive's own GitHub credentials: gh must authenticate as the OPERATOR
// (their gh login), never as the hive App token that happens to be in the
// environment. A regression here silently enrolls spokes with the wrong
// identity, so the scrub is pinned by tests.

func TestWithoutGitHubTokenStripsTokenVars(t *testing.T) {
	env := []string{
		"HOME=/home/op",
		"GITHUB_TOKEN=ghp_secret",
		"PATH=/usr/bin",
		"GH_TOKEN=gho_secret",
		"HIVE_REPO=org/repo",
	}
	got := withoutGitHubToken(env)
	want := []string{"HOME=/home/op", "PATH=/usr/bin", "HIVE_REPO=org/repo"}
	if len(got) != len(want) {
		t.Fatalf("got %d vars %v, want %d %v", len(got), got, len(want), want)
	}
	for i, kv := range want {
		if got[i] != kv {
			t.Fatalf("got[%d] = %q, want %q (order must be preserved)", i, got[i], kv)
		}
	}
	for _, kv := range got {
		if strings.Contains(kv, "secret") {
			t.Fatalf("token leaked through scrub: %q", kv)
		}
	}
}

func TestWithoutGitHubTokenKeepsNonTokenLookalikes(t *testing.T) {
	// Only the exact GITHUB_TOKEN= / GH_TOKEN= keys are stripped; other vars
	// that merely mention tokens (e.g. GH_TOKEN_FILE) must survive.
	env := []string{
		"GH_TOKEN_FILE=/tmp/tok",
		"MY_GITHUB_TOKEN=keep",
		"GITHUB_TOKEN_BACKUP=keep",
	}
	got := withoutGitHubToken(env)
	if len(got) != 3 {
		t.Fatalf("lookalike vars dropped: got %v", got)
	}
}

func TestWithoutGitHubTokenEmptyEnv(t *testing.T) {
	if got := withoutGitHubToken(nil); len(got) != 0 {
		t.Fatalf("nil env: got %v, want empty", got)
	}
	if got := withoutGitHubToken([]string{"GITHUB_TOKEN=x", "GH_TOKEN=y"}); len(got) != 0 {
		t.Fatalf("all-token env: got %v, want empty", got)
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
