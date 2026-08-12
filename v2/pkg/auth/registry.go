package auth

import (
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// httpClient returns a client with this package's uniform timeout. A fresh client
// per call is cheap and keeps the package free of shared mutable state; the
// transport's connection pooling still applies at the default-transport level.
func httpClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

// neturlValues is a tiny indirection so the file that builds query/form bodies
// does not import net/url directly (keeps the OIDC file's import list focused on
// crypto/JWT). It returns an empty url.Values.
func neturlValues() url.Values {
	return url.Values{}
}

// nowUTC is the single clock reference, so a test can reason about jwksSetAt
// without reaching into time inside the OIDC file.
func nowUTC() time.Time {
	return time.Now().UTC()
}

// splitScopes parses a scope override env value. Accepts space- or comma-
// separated scopes ("openid email profile" or "openid,email,profile"); empties
// are dropped. Used for <PREFIX>_SCOPES, chiefly the generic "custom" provider
// whose IdP may need different scopes than the openid/email/profile default.
func splitScopes(s string) []string {
	f := func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' }
	parts := strings.FieldsFunc(s, f)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Registry holds the enabled human-login providers, in a stable display order.
type Registry struct {
	providers []*Provider
	byName    map[string]*Provider
}

// NewRegistry builds a registry from an explicit provider list, preserving order.
// BuildRegistry is the production path (env-driven); this exists for composition
// and tests that need a registry with a specific provider set.
func NewRegistry(providers ...*Provider) *Registry {
	r := &Registry{byName: map[string]*Provider{}}
	for _, p := range providers {
		if p == nil {
			continue
		}
		r.providers = append(r.providers, p)
		r.byName[strings.ToLower(p.Name)] = p
	}
	return r
}

// Get returns the provider with the given canonical name, or nil.
func (r *Registry) Get(name string) *Provider {
	if r == nil {
		return nil
	}
	return r.byName[strings.ToLower(name)]
}

// Providers returns the enabled providers in display order (GitHub first when
// present, then OIDC providers alphabetically by display name).
func (r *Registry) Providers() []*Provider {
	if r == nil {
		return nil
	}
	return r.providers
}

// OIDCProviders returns only the enabled OIDC providers (excludes GitHub).
func (r *Registry) OIDCProviders() []*Provider {
	var out []*Provider
	for _, p := range r.Providers() {
		if p.IsOIDC {
			out = append(out, p)
		}
	}
	return out
}

// Count is the number of enabled providers.
func (r *Registry) Count() int {
	if r == nil {
		return 0
	}
	return len(r.providers)
}

// oidcProviderSpec is the static per-provider config the env-driven registry
// builds from. Issuer defaults live here; env can override the issuer (needed for
// IBMid/Red Hat whose issuer is tenant-specific).
type oidcProviderSpec struct {
	name          string
	display       string
	defaultIssuer string // "" means the issuer MUST come from env
	envPrefix     string // e.g. "HIVE_HUB_OIDC_GOOGLE"
	scopes        []string
	// subjectClaim overrides which id_token claim carries the STABLE per-user
	// identifier. Empty means the OIDC-standard "sub". IBMid does not advertise
	// "sub" in its discovery claims_supported — its stable identifier is "uid"
	// (an IBMid serial); preferred_username/email are reassignable and must NOT
	// be the key. An env override (<PREFIX>_SUBJECT_CLAIM) lets us retune from a
	// real token without a code change if IBMid does emit a usable "sub".
	subjectClaim string
}

// oidcSpecs is the closed set of OIDC providers the hub can enable. A provider is
// enabled iff its <PREFIX>_CLIENT_ID env var is set — mirroring the GitHub OAuth
// gate (HIVE_HUB_OAUTH_CLIENT_ID). Order here is the fallback display order among
// OIDC providers.
var oidcSpecs = []oidcProviderSpec{
	{
		name:          "google",
		display:       "Google",
		defaultIssuer: "https://accounts.google.com",
		envPrefix:     "HIVE_HUB_OIDC_GOOGLE",
		scopes:        []string{"openid", "email", "profile"},
	},
	{
		name:          "ibmid",
		display:       "IBMid",
		defaultIssuer: "", // tenant-specific (IBM App ID / w3id): must set _ISSUER
		envPrefix:     "HIVE_HUB_OIDC_IBMID",
		scopes:        []string{"openid", "email", "profile"},
		// IBMid's discovery does not list "sub"; "uid" is the stable IBMid serial.
		// Confirmed against the live IBMid discovery claims_supported (uid present,
		// sub absent). Overridable via HIVE_HUB_OIDC_IBMID_SUBJECT_CLAIM.
		subjectClaim: "uid",
	},
	{
		name:          "redhat",
		display:       "Red Hat",
		defaultIssuer: "https://sso.redhat.com/auth/realms/redhat-external",
		envPrefix:     "HIVE_HUB_OIDC_REDHAT",
		scopes:        []string{"openid", "email", "profile"},
	},
	{
		// Generic/BYO OIDC provider. Lets an operator wire ANY standards-compliant
		// OIDC IdP (Okta, Auth0, Microsoft Entra single-tenant, Keycloak, Ping,
		// ForgeRock, Dex, Cognito, …) with no code change — everything comes from
		// env. The issuer MUST be provided (no default); the display label defaults
		// to "Single Sign-On" but is overridable via _DISPLAY. Its canonical
		// provider slug is "custom", so users key as custom:<sub>.
		name:          "custom",
		display:       "Single Sign-On",
		defaultIssuer: "", // must set HIVE_HUB_OIDC_CUSTOM_ISSUER
		envPrefix:     "HIVE_HUB_OIDC_CUSTOM",
		scopes:        []string{"openid", "email", "profile"},
	},
}

// BuildRegistry assembles the enabled human-login providers from the environment.
//
//   - githubClientID: the hub's existing HIVE_HUB_OAUTH_CLIENT_ID. When non-empty,
//     GitHub is added as the first (non-OIDC) provider using the passed endpoints
//     (the hub already has these as vars for its test seam).
//   - Each OIDC spec is enabled iff <PREFIX>_CLIENT_ID is set; it reads
//     _CLIENT_SECRET and an optional _ISSUER override (required when the spec has
//     no default issuer).
//
// A provider whose env is present but invalid (e.g. IBMid with no issuer) is
// SKIPPED, not fatal — a misconfigured provider must never take down login for
// the working ones.
func BuildRegistry(githubClientID, ghAuthorizeURL, ghTokenURL string) *Registry {
	reg := &Registry{byName: map[string]*Provider{}}

	if githubClientID != "" {
		gh := &Provider{
			Name:         "github",
			DisplayName:  "GitHub",
			IsOIDC:       false,
			AuthorizeURL: ghAuthorizeURL,
			TokenURL:     ghTokenURL,
			ClientID:     githubClientID,
			Scopes:       []string{""},
		}
		reg.providers = append(reg.providers, gh)
		reg.byName[gh.Name] = gh
	}

	var oidc []*Provider
	for _, spec := range oidcSpecs {
		clientID := os.Getenv(spec.envPrefix + "_CLIENT_ID")
		if clientID == "" {
			continue // provider not configured → not enabled
		}
		issuer := os.Getenv(spec.envPrefix + "_ISSUER")
		if issuer == "" {
			issuer = spec.defaultIssuer
		}
		if issuer == "" {
			// Configured client id but no issuer we can use → skip, don't crash.
			continue
		}
		// Optional per-provider env overrides. These matter most for the generic
		// "custom" provider (whose brand/label/scopes we can't know in advance),
		// but are honored for any provider so an operator can retune without code.
		display := spec.display
		if d := strings.TrimSpace(os.Getenv(spec.envPrefix + "_DISPLAY")); d != "" {
			display = d
		}
		scopes := spec.scopes
		if s := strings.TrimSpace(os.Getenv(spec.envPrefix + "_SCOPES")); s != "" {
			scopes = splitScopes(s)
		}
		// Subject claim: env override wins, else the spec default (e.g. IBMid "uid"),
		// else empty → the verifier falls back to the OIDC-standard "sub".
		subjectClaim := strings.TrimSpace(os.Getenv(spec.envPrefix + "_SUBJECT_CLAIM"))
		if subjectClaim == "" {
			subjectClaim = spec.subjectClaim
		}
		p := &Provider{
			Name:         spec.name,
			DisplayName:  display,
			IsOIDC:       true,
			Issuer:       strings.TrimRight(issuer, "/"),
			ClientID:     clientID,
			ClientSecret: os.Getenv(spec.envPrefix + "_CLIENT_SECRET"),
			Scopes:       scopes,
			SubjectClaim: subjectClaim,
		}
		oidc = append(oidc, p)
	}
	// Stable display order among OIDC providers: by display name.
	sort.Slice(oidc, func(i, j int) bool { return oidc[i].DisplayName < oidc[j].DisplayName })
	for _, p := range oidc {
		reg.providers = append(reg.providers, p)
		reg.byName[p.Name] = p
	}
	return reg
}
