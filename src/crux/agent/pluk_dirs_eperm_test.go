package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChmodPlukDirToleratesEPERMWhenModeAlreadyShared(t *testing.T) {
	orig := plukChmod
	t.Cleanup(func() { plukChmod = orig })
	plukChmod = func(string, os.FileMode) error { return os.ErrPermission }

	good := filepath.Join(t.TempDir(), "good")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(good, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := chmodPlukDir(good, plukSharedDirMode); err != nil {
		t.Errorf("EPERM on already-shared dir should be tolerated, got %v", err)
	}

	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := chmodPlukDir(bad, plukSharedDirMode); err == nil {
		t.Error("EPERM on dir lacking group access must still be reported")
	}
}
