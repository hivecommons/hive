package keystore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testKeyA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testKeyB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func newTestProvider(t *testing.T, current string, previous ...string) *EnvKeyProvider {
	t.Helper()
	var prev [][]byte
	for _, p := range previous {
		prev = append(prev, []byte(p))
	}
	p, err := NewEnvKeyProvider([]byte(current), prev...)
	if err != nil {
		t.Fatalf("NewEnvKeyProvider: %v", err)
	}
	return p
}

// useProvider installs p as the active provider for the test and restores the
// package to its uninitialised state afterwards.
func useProvider(t *testing.T, p Provider) {
	t.Helper()
	ResetForTest()
	SetActive(p)
	t.Cleanup(ResetForTest)
}

func TestEnvKeyProviderRoundTripAndKeyID(t *testing.T) {
	p := newTestProvider(t, testKeyA)
	if len(p.KeyID()) != keyIDBytes*2 {
		t.Fatalf("KeyID %q: want %d hex chars", p.KeyID(), keyIDBytes*2)
	}
	again := newTestProvider(t, testKeyA)
	if again.KeyID() != p.KeyID() {
		t.Fatalf("same material derived different key ids: %s vs %s", p.KeyID(), again.KeyID())
	}
	if newTestProvider(t, testKeyB).KeyID() == p.KeyID() {
		t.Fatal("different material derived the same key id")
	}

	ctx := context.Background()
	ct, err := p.Encrypt(ctx, []byte("sk-secret"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ct), "sk-secret") {
		t.Fatal("ciphertext contains the plaintext")
	}
	pt, err := p.Decrypt(ctx, p.KeyID(), ct, []byte("aad"))
	if err != nil || string(pt) != "sk-secret" {
		t.Fatalf("Decrypt = %q, %v", pt, err)
	}
	ct2, _ := p.Encrypt(ctx, []byte("sk-secret"), []byte("aad"))
	if string(ct2) == string(ct) {
		t.Fatal("two seals of the same plaintext produced identical ciphertext (nonce reuse)")
	}
}

func TestEnvKeyProviderAADBindingAndTamper(t *testing.T) {
	p := newTestProvider(t, testKeyA)
	ctx := context.Background()
	ct, err := p.Encrypt(ctx, []byte("value"), []byte("file:a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Decrypt(ctx, p.KeyID(), ct, []byte("file:b")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong aad: err = %v, want ErrAuthentication", err)
	}
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := p.Decrypt(ctx, p.KeyID(), tampered, []byte("file:a")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("tampered: err = %v, want ErrAuthentication", err)
	}
	if _, err := p.Decrypt(ctx, p.KeyID(), ct[:5], []byte("file:a")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("truncated: err = %v, want ErrAuthentication", err)
	}
	if _, err := p.Decrypt(ctx, "deadbeef", ct, []byte("file:a")); !errors.Is(err, ErrUnknownKeyGeneration) {
		t.Fatalf("unknown generation: err = %v, want ErrUnknownKeyGeneration", err)
	}
	if !strings.Contains(err2str(err2(p.Decrypt(ctx, "deadbeef", ct, nil))), EnvSecretsKeyPrevious) {
		t.Fatal("unknown-generation error should name the previous-key variable")
	}
}

func err2(_ []byte, err error) error { return err }
func err2str(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestEnvKeyProviderRejectsShortMaterial(t *testing.T) {
	if _, err := NewEnvKeyProvider([]byte("short")); !errors.Is(err, ErrKeyMaterialTooShort) {
		t.Fatalf("err = %v, want ErrKeyMaterialTooShort", err)
	}
	if _, err := NewEnvKeyProvider([]byte(testKeyA), []byte("short")); !errors.Is(err, ErrKeyMaterialTooShort) {
		t.Fatalf("short previous: err = %v, want ErrKeyMaterialTooShort", err)
	}
	if _, err := NewEnvKeyProvider([]byte(testKeyA), []byte(""), []byte("  ")); err != nil {
		t.Fatalf("empty previous entries should be skipped: %v", err)
	}
}

func TestEnvelopeFormat(t *testing.T) {
	p := newTestProvider(t, testKeyA)
	ctx := context.Background()
	env, err := Seal(ctx, p, []byte("token"), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env, Prefix+Version+":env:"+p.KeyID()+":") {
		t.Fatalf("envelope %q lacks the expected header", env)
	}
	if strings.ContainsAny(env, " \n\t") {
		t.Fatalf("envelope %q is not a single printable token", env)
	}
	if !IsEnvelopeString(env) || IsEnvelopeString("ghp_plain") || !IsEnvelope([]byte("  "+env+"\n")) {
		t.Fatal("IsEnvelope disagrees with Seal output")
	}
	pt, stale, err := Open(ctx, p, env, []byte("x"))
	if err != nil || stale || string(pt) != "token" {
		t.Fatalf("Open = %q, stale=%v, %v", pt, stale, err)
	}
	if _, _, err := Open(ctx, p, "ghp_plain", nil); !errors.Is(err, ErrNotEnvelope) {
		t.Fatalf("plaintext: err = %v, want ErrNotEnvelope", err)
	}
	for _, bad := range []string{Prefix, Prefix + "v1:env:abcd", Prefix + "v9:env:abcd:QUJD", Prefix + "v1::abcd:QUJD", Prefix + "v1:env:abcd:not base64!"} {
		if _, err := ParseEnvelope(bad); !errors.Is(err, ErrMalformedEnvelope) {
			t.Fatalf("ParseEnvelope(%q) = %v, want ErrMalformedEnvelope", bad, err)
		}
	}
	if _, _, err := Open(ctx, nil, env, nil); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("nil provider: err = %v, want ErrNoProvider", err)
	}
	if _, err := Seal(ctx, nil, []byte("x"), nil); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("Seal nil provider: err = %v", err)
	}
	other := &renamedProvider{p}
	if _, _, err := Open(ctx, other, env, []byte("x")); !errors.Is(err, ErrProviderMismatch) {
		t.Fatalf("provider mismatch: err = %v", err)
	}
}

