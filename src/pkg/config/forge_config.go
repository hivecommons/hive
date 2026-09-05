// Forge identity and connection configuration: GitHubConfig (App/OAuth/GHE
// resolution), GitLabConfig, GiteaConfig, and the known-forge identity table.
package config

import (
	"fmt"
	"strings"
)

type GitHubConfig struct {
	AppID              int64  `yaml:"app_id"`
	InstallationID     int64  `yaml:"installation_id"`
	DocsInstallationID int64  `yaml:"docs_installation_id"`
	KeyFile            string `yaml:"key_file"`
	Token              string `yaml:"token"`
	OAuthClientID      string `yaml:"oauth_client_id"`
	// Forge_ names the GitHub instance this hive's App and repos live on, as a
	// bare host: "github.com" or "github.ibm.com". It is the SINGLE
	// AUTHORITATIVE identity field — app_id, app_slug, api_url and base_url are
	// all derivable from it (see Forge, ResolvedAppID, forgeIdentities).
	//
	// Read it through the Forge() method, never directly: Forge() applies the
	// dual-read fallback that keeps a not-yet-backfilled spoke resolving exactly
	// as it does today. The trailing underscore exists solely because the method
	// owns the good name; the YAML key is a clean `forge`.
	//
	// Empty is valid during the migration window and means "infer from the URL
	// fields". Once the fleet is backfilled it becomes mandatory — per the
	// owner: explicitly defined on every spoke on every cluster, no empties, no
	// inheritance. The implicit empty is what disguised the 2026-07-31 damage.
	Forge_ string `yaml:"forge,omitempty"`
	// AppSlug is the GitHub App URL slug for the install link.
	// For public GitHub: "kubestellar-hive". For GHE: your app's slug.
	AppSlug string `yaml:"app_slug"`
	// APIURL is the GitHub API base URL. Defaults to DefaultGitHubAPIURL.
	// For GitHub Enterprise, set to e.g. "https://github.ibm.com/api/v3".
	APIURL string `yaml:"api_url"`
	// BaseURL is the GitHub web base URL. Defaults to DefaultGitHubBaseURL.
	// For GitHub Enterprise, set to e.g. "https://github.ibm.com".
	BaseURL string `yaml:"base_url"`
	// AppAuthoredPRs controls App-bot authorship: when enabled AND ai_author is
	// empty, agents author PRs/commits as the GitHub App bot ("<slug>[bot]") via
	// the App installation token, instead of the Copilot-login user.
	//
	// It is a *bool so absent (nil) is distinct from an explicit false. Default is
	// ON (see AppAuthoredPRsEnabled): App-bot authorship is now the fleet norm, so
	// a hive that says nothing gets it. Set `app_authored_prs: false` to opt a
	// specific hive OUT. This only affects write-capable hives — the token is
	// injected solely for agents whose tier CanPush() and only when a real App is
	// installed (m.appAuth != nil), so an App-less or advisory hive is unaffected.
	AppAuthoredPRs *bool `yaml:"app_authored_prs,omitempty"`
	// AppSignedCommits makes every agent-opened PR carry commits that GitHub
	// itself signed. Agents commit with plain git in their pane and cannot sign:
	// a GitHub App has no account to hold a GPG or SSH key. GitHub does sign
	// commits created through the createCommitOnBranch GraphQL mutation and
	// marks them Verified, so when this is on the PR-request watcher — the one
	// server-side choke point every agent PR passes through — re-authors the
	// head branch through that mutation with the App installation token before
	// opening the PR: one commit, signed by GitHub, authored by "<slug>[bot]",
	// carrying the agents' original messages (DCO trailers included). Without
	// it a `required_signatures` ruleset on the base branch blocks every agent
	// PR from merging no matter who approves it.
	//
	// Opt-in (nil == false) — it rewrites the agent's branch. Only meaningful
	// with an installed App; PAT-authenticated hives ignore it. Falls back to the
	// plain push, with a WARN and a reason in the request's result file, for
	// changes the mutation cannot express (file modes, symlinks, submodules) or
	// that are too large — the PR still opens, just unsigned.
	AppSignedCommits *bool `yaml:"app_signed_commits,omitempty"`
	// OAuthBaseURLOverride and OAuthAPIURLOverride redirect the DEVICE-FLOW LOGIN
	// endpoints away from public github.com. They are a TEST SEAM ONLY — see the
	// comment on OAuthBaseURL(). They carry the `-` yaml tag so they can never be
	// set from a hive.yaml, on the hub, or over the config API: only Go code in a
	// test can assign them.
	OAuthBaseURLOverride string `yaml:"-"`
	OAuthAPIURLOverride  string `yaml:"-"`
}

