package dashboard

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeLiteLLMKeyFile creates the secrets dir and writes the key 0600 when the
// parent is writable — the normal PVC path.
func TestWriteLiteLLMKeyFile_CreatesDirAndFile(t *testing.T) {
	orig := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(t.TempDir(), "secrets", "litellm_api_key")
	t.Cleanup(func() { writableLiteLLMKeyFile = orig })

	key := "sk" + "-keyvalue-abcdefghijklmnop"
	if err := writeLiteLLMKeyFile(key); err != nil {
		t.Fatalf("writeLiteLLMKeyFile: %v", err)
	}

	got, err := os.ReadFile(writableLiteLLMKeyFile)
	if err != nil {
		t.Fatalf("reading key file: %v", err)
	}
	if string(got) != key {
		t.Errorf("key file content mismatch")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(writableLiteLLMKeyFile)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != litellmKeyFileMode {
			t.Errorf("key file mode = %v, want %v", info.Mode().Perm(), os.FileMode(litellmKeyFileMode))
		}
		dirInfo, err := os.Stat(filepath.Dir(writableLiteLLMKeyFile))
		if err != nil {
			t.Fatal(err)
		}
		if dirInfo.Mode().Perm() != litellmKeyDirMode {
			t.Errorf("secrets dir mode = %v, want %v", dirInfo.Mode().Perm(), os.FileMode(litellmKeyDirMode))
		}
	}
}

// When the secrets dir pre-exists but is not writable (the root-owned-dir /
// misowned-PVC case that produced the original EACCES bug), the error is
// human-readable and actionable — not a bare "permission denied" — and never
// contains the key value.
func TestWriteLiteLLMKeyFile_UnwritableDirYieldsActionableError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission model")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	base := t.TempDir()
	secretsDir := filepath.Join(base, "secrets")
	if err := os.Mkdir(secretsDir, 0o500); err != nil { // r-x, no write
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secretsDir, 0o700) }) // let TempDir cleanup remove it

	orig := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(secretsDir, "litellm_api_key")
	t.Cleanup(func() { writableLiteLLMKeyFile = orig })

	key := "sk" + "-verysecretkeyvalue123456"
	err := writeLiteLLMKeyFile(key)
	if err == nil {
		t.Fatal("expected an error writing into an unwritable dir")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error should wrap os.ErrPermission, got: %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, key) {
		t.Fatal("error message leaks the key value")
	}
	for _, want := range []string{"read-only or misowned", writableLiteLLMKeyFile} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; got: %s", want, msg)
		}
	}
}
