package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// testRSAKeyBits is deliberately small: these tests only need a parseable RSA
// key, never a secure one, and 1024 keeps the suite fast.
const testRSAKeyBits = 1024

// writeTestKey writes a throwaway RSA private key at path and returns its
// fingerprint. A copy of the helper that moved to pkg/appkey with the App-key
// file logic (hivecommons/hive#5898 phase 1) — cmd/hive still has tests that
// need to plant a key on disk, and duplicating ten lines of test scaffolding
// beats exporting a testing helper from a production package.
func writeTestKey(t *testing.T, path string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, testRSAKeyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
	if err := os.WriteFile(path, pemData, appKeys.FileMode); err != nil {
		t.Fatalf("write key %s: %v", path, err)
	}
	fp, err := config.AppKeyFingerprint(string(pemData))
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fp
}