// PlaceholderAppID is the sentinel `github.app_id` written into a hive's config
// when the hive is provisioned BEFORE its real GitHub App exists — a pooled
// "available-*" placeholder, or a hive whose owner has not installed the App
// yet. Config validation requires either github.token or github.app_id, so the
// seed cannot leave app_id empty; this value fills the slot without ever naming
// a real App.
//
// It is NOT a valid GitHub App ID and must never be handed to
// github.NewAppAuth. Every test for "is a GitHub App configured?" must use
// GitHubConfig.HasApp() rather than a bare `AppID != 0` — the sentinel is
// non-zero, so a bare zero-test reports a placeholder as a real App. That is
// exactly the bug that put hosted hives into permanent CrashLoopBackOff: the
// moment installation_id became non-zero (the owner installed the App), the
// spoke committed to App auth, failed to read a PEM that was never provisioned,
// and exited before the HTTP listener bound — invisible from the dashboard.
const PlaceholderAppID int64 = 999999999

const (
	// DefaultGitHubAPIURL is the default GitHub API endpoint (public github.com).
	DefaultGitHubAPIURL = "https://api.github.com"
	// DefaultGitHubBaseURL is the default GitHub web URL (public github.com).
	DefaultGitHubBaseURL = "https://github.com"
	// DefaultGitHubAppSlug is the public Hive GitHub App slug.
	DefaultGitHubAppSlug = "kubestellar-hive"
	// DefaultOAuthClientID is the PUBLIC github.com Hive App client ID used for
	// device-flow login. Login is always github.com (see OAuthBaseURL), so this
	// is the correct client for every hive — including GHE hives, whose users
	// still sign in with a github.com identity. No secret; safe to hardcode.
	DefaultOAuthClientID = "Ov23ligE2p0gjXg6xAUf"
)

// GitLabConfig configures the GitLab forge (used when project.forge == "gitlab").
// It is entirely additive: a config that never mentions gitlab is unaffected,
// and the zero value points at public gitlab.com.
//
// The token itself is NOT stored here. Following Hive's no-hardcoded-secrets
// rule, TokenEnv names the environment variable to read the GitLab access token
// from at runtime (default GITLAB_TOKEN); the secret is never written to config.
type GitLabConfig struct {
	// URL is the GitLab instance root, e.g. "https://gitlab.com" (default) or a
	// self-managed instance "https://gitlab.example.com". The "/api/v4" path is
	// appended by the client.
	URL string `yaml:"gitlab_url,omitempty"`
	// TokenEnv is the environment variable holding the GitLab access token.
	// Defaults to DefaultGitLabTokenEnv. The token value is resolved via
	// os.Getenv at runtime and never persisted in config.
	TokenEnv string `yaml:"token_env,omitempty"`
}

const (
	// DefaultGitLabURL is the public GitLab SaaS instance root.
	DefaultGitLabURL = "https://gitlab.com"
	// DefaultGitLabTokenEnv is the default env var name for the GitLab token.
	DefaultGitLabTokenEnv = "GITLAB_TOKEN"
)

// InstanceURL returns the configured GitLab instance root, defaulting to
// DefaultGitLabURL when unset.
func (g GitLabConfig) InstanceURL() string {
	if g.URL == "" {
		return DefaultGitLabURL
	}
	return g.URL
}

