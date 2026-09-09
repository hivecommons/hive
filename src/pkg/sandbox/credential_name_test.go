package sandbox

import "testing"

// IsCredentialName is the exported form of the rule every launcher applies;
// pkg/kubejob relies on it matching isCredentialName exactly.
func TestIsCredentialNameMatchesInternalRule(t *testing.T) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN", "MY_SECRET", "SOME_TOKEN_X", "GIT_ASKPASS", "PATH", "HOME", "ANTHROPIC_MODEL"} {
		if IsCredentialName(name) != isCredentialName(name) {
			t.Errorf("IsCredentialName(%q) diverged from isCredentialName", name)
		}
	}
	if !IsCredentialName("GITHUB_TOKEN") || IsCredentialName("PATH") {
		t.Error("IsCredentialName must reject GITHUB_TOKEN and allow PATH")
	}
}
