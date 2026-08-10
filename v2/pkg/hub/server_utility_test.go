package hub

import (
	"strings"
	"testing"
)

func TestIsValidNameAcceptsSafeRegistryNames(t *testing.T) {
	for _, name := range []string{"hello", "my-org.repo_v2", "a", strings.Repeat("x", 100)} {
		if !isValidName(name) {
			t.Fatalf("isValidName(%q) = false, want true", name)
		}
	}
}

func TestIsValidNameRejectsUnsafeRegistryNames(t *testing.T) {
	for _, name := range []string{"", "has space", "has/slash", "has:colon", strings.Repeat("x", 101)} {
		if isValidName(name) {
			t.Fatalf("isValidName(%q) = true, want false", name)
		}
	}
}
