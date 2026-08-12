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
		return nil, err
	}
	rawIDToken, err := p.fetchIDToken(ctx, code, redirectURI)
	if err != nil {
		return nil, err
	}
	return p.verifyIDToken(ctx, rawIDToken, expectNonce)
}

// fetchIDToken posts the code to the token endpoint and returns the raw id_token.
func (p *Provider) fetchIDToken(ctx context.Context, code, redirectURI string) (string, error) {
	form := neturlValues()
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", p.ClientID)
	form.Set("client_secret", p.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, "POST", p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var tok struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	if tok.Error != "" {
		return "", fmt.Errorf("token endpoint error %q: %s", tok.Error, tok.ErrorDesc)
	}
	if tok.IDToken == "" {
		return "", errors.New("token response carried no id_token")
	}
	return tok.IDToken, nil
}

// verifyIDToken validates the id_token's signature and claims, then extracts
// identity. It enforces, in order: a supported RS256/RS384/RS512 alg (never
// "none", never HMAC — which would let a client-known secret forge a token); the
// signing key resolved from JWKS by `kid`; iss == p.Issuer; aud contains
// p.ClientID; exp not passed (with the library's small leeway); and nonce ==
// expectNonce. Any failure returns an error and NO claims.
func (p *Provider) verifyIDToken(ctx context.Context, raw, expectNonce string) (*Claims, error) {
	keyFunc := func(t *jwt.Token) (interface{}, error) {
		// Reject anything but RSA signing. "none" and HMAC ("HS*") are the two
		// classic OIDC token-forgery vectors; the parser options below also pin
		// valid methods, this is defense in depth on the key side.
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
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

	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512"}),
		jwt.WithIssuer(p.Issuer),
		jwt.WithAudience(p.ClientID),
		jwt.WithExpirationRequired(),
	)
	claims := jwt.MapClaims{}
	if _, err := parser.ParseWithClaims(raw, claims, keyFunc); err != nil {
		return nil, fmt.Errorf("id_token verification failed: %w", err)
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

	sub := stringClaim(claims, p.claimName(p.SubjectClaim, claimSub))
	if sub == "" {
		return nil, errors.New("id_token has no subject")
	}
	return &Claims{
		Subject:   sub,
		Name:      stringClaim(claims, p.claimName(p.NameClaim, claimName)),
		AvatarURL: stringClaim(claims, p.claimName(p.AvatarClaim, claimPicture)),
		Email:     stringClaim(claims, p.claimName(p.EmailClaim, claimEmail)),
	}, nil
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
	// Discovery's issuer MUST be present AND equal the configured issuer. This
	// prevents a swapped/hostile discovery doc from redirecting token/JWKS to an
	// attacker origin. Requiring it (rather than skipping the check when the doc
	// omits `issuer`) closes the downgrade where a doc with no issuer but hostile
	// jwks_uri would be trusted; per OIDC Discovery the issuer field is REQUIRED.
	if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(p.Issuer, "/") {
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
	defer resp.Body.Close()
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
	defer resp.Body.Close()
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
	claimSub     = "sub"
	claimName    = "name"
	claimPicture = "picture"
	claimEmail   = "email"
	claimNonce   = "nonce"
)

func stringClaim(m jwt.MapClaims, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