// TokenEnvName returns the env var name to read the GitLab token from,
// defaulting to DefaultGitLabTokenEnv when unset.
func (g GitLabConfig) TokenEnvName() string {
	if g.TokenEnv == "" {
		return DefaultGitLabTokenEnv
	}
	return g.TokenEnv
}

// GiteaConfig configures the Gitea/Forgejo forge (used when project.forge ==
// "gitea"). Like GitLabConfig it is entirely additive: a config that never
// mentions gitea is unaffected.
//
// Gitea has no single public SaaS host, so URL has no default and must be set
// when the Gitea forge is selected. Following Hive's no-hardcoded-secrets rule,
// the token itself is NOT stored here: TokenEnv names the environment variable
// to read the Gitea access token from at runtime (default GITEA_TOKEN).
type GiteaConfig struct {
	// URL is the Gitea/Forgejo instance root, e.g. "https://codeberg.org" or a
	// self-managed "https://gitea.example.com". The "/api/v1" path is appended by
	// the client. There is no default; it must be supplied for the Gitea forge.
	URL string `yaml:"gitea_url,omitempty"`
	// TokenEnv is the environment variable holding the Gitea access token.
	// Defaults to DefaultGiteaTokenEnv. The token value is resolved via
	// os.Getenv at runtime and never persisted in config.
	TokenEnv string `yaml:"token_env,omitempty"`
}

const (
	// DefaultGiteaTokenEnv is the default env var name for the Gitea token.
	DefaultGiteaTokenEnv = "GITEA_TOKEN"
)

// InstanceURL returns the configured Gitea instance root. Unlike GitLab there is
// no public default, so this returns whatever was configured (possibly empty);
// selecting the Gitea forge with an empty URL is a configuration error surfaced
// at client construction.
func (g GiteaConfig) InstanceURL() string {
	return g.URL
}

// TokenEnvName returns the env var name to read the Gitea token from, defaulting
// to DefaultGiteaTokenEnv when unset.
func (g GiteaConfig) TokenEnvName() string {
	if g.TokenEnv == "" {
		return DefaultGiteaTokenEnv
	}
	return g.TokenEnv
}

// NOTE: two independent "forge" concerns coexist in this file and do NOT collide:
//
//   - The SPOKE FORGE SELECTOR (above): ForgeGitHub/ForgeGitLab/ForgeGitea and
//     ProjectConfig.ForgeKind() choose which forge FAMILY (GitHub, GitLab, Gitea)
//     a spoke executes against, selecting the pkg/forge adapter.
//   - The FORGE IDENTITY system (below): ForgePublic/ForgeGHEIBM, forgeIdentity
//     and GitHubConfig.Forge() derive which GitHub INSTANCE (github.com vs a
//     GitHub Enterprise host) a hive's App/repos live on.
//
// Different names, different receivers (ProjectConfig vs GitHubConfig), different
// values ("github"/"gitlab"/"gitea" vs "github.com"/"github.ibm.com"). Both are
// retained verbatim; nothing is declared twice.

// ─────────────────────────────────────────────────────────────────────────
// FORGE IDENTITY CONSTANTS
//
// A "forge" is the GitHub instance a hive's App and repos live on. Every
// identity value below is derived from the forge — never written independently
// — so the mixed states that broke seven hives on 2026-07-31 (a GHE app_id
// beside an empty, therefore-public api_url) cannot be constructed.
//
// These are the SPOKE-side well-known defaults. The hub carries the
// authoritative per-cluster registration in clusters.json, which may name a
// different App on either forge; these values are what a spoke falls back to
// when the hub has pushed nothing.
// ─────────────────────────────────────────────────────────────────────────
const (
	// ForgePublic is the canonical host label for public github.com. It is a
	// HOSTNAME, not a URL and not a sentinel word — unlike the hub's legacy
	// "public" request sentinel, it is directly comparable to HostLabel().
	ForgePublic = "github.com"

	// ForgeGHEIBM is the IBM GitHub Enterprise host. It is the only GHE forge
	// the fleet currently runs on; naming it as a constant keeps the derivation
	// table literal-free and makes a second GHE forge a one-line addition.
	ForgeGHEIBM = "github.ibm.com"
)

