package agent

import "testing"

// GitHubTokenLogin is the exported face of the githubTokenLogin seam — the
// dashboard's model-discovery notice depends on it to name the account behind
// a rejected Copilot credential (#7302). Guard that the export stays wired to
// the overridable seam and passes the token through untouched.
func TestGitHubTokenLoginDelegatesToSeam(t *testing.T) {
	orig := githubTokenLogin
	t.Cleanup(func() { githubTokenLogin = orig })

	var gotToken string
	githubTokenLogin = func(token string) string {
		gotToken = token
		return "alice"
	}

	if login := GitHubTokenLogin("tok-123"); login != "alice" {
		t.Fatalf("GitHubTokenLogin = %q, want %q", login, "alice")
	}
	if gotToken != "tok-123" {
		t.Fatalf("seam received token %q, want %q", gotToken, "tok-123")
	}
}
