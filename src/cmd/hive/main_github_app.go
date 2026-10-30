package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/apphealth"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/mint"
	"github.com/hivecommons/hive/pkg/spokealerts"
)

// prospectiveGitHubIdentity returns the GitHub identity the spoke WOULD hold
// after adopting ghCfg, or nil when the push speaks to no identity field and
// there is nothing to validate.
//
// It mirrors the adoption rules in the GitHubAppConfigCallback exactly — a
// zero app_id and an empty app_slug both mean "not speaking to this field", and
// the placeholder sentinel is never adopted over a real App. Mirroring rather
// than validating ghCfg alone is what makes the check correct: the damaging
// state is a combination of PUSHED and EXISTING fields (a GHE app_id landing
// beside the spoke's own empty api_url), and validating the push in isolation
// cannot see it.
func prospectiveGitHubIdentity(cur config.GitHubConfig, ghCfg *spoke.HeartbeatGitHubAppConfig) *config.GitHubConfig {
	if ghCfg == nil {
		return nil
	}
	touched := false
	next := cur
	if ghCfg.AppID != 0 && ghCfg.AppID != config.PlaceholderAppID {
		next.AppID = ghCfg.AppID
		touched = true
	}
	if ghCfg.AppSlug != "" && ghCfg.AppSlug != cur.AppSlug {
		next.AppSlug = ghCfg.AppSlug
		touched = true
	}
	// The forge URLs are part of the SAME set as the App above, so they are
	// adopted here and validated with it rather than arriving separately on the
	// project-config channel. An App ID presented to the wrong forge returns
	// "404 Integration not found", so applying one half without the other is the
	// live failure this function exists to prevent.
	//
	// Empty means "unchanged", matching AppSlug. It cannot mean "make me
	// public": empty URLs are also the correct steady state for a public hive
	// (~41 of 50 spokes), so silence here is indistinguishable from "no opinion"
	// and must never blank a working GHE URL. A hive moving TO public gets that
	// from its app_id, which the resolver derives from its forge.
	if ghCfg.APIURL != "" && ghCfg.APIURL != cur.APIURL {
		next.APIURL = ghCfg.APIURL
		touched = true
	}
	if ghCfg.BaseURL != "" && ghCfg.BaseURL != cur.BaseURL {
		next.BaseURL = ghCfg.BaseURL
		touched = true
	}
	if !touched {
		return nil
	}
	return &next
}

// nextInstallationID decides what a hive's installation_id becomes after a
// hub delivery, and reports whether the change is an operator RESET.
//
// Three cases, and the difference between the last two is load-bearing:
//
//	ResetInstallation  -> 0. The operator clicked "Reset App". Clearing makes
//	                     HasUsableApp() false, which raises githubAppRequired:
//	                     the owner is prompted to install the App again and the
//	                     self-heal ticker starts, whose RediscoverAndAdopt
//	                     adopts the correct installation for whatever they
//	                     install.
//	non-zero pushed    -> adopt it.
//	zero pushed        -> KEEP the current value. Zero means "the hub is not
//	                     speaking to this field", not "clear it". The
//	                     cluster-wide key reconcile sends zero on every beat
//	                     because it repairs KEYS on hives whose installation the
//	                     hub does not track; reading that as a clear would blank
//	                     a working installation fleet-wide and turn a key-only
//	                     fault into a total auth outage.
//
// docs_installation_id is DELIBERATELY untouched by all three cases. It is the
// optional docs-org add-on installation, has no rediscovery flow (zero simply
// disables the docs token refresh, permanently), and a stale value is
// non-fatal: the periodic docs mint warns and retries. After an app-changing
// delivery it can therefore briefly equal the PREVIOUS app's installation id —
// which looks like the old installation_id was "parked" there, but is just the
// provisioned docs value (public hives commonly provision both fields with the
// same installation) surviving a reset that correctly cleared only
// installation_id. On a flip-back to the original App it becomes valid again
// on its own.
func nextInstallationID(current int64, ghCfg *spoke.HeartbeatGitHubAppConfig) (next int64, reset bool) {
	if ghCfg == nil {
		return current, false
	}
	if ghCfg.ResetInstallation {
		return 0, current != 0
	}
	if ghCfg.InstallationID != 0 {
		return ghCfg.InstallationID, false
	}
	return current, false
}

