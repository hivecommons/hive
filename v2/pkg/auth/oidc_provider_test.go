package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// oidcTestServer is a minimal OIDC provider: discovery, JWKS, and a token
// endpoint that returns a pre-seeded id_token. It signs with one RSA key.
type oidcTestServer struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	issuer string
	// discoveryIssuer, when set, is served as the discovery doc's `issuer` instead
	// of the server URL. Multi-tenant tests set this to the "{tenantid}" template
	// so ensureDiscovered's template cross-check passes (real Entra does the same).
	discoveryIssuer string
	// idToken is what the token endpoint returns for any code.
	idToken string
}

func newOIDCTestServer(t *testing.T) *oidcTestServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ots := &oidcTestServer{key: key, kid: "test-kid-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		iss := ots.issuer
		if ots.discoveryIssuer != "" {
			iss = ots.discoveryIssuer
		}
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 iss,
			"authorization_endpoint": ots.issuer + "/authorize",
			"token_endpoint":         ots.issuer + "/token",
			"jwks_uri":               ots.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := key.Public().(*rsa.PublicKey)
		n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{
				{"kty": "RSA", "kid": ots.kid, "use": "sig", "alg": "RS256", "n": n, "e": e},
			},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"id_token": ots.idToken})
	})
	ots.srv = httptest.NewServer(mux)
	ots.issuer = ots.srv.URL
	return ots
}

func (o *oidcTestServer) close() { o.srv.Close() }

// signToken signs claims as an RS256 JWT with the server's key and kid.
func (o *oidcTestServer) signToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = o.kid
	s, err := tok.SignedString(o.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// baseClaims returns a valid claim set for the given audience/nonce.
func baseClaims(issuer, aud, nonce string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     issuer,
		"aud":     aud,
		"sub":     "1078901234567890",
		"exp":     time.Now().Add(1 * time.Hour).Unix(),
		"iat":     time.Now().Unix(),
		"nonce":   nonce,
		"email":   "person@example.com",
		"name":    "A Person",
		"picture": "https://example.com/p.png",
	}
}

func googleLikeProvider(o *oidcTestServer, clientID string) *Provider {
	return &Provider{
		Name:        "google",
		DisplayName: "Google",
		IsOIDC:      true,
		Issuer:      o.issuer,
		ClientID:    clientID,
		Scopes:      []string{"openid", "email", "profile"},
	}
}

func TestOIDC_HappyPath(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	o.idToken = o.signToken(t, baseClaims(o.issuer, clientID, nonce))

	p := googleLikeProvider(o, clientID)
	claims, err := p.Exchange(context.Background(), "code123", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Subject != "1078901234567890" {
		t.Errorf("subject = %q", claims.Subject)
	}
	if claims.Email != "person@example.com" || claims.Name != "A Person" || claims.AvatarURL != "https://example.com/p.png" {
		t.Errorf("claims = %+v", claims)
	}
}

func TestOIDC_RejectsWrongAudience(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	// Token minted for a DIFFERENT audience — the classic token-confusion attack.
	o.idToken = o.signToken(t, baseClaims(o.issuer, "some-other-client", nonce))

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce); err == nil {
		t.Fatal("expected rejection for wrong audience, got nil")
	}
}

func TestOIDC_RejectsWrongIssuer(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	o.idToken = o.signToken(t, baseClaims("https://evil.example.com", clientID, nonce))

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce); err == nil {
		t.Fatal("expected rejection for wrong issuer, got nil")
	}
}

func TestOIDC_RejectsExpired(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	c := baseClaims(o.issuer, clientID, nonce)
	c["exp"] = time.Now().Add(-1 * time.Hour).Unix()
	o.idToken = o.signToken(t, c)

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce); err == nil {
		t.Fatal("expected rejection for expired token, got nil")
	}
}

func TestOIDC_RejectsBadNonce(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	o.idToken = o.signToken(t, baseClaims(o.issuer, clientID, "the-real-nonce"))

	p := googleLikeProvider(o, clientID)
	// We expected a different nonce than the token carries → replay/injection.
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", "a-different-nonce"); err == nil {
		t.Fatal("expected rejection for nonce mismatch, got nil")
	}
}

func TestOIDC_RejectsAlgNone(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	// Forge an unsigned ("alg":"none") token — must be rejected outright.
	claims := baseClaims(o.issuer, clientID, nonce)
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tok.Header["kid"] = o.kid
	raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	o.idToken = raw

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce); err == nil {
		t.Fatal("expected rejection for alg=none, got nil")
	}
}