// forgeIdentity is the complete, indivisible identity set for one forge. The
// four fields move together or not at all — that atomicity is the entire point
// of deriving them from a single `forge` value.
type forgeIdentity struct {
	AppID   int64
	AppSlug string
	APIURL  string
	BaseURL string
}

// Forge identity values.
//
// PublicGitHubAppID/PublicGitHubAppSlug and the Enterprise* set are declared
// once, in provenance.go, alongside the rules that validate them — a second
// declaration of the same literal is exactly the drift those rules exist to
// catch. The GHEIBM* names below are aliases so the derivation table reads in
// forge terms without duplicating any value.
const (
	// GHEIBMAppID is the github.ibm.com Hive App's numeric ID.
	GHEIBMAppID = EnterpriseGitHubAppID
	// GHEIBMAppSlug is the github.ibm.com Hive App's URL slug. A GHE instance
	// hosts its own App registration and it is NOT named "kubestellar-hive";
	// falling back to the public slug builds an install link that 404s.
	GHEIBMAppSlug = EnterpriseGitHubAppSlug
	// GHEIBMBaseURL is the github.ibm.com web base URL.
	GHEIBMBaseURL = EnterpriseGitHubBaseURL
	// GHEIBMAPIURL is the github.ibm.com REST API root. GHE serves its API
	// under /api/v3 rather than on a separate api.* hostname.
	GHEIBMAPIURL = EnterpriseGitHubAPIURL
)

// forgeIdentities is the derivation table: forge host -> its complete identity
// set. This is the single source of truth that replaces four independently
// writable fields. Adding a forge means adding a row here; there is no way to
// express a half-identity.
var forgeIdentities = map[string]forgeIdentity{
	ForgePublic: {
		AppID:   PublicGitHubAppID,
		AppSlug: PublicGitHubAppSlug,
		// Empty, not DefaultGitHubAPIURL/DefaultGitHubBaseURL: the public forge's
		// canonical on-disk representation has ALWAYS been the empty string, and
		// ResolvedAPIURL/ResolvedBaseURL already map empty -> the public default.
		// Storing the URLs literally here would make the migration a visible
		// rewrite of every healthy public spoke's config rather than a no-op.
		APIURL:  "",
		BaseURL: "",
	},
	ForgeGHEIBM: {
		AppID:   GHEIBMAppID,
		AppSlug: GHEIBMAppSlug,
		APIURL:  GHEIBMAPIURL,
		BaseURL: GHEIBMBaseURL,
	},
}

// KnownForge reports whether host names a forge with a known identity set.
// An unknown forge is not an error — a hive may legitimately run against a
// third GHE instance the table does not name — but its identity cannot be
// DERIVED, so explicit fields remain load-bearing there.
func KnownForge(host string) bool {
	_, ok := forgeIdentities[normalizeForgeHost(host)]
	return ok
}

// normalizeForgeHost reduces any spelling of a forge — bare host, web URL, API
// URL, mixed case, trailing slash — to its canonical bare-host label.
func normalizeForgeHost(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(strings.Trim(s, "/"), "/api/v3")
	host := strings.SplitN(s, "/", 2)[0]
	if host == "" || host == "api.github.com" {
		return ForgePublic
	}
	return host
}

