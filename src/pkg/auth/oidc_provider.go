// Package auth provides a small, self-contained OIDC/OAuth provider abstraction
// for HUMAN dashboard login on the hive Hub (and, later, direct-route spokes).
//
// Scope boundary (important): this is ONLY about authenticating a human and
// deriving their canonical `provider:subject` identity for access control. It
// has nothing to do with:
//   - the GitHub App installation token (kubestellar-hive[bot]) that performs the
//     hive's actual GitHub work — that path is untouched; a non-GitHub human can
//     fully own a GitHub-repo hive because the App, not the user, does the work.
//   - agent/backend logins (Copilot→GitHub, Claude→Gmail) — those are how the AI
//     CLIs authenticate to do work, entirely separate from human login.
//
// A provider is either:
//   - GitHub OAuth (IsOIDC=false): the legacy path — authorize, exchange code,
//     read GET /user for the login. The hub keeps its own GitHub handler; this
//     package models GitHub only so the picker can enumerate it uniformly.
//   - OIDC (IsOIDC=true): Google/IBMid/Red Hat — authorize with an OIDC `nonce`,
//     exchange code for an id_token (a signed JWT), and verify that JWT against
//     the provider's JWKS, enforcing iss/aud/exp AND the nonce. The stable `sub`
//     claim (NEVER email — emails are reassignable) becomes the canonical subject.
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// httpTimeout bounds every outbound call this package makes (discovery, JWKS,
// token exchange). Matches the hub's existing oauthTimeout so behavior is
// consistent across the two login paths.
const httpTimeout = 10 * time.Second

// maxResponseBytes caps how much of any provider response we read, mirroring the
// hub's maxOAuthResponseBytes. A discovery/JWKS/token document is a few KB; this
// is a memory-safety bound against a hostile or broken endpoint.
const maxResponseBytes = 1 << 16

// jwtClockSkewLeeway tolerates small clock drift between the hub and the
// provider when validating exp/nbf/iat. golang-jwt v5 defaults to ZERO leeway
// (there is no built-in "small leeway"), so without this a provider whose clock
// is seconds ahead of ours mints id_tokens the hub rejects as "not valid yet".
// Two minutes matches common OIDC RP practice and stays far below any token
// lifetime.
const jwtClockSkewLeeway = 2 * time.Minute

// Step sentinels: Exchange wraps its error with exactly one of these so the hub
// can log WHICH step of the OIDC callback failed (discovery / token exchange /
// id_token verification) without parsing error strings. Server-side diagnostics
// only — never shown to the user.
var (
	ErrStepDiscovery     = errors.New("step=discovery")
	ErrStepTokenExchange = errors.New("step=token_exchange")
	ErrStepVerifyToken   = errors.New("step=id_token_verify")
)

// FailedStep classifies an Exchange error by the step sentinel it wraps.
// Returns "unknown" for a nil or unclassified error.
func FailedStep(err error) string {
	switch {
	case errors.Is(err, ErrStepDiscovery):
		return "discovery"
	case errors.Is(err, ErrStepTokenExchange):
		return "token_exchange"
	case errors.Is(err, ErrStepVerifyToken):
		return "id_token_verify"
	}
	return "unknown"
}

// jwksRefreshMinInterval throttles JWKS re-fetches triggered by an unknown `kid`.
// Providers rotate signing keys occasionally; a flood of tokens with an unknown
// kid (garbage or an attack) must not turn into a fetch-per-request DoS on the
// provider's JWKS endpoint.
const jwksRefreshMinInterval = 60 * time.Second