// renamedProvider wraps a provider under another name.
type renamedProvider struct{ Provider }

func (renamedProvider) Name() string { return "other" }

func TestRotationAcrossGenerations(t *testing.T) {
	ctx := context.Background()
	gen1 := newTestProvider(t, testKeyA)
	env, err := Seal(ctx, gen1, []byte("rotate-me"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	// New current key, old one retained for decryption.
	gen2 := newTestProvider(t, testKeyB, testKeyA)
	if gen2.KeyID() == gen1.KeyID() {
		t.Fatal("rotation did not change the key id")
	}
	if got := gen2.Generations(); len(got) != 2 || got[0] != gen2.KeyID() {
		t.Fatalf("Generations = %v", got)
	}
	pt, stale, err := Open(ctx, gen2, env, []byte("aad"))
	if err != nil || !stale || string(pt) != "rotate-me" {
		t.Fatalf("Open under gen2 = %q, stale=%v, %v (want stale=true)", pt, stale, err)
	}
	// Without the old material the envelope is unreadable, and says why.
	gen2only := newTestProvider(t, testKeyB)
	if _, _, err := Open(ctx, gen2only, env, []byte("aad")); !errors.Is(err, ErrUnknownKeyGeneration) {
		t.Fatalf("Open without previous key = %v, want ErrUnknownKeyGeneration", err)
	}
}

func TestSecretFileRoundTripNoPlaintextOnDisk(t *testing.T) {
	useProvider(t, newTestProvider(t, testKeyA))
	dir := t.TempDir()
	defer SetRewriteRootsForTest(dir)()
	path := filepath.Join(dir, "secrets", "litellm_api_key")
	ctx := context.Background()
	// The caller owns the directory: a missing one is an error, not created.
	if err := WriteFile(ctx, path, []byte("sk-live-1234")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write into a missing dir: err = %v, want ErrNotExist", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(ctx, path, []byte("sk-live-1234")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-live") || !IsEnvelope(raw) {
		t.Fatalf("file holds plaintext or no envelope: %q", raw)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != DefaultFileMode {
		t.Fatalf("mode = %o, want %o", fi.Mode().Perm(), DefaultFileMode)
	}
	got, err := ReadString(ctx, path)
	if err != nil || got != "sk-live-1234" {
		t.Fatalf("ReadString = %q, %v", got, err)
	}
	// A ciphertext moved to another file name does not open (AAD binding).
	moved := filepath.Join(dir, "secrets", "bob_api_key")
	if err := os.WriteFile(moved, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(ctx, moved); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("moved ciphertext: err = %v, want ErrAuthentication", err)
	}
	// No stray temp files.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if _, err := ReadFile(ctx, filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: err = %v, want ErrNotExist", err)
	}
}

func TestSecretFileLegacyReadRewritesInPlace(t *testing.T) {
	useProvider(t, newTestProvider(t, testKeyA))
	dir := t.TempDir()
	defer SetRewriteRootsForTest(dir)()
	path := filepath.Join(dir, "github-owner-token")
	if err := os.WriteFile(path, []byte(`{"login":"x","token":"ghp_y"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := ReadFile(ctx, path)
	if err != nil || string(got) != `{"login":"x","token":"ghp_y"}` {
		t.Fatalf("legacy read = %q, %v", got, err)
	}
	raw, _ := os.ReadFile(path)
	if !IsEnvelope(raw) {
		t.Fatalf("legacy file was not re-sealed: %q", raw)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Fatalf("re-seal changed the mode to %o", fi.Mode().Perm())
	}
	again, err := ReadFile(ctx, path)
	if err != nil || string(again) != string(got) {
		t.Fatalf("second read = %q, %v", again, err)
	}
}

func TestSecretFileOutsideRewriteRootsIsLeftAlone(t *testing.T) {
	useProvider(t, newTestProvider(t, testKeyA))
	defer SetRewriteRootsForTest("/nonexistent-root")()
	path := filepath.Join(t.TempDir(), "mounted_key")
	if err := os.WriteFile(path, []byte("from-k8s-secret\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := ReadString(context.Background(), path)
	if err != nil || got != "from-k8s-secret" {
		t.Fatalf("ReadString = %q, %v", got, err)
	}
	raw, _ := os.ReadFile(path)
	if IsEnvelope(raw) {
		t.Fatal("a file outside the rewrite roots was rewritten")
	}
}

func TestSecretFileRotationRewritesStaleGeneration(t *testing.T) {
	dir := t.TempDir()
	defer SetRewriteRootsForTest(dir)()
	path := filepath.Join(dir, "backup_encryption_key")
	ctx := context.Background()
	useProvider(t, newTestProvider(t, testKeyA))
	if err := WriteFile(ctx, path, []byte("hexkey")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	gen2 := newTestProvider(t, testKeyB, testKeyA)
	SetActive(gen2)
	n, err := MigrateDir(ctx, dir)
	if err != nil || n != 1 {
		t.Fatalf("MigrateDir = %d, %v", n, err)
	}
	after, _ := os.ReadFile(path)
	env, err := ParseEnvelope(string(after))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == string(before) || env.KeyID != gen2.KeyID() {
		t.Fatalf("stale file not re-sealed under the new generation: key %s, want %s", env.KeyID, gen2.KeyID())
	}
	// Now only the new key: still readable, no rewrite needed.
	SetActive(newTestProvider(t, testKeyB))
	got, err := ReadString(ctx, path)
	if err != nil || got != "hexkey" {
		t.Fatalf("after rotation = %q, %v", got, err)
	}
	if n, err := MigrateDir(ctx, filepath.Join(dir, "absent")); n != 0 || err != nil {
		t.Fatalf("MigrateDir on a missing dir = %d, %v", n, err)
	}
}

func TestSecretFileLegacyModeWritesPlaintext(t *testing.T) {
	useProvider(t, nil)
	dir := t.TempDir()
	defer SetRewriteRootsForTest(dir)()
	path := filepath.Join(dir, "k")
	ctx := context.Background()
	if err := WriteFile(ctx, path, []byte("plain")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "plain" {
		t.Fatalf("legacy mode wrote %q", raw)
	}
	if got, err := ReadString(ctx, path); err != nil || got != "plain" {
		t.Fatalf("ReadString = %q, %v", got, err)
	}
	if n, err := MigrateDir(ctx, dir); n != 0 || err != nil {
		t.Fatalf("MigrateDir without a provider = %d, %v (want a no-op)", n, err)
	}
	// An envelope on disk with no provider is an error, not plaintext.
	sealed, _ := Seal(ctx, newTestProvider(t, testKeyA), []byte("v"), FileAAD(path))
	if err := os.WriteFile(path, []byte(sealed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(ctx, path); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("envelope without provider: err = %v, want ErrNoProvider", err)
	}
}

func TestSecretFileTooLarge(t *testing.T) {
	useProvider(t, nil)
	path := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(path, make([]byte, MaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(context.Background(), path); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
}

func TestSealActiveOpenActive(t *testing.T) {
	ctx := context.Background()
	useProvider(t, nil)
	if v, err := SealActive(ctx, "ghp_x", []byte("a")); err != nil || v != "ghp_x" {
		t.Fatalf("legacy SealActive = %q, %v", v, err)
	}
	if v, legacy, _, err := OpenActive(ctx, "ghp_x", []byte("a")); err != nil || !legacy || v != "ghp_x" {
		t.Fatalf("legacy OpenActive = %q, legacy=%v, %v", v, legacy, err)
	}
	SetActive(newTestProvider(t, testKeyA))
	sealed, err := SealActive(ctx, "ghp_x", []byte("a"))
	if err != nil || !IsEnvelopeString(sealed) {
		t.Fatalf("SealActive = %q, %v", sealed, err)
	}
	if twice, _ := SealActive(ctx, sealed, []byte("a")); twice != sealed {
		t.Fatal("SealActive re-sealed an envelope")
	}
	if empty, _ := SealActive(ctx, "", []byte("a")); empty != "" {
		t.Fatal("SealActive sealed an empty value")
	}
	v, legacy, stale, err := OpenActive(ctx, sealed, []byte("a"))
	if err != nil || legacy || stale || v != "ghp_x" {
		t.Fatalf("OpenActive = %q, legacy=%v, stale=%v, %v", v, legacy, stale, err)
	}
	if _, _, _, err := OpenActive(ctx, sealed, []byte("b")); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong aad: %v", err)
	}
}
