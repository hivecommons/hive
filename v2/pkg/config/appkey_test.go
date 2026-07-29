package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testRSAKeyBits is deliberately the smallest size Go will generate quickly.
// These keys are never used for real signing — only to exercise fingerprinting.
const testRSAKeyBits = 2048

func mustRSAPEMs(t *testing.T) (pkcs1 string, pkcs8 string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, testRSAKeyBits)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	p1 := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	p8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	return string(p1), string(p8)
}

// TestAppKeyFingerprintStableAcrossEncodings is the property the whole
// self-correction loop rests on: the SAME key must fingerprint identically no
// matter how it was encoded or whitespace-mangled. If it did not, the hub would
// see a permanent mismatch and re-push a credential on every heartbeat forever.
func TestAppKeyFingerprintStableAcrossEncodings(t *testing.T) {
	pkcs1, pkcs8 := mustRSAPEMs(t)

	base, err := AppKeyFingerprint(pkcs1)
	if err != nil {
		t.Fatalf("fingerprint pkcs1: %v", err)
	}

	tests := []struct {
		name string
		in   string
	}{
		{"pkcs1 identical", pkcs1},
		{"pkcs8 same key", pkcs8},
		{"leading and trailing whitespace", "\n\t " + pkcs1 + "\n\n"},
		{"no trailing newline", strings.TrimRight(pkcs1, "\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AppKeyFingerprint(tc.in)
			if err != nil {
				t.Fatalf("AppKeyFingerprint: %v", err)
			}
			if got != base {
				t.Errorf("fingerprint = %q, want %q (same key, different encoding)", got, base)
			}
		})
	}
}

func TestAppKeyFingerprintDistinguishesKeys(t *testing.T) {
	a, _ := mustRSAPEMs(t)
	b, _ := mustRSAPEMs(t)

	fpA, err := AppKeyFingerprint(a)
	if err != nil {
		t.Fatalf("fingerprint a: %v", err)
	}
	fpB, err := AppKeyFingerprint(b)
	if err != nil {
		t.Fatalf("fingerprint b: %v", err)
	}
	if fpA == fpB {
		t.Fatal("two distinct keys produced the same fingerprint")
	}
}

// TestAppKeyFingerprintLeaksNoKeyMaterial guards the security property directly:
// nothing recognizable from the private key may appear in its fingerprint.
func TestAppKeyFingerprintLeaksNoKeyMaterial(t *testing.T) {
	pkcs1, _ := mustRSAPEMs(t)
	fp, err := AppKeyFingerprint(pkcs1)
	if err != nil {
		t.Fatalf("AppKeyFingerprint: %v", err)
	}
	if strings.Contains(fp, "BEGIN") || strings.Contains(fp, "PRIVATE") {
		t.Fatalf("fingerprint %q looks like it contains PEM material", fp)
	}
	// Every base64 body line of the PEM must be absent from the fingerprint.
	for _, line := range strings.Split(pkcs1, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 20 || strings.HasPrefix(line, "-----") {
			continue
		}
		if strings.Contains(fp, line) {
			t.Fatalf("fingerprint contains a line of the private key")
		}
	}
	if !strings.HasPrefix(fp, "sha256:") {
		t.Errorf("fingerprint %q missing scheme prefix", fp)
	}
	if len(fp) != len("sha256:")+AppKeyFingerprintLen {
		t.Errorf("fingerprint %q has unexpected length %d", fp, len(fp))
	}
}

func TestAppKeyFingerprintErrors(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr error // nil means "any error"
		wantAny bool
	}{
		{name: "empty", in: "", wantErr: ErrNoAppKey},
		{name: "whitespace only", in: "   \n\t ", wantErr: ErrNoAppKey},
		{name: "not pem", in: "hello world", wantAny: true},
		{
			name:    "pem envelope with garbage body",
			in:      "-----BEGIN RSA PRIVATE KEY-----\nZ m9v\n-----END RSA PRIVATE KEY-----\n",
			wantAny: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AppKeyFingerprint(tc.in)
			if err == nil {
				t.Fatalf("expected an error, got fingerprint %q", got)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
			if got != "" {
				t.Errorf("expected empty fingerprint on error, got %q", got)
			}
		})
	}
}

func TestAppKeyFingerprintECKey(t *testing.T) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatalf("marshal ec: %v", err)
	}
	sec1 := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))

	fp, err := AppKeyFingerprint(sec1)
	if err != nil {
		t.Fatalf("AppKeyFingerprint(EC): %v", err)
	}
	if !strings.HasPrefix(fp, "sha256:") {
		t.Errorf("EC fingerprint %q missing prefix", fp)
	}
}

func TestAppKeyFingerprintFromFile(t *testing.T) {
	dir := t.TempDir()
	pkcs1, _ := mustRSAPEMs(t)

	good := filepath.Join(dir, "good.pem")
	if err := os.WriteFile(good, []byte(pkcs1), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wantGood, err := AppKeyFingerprint(pkcs1)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr error
	}{
		{name: "existing key", path: good, want: wantGood},
		{name: "missing file", path: filepath.Join(dir, "nope.pem"), wantErr: ErrNoAppKey},
		{name: "empty path", path: "", wantErr: ErrNoAppKey},
		// The four live hives with NO key have exactly this: a zero-byte file.
		{name: "empty file", path: empty, wantErr: ErrNoAppKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AppKeyFingerprintFromFile(tc.path)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				if got != "" {
					t.Errorf("got %q, want empty on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