// Provider describes one human-login provider. GitHub is modeled with IsOIDC
// false (the hub owns its handler); Google/IBMid/Red Hat are OIDC.
type Provider struct {
	// Name is the canonical provider slug used in the identity wire form
	// ("github", "google", "ibmid", "redhat"). Must match the hub's
	// identityProviders set.
	Name string
	// DisplayName is the human label on the login picker ("Google", "Red Hat").
	DisplayName string
	// IsOIDC distinguishes the OIDC path (id_token + JWKS) from GitHub OAuth.
	IsOIDC bool

	// Issuer is the OIDC issuer URL. Discovery is fetched from
	// Issuer + "/.well-known/openid-configuration". Required for OIDC.
	Issuer string
	// Endpoints. For OIDC these are normally filled from discovery; for GitHub
	// they are set explicitly by the hub. AuthorizeURL/TokenURL are always set;
	// JWKSURL/UserInfoURL only for OIDC.
	AuthorizeURL string
	TokenURL     string
	JWKSURL      string
	// UserInfoURL is the OIDC userinfo endpoint, filled from discovery. Used only
	// as a best-effort enrichment source when the id_token itself lacks display
	// claims (name/email/picture) — some providers (IBMid) issue minimal
	// id_tokens and put profile claims behind userinfo.
	UserInfoURL string

	// ClientID / ClientSecret from the provider's app registration. A provider is
	// enabled iff ClientID is set (mirrors the hub's registerOAuth gate).
	ClientID     string
	ClientSecret string

	// Scopes requested at authorize time. OIDC always needs "openid"; "email
	// profile" get the display name/avatar. GitHub uses "" (identity-only).
	Scopes []string

	// Claim names for extracting identity from the id_token. Defaults suit the
	// standard OIDC claim set (sub/name/picture/email); overridable per provider.
	SubjectClaim string
	NameClaim    string
	AvatarClaim  string
	EmailClaim   string

	// IssuerTemplate enables MULTI-TENANT providers (Microsoft Entra "common"/
	// "organizations", multi-tenant Okta orgs). Multi-tenant issuers embed a
	// per-tenant id: the discovery/config issuer contains a literal "{tenantid}"
	// placeholder while each id_token's `iss` carries the REAL tenant id. A single
	// strict `iss == Issuer` check can't express that, so when IssuerTemplate is
	// set (e.g. "https://login.microsoftonline.com/{tenantid}/v2.0") the verifier
	// instead matches the token's `iss` against the template, extracts the tenant,
	// enforces AllowedTenants (if any), and namespaces the subject as
	// "<tenant>:<sub>" so identities never collide across tenants. Empty means the
	// single-tenant strict-issuer path (unchanged).
	IssuerTemplate string
	// AllowedTenants optionally restricts a multi-tenant provider to specific
	// tenant ids (Entra directory GUIDs). Empty = any tenant that satisfies the
	// template is accepted.
	AllowedTenants []string

	// discovered caches the OIDC discovery document + JWKS behind mu.
	mu         sync.Mutex
	discovered bool
	jwks       map[string]*rsa.PublicKey // kid -> RSA public key
	jwksSetAt  time.Time
}

// Claims is the identity extracted from a verified id_token (or GitHub /user).
type Claims struct {
	Subject   string // the stable `sub` — becomes the canonical subject
	Name      string // display name (optional)
	AvatarURL string // picture (optional)
	Email     string // email (display only; NEVER the identity key)

	// rawSub is the id_token's literal `sub` claim, kept for the OIDC Core
	// §5.3.2 userinfo cross-check (userinfo's sub MUST match the id_token's).
	// Unexported: identity decisions use Subject, never this.
	rawSub string
}

// incomplete reports whether any display claim is missing — the trigger for the
// best-effort userinfo enrichment fetch.
func (c *Claims) incomplete() bool {
	return c.Name == "" || c.Email == "" || c.AvatarURL == ""
}

// scopeString renders the space-delimited scope parameter.
func (p *Provider) scopeString() string {
	return strings.Join(p.Scopes, " ")
}

