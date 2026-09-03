package hub

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hivecommons/hive/pkg/auth"
)

// fakeOIDCProvider is a minimal OIDC server (discovery + JWKS + token endpoint)
// returning one signed id_token, used to drive the hub's OIDC callback end to end.
type fakeOIDCProvider struct {
	srv     *httptest.Server
	key     *rsa.PrivateKey
	kid     string
	issuer  string
	idToken string
}

func newFakeOIDCProvider(t *testing.T) *fakeOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDCProvider{key: key, kid: "hub-test-kid"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.issuer,
			"authorization_endpoint": f.issuer + "/authorize",
			"token_endpoint":         f.issuer + "/token",
			"jwks_uri":               f.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := key.Public().(*rsa.PublicKey)
		n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{"kty": "RSA", "kid": f.kid, "use": "sig", "alg": "RS256", "n": n, "e": e}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"id_token": f.idToken})
	})
	f.srv = httptest.NewServer(mux)
	f.issuer = f.srv.URL
	return f
}

func (f *fakeOIDCProvider) close() { f.srv.Close() }

func (f *fakeOIDCProvider) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestOIDCCallback_CreatesCanonicalUser drives a full Google login through the
// hub callback: the id_token verifies, a canonical google:<sub> user is created
// with provider/avatar/email stamped, and the Ed25519 session cookie is minted
// over the canonical id (v4 mints ONLY the asymmetric format —
// mintHubUserCookieValueV2 — so a spoke can verify but never forge it).
func TestOIDCCallback_CreatesCanonicalUser(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	f := newFakeOIDCProvider(t)
	defer f.close()

	const clientID = "hub-oidc-client"
	const nonce = "oidc-nonce-123"
	const sub = "108877665544332211"
	f.idToken = f.sign(t, jwt.MapClaims{
		"iss":     f.issuer,
		"aud":     clientID,
		"sub":     sub,
		"exp":     time.Now().Add(time.Hour).Unix(),
		"iat":     time.Now().Unix(),
		"nonce":   nonce,
		"email":   "person@example.com",
		"name":    "A Person",
		"picture": "https://example.com/p.png",
	})

	// A hub with a real secret (mints signed cookies) and a registry carrying our
	// fake Google provider.
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	s.authProviders = auth.NewRegistry(&auth.Provider{
		Name:        "google",
		DisplayName: "Google",
		IsOIDC:      true,
		Issuer:      f.issuer,
		ClientID:    clientID,
		Scopes:      []string{"openid", "email", "profile"},
	})

	// state = nonce : google : /dashboard, with matching state + OIDC nonce cookies.
	stateNonce, _ := oauthStateNonce()
	state := url.QueryEscape(stateNonce + oauthStateSeparator + "google" + oauthStateSeparator + "/dashboard")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=xyz&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: stateNonce})
	req.AddCookie(&http.Cookie{Name: oidcNonceCookieName, Value: nonce})

	rec := httptest.NewRecorder()
	s.handleOAuthCallback(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/dashboard" {
		t.Errorf("redirect = %q, want /dashboard", loc)
	}

	// A canonical google user must exist with the provider fields stamped.
	u := loadSaaSUser("google:" + sub)
	if u == nil {
		t.Fatalf("expected google:%s user to be created", sub)
	}
	if u.Provider != "google" {
		t.Errorf("Provider = %q, want google", u.Provider)
	}
	if u.CanonicalID != "google:"+sub {
		t.Errorf("CanonicalID = %q, want google:%s", u.CanonicalID, sub)
	}
	if u.AvatarURL != "https://example.com/p.png" {
		t.Errorf("AvatarURL = %q", u.AvatarURL)
	}
	if u.Email != "person@example.com" {
		t.Errorf("Email = %q", u.Email)
	}
	if u.EncryptedToken != "" {
		t.Error("a Google user must NOT carry a GitHub EncryptedToken")
	}

	// The session cookie is minted over the canonical id and verifies through
	// the hub's own verifier.
	//
	// AUDIT F10 — updated from hubCookieIsV2 to hubCookieIsV3, a tightening
	// rather than a relaxation: v3 is Ed25519-signed like v2 and additionally
	// carries a signed expiry and a revocable session id. See the same change in
	// oauth_provision_coverage_test.go.
	var gotSession bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.Value != "" {
			gotSession = true
			if !hubCookieIsV3(c.Value) {
				t.Errorf("hive_hub_user is not the Ed25519 v3 format: %q", c.Value)
			}
			if user, ok := s.verifyHubUserCookie(c.Value); !ok || user != "google:"+sub {
				t.Errorf("hive_hub_user verifies to %q (ok=%v), want google:%s", user, ok, sub)
			}
		}
	}
	if !gotSession {
		t.Error("expected an hive_hub_user session cookie to be minted")
	}
}

// TestOIDCCallback_RejectsBadNonce confirms the hub callback fails closed when
// the id_token's nonce does not match the browser's OIDC nonce cookie (replay /
// injection), minting NO session.
func TestOIDCCallback_RejectsBadNonce(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	f := newFakeOIDCProvider(t)
	defer f.close()

	const clientID = "hub-oidc-client"
	f.idToken = f.sign(t, jwt.MapClaims{
		"iss": f.issuer, "aud": clientID, "sub": "s1",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": "the-real-nonce",
	})

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	s.authProviders = auth.NewRegistry(&auth.Provider{
		Name: "google", DisplayName: "Google", IsOIDC: true,
		Issuer: f.issuer, ClientID: clientID, Scopes: []string{"openid"},
	})

	stateNonce, _ := oauthStateNonce()
	state := url.QueryEscape(stateNonce + oauthStateSeparator + "google" + oauthStateSeparator + "/dashboard")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=xyz&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: stateNonce})
	// The browser's cookie carries a DIFFERENT nonce than the token.
	req.AddCookie(&http.Cookie{Name: oidcNonceCookieName, Value: "a-different-nonce"})

	rec := httptest.NewRecorder()
	s.handleOAuthCallback(rec, req)

	if rec.Code == http.StatusTemporaryRedirect {
		t.Fatal("callback must not succeed on nonce mismatch")
	}
	if loadSaaSUser("google:s1") != nil {
		t.Error("no user should be created on a rejected login")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.Value != "" {
			t.Errorf("a session cookie %q was minted on a rejected login", c.Name)
		}
	}
}