var githubAppTokenCachePath = github.TokenCachePath

// resumeKickHeldAlerts tracks crash-restarted agents the resume-kick gate
// refused, so the eval loop can raise and later clear their dashboard alert.
var resumeKickHeldAlerts = spokealerts.NewResumeKickHeld()

func githubAppTokenHeartbeatFields(cfg *config.Config, detail string) (status, lastMintAt, lastErr string) {
	if cfg == nil || !cfg.GitHub.HasApp() {
		return "", "", ""
	}
	info, err := os.Stat(githubAppTokenCachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return spoke.GitHubAppTokenStatusMissing, "", detail
		}
		return spoke.GitHubAppTokenStatusError, "", err.Error()
	}
	lastMintAt = info.ModTime().UTC().Format(time.RFC3339)
	if time.Since(info.ModTime()) > spoke.GitHubAppTokenStaleAfter {
		return spoke.GitHubAppTokenStatusStale, lastMintAt, detail
	}
	return spoke.GitHubAppTokenStatusOK, lastMintAt, ""
}

var githubHTTPStatusRe = regexp.MustCompile(`\b([1-5][0-9]{2})\b`)

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func githubAppStructuredFailure(state, detail string) (class string, httpStatus int) {
	switch strings.TrimSpace(state) {
	case github.AppStateNotInstalled.String():
		class = "not-installed"
	case github.AppStateWrongInstallation.String():
		class = "wrong-installation"
	case github.AppStateInsufficientPerms.String():
		class = "insufficient-permissions"
	case github.AppStateKeyMissing.String():
		class = "key-missing"
	case github.AppStateKeyInvalid.String():
		class = "key-invalid"
	case github.AppStateNoAppAssigned.String():
		class = "no-app-assigned"
	case github.AppStateRepoNotCovered.String():
		class = "repo-not-covered"
	case github.AppStateRepoMoved.String():
		class = "repo-moved"
	case github.AppStateWriteForbidden.String():
		class = "write-forbidden"
	}
	if class == "" && strings.TrimSpace(detail) != "" {
		class = "token-error"
	}
	for _, m := range githubHTTPStatusRe.FindAllStringSubmatch(detail, -1) {
		if len(m) == 2 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				httpStatus = n
			}
		}
	}
	if httpStatus != 0 {
		if (class == "token-error" || class == "not-installed") && httpStatus == http.StatusNotFound {
			class = "installation-not-found"
		}
	}
	return class, httpStatus
}

// githubAuth is the outcome of resolving this hive's GitHub credentials at
// startup. Every field is optional: a hive with no usable credentials is a
// legitimate, bootable state.
type githubAuth struct {
	// Client is nil when no credentials could be resolved. Callers must treat a
	// nil Client as "GitHub is unavailable", never as a fatal condition.
	Client *github.Client
	// AppAuth is non-nil only when App auth was successfully initialized.
	AppAuth *github.AppAuth
	// Failure, when non-empty, is the operator-facing reason there is no
	// working GitHub client. It is shown in the dashboard's GitHub App banner.
	Failure string
	// State classifies Failure so the banner and the hub's journey nudges can
	// tell an operator-side fault (a key that was never delivered) from a
	// user-actionable one (the App is not installed). Escalating against an
	// owner for a key WE failed to provision is precisely the mistake
	// github.AppAuthState exists to prevent.
	State github.AppAuthState
	// TokenScopes is the boot-time PAT scope probe (see
	// github.CheckTokenScopes). Zero value / ScopeStatusSkipped on the App
	// path. It is advisory: a ScopeStatusMissing result never blocks startup,
	// it only gives github_auth a specific detail string instead of leaving the
	// operator to decode a runtime 403.
	TokenScopes github.ScopeResult
}