// Forge returns the forge this hive's App and repos live on, as a bare host
// label ("github.com", "github.ibm.com").
//
// PRECEDENCE — and this is the whole design in three lines:
//
//  1. An explicit `forge:` wins. It is the single authoritative field.
//  2. Otherwise infer from the legacy URL fields via HostLabel()/IsGHE().
//  3. Empty everything means public github.com, as it always has.
//
// Step 2 is the DUAL-READ WINDOW. Until every spoke carries `forge:`, a spoke
// that has not been backfilled must resolve exactly as it does today — so this
// never returns anything a pre-migration hive would not already have resolved.
func (g GitHubConfig) Forge() string {
	if f := strings.TrimSpace(g.Forge_); f != "" {
		return normalizeForgeHost(f)
	}
	return normalizeForgeHost(g.HostLabel())
}

// ResolvedAppID returns the GitHub App ID for this hive, completing the
// resolver set alongside ResolvedAPIURL/ResolvedBaseURL/ResolvedAppSlug.
//
// An explicitly configured app_id wins — including the placeholder sentinel,
// which callers detect with IsPlaceholderApp() and must keep seeing. Only when
// app_id is unset is it derived from the forge, which is what lets a hive be
// described by `forge:` alone.
func (g GitHubConfig) ResolvedAppID() int64 {
	if g.AppID != 0 {
		return g.AppID
	}
	if id, ok := forgeIdentities[g.Forge()]; ok {
		return id.AppID
	}
	return 0
}

// ForgeIdentityMismatches reports every identity field that CONTRADICTS this
// hive's forge. Empty means the identity set is coherent.
//
// This is the check that would have caught the 2026-07-31 incident at the
// write boundary. It differs from IdentitySetIssues in the way that matters:
// IdentitySetIssues compares the four fields against EACH OTHER, so it is blind
// whenever the pushed value is the only GHE-looking field present (a GHE app_id
// beside a public slug and empty URLs produces no disagreement at all — verified).
// This compares each field against the DECLARED forge, which is an external
// anchor the pusher does not control.
//
// A field that is empty is not a mismatch: empty means "derive me".
func (g GitHubConfig) ForgeIdentityMismatches() []string {
	forge := g.Forge()
	want, known := forgeIdentities[forge]
	if !known {
		// A forge outside the table has no derivable identity to compare
		// against. Reporting mismatches here would flag every legitimate
		// third-forge hive; saying nothing keeps explicit fields authoritative
		// exactly where they still need to be.
		return nil
	}
	var out []string
	// app_id is compared only when a REAL App is configured. The placeholder
	// sentinel means "no App yet" and legitimately appears on both forges.
	if g.AppID != 0 && !g.IsPlaceholderApp() && g.AppID != want.AppID {
		out = append(out, fmt.Sprintf(
			"app_id %d does not belong to forge %s (expected %d) — an App ID from another forge returns 404 Integration not found on token creation",
			g.AppID, forge, want.AppID))
	}
	if s := strings.TrimSpace(g.AppSlug); s != "" && !strings.EqualFold(s, want.AppSlug) {
		out = append(out, fmt.Sprintf(
			"app_slug %q does not belong to forge %s (expected %q) — the App install link would 404",
			s, forge, want.AppSlug))
	}
	if u := strings.TrimSpace(g.APIURL); u != "" && normalizeForgeHost(u) != forge {
		out = append(out, fmt.Sprintf(
			"api_url %q names forge %s, not %s", u, normalizeForgeHost(u), forge))
	}
	if u := strings.TrimSpace(g.BaseURL); u != "" && normalizeForgeHost(u) != forge {
		out = append(out, fmt.Sprintf(
			"base_url %q names forge %s, not %s", u, normalizeForgeHost(u), forge))
	}
	return out
}

// IsPlaceholderApp reports whether app_id is the "no real App yet" sentinel.
func (g GitHubConfig) IsPlaceholderApp() bool {
	return g.AppID == PlaceholderAppID
}

// HasApp reports whether a REAL GitHub App is configured — a non-zero app_id
// that is not the placeholder sentinel. This is the only correct presence test
// for App auth; `AppID != 0` treats a placeholder as real.
//
// It deliberately says nothing about installation_id or the key file: those are
// separate readiness conditions (see HasUsableApp) that a hive can acquire
// later, over the heartbeat or the config API.
func (g GitHubConfig) HasApp() bool {
	return g.AppID != 0 && !g.IsPlaceholderApp()
}

