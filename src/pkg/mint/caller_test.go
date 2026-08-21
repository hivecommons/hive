package mint

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for caller identity and entitlement on /mint (#3915).
//
// The finding had three parts and each gets a test: the shared secret proves
// possession and not identity, any holder could mint ANYTHING, and nothing
// recorded who asked. The third is covered by asserting the identity reaches
// the handler; the log line itself is built from it.

// mintOnce posts a mint request through the handler and returns the status.
func mintOnce(t *testing.T, srv *Server, authHeader string, req MintRequest) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, MintPath, bytes.NewReader(body))
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func fullRequest() MintRequest {
	return MintRequest{Subject: testSub, Audience: testAud, Scopes: []string{"registry:pull"}}
}

// --- the seam -----------------------------------------------------------------

func TestSharedSecretAuthenticatorIsTheDefault(t *testing.T) {
	srv, _ := newTestServer(t)
	if srv.auth == nil {
		t.Fatal("server has no authenticator")
	}
	if got := srv.auth.Name(); got != KindSharedSecret {
		t.Errorf("default authenticator = %q, want %q", got, KindSharedSecret)
	}
}

func TestSharedSecretAuthenticatorRejectsEmptySecret(t *testing.T) {
	if _, err := NewSharedSecretAuthenticator(""); err == nil {
		t.Error("empty secret must be rejected (fail closed)")
	}
}

func TestSharedSecretIdentityIsNotPerCaller(t *testing.T) {
	// A shared secret cannot tell its holders apart. The identity must say so
	// rather than invent a name the mechanism never established.
	a, err := NewSharedSecretAuthenticator(testSecret)
	if err != nil {
		t.Fatalf("NewSharedSecretAuthenticator: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, MintPath, nil)
	r.Header.Set("Authorization", "Bearer "+testSecret)
	id, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Kind != KindSharedSecret || id.Name != SharedSecretIdentityName {
		t.Errorf("identity = %+v, want kind=%s name=%s", id, KindSharedSecret, SharedSecretIdentityName)
	}
	if id.String() != KindSharedSecret+":"+SharedSecretIdentityName {
		t.Errorf("String() = %q", id.String())
	}
}

func TestSharedSecretAuthenticatorRejectsBadCredentials(t *testing.T) {
	a, _ := NewSharedSecretAuthenticator(testSecret)
	for _, tc := range []struct{ name, header string }{
		{"missing", ""},
		{"wrong scheme", "Token " + testSecret},
		{"wrong secret", "Bearer nope"},
		{"empty bearer", "Bearer "},
		{"prefix of secret", "Bearer " + testSecret[:5]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, MintPath, nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			if _, err := a.Authenticate(r); err == nil {
				t.Error("expected rejection")
			}
		})
	}
}

// stubAuthenticator stands in for a TokenReview or mTLS backend: it proves the
// seam works without pulling in a Kubernetes client.
type stubAuthenticator struct {
	id Identity
	ok bool
}

func (s stubAuthenticator) Name() string { return "stub" }
func (s stubAuthenticator) Authenticate(*http.Request) (Identity, error) {
	if !s.ok {
		return Identity{}, ErrUnauthenticated
	}
	return s.id, nil
}

func TestWithAuthenticatorReplacesTheSharedSecret(t *testing.T) {
	m, _ := newTestMinter(t)
	spoke := Identity{Kind: "serviceaccount", Name: "system:serviceaccount:hive:spoke"}
	srv, err := NewServer(m, testSecret, nil, WithAuthenticator(stubAuthenticator{id: spoke, ok: true}))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// No Authorization header at all: the substituted backend vouches for the caller.
	code, _ := mintOnce(t, srv, "", fullRequest())
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the substituted authenticator should have admitted the caller", code)
	}
}

