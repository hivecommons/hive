package watsonx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestMinter builds a minter pointed at a fake IAM server, with a
// controllable clock. Returns the minter and a pointer to the current fake time
// the caller can advance.
func newTestMinter(endpoint string) (*TokenMinter, *int64) {
	var nowNanos int64 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	m := &TokenMinter{
		cache:    make(map[string]iamToken),
		endpoint: endpoint,
		now:      func() time.Time { return time.Unix(0, atomic.LoadInt64(&nowNanos)) },
		client:   &http.Client{Timeout: iamTokenRequestTimeout},
	}
	return m, &nowNanos
}

// Success: the exchange sends the apikey grant and returns the access_token as
// the bearer. The API key never appears in the returned value.
func TestTokenMintSuccess(t *testing.T) {
	var gotGrant, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotKey = r.Form.Get("apikey")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "IAM-JWT-abc",
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	m, _ := newTestMinter(srv.URL)
	tok, err := m.Token(context.Background(), "my-ibm-key")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "IAM-JWT-abc" {
		t.Fatalf("token = %q, want the minted access_token", tok)
	}
	if gotGrant != iamAPIKeyGrantType {
		t.Fatalf("grant_type = %q, want %q", gotGrant, iamAPIKeyGrantType)
	}
	if gotKey != "my-ibm-key" {
		t.Fatalf("apikey sent = %q, want the api key", gotKey)
	}
	if strings.Contains(tok, "my-ibm-key") {
		t.Fatal("returned bearer must never contain the raw API key")
	}
}

// Caching: a second Token call within the token's lifetime reuses the cached
// bearer and does NOT hit the IAM server again.
func TestTokenMintCaches(t *testing.T) {
	var mints int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&mints, 1)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": fmt.Sprintf("IAM-JWT-%d", atomic.LoadInt32(&mints)),
			"expires_in":   3600,
		})
	}))
	defer srv.Close()

	m, _ := newTestMinter(srv.URL)
	first, err := m.Token(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Token(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("cached call returned a different token (%q vs %q)", first, second)
	}
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Fatalf("expected exactly 1 mint (second served from cache), got %d", got)
	}
}

// Expiry re-mint: once the cached token nears expiry (clock advanced past
// lifetime minus skew), the next call mints a fresh token.
func TestTokenMintReMintsOnExpiry(t *testing.T) {
	var mints int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&mints, 1)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": fmt.Sprintf("IAM-JWT-%d", n),
			"expires_in":   3600, // 1h
		})
	}))
	defer srv.Close()

	m, nowNanos := newTestMinter(srv.URL)
	first, err := m.Token(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	// Advance past (lifetime - skew) so the cached token is treated as expired.
	atomic.AddInt64(nowNanos, int64(time.Hour-iamTokenExpirySkew+time.Minute))
	second, err := m.Token(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected a fresh token after expiry, got the same cached one")
	}
	if got := atomic.LoadInt32(&mints); got != 2 {
		t.Fatalf("expected 2 mints (initial + re-mint), got %d", got)
	}
}

// A blank API key is rejected without an HTTP call (a watsonx gateway must have
// a key).
func TestTokenMintRejectsBlankKey(t *testing.T) {
	m, _ := newTestMinter("http://unused.invalid")
	if _, err := m.Token(context.Background(), "   "); err == nil {
		t.Fatal("blank api key must error")
	}
}

// An IAM error response surfaces as an error (with the status), and the error
// text never contains the API key.
func TestTokenMintUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorMessage":"Provided API key could not be found."}`))
	}))
	defer srv.Close()

	m, _ := newTestMinter(srv.URL)
	_, err := m.Token(context.Background(), "secret-key-XYZ")
	if err == nil {
		t.Fatal("expected an error on IAM 400")
	}
	if strings.Contains(err.Error(), "secret-key-XYZ") {
		t.Fatalf("error text leaked the API key: %v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error should carry the status: %v", err)
	}
}
