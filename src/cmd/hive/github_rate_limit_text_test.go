package main

import "testing"

// TestIsGitHubRateLimitText covers the one place error-string matching survives
// — where it can only ever cause an extra correct classification, never an
// accusation.
func TestIsGitHubRateLimitText(t *testing.T) {
	if isGitHubRateLimitText(nil) {
		t.Error("nil is not a rate limit")
	}
	for _, s := range []string{"API rate limit exceeded", "secondary RATE LIMIT hit"} {
		if !isGitHubRateLimitText(errString(s)) {
			t.Errorf("%q should be detected as a rate limit", s)
		}
	}
	if isGitHubRateLimitText(errString("403 Resource not accessible by integration")) {
		t.Error("a plain 403 is not a rate limit")
	}
}

// errString is a minimal error carrying exactly the message given.
type errString string

func (e errString) Error() string { return string(e) }
