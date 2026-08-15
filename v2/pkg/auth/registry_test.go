package auth

import (
	"os"
	"testing"
)

// --- splitScopes ---

func TestSplitScopes_SpaceSeparated(t *testing.T) {
	got := splitScopes("openid email profile")
	want := []string{"openid", "email", "profile"}
	assertStrings(t, got, want)
}

func TestSplitScopes_CommaSeparated(t *testing.T) {
	got := splitScopes("openid,email,profile")
	want := []string{"openid", "email", "profile"}
	assertStrings(t, got, want)
}

func TestSplitScopes_MixedDelimiters(t *testing.T) {
	got := splitScopes("openid, email\tprofile\noffline_access")
	want := []string{"openid", "email", "profile", "offline_access"}
	assertStrings(t, got, want)
}

func TestSplitScopes_EmptyInput(t *testing.T) {
	got := splitScopes("")
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestSplitScopes_WhitespaceOnly(t *testing.T) {
	got := splitScopes("   \t\n  ")
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

// --- NewRegistry ---

func TestNewRegistry_NilProviders(t *testing.T) {
	r := NewRegistry(nil, &Provider{Name: "google"}, nil)
	if r.Count() != 1 {
		t.Fatalf("expected 1 provider, got %d", r.Count())
	}
}

func TestNewRegistry_CaseInsensitiveLookup(t *testing.T) {
	r := NewRegistry(&Provider{Name: "GitHub"})
	if r.Get("github") == nil {
		t.Fatal("expected case-insensitive Get to find 'GitHub' via 'github'")
	}
	if r.Get("GITHUB") == nil {
		t.Fatal("expected case-insensitive Get to find 'GitHub' via 'GITHUB'")
	}
}

func TestNewRegistry_Empty(t *testing.T) {
	r := NewRegistry()
	if r.Count() != 0 {
		t.Fatalf("expected 0 providers, got %d", r.Count())
	}
	if r.Providers() != nil && len(r.Providers()) != 0 {
		t.Fatal("expected nil or empty Providers()")
	}
}

// --- Nil-safety of Registry methods ---

func TestRegistry_NilReceiver(t *testing.T) {
	var r *Registry
	if r.Get("github") != nil {
		t.Fatal("nil registry Get should return nil")
	}
	if r.Providers() != nil {
		t.Fatal("nil registry Providers should return nil")
	}
	if r.Count() != 0 {
		t.Fatal("nil registry Count should return 0")
	}
}

// --- OIDCProviders ---

func TestOIDCProviders_FiltersCorrectly(t *testing.T) {
	gh := &Provider{Name: "github", IsOIDC: false}
	google := &Provider{Name: "google", IsOIDC: true}
	ibm := &Provider{Name: "ibmid", IsOIDC: true}
	r := NewRegistry(gh, google, ibm)

	oidc := r.OIDCProviders()
	if len(oidc) != 2 {
		t.Fatalf("expected 2 OIDC providers, got %d", len(oidc))
	}
	for _, p := range oidc {
		if !p.IsOIDC {
			t.Fatalf("non-OIDC provider %q in OIDCProviders()", p.Name)
		}
	}
}

// --- BuildRegistry ---

func TestBuildRegistry_GitHubOnly(t *testing.T) {
	clearOIDCEnv(t)
	r := BuildRegistry("my-client-id", "https://github.com/login/oauth/authorize", "https://github.com/login/oauth/access_token")
	if r.Count() != 1 {
		t.Fatalf("expected 1 provider, got %d", r.Count())
	}
	gh := r.Get("github")
	if gh == nil {
		t.Fatal("expected github provider")
	}
	if gh.IsOIDC {
		t.Fatal("github should not be OIDC")
	}
	if gh.ClientID != "my-client-id" {
		t.Fatalf("wrong client id: %s", gh.ClientID)
	}
}

func TestBuildRegistry_NoGitHub(t *testing.T) {
	clearOIDCEnv(t)
	r := BuildRegistry("", "", "")
	if r.Count() != 0 {
		t.Fatalf("expected 0 providers when no github and no OIDC, got %d", r.Count())
	}
}

func TestBuildRegistry_GoogleOIDC(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("HIVE_HUB_OIDC_GOOGLE_CLIENT_ID", "google-id")
	t.Setenv("HIVE_HUB_OIDC_GOOGLE_CLIENT_SECRET", "google-secret")

	r := BuildRegistry("", "", "")
	if r.Count() != 1 {
		t.Fatalf("expected 1 provider, got %d", r.Count())
	}
	g := r.Get("google")
	if g == nil {
		t.Fatal("expected google provider")
	}
	if !g.IsOIDC {
		t.Fatal("google should be OIDC")
	}
	if g.Issuer != "https://accounts.google.com" {
		t.Fatalf("unexpected issuer: %s", g.Issuer)
	}
	if g.ClientSecret != "google-secret" {
		t.Fatalf("unexpected secret: %s", g.ClientSecret)
	}
}

func TestBuildRegistry_IBMidSkippedWithoutIssuer(t *testing.T) {
	clearOIDCEnv(t)
	// IBMid has no default issuer; CLIENT_ID alone is not enough.
	t.Setenv("HIVE_HUB_OIDC_IBMID_CLIENT_ID", "ibm-id")

	r := BuildRegistry("", "", "")
	if r.Get("ibmid") != nil {
		t.Fatal("IBMid should be skipped when no issuer is set")
	}
}

func TestBuildRegistry_IBMidWithIssuer(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("HIVE_HUB_OIDC_IBMID_CLIENT_ID", "ibm-id")
	t.Setenv("HIVE_HUB_OIDC_IBMID_ISSUER", "https://iam.ibm.com/identity")

	r := BuildRegistry("", "", "")
	ibm := r.Get("ibmid")
	if ibm == nil {
		t.Fatal("expected ibmid provider")
	}
	if ibm.SubjectClaim != "uid" {
		t.Fatalf("expected SubjectClaim 'uid', got %q", ibm.SubjectClaim)
	}
}

func TestBuildRegistry_MicrosoftMultiTenantDefault(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_CLIENT_ID", "ms-id")

	r := BuildRegistry("", "", "")
	ms := r.Get("microsoft")
	if ms == nil {
		t.Fatal("expected microsoft provider")
	}
	// Default tenant is "organizations"
	if ms.Issuer != "https://login.microsoftonline.com/organizations/v2.0" {
		t.Fatalf("unexpected issuer: %s", ms.Issuer)
	}
	if ms.IssuerTemplate != "https://login.microsoftonline.com/{tenantid}/v2.0" {
		t.Fatalf("unexpected issuer template: %s", ms.IssuerTemplate)
	}
}

func TestBuildRegistry_MicrosoftSingleTenant(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_CLIENT_ID", "ms-id")
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_TENANT", "abc-def-123")

	r := BuildRegistry("", "", "")
	ms := r.Get("microsoft")
	if ms == nil {
		t.Fatal("expected microsoft provider")
	}
	if ms.Issuer != "https://login.microsoftonline.com/abc-def-123/v2.0" {
		t.Fatalf("unexpected issuer: %s", ms.Issuer)
	}
	// Single tenant (concrete GUID) → no issuer template
	if ms.IssuerTemplate != "" {
		t.Fatalf("single-tenant should have no issuer template, got %q", ms.IssuerTemplate)
	}
}

func TestBuildRegistry_MicrosoftAllowedTenantsFromEnv(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_CLIENT_ID", "ms-id")
	t.Setenv("HIVE_HUB_OIDC_MICROSOFT_ALLOWED_TENANTS", "tenant-a, tenant-b")

	r := BuildRegistry("", "", "")
	ms := r.Get("microsoft")
	if ms == nil {
		t.Fatal("expected microsoft provider")
	}
	if len(ms.AllowedTenants) != 2 {
		t.Fatalf("expected 2 allowed tenants, got %d: %v", len(ms.AllowedTenants), ms.AllowedTenants)
	}
}

func TestBuildRegistry_CustomProvider(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_CLIENT_ID", "custom-id")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_ISSUER", "https://auth.example.com")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_DISPLAY", "My Corp SSO")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_SCOPES", "openid,groups")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_SUBJECT_CLAIM", "employee_id")

	r := BuildRegistry("", "", "")
	c := r.Get("custom")
	if c == nil {
		t.Fatal("expected custom provider")
	}
	if c.DisplayName != "My Corp SSO" {
		t.Fatalf("wrong display name: %s", c.DisplayName)
	}
	assertStrings(t, c.Scopes, []string{"openid", "groups"})
	if c.SubjectClaim != "employee_id" {
		t.Fatalf("wrong subject claim: %s", c.SubjectClaim)
	}
}