// AuthCodeURL builds the provider's authorization URL. state carries the hub's
// CSRF nonce (+ redirect); nonce is the OIDC replay-protection nonce echoed back
// in the id_token (ignored by non-OIDC providers). redirectURI must exactly match
// a URI registered on the provider side.
func (p *Provider) AuthCodeURL(redirectURI, state, nonce string) (string, error) {
	if err := p.ensureDiscovered(context.Background()); err != nil {
		return "", err
	}
	v := neturlValues()
	v.Set("client_id", p.ClientID)
	v.Set("redirect_uri", redirectURI)
	v.Set("response_type", "code")
	v.Set("scope", p.scopeString())
	v.Set("state", state)
	if p.IsOIDC && nonce != "" {
		v.Set("nonce", nonce)
	}
	sep := "?"
	if strings.Contains(p.AuthorizeURL, "?") {
		sep = "&"
	}
	return p.AuthorizeURL + sep + v.Encode(), nil
}

// Exchange trades an authorization code for identity claims. For OIDC it fetches
// the token endpoint, extracts the id_token, and VERIFIES it (signature via JWKS,
// plus iss/aud/exp and the expected nonce) before returning any claim. For a
// non-OIDC (GitHub) provider it returns an error — the hub owns that path.
func (p *Provider) Exchange(ctx context.Context, code, redirectURI, expectNonce string) (*Claims, error) {
	if !p.IsOIDC {
		return nil, errors.New("Exchange is only for OIDC providers; GitHub uses the hub's own handler")
	}
	if err := p.ensureDiscovered(ctx); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStepDiscovery, err)
	}
	rawIDToken, accessToken, err := p.fetchIDToken(ctx, code, redirectURI)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStepTokenExchange, err)
	}
	claims, err := p.verifyIDToken(ctx, rawIDToken, expectNonce)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStepVerifyToken, err)
	}
	// Best-effort display enrichment: some providers (IBMid) issue minimal
	// id_tokens and serve name/email/picture only from userinfo. Fills ONLY the
	// missing display claims; identity (Subject) is never touched, and a
	// userinfo failure never fails an already-verified login.
	if claims.incomplete() && accessToken != "" && p.UserInfoURL != "" {
		p.enrichFromUserInfo(ctx, claims, accessToken)
	}
	return claims, nil
}

// enrichFromUserInfo fetches the provider's userinfo endpoint and fills the
// MISSING display claims (name/email/picture) on c. Per OIDC Core §5.3.2 the
// userinfo response's `sub` MUST match the id_token's — a mismatched (or, when
// the id_token carried one, absent) sub discards the response entirely, so a
// confused-deputy userinfo can never relabel a verified identity. Best-effort:
// any error is silently a no-op; the login is already verified.
func (p *Provider) enrichFromUserInfo(ctx context.Context, c *Claims, accessToken string) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.UserInfoURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	info := jwt.MapClaims{}
	if err := json.Unmarshal(body, &info); err != nil {
		return
	}
	if c.rawSub != "" && stringClaim(info, claimSub) != c.rawSub {
		return // §5.3.2 cross-check failed — do not trust any of it
	}
	if c.Name == "" {
		c.Name = displayNameFromClaims(info, p.claimName(p.NameClaim, claimName))
	}
	if c.Email == "" {
		c.Email = stringClaim(info, p.claimName(p.EmailClaim, claimEmail))
	}
	if c.AvatarURL == "" {
		c.AvatarURL = stringClaim(info, p.claimName(p.AvatarClaim, claimPicture))
	}
}

// displayNameFromClaims extracts a human display name: the configured name
// claim first, then the standard given_name + family_name composition (IBMid
// and Entra often carry the parts without a composite `name`).
func displayNameFromClaims(m jwt.MapClaims, nameClaim string) string {
	if n := stringClaim(m, nameClaim); n != "" {
		return n
	}
	given := stringClaim(m, claimGivenName)
	family := stringClaim(m, claimFamilyName)
	return strings.TrimSpace(strings.TrimSpace(given) + " " + strings.TrimSpace(family))
}

// fetchIDToken posts the code to the token endpoint and returns the raw
// id_token plus the access_token (used only for the userinfo enrichment fetch).
func (p *Provider) fetchIDToken(ctx context.Context, code, redirectURI string) (idToken, accessToken string, err error) {
	form := neturlValues()
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", p.ClientID)
	form.Set("client_secret", p.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, "POST", p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var tok struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", "", fmt.Errorf("parse token response: %w", err)
	}
	if tok.Error != "" {
		return "", "", fmt.Errorf("token endpoint error %q: %s", tok.Error, tok.ErrorDesc)
	}
	if tok.IDToken == "" {
		return "", "", errors.New("token response carried no id_token")
	}
	return tok.IDToken, tok.AccessToken, nil
}