// initGitHubAuth resolves this hive's GitHub credentials.
//
// It NEVER exits the process. A hive that cannot authenticate to GitHub must
// still boot and serve its dashboard, because the dashboard is the only place
// its owner can see what is wrong and fix it. Exiting here — which is what this
// code used to do on a key-read failure — happens before the HTTP listener
// binds, so the pod crashloops with the diagnosis visible only in kubectl logs:
// the hub shows the hive offline, the heartbeat never starts, and a rollout
// hangs forever because the new pod never goes Ready.
//
// The placeholder app_id is the reason that path was reachable at all. See
// config.PlaceholderAppID.
func initGitHubAuth(ctx context.Context, cfg *config.Config, logger *slog.Logger) githubAuth {
	var out githubAuth
	appKeyFile := appKeys.Resolve(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID)

	// HasApp() rejects config.PlaceholderAppID. Build AppAuth as soon as a real
	// app_id and key are present, even when installation_id is still empty: the
	// App JWT is exactly what automatic installation-ID discovery needs.
	if cfg.GitHub.HasApp() {
		appAuth, err := github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, appKeyFile, logger, cfg.GitHub.ResolvedAPIURL())
		if err != nil {
			// A genuinely-configured App whose key is missing or malformed is a
			// real, actionable fault — but not a reason to refuse to boot.
			out.Failure = describeAppKeyFailure(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), appKeyFile, err)
			// Both states are operator-actionable: the hive's owner cannot
			// deliver a key. Absent vs. unparseable is the distinction the hub
			// needs to tell "never pushed" from "pushed something broken".
			out.State = github.AppStateKeyInvalid
			if errors.Is(err, fs.ErrNotExist) {
				out.State = github.AppStateKeyMissing
			}
			logger.Error("GitHub App auth unavailable — starting in dashboard-only mode",
				"app_id", cfg.GitHub.AppID,
				"installation_id", cfg.GitHub.InstallationID,
				"key_file", appKeyFile,
				"state", out.State.String(),
				"detail", out.Failure,
				"error", err,
			)
		} else {
			out.AppAuth = appAuth
		}
	}

	if out.AppAuth != nil {
		logger.Info("using GitHub App authentication", "app_id", cfg.GitHub.AppID)
		if cfg.GitHub.InstallationID == 0 {
			out.Failure = "The GitHub App is configured but has no installation. Install the app on your org; this hive will discover the installation automatically."
			out.State = github.AppStateNotInstalled
			logger.Warn("GitHub App configured without installation_id — starting in dashboard-only mode while auto-discovery polls")
			return out
		}
		// Correct a stale/wrong installation_id BEFORE building the client, so
		// the very first token this process mints is scoped to the right org
		// rather than 403ing on every write until the self-heal tick runs.
		healGitHubAppInstallation(ctx, out.AppAuth, cfg, logger)
		// Hooks (hold labels, #6203 repo pause, #6204 agent scope, and the
		// rest) are installed by bootGitHub through configureGitHubClient, the
		// same function every rebuild path uses (#9614).
		out.Client = github.NewClientFromAppWithBotLogin(out.AppAuth, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.BotLogin())
		startDocsTokenRefresh(ctx, cfg, appKeyFile, logger)
		return out
	}

	ghToken := cfg.GitHub.Token
	if ghToken == "" {
		ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
	}
	switch {
	case ghToken != "":
		out.Client = github.NewClient(ghToken, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.ResolvedAPIURL()) // hooks: see the App branch above
		// PAT path only: introspect the token's granted scopes ONCE, here, so a
		// too-narrow token is named at boot instead of surfacing hours later as
		// a generic 403 inside an agent — or, worse, as an empty backlog that
		// looks like "no work to do". Fail-soft and bounded (see
		// CheckTokenScopes); it never blocks or fails startup. The App branch
		// above returns before this point: Apps have permissions, not scopes.
		// An unset acmm_level is passed through as github.ACMMLevelUnset rather
		// than inferACMMLevel's L1 default: L1 requires no scopes at all, so
		// defaulting to it would silently suppress every warning on exactly the
		// hives whose intent we cannot read. See ACMMLevelUnset.
		scopeLevel := github.ACMMLevelUnset
		if cfg.ACMMLevel != nil {
			scopeLevel = *cfg.ACMMLevel
		}
		out.TokenScopes = out.Client.LogTokenScopeCheck(ctx, logger, scopeLevel)
	case out.Failure != "":
		// Real App, unusable key. Already logged; leave Client nil so nothing
		// tries to act on GitHub with credentials that do not work.
	case cfg.GitHub.IsPlaceholderApp():
		// OPERATOR-actionable, not user-actionable. This hive was provisioned as
		// a placeholder and never assigned a real app_id, so the JWT it signs
		// names no App and fails before any installation is consulted. Nothing
		// the owner enters in the installation-ID box can fix it — reporting this
		// as AppStateNotInstalled sent owners to re-install an App that was
		// already installed and to re-enter an installation_id that was already
		// correct.
		out.Failure = "This hive was never assigned a GitHub App ID: it still carries the placeholder github.app_id, " +
			"which does not name a real GitHub App. Setting an installation ID cannot resolve this — " +
			"the hub operator must assign the real App ID."
		out.State = github.AppStateNoAppAssigned
		logger.Warn("placeholder github.app_id — hive starting in dashboard-only mode",
			"placeholder_app_id", config.PlaceholderAppID,
			"installation_id", cfg.GitHub.InstallationID,
		)
	case cfg.GitHub.AppID != 0:
		out.Failure = "The GitHub App is configured but has no installation. Install the app on your org to enable agents."
		out.State = github.AppStateNotInstalled
		logger.Warn("GitHub App configured without credentials — hive starting in dashboard-only mode. Install the app and provide installation_id + key to enable agents.")
	default:
		// Neither a token nor any app_id at all. config.validate() rejects this
		// at load, so reaching it means the config was mutated afterwards.
		// Still a degraded boot rather than an exit: the dashboard is where an
		// operator fixes it.
		out.Failure = "No GitHub credentials configured. Set github.token, or github.app_id plus an App installation."
		logger.Error("no GitHub token configured (set github.token or github.app_id in config) — starting in dashboard-only mode")
	}
	return out
}

