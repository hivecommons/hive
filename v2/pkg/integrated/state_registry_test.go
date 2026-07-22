package integrated

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentStateRegistrySwitchesBetweenRepositoryDirectories(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "repositories", "owner--first")
	second := filepath.Join(root, "repositories", "owner--second")
	for _, stateDir := range []string{first, second} {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, exists, err := CurrentState(root); err != nil || exists {
		t.Fatalf("empty registry = exists %t err %v", exists, err)
	}
	if err := RememberCurrentState(root, first); err != nil {
		t.Fatal(err)
	}
	if current, exists, err := CurrentState(root); err != nil || !exists || current != first {
		t.Fatalf("first selection = %q exists %t err %v", current, exists, err)
	}
	if err := RememberCurrentState(root, second); err != nil {
		t.Fatal(err)
	}
	if current, exists, err := CurrentState(root); err != nil || !exists || current != second {
		t.Fatalf("second selection = %q exists %t err %v", current, exists, err)
	}
	if err := os.RemoveAll(second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CurrentState(root); err == nil {
		t.Fatal("missing selected repository state was silently accepted")
	}
}
