package auth

import "testing"

// TestBuildRegistry_GenericCustomProvider: an operator can wire ANY OIDC IdP via
// the "custom" env prefix — enabled by CLIENT_ID, issuer required, with an
// overridable display label, scopes, and subject claim.
func TestBuildRegistry_GenericCustomProvider(t *testing.T) {
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_CLIENT_ID", "acme-client")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_CLIENT_SECRET", "s")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_ISSUER", "https://acme.okta.com")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_DISPLAY", "Acme SSO")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_SCOPES", "openid,email,groups")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_SUBJECT_CLAIM", "oid")

	p := BuildRegistry("", "", "").Get("custom")
	if p == nil {
		t.Fatal("custom provider should be enabled")
	}
	if p.DisplayName != "Acme SSO" {
		t.Errorf("display = %q, want Acme SSO", p.DisplayName)
	}
	if p.Issuer != "https://acme.okta.com" {
		t.Errorf("issuer = %q", p.Issuer)
	}
	if len(p.Scopes) != 3 || p.Scopes[2] != "groups" {
		t.Errorf("scopes = %v", p.Scopes)
	}
	if p.SubjectClaim != "oid" {
		t.Errorf("subjectClaim = %q, want oid", p.SubjectClaim)
	}
}

// TestBuildRegistry_GenericDefaults: with only the required env, sensible
// defaults apply (label "Single Sign-On", standard scopes, sub subject).
func TestBuildRegistry_GenericDefaults(t *testing.T) {
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_CLIENT_ID", "c")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_ISSUER", "https://idp.example.com")
	p := BuildRegistry("", "", "").Get("custom")
	if p == nil {
		t.Fatal("custom should be enabled with just client id + issuer")
	}
	if p.DisplayName != "Single Sign-On" {
		t.Errorf("default display = %q", p.DisplayName)
	}
	if p.SubjectClaim != "" {
		t.Errorf("default subjectClaim should be empty (→ sub), got %q", p.SubjectClaim)
	}
	if len(p.Scopes) != 3 {
		t.Errorf("default scopes = %v", p.Scopes)
	}
}

// TestBuildRegistry_GenericNeedsIssuer: no issuer → skipped, not fatal.
func TestBuildRegistry_GenericNeedsIssuer(t *testing.T) {
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_CLIENT_ID", "c")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_ISSUER", "")
	if BuildRegistry("", "", "").Get("custom") != nil {
		t.Error("custom must be skipped when no issuer is configured")
	}
}

func TestSplitScopes(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"openid email profile", 3},
		{"openid,email,profile", 3},
		{"openid, email ,  profile", 3},
		{"", 0},
		{"  openid  ", 1},
	} {
		if got := len(splitScopes(c.in)); got != c.want {
			t.Errorf("splitScopes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