// startDocsTokenRefresh mints and periodically refreshes a token for the
// separate docs-org installation, when one is configured. A failure here is
// always non-fatal: the docs org is an add-on, not this hive's primary auth.
func startDocsTokenRefresh(ctx context.Context, cfg *config.Config, appKeyFile string, logger *slog.Logger) {
	if cfg.GitHub.DocsInstallationID == 0 {
		return
	}
	docsAuth, err := github.NewAppAuthWithCache(
		cfg.GitHub.AppID, cfg.GitHub.DocsInstallationID,
		appKeyFile, github.DocsTokenCachePath, logger, cfg.GitHub.ResolvedAPIURL(),
	)
	if err != nil {
		logger.Warn("failed to init docs org token", "error", err)
		return
	}
	go func() {
		// Mint the initial docs token in the BACKGROUND, not on the startup
		// path. The docs org is an add-on; blocking here on a docs-installation
		// mint that hangs (unreachable/uninstalled GHE) would delay the whole
		// process reaching MarkReady — the #2439 readiness-stall pattern. The
		// mint is bounded (tokenMintTimeout) and non-fatal regardless.
		if _, err := docsAuth.Token(ctx); err != nil {
			logger.Warn("failed to generate initial docs org token", "error", err)
		} else {
			logger.Info("docs org token cached", "installation_id", cfg.GitHub.DocsInstallationID)
		}
		const docsTokenRefreshInterval = 45 * time.Minute
		ticker := time.NewTicker(docsTokenRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := docsAuth.Token(ctx); err != nil {
					logger.Warn("docs token refresh failed", "error", err)
				}
			}
		}
	}()
}

