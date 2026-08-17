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

func TestForgetCurrentStateRemovesOnlyExactSelectionAfterTargetDeletion(t *testing.T) {
	root := t.TempDir()
	selected := filepath.Join(root, "selected")
	other := filepath.Join(root, "other")
	for _, path := range []string{selected, other} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := RememberCurrentState(root, selected); err != nil {
		t.Fatal(err)
	}
	if removed, err := ForgetCurrentState(root, other); err != nil || removed {
		t.Fatalf("different repository selection was cleared: removed=%t err=%v", removed, err)
	}
	if current, exists, err := CurrentState(root); err != nil || !exists || !sameFilesystemPath(current, selected) {
		t.Fatalf("different-target attempt changed selection: current=%q exists=%t err=%v", current, exists, err)
	}
	if err := os.RemoveAll(selected); err != nil {
		t.Fatal(err)
	}
	if removed, err := ForgetCurrentState(root, selected); err != nil || !removed {
		t.Fatalf("exact deleted repository selection was not cleared: removed=%t err=%v", removed, err)
	}
	if _, exists, err := CurrentState(root); err != nil || exists {
		t.Fatalf("cleared selection remained active: exists=%t err=%v", exists, err)
	}
	if removed, err := ForgetCurrentState(root, selected); err != nil || removed {
		t.Fatalf("repeated clear was not idempotent: removed=%t err=%v", removed, err)
	}
}
