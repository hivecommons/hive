package keystore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The facade re-exports crux/keystore; this pins that the names resolve and
// behave as one package end to end.
func TestFacadeRoundTrip(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	p, err := NewEnvKeyProvider([]byte(strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	SetActive(p)
	if Active() == nil || Active().Name() != ProviderEnv {
		t.Fatal("SetActive did not install the provider")
	}
	dir := t.TempDir()
	defer SetRewriteRootsForTest(dir)()
	path := filepath.Join(dir, "k")
	ctx := context.Background()
	if err := WriteFile(ctx, path, []byte("v")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadString(ctx, path)
	if err != nil || got != "v" {
		t.Fatalf("ReadString = %q, %v", got, err)
	}
	sealed, err := SealActive(ctx, "x", FileAAD(path))
	if err != nil || !IsEnvelopeString(sealed) || !strings.HasPrefix(sealed, Prefix) {
		t.Fatalf("SealActive = %q, %v", sealed, err)
	}
	if _, err := ParseEnvelope(sealed); err != nil {
		t.Fatal(err)
	}
	if !Rewritable(path) || len(Registered()) < 2 {
		t.Fatal("facade helpers disagree with crux/keystore")
	}
}