func TestBuildRegistry_CustomSkippedWithoutIssuer(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_CLIENT_ID", "custom-id")
	// No issuer set

	r := BuildRegistry("", "", "")
	if r.Get("custom") != nil {
		t.Fatal("custom should be skipped without issuer")
	}
}

func TestBuildRegistry_OIDCDisplayOrder(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("HIVE_HUB_OIDC_GOOGLE_CLIENT_ID", "g-id")
	t.Setenv("HIVE_HUB_OIDC_REDHAT_CLIENT_ID", "rh-id")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_CLIENT_ID", "c-id")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_ISSUER", "https://auth.example.com")
	t.Setenv("HIVE_HUB_OIDC_CUSTOM_DISPLAY", "Acme SSO")

	r := BuildRegistry("gh-id", "https://example.com/auth", "https://example.com/token")
	ps := r.Providers()
	// GitHub first, then OIDC alphabetically by display name
	if ps[0].Name != "github" {
		t.Fatalf("expected github first, got %s", ps[0].Name)
	}
	// OIDC order: Acme SSO < Google < Red Hat
	if ps[1].DisplayName != "Acme SSO" {
		t.Fatalf("expected 'Acme SSO' second, got %s", ps[1].DisplayName)
	}
	if ps[2].DisplayName != "Google" {
		t.Fatalf("expected 'Google' third, got %s", ps[2].DisplayName)
	}
	if ps[3].DisplayName != "Red Hat" {
		t.Fatalf("expected 'Red Hat' fourth, got %s", ps[3].DisplayName)
	}
}