// buildAgentMinter constructs the opt-in per-agent mint credential issuer from
// config. It loads (or creates) the signing key at cfg.Mint.KeyPath, builds a
// Minter with the configured issuer/hive-id/TTL, and wraps it as an AgentMinter.
// Callers gate on cfg.Mint.Enabled before calling. The signing key path comes
// from config (never hardcoded); a missing key file is created with 0600 perms
// by the mint package.
func buildAgentMinter(cfg *config.Config, logger *slog.Logger) (*mint.AgentMinter, error) {
	if cfg.Mint.KeyPath == "" {
		return nil, fmt.Errorf("mint.key_path is required when mint is enabled")
	}
	if cfg.Mint.Issuer == "" {
		return nil, fmt.Errorf("mint.issuer is required when mint is enabled")
	}
	key, err := mint.LoadOrCreateKey(cfg.Mint.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading mint signing key: %w", err)
	}
	maxTTL := time.Duration(cfg.Mint.MaxTTLSeconds) * time.Second
	minter, err := mint.NewMinter(key, cfg.Mint.Issuer,
		mint.WithHiveID(cfg.HiveID),
		mint.WithMaxTTL(maxTTL),
	)
	if err != nil {
		return nil, fmt.Errorf("building minter: %w", err)
	}
	logger.Debug("mint signing key loaded", "key_path", cfg.Mint.KeyPath)
	// ttl<=0 lets the minter fall back to its configured max on each Mint.
	return mint.NewAgentMinter(minter, maxTTL), nil
}

// healGitHubAppInstallation self-heals a hive whose github.installation_id
// points at the WRONG account — the failure mode diagnoseGitHubApp
// already detects and reports ("installation N belongs to 'X', not 'Y'"). It
// asks pkg/github to rediscover the installation covering cfg.Project.Org via
// the App JWT and, only on an unambiguous match, adopts it in place and
// persists it so the fix survives a pod restart.
//
// Every failure path is soft and silent-ish: a hive with no App key is not
// App-authenticated (skip), an API error or an ambiguous/absent discovery
// result leaves installation_id exactly as configured so the existing
// "check github.installation_id" banner still stands. It never returns an
// error to the caller and never blocks startup or a heartbeat.
//
// Rediscovery is rate-limited by pkg/github's discovery cache
// (github.InstallationDiscoveryTTL), so calling this from the self-heal tick
// is cheap even when the App is genuinely not installed on the org.
func healGitHubAppInstallation(ctx context.Context, appAuth *github.AppAuth, cfg *config.Config, logger *slog.Logger) {
	if appAuth == nil || !appAuth.HasKey() || cfg == nil {
		return
	}
	org := cfg.Project.Org
	if org == "" {
		return
	}
	newID, err := appAuth.RediscoverAndAdopt(ctx, org, logger)
	if err != nil {
		logger.Debug("github app installation rediscovery did not adopt a new id",
			"org", org, "error", err)
		return
	}
	if newID == 0 {
		return // already correct, or nothing safe to adopt
	}
	cfg.GitHub.InstallationID = newID
	if err := cfg.Save(); err != nil {
		logger.Error("adopted rediscovered installation_id but failed to persist it — "+
			"it will revert on the next pod restart",
			"installation_id", newID, "error", err)
		return
	}
	logger.Info("persisted rediscovered github app installation_id",
		"installation_id", newID, "org", org)
}

// githubRateLimitErrText is the substring GitHub's client surfaces on a rate or
// abuse limit. Matching text is acceptable ONLY here: a rate limit is a reason
// to skip classification entirely, never a reason to accuse anyone of anything,
// so a false negative costs one extra (correct) classification round-trip.
const githubRateLimitErrText = "rate limit"

// isGitHubRateLimitText reports whether an error looks like a GitHub rate limit.
func isGitHubRateLimitText(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), githubRateLimitErrText)
}