// HasUsableApp reports whether App auth can actually be attempted: a real
// app_id AND an installation to mint tokens against. The key file is resolved
// separately (config value, then $GH_APP_KEY_FILE, then the on-disk fallbacks),
// so it is not part of this test.
func (g GitHubConfig) HasUsableApp() bool {
	return g.HasApp() && g.InstallationID != 0
}

// ConfiguredButUninstalled reports the half-configured state that must NEVER
// read as healthy: a real App is named but there is no installation to mint
// tokens against. Every token this hive will ever hold is App-minted, so this
// state means auth is a countdown — any client still working is riding a
// cached token that cannot be renewed. Callers use this to raise the install
// banner and fail github_auth from CONFIG truth, not from whether a residual
// token still breathes.
func (g GitHubConfig) ConfiguredButUninstalled() bool {
	return g.HasApp() && g.InstallationID == 0
}

// ResolvedAPIURL returns the configured API URL or the default for github.com.
func (g GitHubConfig) ResolvedAPIURL() string {
	if g.APIURL != "" {
		return g.APIURL
	}
	return DefaultGitHubAPIURL
}

// ResolvedBaseURL returns the base URL of the GitHub the hive's App/repos live
// on, as a full "https://host" URL. It prefers an explicit base_url, but falls
// back to the host of api_url when base_url is blank — pooled/placeholder GHE
// hives legitimately carry base_url: "" with api_url: https://github.ibm.com/api/v3,
// and resolving those to github.com breaks App-host-derived URLs (notably the
// App install link, which would point at github.com/apps/... instead of the GHE
// github.ibm.com/github-apps/... path). This mirrors HostLabel()'s base-or-api
// derivation. Login/device-flow paths deliberately do NOT use this — see
// OAuthBaseURL, which is always public github.com.
func (g GitHubConfig) ResolvedBaseURL() string {
	if g.BaseURL != "" {
		return g.BaseURL
	}
	// base_url blank: derive from api_url's host if it points at a GHE instance.
	if host := g.HostLabel(); host != DefaultGitHubBaseURL[len("https://"):] {
		return "https://" + host
	}
	return DefaultGitHubBaseURL
}

// IsGHE returns true if the hive's App/repos live on a GitHub Enterprise instance
// rather than public github.com. It looks at BOTH base_url and api_url (via
// HostLabel), so a hive carrying only a GHE api_url (base_url: "") is still
// correctly recognized as GHE — matching ResolvedBaseURL and HostLabel.
func (g GitHubConfig) IsGHE() bool {
	return g.HostLabel() != DefaultGitHubBaseURL[len("https://"):]
}

// HostLabel returns the bare GitHub hostname this hive's App/repos live on:
// "github.com" for public GitHub, or the GHE hostname (e.g. "github.ibm.com").
//
// It is the DEFINITIVE host for display and reporting, and deliberately looks at
// BOTH base_url and api_url. Many hives (notably pooled placeholders) carry an
// empty base_url but a GHE api_url (api_url: https://github.ibm.com/api/v3,
// base_url: "") — reporting the host from base_url alone renders those as
// github.com and under-counts GHE hives in the spokes table. Preferring base_url
// when set, then falling back to the api_url's host, makes the reported host
// match the GitHub the hive actually talks to.
func (g GitHubConfig) HostLabel() string {
	pick := g.BaseURL
	if pick == "" {
		pick = g.APIURL
	}
	pick = strings.TrimSpace(pick)
	pick = strings.TrimPrefix(pick, "https://")
	pick = strings.TrimPrefix(pick, "http://")
	pick = strings.TrimSuffix(strings.Trim(pick, "/"), "/api/v3")
	host := strings.SplitN(pick, "/", 2)[0]
	if host == "" || strings.EqualFold(host, "api.github.com") {
		return DefaultGitHubBaseURL[len("https://"):] // "github.com"
	}
	return host
}