// verifyIDToken validates the id_token's signature and claims, then extracts
// identity. It enforces, in order: a supported RSA alg — RS256/384/512 or the
// RSA-PSS variants PS256/384/512 (IBMid advertises PS*), never "none", never
// HMAC — which would let a client-known secret forge a token; the signing key
// resolved from JWKS by `kid`; iss == p.Issuer; aud contains p.ClientID; exp not
// passed (with jwtClockSkewLeeway tolerance — golang-jwt v5 has NO default
// leeway); and nonce == expectNonce. Any failure returns an error and NO claims.
func (p *Provider) verifyIDToken(ctx context.Context, raw, expectNonce string) (*Claims, error) {
	keyFunc := func(t *jwt.Token) (interface{}, error) {
		// Reject anything but RSA/RSA-PSS signing. "none" and HMAC ("HS*") are
		// the two classic OIDC token-forgery vectors; the parser options below
		// also pin valid methods, this is defense in depth on the key side. Both
		// RS* and PS* verify against the same JWKS RSA public keys.
		switch t.Method.(type) {
		case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS:
		default:
			return nil, fmt.Errorf("unexpected signing method %q", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("id_token has no kid")
		}
		key, err := p.keyForKID(ctx, kid)
		if err != nil {
			return nil, err
		}
		return key, nil
	}

	// Issuer validation differs for single- vs multi-tenant. Single-tenant: the
	// jwt parser enforces iss == p.Issuer. Multi-tenant: iss is per-tenant, so we
	// validate it against IssuerTemplate ourselves AFTER parsing (below) and do
	// NOT pin a fixed issuer here.
	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"}),
		jwt.WithAudience(p.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(jwtClockSkewLeeway),
	}
	if p.IssuerTemplate == "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(p.Issuer))
	}
	parser := jwt.NewParser(parserOpts...)
	claims := jwt.MapClaims{}
	if _, err := parser.ParseWithClaims(raw, claims, keyFunc); err != nil {
		return nil, fmt.Errorf("id_token verification failed: %w", err)
	}

	// Multi-tenant: match the token issuer against the template, extract + gate the
	// tenant. Fails closed on a non-matching iss or a disallowed tenant.
	tenant := ""
	if p.IssuerTemplate != "" {
		iss, _ := claims["iss"].(string)
		t, ok := tenantFromIssuer(p.IssuerTemplate, iss)
		if !ok {
			return nil, fmt.Errorf("id_token issuer %q does not match template %q", iss, p.IssuerTemplate)
		}
		if len(p.AllowedTenants) > 0 && !containsFold(p.AllowedTenants, t) {
			return nil, fmt.Errorf("id_token tenant %q is not in the allowed set", t)
		}
		tenant = t
	}

	// Nonce binds the id_token to the login THIS browser started (replay/
	// injection defense). Enforced explicitly because jwt has no built-in nonce
	// validator. This MUST fail closed: the caller always mints and cookies a
	// nonce before authorize, so an empty expectNonce at verify time means the
	// browser's nonce cookie was missing/cleared — exactly the state a token-
	// injection attacker engineers. Rejecting on empty is what closes that hole
	// (an earlier version skipped the check when expectNonce=="", failing OPEN).
	if expectNonce == "" {
		return nil, errors.New("no expected nonce for OIDC verification (nonce cookie missing) — refusing to skip replay protection")
	}
	got, _ := claims[claimNonce].(string)
	if got == "" || got != expectNonce {
		return nil, errors.New("id_token nonce mismatch")
	}

	// Subject extraction. The configured claim (e.g. IBMid's "uid") is preferred,
	// but discovery `claims_supported` describes what the OP can supply — mostly
	// via userinfo — NOT what the id_token carries, and this package never calls
	// userinfo. OIDC Core REQUIRES `sub` in every id_token, so when the override
	// claim is absent we fall back to the standard `sub` rather than failing the
	// whole login with "id_token has no subject". Both are provider-issued stable
	// identifiers; neither is a reassignable email/username.
	subjectClaim := p.claimName(p.SubjectClaim, claimSub)
	sub := stringClaim(claims, subjectClaim)
	if sub == "" && subjectClaim != claimSub {
		sub = stringClaim(claims, claimSub)
	}
	if sub == "" {
		return nil, fmt.Errorf("id_token has no subject (checked claim %q and %q)", subjectClaim, claimSub)
	}
	// Multi-tenant: namespace the subject by tenant so the SAME sub value in two
	// different tenants yields two distinct identities ("<tenant>:<sub>"). A bare
	// sub is per-issuer-unique, and in multi-tenant mode the issuer varies, so
	// without this two tenants could collide. Uses a '.' separator, not ':',
	// because ':' is the canonical provider separator upstream.
	if tenant != "" {
		sub = tenant + "." + sub
	}
	return &Claims{
		Subject:   sub,
		Name:      displayNameFromClaims(claims, p.claimName(p.NameClaim, claimName)),
		AvatarURL: stringClaim(claims, p.claimName(p.AvatarClaim, claimPicture)),
		Email:     stringClaim(claims, p.claimName(p.EmailClaim, claimEmail)),
		rawSub:    stringClaim(claims, claimSub),
	}, nil
}