func TestOIDC_RejectsHMACConfusion(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	// Sign HS256 using the RSA public modulus bytes as the HMAC secret — the
	// classic RS→HS confusion. keyFunc must refuse the non-RSA method before the
	// secret is ever consulted.
	pub := o.key.Public().(*rsa.PublicKey)
	claims := baseClaims(o.issuer, clientID, nonce)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = o.kid
	raw, err := tok.SignedString(pub.N.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	o.idToken = raw

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce); err == nil {
		t.Fatal("expected rejection for HS256 confusion, got nil")
	}
}

func TestOIDC_RejectsUnknownKID(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	const nonce = "nonce-xyz"
	// Sign with a kid the JWKS does not publish.
	claims := baseClaims(o.issuer, clientID, nonce)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "kid-not-in-jwks"
	raw, err := tok.SignedString(o.key)
	if err != nil {
		t.Fatal(err)
	}
	o.idToken = raw

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", nonce); err == nil {
		t.Fatal("expected rejection for unknown kid, got nil")
	}
}

func TestOIDC_DiscoveryIssuerMismatchFails(t *testing.T) {
	// A discovery doc whose issuer disagrees with the configured issuer must be
	// rejected (prevents a swapped discovery pointing token/JWKS at an attacker).
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	_ = key
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 "https://not-me.example.com",
			"authorization_endpoint": "https://x/authorize",
			"token_endpoint":         "https://x/token",
			"jwks_uri":               "https://x/jwks",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &Provider{Name: "google", IsOIDC: true, Issuer: srv.URL, ClientID: "c", Scopes: []string{"openid"}}
	if err := p.ensureDiscovered(context.Background()); err == nil {
		t.Fatal("expected discovery issuer-mismatch rejection, got nil")
	}
}

func TestOIDC_RejectsEmptyExpectedNonce(t *testing.T) {
	// A missing/cleared browser nonce cookie surfaces as expectNonce=="" at the
	// hub. Verification MUST fail closed rather than skip the nonce check (the
	// token-injection fail-open). Even a token that carries a valid-looking nonce
	// must be rejected when we have nothing to compare it against.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "client-abc"
	o.idToken = o.signToken(t, baseClaims(o.issuer, clientID, "some-nonce"))

	p := googleLikeProvider(o, clientID)
	if _, err := p.Exchange(context.Background(), "c", o.issuer+"/cb", ""); err == nil {
		t.Fatal("expected hard rejection when expected nonce is empty, got nil")
	}
}

func TestOIDC_DiscoveryMissingIssuerFails(t *testing.T) {
	// A discovery doc that OMITS `issuer` must be rejected (an empty issuer used
	// to skip the cross-check, trusting a possibly-hostile jwks_uri).
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			// no "issuer"
			"authorization_endpoint": "https://x/authorize",
			"token_endpoint":         "https://x/token",
			"jwks_uri":               "https://x/jwks",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := &Provider{Name: "google", IsOIDC: true, Issuer: srv.URL, ClientID: "c", Scopes: []string{"openid"}}
	if err := p.ensureDiscovered(context.Background()); err == nil {
		t.Fatal("expected rejection for discovery doc with no issuer, got nil")
	}
}

func TestAuthCodeURL_CarriesNonceAndState(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	p := googleLikeProvider(o, "client-abc")
	u, err := p.AuthCodeURL(o.issuer+"/cb", "state-val", "nonce-val")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	if q.Get("state") != "state-val" || q.Get("nonce") != "nonce-val" {
		t.Errorf("authorize URL missing state/nonce: %s", u)
	}
	if q.Get("client_id") != "client-abc" || q.Get("response_type") != "code" {
		t.Errorf("authorize URL wrong params: %s", u)
	}
	if q.Get("scope") != "openid email profile" {
		t.Errorf("scope = %q", q.Get("scope"))
	}
}