// The GitHub App credential classifiers moved to pkg/apphealth (#7238). These
// wrappers exist so call sites keep their current shape and so the App key
// paths -- vars that tests repoint at a temp dir -- are read HERE, at call
// time, rather than captured once at init.
func classifyGitHubAppFailure(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, logger *slog.Logger) (bool, string, github.AppAuthState) {
	return apphealth.ClassifyFailure(ctx, appAuth, expectedOwner, appKeyPaths(appAuthID(appAuth)), logger)
}

func classifyGitHubAppWriteForbidden(ctx context.Context, appAuth *github.AppAuth, expectedOwner, repo string) (string, github.AppAuthState) {
	return apphealth.ClassifyWriteForbidden(ctx, appAuth, expectedOwner, repo, appKeyPaths(appAuthID(appAuth)))
}

func appAuthID(appAuth *github.AppAuth) int64 {
	if appAuth == nil {
		return 0
	}
	return appAuth.AppID()
}

func classifyGitHubAppRepoCoverage(ctx context.Context, appAuth *github.AppAuth, org string, repos []string, logger *slog.Logger) (bool, string, github.AppAuthState) {
	return apphealth.ClassifyRepoCoverage(ctx, appAuth, org, repos, logger)
}

// The advisory digest posting policy moved to pkg/advisory (#7238 stage 2).
// These wrappers keep the existing call sites unchanged; advisoryPostGate is
// now an owned instance rather than a package-level struct tests reset.
var advisoryPostGate = advisory.NewPostGate()

func primaryAdvisoryRepo(cfg *config.Config) string {
	return advisory.PrimaryRepo(cfg)
}

func advisoryIssueUnresolved(advisoryIssues map[string]int, repo string) bool {
	return advisory.IssueUnresolved(advisoryIssues, repo)
}

func advisoryIssueNumber(advisoryIssues map[string]int, repo string) (int, bool) {
	return advisory.IssueNumber(advisoryIssues, repo)
}

func shouldBuildAdvisoryDigest(beadStores map[string]*beads.Store, ghClient *github.Client, hasExistingPinnedIssue bool) bool {
	return advisory.ShouldBuildDigest(beadStores, ghClient != nil, hasExistingPinnedIssue)
}

func shouldPostAdvisoryDigest(digest *advisory.Digest, ghClient *github.Client, hasPinnedIssue bool) bool {
	return advisory.ShouldPostDigest(digest, ghClient != nil, hasPinnedIssue)
}

func advisoryPostDue(advCfg config.AdvisoryConfig, repo string, now time.Time, logger *slog.Logger) bool {
	return advisoryPostGate.Due(advCfg, repo, now, logger)
}

func recordAdvisoryPostSuccess(repo string, now time.Time) {
	advisoryPostGate.RecordSuccess(repo, now)
}

func recordAdvisoryPostForbidden(repo string, now time.Time, backoff time.Duration) {
	advisoryPostGate.RecordWriteForbidden(repo, now, backoff)
}

// advisoryIssueMissingError is the error recorded (and reported to the hub) when
// a hive has findings to publish but no advisory issue to publish them to. It is
// deliberately an ERROR rather than a silent skip: the hub's staleness gate
// treats a hive reporting neither a post time nor an error as "not an advisory
// participant" and never alarms, which is how a wedged digest went unnoticed for
// six days in #4167.
func advisoryIssueMissingError(repo string, cause error) string {
	base := fmt.Sprintf("no advisory issue resolved for %s — digest not posted", repo)
	// Issues-disabled is the one ensure failure with a remedy the OPERATOR of
	// the target repo owns (#4329): flipping a repo setting, not fixing App
	// auth. Fold its actionable message into the alert text so the fleet
	// stale-advisory pill says so instead of reading like an auth failure.
	var disabled *github.IssuesDisabledError
	if errors.As(cause, &disabled) {
		return base + ": " + disabled.Error()
	}
	return base
}
