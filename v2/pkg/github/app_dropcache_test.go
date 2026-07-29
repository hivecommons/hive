package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// dropCacheTestKeyBits matches the smallest size GitHub Apps use. This key never
// signs anything real.
const dropCacheTestKeyBits = 2048

func newTestAppAuth(t *testing.T, cachePath string) *AppAuth {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, dropCacheTestKeyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "key.pem")
	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
	if err := os.WriteFile(keyFile, pemData, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	auth, err := NewAppAuthWithCache(1234, 5678, keyFile, cachePath, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	if err != nil {
		t.Fatalf("NewAppAuthWithCache: %v", err)
	}
	return auth
}

// TestDropCachedToken covers the invalidation that has to happen when the App
// private key is replaced. A token was minted by presenting a JWT signed with
// the OLD key; once the key changes that token is dead. Rebuilding the AppAuth in
// memory is not sufficient, because agent processes read the token straight out
// of the on-disk cache — a stale file there keeps handing out a dead credential
// until it expires on its own.
func TestDropCachedToken(t *testing.T) {
	t.Run("clears in-memory token and removes the cache file", func(t *testing.T) {
		cachePath := filepath.Join(t.TempDir(), "gh-app-token.cache")
		if err := os.WriteFile(cachePath, []byte("ghs_stale_token_minted_under_the_old_key"), 0o640); err != nil {
			t.Fatal(err)
		}
		auth := newTestAppAuth(t, cachePath)
		auth.cachedToken = "ghs_stale_token_minted_under_the_old_key"
		auth.tokenExpiry = time.Now().Add(time.Hour)

		auth.DropCachedToken()

		if auth.cachedToken != "" {
			t.Errorf("cachedToken = %q, want empty", auth.cachedToken)
		}
		if !auth.tokenExpiry.IsZero() {
			t.Errorf("tokenExpiry = %v, want zero", auth.tokenExpiry)
		}
		if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
			t.Errorf("cache file still present (err=%v); agents would keep reading the dead token", err)
		}
	})

	t.Run("a missing cache file is not an error", func(t *testing.T) {
		cachePath := filepath.Join(t.TempDir(), "absent.cache")
		auth := newTestAppAuth(t, cachePath)
		auth.DropCachedToken() // must not panic or fail
		if auth.cachedToken != "" {
			t.Error("cachedToken should be empty")
		}
	})

	t.Run("no cache path configured is a no-op on disk", func(t *testing.T) {
		auth := newTestAppAuth(t, "")
		auth.cachedToken = "x"
		auth.DropCachedToken()
		if auth.cachedToken != "" {
			t.Error("cachedToken should be cleared even with no cache path")
		}
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var auth *AppAuth
		auth.DropCachedToken() // the spoke may not have App auth built yet
	})
}
