package pushbroker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// DefaultForgeBaseURL is the forge a repo is cloned from and pushed to when
// nothing names another one: github.com, the only host hive used before
// Forgejo repos could be worked (hivecommons/hive#19494).
const DefaultForgeBaseURL = "https://github.com"

// credentialUsername is the user name git sends with a minted token. GitHub
// requires a non-empty one and ignores its value; Forgejo authenticates the
// token in the password field and ignores the user name too.
const credentialUsername = "x-access-token"

// ForgeRemote is where one repo is cloned from and pushed to, and the minter
// whose tokens that forge accepts. The host resolves it per repo (from
// project.forge, repo_policies[].forge and the forge's own URL); crux never
// reads hive's config.
type ForgeRemote struct {
	// BaseURL is the forge instance root, e.g. "https://forgejo.example.com"
	// or "https://example.com/forgejo". Empty means DefaultForgeBaseURL.
	BaseURL string
	// Minter mints the token sent to BaseURL's host, and to no other host.
	Minter TokenMinter
}

// ForgeRemoteResolver returns the remote for an "owner/name" repo. ok=false
// means the repo is on github.com and the caller keeps its GitHub App minter.
// An error means the repo is on a forge hive cannot clone from or push to;
// the caller must fail rather than fall back to github.com, because the
// fallback would send a GitHub token to the wrong host or work the wrong repo.
type ForgeRemoteResolver func(repo string) (remote ForgeRemote, ok bool, err error)

// TieredMinter is a minter that can narrow its tokens to a token tier. The
// GitHub App minter does this through installation-token permissions; a
// forge minter does it however its forge allows.
type TieredMinter interface {
	TokenMinter
	WithTier(tier string) TokenMinter
}

// WithTier returns m narrowed to tier, or m unchanged when it cannot narrow.
func WithTier(m TokenMinter, tier string) TokenMinter {
	switch minter := m.(type) {
	case nil:
		return nil
	case GitHubAppMinter:
		minter.Tier = tier
		return minter
	case *GitHubAppMinter:
		if minter == nil {
			return nil
		}
		cp := *minter
		cp.Tier = tier
		return cp
	case TieredMinter:
		return minter.WithTier(tier)
	default:
		return m
	}
}

// NormalizeForgeBaseURL validates a forge instance root and returns it
// without a trailing slash. Empty is DefaultForgeBaseURL. It must be an
// http(s) URL with a host and no user info, query or fragment: user info would
// put a credential in every clone URL, and a query or fragment has no meaning
// in a git remote.
func NormalizeForgeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultForgeBaseURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("pushbroker: forge URL %q is not an http(s) URL with a host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("pushbroker: forge URL %q must be a bare instance root (no credentials, query or fragment)", raw)
	}
	// A token sent over plain http can be read on the wire. Only a loopback
	// forge (a local test instance) may use it.
	if strings.EqualFold(u.Scheme, "http") && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("pushbroker: forge URL %q must use https; plain http is allowed only for a loopback host", raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// isLoopbackHost reports whether host is localhost or a loopback IP.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ForgeRepoURL is the clone and push URL of an "owner/name" repo on the forge
// at base. Empty when repo is not an "owner/name" pair or base is invalid.
func ForgeRepoURL(base, repo string) string {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return ""
	}
	root, err := NormalizeForgeBaseURL(base)
	if err != nil {
		return ""
	}
	return root + "/" + owner + "/" + name + ".git"
}

// credentialOrigin is the scheme://host[:port] git matches a credential
// context against. Without credential.useHttpPath git's context carries no
// path, so the origin, not the instance root, is what a URL-scoped helper key
// must name.
func credentialOrigin(base string) (string, error) {
	root, err := NormalizeForgeBaseURL(base)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(root)
	if err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

// CredentialHelperKey is the git config key of a credential helper that git
// consults only for the forge at base. Git asks a URL-scoped helper only when
// the scheme, host and port of the request match exactly, so a token handed
// to it can never reach a redirect target, a submodule host or any other
// remote.
func CredentialHelperKey(base string) (string, error) {
	origin, err := credentialOrigin(base)
	if err != nil {
		return "", err
	}
	return "credential." + origin + ".helper", nil
}

// CredentialStoreLine is the `git credential-store` line for token on the
// forge at base. The store helper answers only for the exact scheme, host and
// port in the line.
func CredentialStoreLine(base, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", errors.New("pushbroker: empty forge token")
	}
	if strings.ContainsAny(token, "\r\n@/:") {
		return "", errors.New("pushbroker: forge token contains a character that cannot appear in a credential URL")
	}
	origin, err := credentialOrigin(base)
	if err != nil {
		return "", err
	}
	scheme, host, _ := strings.Cut(origin, "://")
	return scheme + "://" + credentialUsername + ":" + token + "@" + host + "\n", nil
}

// StaticTokenMinter hands out one token read from Token at every mint. It is
// for a forge that has no per-repo or per-tier token of its own (a Forgejo
// access token), so every tier gets the same credential; WithTier is a no-op.
type StaticTokenMinter struct {
	// Token returns the current token, so a rotated secret is picked up on
	// the next mint.
	Token func() string
}

// MintPushToken implements TokenMinter.
func (m StaticTokenMinter) MintPushToken(context.Context, string) (string, error) {
	if m.Token == nil {
		return "", errors.New("pushbroker: static forge token source is not configured")
	}
	token := strings.TrimSpace(m.Token())
	if token == "" {
		return "", errors.New("pushbroker: forge token is empty")
	}
	return token, nil
}