// OAuthBaseURL and OAuthAPIURL are the hosts used for DEVICE-FLOW LOGIN and
// user-identity token validation. They are ALWAYS public github.com, decoupled
// from the App/repo host (ResolvedBaseURL/ResolvedAPIURL, which may be GHE).
//
// Why the split: the Hive login OAuth app (oauth_client_id, default the public
// "Ov23ligE2p0gjXg6xAUf") is a github.com app, and every user signs in with a
// github.com identity — even on a hive whose REPOS and GitHub App live on GHE.
// Pointing the device-flow endpoints at a GHE host (as ResolvedBaseURL does for
// a GHE hive) makes GitHub return "Not Found" and the dashboard shows "could not
// verify GitHub identity". Login must therefore use these fixed public hosts;
// only the App-token / repo-API paths follow the per-hive (possibly GHE) host.
// The oauth_*_url_override fields exist ONLY as a test seam. They are
// deliberately absent from every production config: a hive that sets them would
// send its users' device-flow login to a non-github.com host, which is exactly
// the misconfiguration the split above prevents. Tests point them at an
// httptest server so the login path can be exercised without touching the real
// github.com endpoints — see TestMain in pkg/dashboard, which additionally
// hard-fails any test that leaves them unset and would otherwise dial out.
func (g GitHubConfig) OAuthBaseURL() string {
	if g.OAuthBaseURLOverride != "" {
		return g.OAuthBaseURLOverride
	}
	return DefaultGitHubBaseURL
}

func (g GitHubConfig) OAuthAPIURL() string {
	if g.OAuthAPIURLOverride != "" {
		return g.OAuthAPIURLOverride
	}
	return DefaultGitHubAPIURL
}

// OAuthClientIDResolved returns the client ID for device-flow login: the
// configured oauth_client_id, or the public github.com default. Because login
// is always github.com, the default is correct for every hive — a GHE hive must
// not fall back to a GHE client ID here, or device flow against github.com fails
// with an unknown-client error. A hive that has explicitly set oauth_client_id
// to a github.com app keeps that; the default only fills a blank.
func (g GitHubConfig) OAuthClientIDResolved() string {
	if g.OAuthClientID != "" {
		return g.OAuthClientID
	}
	return DefaultOAuthClientID
}

// ResolvedAppSlug returns the app slug for this hive: the configured value,
// else the slug DERIVED from the forge, else the public default.
//
// The forge step is what stops a GHE hive that names only `forge:` from falling
// back to "kubestellar-hive" — a slug that names no App on GHE and builds an
// install link that 404s. That 404 is the failure this whole design exists to
// make unrepresentable, so the derivation belongs here rather than at call sites.
func (g GitHubConfig) ResolvedAppSlug() string {
	if g.AppSlug != "" {
		return g.AppSlug
	}
	if id, ok := forgeIdentities[g.Forge()]; ok && id.AppSlug != "" {
		return id.AppSlug
	}
	return DefaultGitHubAppSlug
}

// BotLogin returns the GitHub App bot login ("<app-slug>[bot]") when a GitHub
// App is configured (AppID set), or "" otherwise. This is the account that
// actually authors PRs and commits when the hive authenticates as an
// installation rather than a personal token.
func (g GitHubConfig) BotLogin() string {
	// Only a REAL, installed App has a bot that can author anything. A bare
	// `AppID != 0` also passes for the placeholder sentinel (PlaceholderAppID)
	// and for a real app_id that was never installed (installation_id: 0) —
	// neither can mint a token or author a PR, so neither has a bot login. Using
	// HasUsableApp() keeps EffectiveAIAuthor() empty for those, so the UI shows
	// "-" (no author) rather than a phantom "<slug>[bot]" for an App-less hive.
	if !g.HasUsableApp() {
		return ""
	}
	return g.ResolvedAppSlug() + "[bot]"
}