// tenantFromIssuer matches a concrete token issuer against a template containing
// a single "{tenantid}" placeholder and returns the substituted tenant value.
// The non-placeholder parts must match exactly (trailing slashes normalized), and
// the tenant must be a plausible id (non-empty, no '/'). Returns ok=false on any
// mismatch — this is a security check, so it fails closed.
func tenantFromIssuer(template, iss string) (string, bool) {
	const ph = "{tenantid}"
	i := strings.Index(template, ph)
	if i < 0 || iss == "" {
		return "", false
	}
	prefix := template[:i]
	suffix := template[i+len(ph):]
	// Normalize trailing slashes on the whole strings before splitting.
	iss = strings.TrimRight(iss, "/")
	suffix = strings.TrimRight(suffix, "/")
	if !strings.HasPrefix(iss, prefix) || !strings.HasSuffix(iss, suffix) {
		return "", false
	}
	tenant := iss[len(prefix) : len(iss)-len(suffix)]
	tenant = strings.Trim(tenant, "/")
	if tenant == "" || strings.ContainsAny(tenant, "/{}") {
		return "", false
	}
	return tenant, true
}

// containsFold reports whether s is in list, case-insensitively (tenant GUIDs are
// case-insensitive).
func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), s) {
			return true
		}
	}
	return false
}

