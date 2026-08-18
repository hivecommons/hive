package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrepareSpecialistCodexHomeCopiesOnlyBoundedAuthentication(t *testing.T) {
	source := t.TempDir()
	auth := []byte(`{"tokens":{"access_token":"test-only"}}`)
	if err := os.WriteFile(filepath.Join(source, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("unsafe_user_config=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(t.TempDir(), "specialists")
	home, err := prepareSpecialistCodexHome(source, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(home) == filepath.Clean(source) || filepath.Base(filepath.Dir(home)) != ".codex-homes" || !strings.HasPrefix(filepath.Base(home), "manager-") {
		t.Fatalf("isolated Codex home = %q", home)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Fatalf("isolated Codex home copied unexpected files: %v", entries)
	}
	copied, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || string(copied) != string(auth) {
		t.Fatalf("copied authentication changed: data=%q err=%v", copied, err)
	}
	if info, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("copied auth permissions = %o, want 600", info.Mode().Perm())
	}
	if err := cleanupSpecialistCodexHome(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("isolated Codex home was not removed: %v", err)
	}
	remaining, err := os.ReadDir(filepath.Join(workDir, ".codex-homes"))
	if err != nil || len(remaining) != 0 {
		t.Fatalf("Codex home cleanup left entries: %v, err=%v", remaining, err)
	}
}

func TestPrepareSpecialistCodexHomeFailsClosed(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "specialists")
	if _, err := prepareSpecialistCodexHome("relative", workDir); err == nil {
		t.Fatal("relative Codex home was accepted")
	}
	missing := t.TempDir()
	if _, err := prepareSpecialistCodexHome(missing, workDir); err == nil {
		t.Fatal("missing auth.json was accepted")
	}
	invalid := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalid, "auth.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSpecialistCodexHome(invalid, workDir); err == nil {
		t.Fatal("malformed auth.json was accepted")
	}
	linked := t.TempDir()
	target := filepath.Join(linked, "real-auth.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linked, "auth.json")); err == nil {
		if _, err := prepareSpecialistCodexHome(linked, workDir); err == nil {
			t.Fatal("linked auth.json was accepted")
		}
	}
}
