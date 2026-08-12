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
		defaultIssuer: "", // tenant-specific (IBM App ID): must set _ISSUER
		envPrefix:     "HIVE_HUB_OIDC_IBMID",
		scopes:        []string{"openid", "email", "profile"},
	},
	{
		name:          "redhat",
		display:       "Red Hat",
		defaultIssuer: "https://sso.redhat.com/auth/realms/redhat-external",
		envPrefix:     "HIVE_HUB_OIDC_REDHAT",
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
		p := &Provider{
			Name:         spec.name,
			DisplayName:  spec.display,
			IsOIDC:       true,
			Issuer:       strings.TrimRight(issuer, "/"),
			ClientID:     clientID,
			ClientSecret: os.Getenv(spec.envPrefix + "_CLIENT_SECRET"),
			Scopes:       spec.scopes,
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
