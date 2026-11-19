package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOperatorTokenSources(t *testing.T) {
	dir := t.TempDir()
	orig := OperatorTokenFile
	OperatorTokenFile = filepath.Join(dir, "default-token")
	t.Cleanup(func() { OperatorTokenFile = orig })
	t.Setenv(OperatorTokenEnvVar, "")
	t.Setenv(OperatorTokenFileEnvVar, "")

	if got := OperatorToken(); got != "" {
		t.Fatalf("unset: got %q, want empty", got)
	}

	if err := os.WriteFile(OperatorTokenFile, []byte(" sk-ant-oat01-default \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := OperatorToken(); got != "sk-ant-oat01-default" {
		t.Fatalf("default secret file: got %q", got)
	}

	named := filepath.Join(dir, "named-token")
	if err := os.WriteFile(named, []byte("sk-ant-oat01-named\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(OperatorTokenFileEnvVar, named)
	if got := OperatorToken(); got != "sk-ant-oat01-named" {
		t.Fatalf("%s: got %q", OperatorTokenFileEnvVar, got)
	}

	t.Setenv(OperatorTokenEnvVar, "  sk-ant-oat01-env  ")
	if got := OperatorToken(); got != "sk-ant-oat01-env" {
		t.Fatalf("env must win over the file: got %q", got)
	}
}