func TestBuildRegistry_IssuerTrailingSlashStripped(t *testing.T) {
	clearOIDCEnv(t)
	t.Setenv("HIVE_HUB_OIDC_GOOGLE_CLIENT_ID", "g-id")
	t.Setenv("HIVE_HUB_OIDC_GOOGLE_ISSUER", "https://accounts.google.com/")

	r := BuildRegistry("", "", "")
	g := r.Get("google")
	if g == nil {
		t.Fatal("expected google provider")
	}
	if g.Issuer != "https://accounts.google.com" {
		t.Fatalf("trailing slash not stripped: %s", g.Issuer)
	}
}

// --- helpers ---

// clearOIDCEnv unsets all OIDC env vars so tests are isolated.
func clearOIDCEnv(t *testing.T) {
	t.Helper()
	prefixes := []string{
		"HIVE_HUB_OIDC_GOOGLE",
		"HIVE_HUB_OIDC_IBMID",
		"HIVE_HUB_OIDC_REDHAT",
		"HIVE_HUB_OIDC_MICROSOFT",
		"HIVE_HUB_OIDC_CUSTOM",
	}
	suffixes := []string{"_CLIENT_ID", "_CLIENT_SECRET", "_ISSUER", "_DISPLAY", "_SCOPES", "_SUBJECT_CLAIM", "_TENANT", "_ALLOWED_TENANTS"}
	for _, p := range prefixes {
		for _, s := range suffixes {
			os.Unsetenv(p + s)
		}
	}
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}