func TestBuildRegistry_EnabledByClientID(t *testing.T) {
	// GitHub always present when its client id is set. Google enabled via env;
	// IBMid without an issuer is skipped (not fatal).
	t.Setenv("HIVE_HUB_OIDC_GOOGLE_CLIENT_ID", "g-client")
	t.Setenv("HIVE_HUB_OIDC_GOOGLE_CLIENT_SECRET", "g-secret")
	t.Setenv("HIVE_HUB_OIDC_IBMID_CLIENT_ID", "ibm-client") // no issuer → skipped
	t.Setenv("HIVE_HUB_OIDC_IBMID_ISSUER", "")
	t.Setenv("HIVE_HUB_OIDC_REDHAT_CLIENT_ID", "") // disabled

	reg := BuildRegistry("gh-client", "https://github/authorize", "https://github/token")
	if reg.Get("github") == nil {
		t.Error("github should be enabled")
	}
	if reg.Get("google") == nil {
		t.Error("google should be enabled")
	}
	if reg.Get("ibmid") != nil {
		t.Error("ibmid must be skipped when it has no issuer")
	}
	if reg.Get("redhat") != nil {
		t.Error("redhat must be disabled when no client id")
	}
	// GitHub first, then Google.
	ps := reg.Providers()
	if len(ps) != 2 || ps[0].Name != "github" || ps[1].Name != "google" {
		t.Errorf("provider order = %v", providerNames(ps))
	}
	if len(reg.OIDCProviders()) != 1 {
		t.Errorf("OIDCProviders = %v", providerNames(reg.OIDCProviders()))
	}
}

func TestBuildRegistry_IBMidWithIssuerEnabled(t *testing.T) {
	t.Setenv("HIVE_HUB_OIDC_IBMID_CLIENT_ID", "ibm-client")
	t.Setenv("HIVE_HUB_OIDC_IBMID_ISSUER", "https://login.ibm.com/oidc/endpoint/default")
	reg := BuildRegistry("", "", "")
	p := reg.Get("ibmid")
	if p == nil {
		t.Fatal("ibmid should be enabled with an explicit issuer")
	}
	if p.Issuer != "https://login.ibm.com/oidc/endpoint/default" {
		t.Errorf("issuer = %q", p.Issuer)
	}
	// IBMid keys on "uid", not the OIDC-standard "sub" (which it doesn't advertise).
	if p.SubjectClaim != "uid" {
		t.Errorf("ibmid SubjectClaim = %q, want uid", p.SubjectClaim)
	}
}

func TestBuildRegistry_SubjectClaimEnvOverride(t *testing.T) {
	// If a real IBMid token turns out to carry "sub", an operator can override the
	// default "uid" via env without a code change.
	t.Setenv("HIVE_HUB_OIDC_IBMID_CLIENT_ID", "ibm-client")
	t.Setenv("HIVE_HUB_OIDC_IBMID_ISSUER", "https://login.ibm.com/oidc/endpoint/default")
	t.Setenv("HIVE_HUB_OIDC_IBMID_SUBJECT_CLAIM", "sub")
	p := BuildRegistry("", "", "").Get("ibmid")
	if p == nil || p.SubjectClaim != "sub" {
		t.Fatalf("env override not applied: %+v", p)
	}
	// Google keeps the default (empty → sub).
	t.Setenv("HIVE_HUB_OIDC_GOOGLE_CLIENT_ID", "g")
	g := BuildRegistry("", "", "").Get("google")
	if g == nil || g.SubjectClaim != "" {
		t.Errorf("google SubjectClaim = %q, want empty (defaults to sub)", g.SubjectClaim)
	}
}

func TestVerify_IBMidUidSubject(t *testing.T) {
	// An IBMid-shaped id_token carries the stable id in "uid" and NO "sub".
	// Verification with SubjectClaim="uid" must extract the uid as the subject.
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "ibm-client"
	const nonce = "n-ibm"
	o.idToken = o.signToken(t, jwt.MapClaims{
		"iss": o.issuer, "aud": clientID,
		"uid": "2700ABC123", // stable IBMid serial, no "sub"
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": nonce, "preferred_username": "someone@ibm.com", "email": "someone@ibm.com",
	})
	p := &Provider{
		Name: "ibmid", DisplayName: "IBMid", IsOIDC: true,
		Issuer: o.issuer, ClientID: clientID, Scopes: []string{"openid"},
		SubjectClaim: "uid",
	}
	claims, err := p.Exchange(context.Background(), "code", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if claims.Subject != "2700ABC123" {
		t.Errorf("subject = %q, want the uid 2700ABC123", claims.Subject)
	}
}

func providerNames(ps []*Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

// sanity: ensure the test file's fmt import is used even if a case is trimmed.
var _ = fmt.Sprintf
