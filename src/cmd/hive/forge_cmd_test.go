package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeForgeKey(t *testing.T, mode os.FileMode) (path, pemBody string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	path = filepath.Join(t.TempDir(), "jwt-signing.pem")
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path, strings.Split(string(data), "\n")[1]
}

func TestForgeKeyListWritesBothFiles(t *testing.T) {
	oldKey, oldLine := writeForgeKey(t, 0o600)
	newKey, newLine := writeForgeKey(t, 0o400)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runForge([]string{"key-list", "--issuer", "https://hive-keys.example.org", "--key", oldKey, "--key", newKey, "--out", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	readForgeJSON(t, filepath.Join(out, ".well-known", "openid-configuration"), &discovery)
	if discovery.Issuer != "https://hive-keys.example.org" || discovery.JWKSURI != "https://hive-keys.example.org/jwks.json" {
		t.Errorf("discovery = %+v", discovery)
	}
	var jwks struct {
		Keys []map[string]string `json:"keys"`
	}
	readForgeJSON(t, filepath.Join(out, "jwks.json"), &jwks)
	if len(jwks.Keys) != 2 {
		t.Fatalf("key list has %d keys, want 2", len(jwks.Keys))
	}
	for _, k := range jwks.Keys {
		if _, private := k["d"]; private {
			t.Error("key list carries a private exponent")
		}
	}
	printed := stdout.String() + stderr.String()
	for _, line := range []string{oldLine, newLine} {
		if strings.Contains(printed, line) {
			t.Error("output carries private key material")
		}
	}
}

func TestForgeKeyListRefusesReadableKey(t *testing.T) {
	key, line := writeForgeKey(t, 0o644)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := runForge([]string{"key-list", "--issuer", "https://hive-keys.example.org", "--key", key, "--out", out}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "readable beyond its owner") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), line) {
		t.Error("output carries private key material")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("wrote %d entries for a refused key", len(entries))
	}
}

func TestForgeKeyListUsage(t *testing.T) {
	key, _ := writeForgeKey(t, 0o600)
	for _, args := range [][]string{
		nil,
		{"mint"},
		{"key-list", "--issuer", "https://hive-keys.example.org"},
		{"key-list", "--issuer", "http://hive-keys.example.org", "--key", key},
	} {
		var stdout, stderr bytes.Buffer
		if code := runForge(args, &stdout, &stderr); code != 2 {
			t.Errorf("runForge(%q) = %d, want 2", args, code)
		}
	}
}

func readForgeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
