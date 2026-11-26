package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCheckConfigWithForgejoKey is writeCheckConfig plus a forgejo.integration
// block whose key_file is keyFile.
func writeCheckConfigWithForgejoKey(t *testing.T, keyFile string) string {
	t.Helper()
	path := writeCheckConfig(t, nil)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(`
forgejo:
  forgejo_url: https://forgejo.example.com
  integration:
    issuer: https://keys.example.com
    key_file: `+keyFile+`
    audiences:
      advisor: "aud-advisor"
`)...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunConfigCheckForgejoKeyFile pins hivecommons/hive#19919: `hive
// validate` fails on a forgejo.integration.key_file that does not exist, and
// passes on a readable RSA key, instead of printing "config OK" and leaving
// the first clone or push to find out.
func TestRunConfigCheckForgejoKeyFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.pem")
	var stdout, stderr bytes.Buffer
	if code := runConfigCheck([]string{"-config", writeCheckConfigWithForgejoKey(t, missing)}, &stdout, &stderr); code != configCheckExitCode {
		t.Fatalf("missing key_file: runConfigCheck() = %d, want %d\nstdout: %s", code, configCheckExitCode, stdout.String())
	}
	if strings.Contains(stdout.String(), "config OK") {
		t.Errorf("missing key_file printed config OK:\n%s", stdout.String())
	}
	if msg := stderr.String(); !strings.Contains(msg, "forgejo.integration.key_file") || !strings.Contains(msg, missing) {
		t.Errorf("stderr does not name the key_file and its path:\n%s", msg)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "forgejo-jwt.pem")
	if err := os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runConfigCheck([]string{"-config", writeCheckConfigWithForgejoKey(t, good)}, &stdout, &stderr); code != 0 {
		t.Fatalf("readable key_file: runConfigCheck() = %d, want 0\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Errorf("readable key_file did not print config OK:\n%s", stdout.String())
	}
}

// TestRunConfigCheckForgejoPreviousKeyFiles: `hive validate` also checks
// each previous_key_files entry of a served key list (#19996), because the
// served list leaves out a key it cannot read.
func TestRunConfigCheckForgejoPreviousKeyFiles(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "forgejo-jwt.pem")
	if err := os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "old.pem")
	path := writeCheckConfigWithForgejoKey(t, good)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("    serve_key_list: true\n    previous_key_files:\n      - "+missing+"\n")...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runConfigCheck([]string{"-config", path}, &stdout, &stderr); code != configCheckExitCode {
		t.Fatalf("missing previous key: runConfigCheck() = %d, want %d\nstdout: %s\nstderr: %s", code, configCheckExitCode, stdout.String(), stderr.String())
	}
	if msg := stderr.String(); !strings.Contains(msg, "forgejo.integration.previous_key_files[0]") || !strings.Contains(msg, missing) {
		t.Errorf("stderr does not name the previous key and its path:\n%s", msg)
	}
}