// keyForKID returns the RSA public key for a JWKS key id, fetching/refreshing the
// JWKS on an unknown kid (rate-limited) to handle key rotation.
func (p *Provider) keyForKID(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	p.mu.Lock()
	key := p.jwks[kid]
	stale := time.Since(p.jwksSetAt) >= jwksRefreshMinInterval
	p.mu.Unlock()
	if key != nil {
		return key, nil
	}
	if !stale {
		// Unknown kid but we refreshed very recently: refuse rather than hammer
		// the JWKS endpoint. A legitimate rotation resolves on the next attempt
		// after the throttle window.
		return nil, fmt.Errorf("unknown id_token kid %q (JWKS recently refreshed)", kid)
	}
	if err := p.refreshJWKS(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	key = p.jwks[kid]
	p.mu.Unlock()
	if key == nil {
		return nil, fmt.Errorf("unknown id_token kid %q", kid)
	}
	return key, nil
}

// ensureDiscovered lazily runs OIDC discovery (once) to populate the authorize/
// token/JWKS endpoints, then fetches the JWKS. GitHub (non-OIDC) needs no
// discovery — its endpoints are set by the hub.
func (p *Provider) ensureDiscovered(ctx context.Context) error {
	if !p.IsOIDC {
		return nil
	}
	p.mu.Lock()
	done := p.discovered
	p.mu.Unlock()
	if done {
		return nil
	}
	if p.Issuer == "" {
		return errors.New("OIDC provider has no issuer")
	}
	doc, err := fetchDiscovery(ctx, p.Issuer)
	if err != nil {
		return err
	}
	// Discovery's issuer MUST be present AND consistent with configuration. This
	// prevents a swapped/hostile discovery doc from redirecting token/JWKS to an
	// attacker origin. Per OIDC Discovery the issuer field is REQUIRED.
	//
	// Single-tenant: strict equality with p.Issuer.
	// Multi-tenant: the doc's issuer legitimately carries the "{tenantid}"
	// placeholder (e.g. Entra's common/organizations metadata), so it must equal
	// the configured IssuerTemplate instead — still an exact, non-attacker-
	// controllable match, just against the templated form.
	if p.IssuerTemplate != "" {
		if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(p.IssuerTemplate, "/") {
			return fmt.Errorf("discovery issuer %q != configured issuer template %q", doc.Issuer, p.IssuerTemplate)
		}
	} else if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(p.Issuer, "/") {
		return fmt.Errorf("discovery issuer %q != configured issuer %q", doc.Issuer, p.Issuer)
	}
	p.mu.Lock()
	if p.AuthorizeURL == "" {
		p.AuthorizeURL = doc.AuthorizationEndpoint
	}
	if p.TokenURL == "" {
		p.TokenURL = doc.TokenEndpoint
	}
	if p.JWKSURL == "" {
		p.JWKSURL = doc.JWKSURI
	}
	if p.UserInfoURL == "" {
		p.UserInfoURL = doc.UserInfoEndpoint
	}
	p.discovered = true
	p.mu.Unlock()
	return p.refreshJWKS(ctx)
}

// refreshJWKS fetches and parses the provider's JWKS into RSA public keys.
func (p *Provider) refreshJWKS(ctx context.Context) error {
	if p.JWKSURL == "" {
		return errors.New("no JWKS URL")
	}
	keys, err := fetchJWKS(ctx, p.JWKSURL)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.jwks = keys
	p.jwksSetAt = nowUTC()
	p.mu.Unlock()
	return nil
}

func (p *Provider) claimName(override, def string) string {
	if override != "" {
		return override
	}
	return def
}

// --- discovery / JWKS wire types ---

type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	// UserInfoEndpoint is OPTIONAL per OIDC Discovery — absence just disables
	// the display-claim enrichment fetch, never the login.
	UserInfoEndpoint string `json:"userinfo_endpoint"`
}

func fetchDiscovery(ctx context.Context, issuer string) (*discoveryDoc, error) {
	url := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC discovery returned %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	var doc discoveryDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse discovery: %w", err)
	}
	if doc.Issuer == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" || doc.AuthorizationEndpoint == "" {
		return nil, errors.New("discovery document missing required fields (issuer/authorization/token/jwks)")
	}
	return &doc, nil
}

type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func fetchJWKS(ctx context.Context, jwksURL string) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("JWKS fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS returned %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	var set jwksResponse
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pk, err := rsaPublicKeyFromJWK(k)
		if err != nil {
			continue // skip a malformed key rather than fail the whole set
		}
		out[k.Kid] = pk
	}
	if len(out) == 0 {
		return nil, errors.New("JWKS contained no usable RSA keys")
	}
	return out, nil
}

// rsaPublicKeyFromJWK reconstructs an RSA public key from a JWK's base64url n/e.
func rsaPublicKeyFromJWK(k jwkKey) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() < 2 {
		return nil, errors.New("invalid RSA exponent")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// --- standard claim names ---

const (
	claimSub        = "sub"
	claimName       = "name"
	claimGivenName  = "given_name"
	claimFamilyName = "family_name"
	claimPicture    = "picture"
	claimEmail      = "email"
	claimNonce      = "nonce"
)

func stringClaim(m jwt.MapClaims, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
