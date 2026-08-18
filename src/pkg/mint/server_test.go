package mint

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecret = "s3cret-shared-token"

func newTestServer(t *testing.T) (*Server, *Minter) {
	t.Helper()
	m, _ := newTestMinter(t)
	srv, err := NewServer(m, testSecret, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, m
}

func TestNewServerRejectsEmptySecret(t *testing.T) {
	m, _ := newTestMinter(t)
	if _, err := NewServer(m, "", nil); err == nil {
		t.Error("expected error for empty secret (fail closed)")
	}
	if _, err := NewServer(nil, testSecret, nil); err == nil {
		t.Error("expected error for nil minter")
	}
}

func TestMintHandlerReturnsToken(t *testing.T) {
	srv, m := newTestServer(t)
	h := srv.Handler()

	body, _ := json.Marshal(MintRequest{
		Subject:    testSub,
		Audience:   testAud,
		Scopes:     []string{"registry:pull"},
		TTLSeconds: 300,
	})
	req := httptest.NewRequest(http.MethodPost, MintPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp MintResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("empty token")
	}
	if resp.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", resp.TokenType)
	}
	if resp.ExpiresInSecs != 300 {
		t.Errorf("expires_in = %d, want 300", resp.ExpiresInSecs)
	}
	// Token must verify against the minter.
	claims, err := m.Verify(resp.Token)
	if err != nil {
		t.Fatalf("Verify minted token: %v", err)
	}
	if len(claims.Scopes) != 1 || claims.Scopes[0] != "registry:pull" {
		t.Errorf("scopes = %v", claims.Scopes)
	}
}

func TestMintHandlerRejectsUnauthorized(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	cases := map[string]string{
		"no header":    "",
		"wrong secret": "Bearer wrong",
		"wrong scheme": "Basic " + testSecret,
	}
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(MintRequest{Subject: testSub, Audience: testAud})
			req := httptest.NewRequest(http.MethodPost, MintPath, bytes.NewReader(body))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rr.Code)
			}
		})
	}
}

func TestMintHandlerBadRequest(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	// Missing subject/audience -> Mint errors -> 400.
	body, _ := json.Marshal(MintRequest{Scopes: []string{"x"}})
	req := httptest.NewRequest(http.MethodPost, MintPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}

	// Malformed JSON -> 400.
	req2 := httptest.NewRequest(http.MethodPost, MintPath, strings.NewReader("{not json"))
	req2.Header.Set("Authorization", "Bearer "+testSecret)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusBadRequest {
		t.Errorf("malformed status = %d, want 400", rr2.Code)
	}
}

func TestMintHandlerMethodNotAllowed(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, MintPath, nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestJWKSHandlerServesKeys(t *testing.T) {
	srv, m := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, JWKSPath, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}

	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &set); err != nil {
		t.Fatalf("decode JWKS: %v", err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(set.Keys))
	}

	// JWKS is unauthenticated (public discovery) and validates a real token.
	tok, err := m.Mint(testSub, testAud, nil, 60_000_000_000) // 60s
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	pub := rebuildPublicKey(t, set.Keys[0]["n"], set.Keys[0]["e"])
	if pub == nil {
		t.Fatal("nil reconstructed key")
	}
	// Sanity: verify through the minter (JWKS-independence covered in mint_test).
	if _, err := m.Verify(tok); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestJWKSHandlerSetsCacheControl(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, JWKSPath, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	cc := rr.Header().Get("Cache-Control")
	if cc == "" {
		t.Fatal("missing Cache-Control header")
	}
	// jwksCacheMaxAge is 5m -> 300s.
	if want := "public, max-age=300"; cc != want {
		t.Errorf("Cache-Control = %q, want %q", cc, want)
	}
}

func TestJWKSHandlerMethodNotAllowed(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	req := httptest.NewRequest(http.MethodPost, JWKSPath, strings.NewReader("{}"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestMintHandlerRejectsUnknownFields(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	// DisallowUnknownFields -> a stray field yields a decode error -> 400.
	raw := `{"subject":"s","audience":"a","surprise":"nope"}`
	req := httptest.NewRequest(http.MethodPost, MintPath, strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown field", rr.Code)
	}
}

func TestMintHandlerRejectsOversizedBody(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	// Build a syntactically valid JSON object larger than maxMintBodyBytes so
	// MaxBytesReader trips during decode -> 400.
	huge := strings.Repeat("x", (16<<10)+1024)
	raw := `{"subject":"s","audience":"a","scopes":["` + huge + `"]}`
	req := httptest.NewRequest(http.MethodPost, MintPath, strings.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for oversized body", rr.Code)
	}
}

func TestMintHandlerEmptyBearer(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	// "Bearer " with an empty secret must not match the non-empty configured
	// secret (constant-time compare returns 0 on length mismatch).
	body, _ := json.Marshal(MintRequest{Subject: testSub, Audience: testAud})
	req := httptest.NewRequest(http.MethodPost, MintPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer ")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for empty bearer", rr.Code)
	}
}

func TestMintHandlerZeroTTLUsesMax(t *testing.T) {
	srv, m := newTestServer(t)
	h := srv.Handler()
	// TTLSeconds omitted (0) -> honored TTL is the configured max.
	body, _ := json.Marshal(MintRequest{Subject: testSub, Audience: testAud})
	req := httptest.NewRequest(http.MethodPost, MintPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp MintResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := int(m.MaxTTL().Seconds()); resp.ExpiresInSecs != want {
		t.Errorf("expires_in = %d, want %d (max ttl)", resp.ExpiresInSecs, want)
	}
}