func TestWithAuthenticatorRefusalIs401(t *testing.T) {
	m, _ := newTestMinter(t)
	srv, _ := NewServer(m, testSecret, nil, WithAuthenticator(stubAuthenticator{ok: false}))
	// Even presenting the (still-configured) shared secret must not help once a
	// different authenticator is in force.
	code, _ := mintOnce(t, srv, "Bearer "+testSecret, fullRequest())
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

func TestWithAuthenticatorIgnoresNil(t *testing.T) {
	// A nil option must never leave the mint unauthenticated.
	m, _ := newTestMinter(t)
	srv, err := NewServer(m, testSecret, nil, WithAuthenticator(nil))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.auth == nil || srv.auth.Name() != KindSharedSecret {
		t.Fatal("nil authenticator must be ignored, leaving the shared-secret default")
	}
	if code, _ := mintOnce(t, srv, "", fullRequest()); code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

// --- blast radius --------------------------------------------------------------

func TestWithoutEntitlementsAnyHolderMintsAnything(t *testing.T) {
	// The behaviour the finding describes, pinned so the default stays a
	// deliberate, documented choice rather than an accident.
	srv, _ := newTestServer(t)
	code, _ := mintOnce(t, srv, "Bearer "+testSecret, MintRequest{
		Subject:  "system:serviceaccount:hive:hub-privileged",
		Audience: "any-audience-at-all",
		Scopes:   []string{"admin:everything"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — unconfigured entitlements must preserve historical behaviour", code)
	}
}

func entitledServer(t *testing.T) *Server {
	t.Helper()
	m, _ := newTestMinter(t)
	srv, err := NewServer(m, testSecret, nil, WithEntitlements(Entitlements{
		SharedSecretIdentityName: {
			Subjects:  []string{testSub},
			Audiences: []string{testAud},
			Scopes:    []string{"registry:pull"},
		},
	}))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func TestEntitlementAllowsWhatIsGranted(t *testing.T) {
	if code, _ := mintOnce(t, entitledServer(t), "Bearer "+testSecret, fullRequest()); code != http.StatusOK {
		t.Errorf("status = %d, want 200", code)
	}
}

func TestEntitlementRefusesBeyondTheGrant(t *testing.T) {
	// Each dimension of the finding — "any subject, audience, and scope".
	for _, tc := range []struct {
		name string
		req  MintRequest
	}{
		{"subject", MintRequest{Subject: "system:serviceaccount:hive:hub", Audience: testAud, Scopes: []string{"registry:pull"}}},
		{"audience", MintRequest{Subject: testSub, Audience: "somewhere-else", Scopes: []string{"registry:pull"}}},
		{"scope", MintRequest{Subject: testSub, Audience: testAud, Scopes: []string{"registry:push"}}},
		{"one bad scope among good", MintRequest{Subject: testSub, Audience: testAud, Scopes: []string{"registry:pull", "admin:all"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := mintOnce(t, entitledServer(t), "Bearer "+testSecret, tc.req)
			// 403, not 401: the caller is authenticated but not entitled.
			if code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", code)
			}
			if _, hasToken := body["token"]; hasToken {
				t.Error("a refused mint must not return a token")
			}
		})
	}
}

func TestEntitlementDeniesUnknownIdentity(t *testing.T) {
	// Deny-by-default: an identity with no entry mints nothing, even though it
	// authenticated successfully.
	m, _ := newTestMinter(t)
	srv, _ := NewServer(m, testSecret, nil,
		WithAuthenticator(stubAuthenticator{id: Identity{Kind: "serviceaccount", Name: "stranger"}, ok: true}),
		WithEntitlements(Entitlements{"known": {Subjects: []string{Wildcard}, Audiences: []string{Wildcard}, Scopes: []string{Wildcard}}}),
	)
	if code, _ := mintOnce(t, srv, "", fullRequest()); code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an identity with no entitlement entry", code)
	}
}

func TestEmptyDimensionAllowsNothing(t *testing.T) {
	// The trap this design avoids: an entitlement that grants an audience but
	// forgets the scopes must grant NO scope, never every scope.
	m, _ := newTestMinter(t)
	srv, _ := NewServer(m, testSecret, nil, WithEntitlements(Entitlements{
		SharedSecretIdentityName: {Subjects: []string{testSub}, Audiences: []string{testAud}},
	}))
	if code, _ := mintOnce(t, srv, "Bearer "+testSecret, fullRequest()); code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 — an empty scope list must allow nothing", code)
	}
	// ...and a request for no scopes at all is still fine.
	if code, _ := mintOnce(t, srv, "Bearer "+testSecret,
		MintRequest{Subject: testSub, Audience: testAud}); code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a scopeless request within the grant", code)
	}
}

func TestWildcardIsExplicit(t *testing.T) {
	// A caller that genuinely mints arbitrary subjects has to say so.
	m, _ := newTestMinter(t)
	srv, _ := NewServer(m, testSecret, nil, WithEntitlements(Entitlements{
		SharedSecretIdentityName: {Subjects: []string{Wildcard}, Audiences: []string{testAud}, Scopes: []string{Wildcard}},
	}))
	if code, _ := mintOnce(t, srv, "Bearer "+testSecret,
		MintRequest{Subject: "anything", Audience: testAud, Scopes: []string{"any:scope"}}); code != http.StatusOK {
		t.Errorf("status = %d, want 200 under an explicit wildcard", code)
	}
	// The wildcard is per-dimension: audience is still bounded.
	if code, _ := mintOnce(t, srv, "Bearer "+testSecret,
		MintRequest{Subject: "anything", Audience: "elsewhere", Scopes: nil}); code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 — a wildcard on one dimension must not widen another", code)
	}
}

func TestEntitlementsPermitsIsPureAndOrdered(t *testing.T) {
	e := Entitlements{"a": {Subjects: []string{"s"}, Audiences: []string{"aud"}, Scopes: []string{"x"}}}
	id := Identity{Kind: "k", Name: "a"}
	if ok, why := e.permits(id, "s", "aud", []string{"x"}); !ok {
		t.Errorf("expected permit, got refusal: %s", why)
	}
	if ok, why := e.permits(id, "other", "aud", nil); ok || why != "subject not entitled" {
		t.Errorf("subject refusal = (%v, %q)", ok, why)
	}
	if ok, why := e.permits(Identity{Name: "zzz"}, "s", "aud", nil); ok || why != "identity has no entitlement entry" {
		t.Errorf("unknown-identity refusal = (%v, %q)", ok, why)
	}
	if names := e.identityNames(); len(names) != 1 || names[0] != "a" {
		t.Errorf("identityNames = %v", names)
	}
}
