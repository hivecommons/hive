package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestTenantFromIssuer(t *testing.T) {
	const tmpl = "https://login.microsoftonline.com/{tenantid}/v2.0"
	cases := []struct {
		iss    string
		want   string
		wantOK bool
	}{
		{"https://login.microsoftonline.com/9188040d-abc/v2.0", "9188040d-abc", true},
		{"https://login.microsoftonline.com/9188040d-abc/v2.0/", "9188040d-abc", true}, // trailing slash
		{"https://login.microsoftonline.com//v2.0", "", false},                         // empty tenant
		{"https://evil.example.com/9188040d/v2.0", "", false},                          // wrong host
		{"https://login.microsoftonline.com/9188040d/v3.0", "", false},                 // wrong suffix
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := tenantFromIssuer(tmpl, c.iss)
		if ok != c.wantOK || got != c.want {
			t.Errorf("tenantFromIssuer(%q) = (%q,%v), want (%q,%v)", c.iss, got, ok, c.want, c.wantOK)
		}
	}
}

func TestBuildRegistry_MicrosoftMultiTenant(t *testing.T) {
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_CLIENT_ID", "ms-client")
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_CLIENT_SECRET", "s")
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_TENANT", "organizations")
	p := BuildRegistry("", "", "").Get("microsoft")
	if p == nil {
		t.Fatal("microsoft should be enabled")
	}
	if p.Issuer != "https://login.microsoftonline.com/organizations/v2.0" {
		t.Errorf("issuer = %q", p.Issuer)
	}
	if p.IssuerTemplate != "https://login.microsoftonline.com/{tenantid}/v2.0" {
		t.Errorf("issuerTemplate = %q (multi-tenant should template it)", p.IssuerTemplate)
	}
}

func TestBuildRegistry_MicrosoftSingleTenantIsStrict(t *testing.T) {
	// A concrete directory GUID → single-tenant: strict issuer, NO template.
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_CLIENT_ID", "ms-client")
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_TENANT", "11112222-3333-4444-5555-666677778888")
	p := BuildRegistry("", "", "").Get("microsoft")
	if p == nil {
		t.Fatal("microsoft should be enabled")
	}
	if p.IssuerTemplate != "" {
		t.Errorf("single-tenant must NOT set a template, got %q", p.IssuerTemplate)
	}
	if p.Issuer != "https://login.microsoftonline.com/11112222-3333-4444-5555-666677778888/v2.0" {
		t.Errorf("issuer = %q", p.Issuer)
	}
}

func TestBuildRegistry_MicrosoftAllowedTenants(t *testing.T) {
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_CLIENT_ID", "ms-client")
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_TENANT", "common")
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_ALLOWED_TENANTS", "aaa-111, bbb-222")
	p := BuildRegistry("", "", "").Get("microsoft")
	if p == nil || len(p.AllowedTenants) != 2 {
		t.Fatalf("allowed tenants not parsed: %+v", p)
	}
}

// TestVerify_MultiTenant drives a full multi-tenant token verify: the token's iss
// carries a real tenant GUID, which must match the template, gate on the allowed
// set, and namespace the subject as "<tenant>.<sub>".
func TestVerify_MultiTenant(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "ms-client"
	const nonce = "n-ms"
	const tenant = "9188040d-6c67-4c5b-b112-36a304b66dad"
	const sub = "AAAAAAAAAAAAAAAAAAAAAA"
	// The token issuer is the tenant-specific issuer (what Entra actually emits).
	tokenIss := "https://login.microsoftonline.com/" + tenant + "/v2.0"
	o.discoveryIssuer = "https://login.microsoftonline.com/{tenantid}/v2.0"
	o.idToken = o.signToken(t, jwt.MapClaims{
		"iss": tokenIss, "aud": clientID, "sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"nonce": nonce, "name": "MS User", "email": "user@contoso.com",
	})
	// Provider validates against the TEMPLATE (not a fixed issuer), and the JWKS/
	// token endpoints point at our fake server via explicit URLs.
	p := &Provider{
		Name: "microsoft", DisplayName: "Microsoft", IsOIDC: true,
		Issuer:         o.issuer,
		IssuerTemplate: "https://login.microsoftonline.com/{tenantid}/v2.0",
		TokenURL:       o.issuer + "/token", JWKSURL: o.issuer + "/jwks",
		ClientID: clientID, Scopes: []string{"openid"},
		AllowedTenants: []string{tenant},
	}
	claims, err := p.Exchange(context.Background(), "code", o.issuer+"/cb", nonce)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	want := tenant + "." + sub
	if claims.Subject != want {
		t.Errorf("subject = %q, want tenant-namespaced %q", claims.Subject, want)
	}
}

// TestVerify_MultiTenant_DisallowedTenantRejected: a token from a tenant not in
// the allowed set is refused even though it's validly signed.
func TestVerify_MultiTenant_DisallowedTenantRejected(t *testing.T) {
	o := newOIDCTestServer(t)
	defer o.close()
	const clientID = "ms-client"
	const nonce = "n-ms"
	tokenIss := "https://login.microsoftonline.com/UNWANTED-TENANT/v2.0"
	o.discoveryIssuer = "https://login.microsoftonline.com/{tenantid}/v2.0"
	o.idToken = o.signToken(t, jwt.MapClaims{
		"iss": tokenIss, "aud": clientID, "sub": "s",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce,
	})
	p := &Provider{
		Name: "microsoft", IsOIDC: true, Issuer: o.issuer,
		IssuerTemplate: "https://login.microsoftonline.com/{tenantid}/v2.0",
		TokenURL:       o.issuer + "/token", JWKSURL: o.issuer + "/jwks",
		ClientID: clientID, Scopes: []string{"openid"},
		AllowedTenants: []string{"only-this-tenant"},
	}
	if _, err := p.Exchange(context.Background(), "code", o.issuer+"/cb", nonce); err == nil {
		t.Fatal("a token from a disallowed tenant must be rejected")
	}
}