// EffectiveAIAuthor returns the GitHub identity agents should author PRs and
// commits as. An explicitly configured project.ai_author always wins. When
// ai_author is empty, the result depends on the opt-in github.app_authored_prs
// flag: if set, agents author as the GitHub App bot ("<slug>[bot]") — deriving
// the bot login here rather than persisting it into ai_author keeps App-bot mode
// durable across restarts (clearing ai_author is enough; nothing writes the bot
// name back into config). If the flag is NOT set, this returns "" exactly as
// before, so a hive that has not opted in behaves identically on upgrade.
func (c *Config) EffectiveAIAuthor() string {
	if c.Project.AIAuthor != "" {
		return c.Project.AIAuthor
	}
	if c.GitHub.AppAuthoredPRsEnabled() {
		return c.GitHub.BotLogin()
	}
	return ""
}

// AppAuthoredPRsEnabled reports whether App-bot authorship is on for this hive.
// The default is ON (nil == true): App-bot authorship is the fleet norm, so a
// hive that never set app_authored_prs gets it. Only an explicit
// `app_authored_prs: false` disables it. Note this is only the INTENT — the
// token is still injected solely for CanPush() tiers with a real App installed,
// so an App-less or advisory hive never actually writes regardless of this flag.
func (g GitHubConfig) AppAuthoredPRsEnabled() bool {
	if g.AppAuthoredPRs == nil {
		return true
	}
	return *g.AppAuthoredPRs
}

// AppSignedCommitsEnabled reports whether the PR-request watcher re-authors
// agent branches through createCommitOnBranch so their commits are GitHub-
// signed. Opt-in: nil and false both mean off. See AppSignedCommits.
func (g GitHubConfig) AppSignedCommitsEnabled() bool {
	return g.AppSignedCommits != nil && *g.AppSignedCommits
}

// AppInstallURL returns the full URL to install the GitHub App.
// For GHE: {base_url}/github-apps/{slug}/installations/new
// For github.com: https://github.com/apps/{slug}/installations/new
//
// It returns "" for a GitHub Enterprise host whose App slug is neither
// configured nor derivable from the forge-identity table.
//
// # WHY EMPTY RATHER THAN A BEST-EFFORT URL
//
// DefaultGitHubAppSlug ("kubestellar-hive") names the App registered on PUBLIC
// github.com. GitHub Enterprise hosts a SEPARATE App registry, and an enterprise
// registration is rarely given the same slug — so falling back to the default on
// a GHE host emits a link to an App that provably does not exist there, and the
// user lands on a GitHub 404. That is strictly worse than no link: a dead link
// reads as "this product is broken", and it sent a real user to
// github.ibm.com/github-apps/kubestellar-hive/installations/new.
//
// A KNOWN forge is different: forgeIdentities records the slug of the App
// actually registered on that host, so deriving from the table can never build
// the public-slug 404 above. This matters after a "Reset Forge App": the raw
// app_slug/base_url go blank while the resolved identity (what the Forge App
// tab displays) still names github.ibm.com — and returning "" here suppressed
// the install banner on exactly the hive that needed it (vllmd-13).
//
// For a GHE host OUTSIDE the table the empty string remains the honest answer
// — "this host's App slug was never recorded" — and callers render it as a
// legible message naming the missing config instead of a button that 404s.
// Public github.com keeps the default, where it is correct by construction.
func (g GitHubConfig) AppInstallURL() string {
	base := strings.TrimRight(g.ResolvedBaseURL(), "/")
	if g.IsGHE() {
		// An explicit slug wins; otherwise only the forge-identity table may
		// supply one — never DefaultGitHubAppSlug, which names no App here.
		slug := strings.TrimSpace(g.AppSlug)
		if slug == "" {
			if id, ok := forgeIdentities[g.Forge()]; ok {
				slug = id.AppSlug
			}
		}
		if slug == "" {
			return ""
		}
		return base + "/github-apps/" + slug + "/installations/new"
	}
	return base + "/apps/" + g.ResolvedAppSlug() + "/installations/new"
}
