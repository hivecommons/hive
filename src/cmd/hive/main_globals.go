package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	// automaxprocs sets GOMAXPROCS to match the container's CPU quota (Linux
	// CFS) at init. Without it the Go runtime sizes its P count to the whole
	// NODE's core count, so on a many-core IKS worker a pod limited to a few
	// CPUs spawns far more runnable Ps than its CFS quota can service; when the
	// quota is exhausted mid-period EVERY goroutine — including the netpoller
	// that answers the :3002 liveness probe and the heartbeat loop — is
	// throttled until the next CFS period, which stacks on top of the NFS
	// stalls to push probe latency past the kubelet timeout. Matching GOMAXPROCS
	// to the quota removes that self-inflicted throttling.
	//
	// This is called explicitly rather than via the package's blank import
	// because that import's init writes a line to the default logger (stderr)
	// unconditionally. `hive` re-execs itself as a Git transport shim, and the
	// setup path captures a child's stdout and stderr into a single buffer to
	// parse (e.g. `symbolic-ref --short origin/HEAD`), so an init-time banner
	// is indistinguishable from Git's answer and corrupts the parsed branch
	// name. Setting it with a no-op logger keeps the GOMAXPROCS behaviour and
	// drops the banner.
	"go.uber.org/automaxprocs/maxprocs"

	"github.com/hivecommons/hive/pkg/appkey"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/mint"
)

// version is the single source of truth for the version string Hive reports
// in `--version`, hub heartbeats, and dashboard registration payloads.
//
// It is overridable at build time via `-ldflags -X main.version=...`, exactly
// like gitHash/gitShort/gitBranch below. A plain branch build (no ldflag)
// falls back to "0.0.0-dev" rather than an empty string, so an operator who
// builds locally or from an untagged CI run still sees a sensible, obviously
// non-release value instead of "hive  (commit ...)" or a version that lies by
// claiming a release number it isn't. src/Dockerfile and src/Dockerfile.hub
// leave the VERSION build-arg empty for ordinary branch builds, so this Go
// default is what ships; tagged-release.yml never rebuilds (it retags an
// already-published image — see src/docs/releases.md), so today no build
// path actually passes -X main.version=... yet. That gap is recorded as a
// known limitation in src/docs/releases.md rather than silently masked here.
var version = "0.0.0-dev"

var (
	gitHash   = "unknown"
	gitShort  = "unknown"
	gitBranch = "unknown"
)

// GitHub App private-key locations on a spoke live in pkg/appkey, which owns
// where they are, which one is correct for the App this hive claims, and how a
// hub-delivered key is written down (hivecommons/hive#5898 phase 1).
//
// A var, not a const, for the same reason the paths themselves used to be: a
// test points it at a temp dir and exercises the real resolution order.
// Production never reassigns it.
var appKeys = appkey.Default()

// traceShutdownTimeout bounds how long we wait for the OTel exporter to flush
// pending spans during shutdown, so a slow/unreachable collector can't hang
// process exit.
const traceShutdownTimeout = 5 * time.Second

// reachStatePath is where the component reach counters (#3993) persist on the
// PVC. Deliberately its OWN file, not the main /data/hive-state.json (#3973
// resolved OQ-2): reach state is append-mostly telemetry keyed by the running
// commit, and a parse failure in it must never take agent/governor state down
// with it (or vice versa). Written on the same cadence as the main state file
// (persistState), loaded once at boot.
const reachStatePath = "/data/reach-state.json"

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
func prospectiveGitHubIdentity(cur config.GitHubConfig, ghCfg *hub.HeartbeatGitHubAppConfig) *config.GitHubConfig {
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
func nextInstallationID(current int64, ghCfg *hub.HeartbeatGitHubAppConfig) (next int64, reset bool) {
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

// deliveredKeyPath is where a hub-delivered private key for appID is stored.
//
// The filename NAMES the App, so a key can only ever be found under the App it
// was delivered for. The generic /data/gh-app-key.pem carries no such evidence:
// a key written there for one App silently becomes "the key" for whatever
// app_id the config later claims, which is how all 33 heartbeat-only-cluster spokes ended up
// signing as the public App with the GHE key and getting
// 404 Integration not found.
//
// Falls back to the generic path only when the delivery names no App, so a key
// is never dropped on the floor.
func deliveredKeyPath(appID int64) string {
	if p := appKeys.PerAppIDKeyPath(appID); p != "" {
		return p
	}
	return appKeys.DataKeyPath
}

// describeAppKeyFailure turns a bare wrapped os error from github.NewAppAuth
// into a message an operator can act on without reading the source: it names
// the path actually tried, the full resolution order that produced it, and the
// underlying cause.
//
// The generic "reading app key /secrets/gh-app-key.pem: no such file" that this
// replaces gave no hint that key_file, $GH_APP_KEY_FILE, the PVC path and the
// provisioning mount are all consulted in a fixed order — so the usual response
// was to put the key in the wrong one of the four.
func describeAppKeyFailure(configured, envOverride, resolved string, err error) string {
	order := []string{
		fmt.Sprintf("$GH_APP_KEY_FILE=%s", describeKeySource(envOverride)),
		fmt.Sprintf("github.key_file=%s", describeKeySource(configured)),
		fmt.Sprintf("per-app-id PVC key %s/gh-app-key-<app_id>.pem", appKeys.DataDir),
		fmt.Sprintf("per-app-id provisioning key %s/gh-app-key-<app_id>.pem", appKeys.ProvisionedDir),
		fmt.Sprintf("PVC fallback %s", appKeys.DataKeyPath),
		fmt.Sprintf("provisioning mount %s", appKeys.ProvisionedKeyPath),
	}
	return fmt.Sprintf(
		"GitHub App private key could not be loaded from %q: %v. "+
			"Resolution order (first non-empty wins): %s. "+
			"Write a PEM-encoded RSA private key to that path, or point github.key_file at one.",
		resolved, err, strings.Join(order, " → "),
	)
}

// describeKeySource renders an unset key-file source as "(unset)" so the
// resolution order in describeAppKeyFailure reads unambiguously.
func describeKeySource(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(unset)"
	}
	return v
}

var githubAppTokenCachePath = github.TokenCachePath

func githubAppTokenHeartbeatFields(cfg *config.Config, detail string) (status, lastMintAt, lastErr string) {
	if cfg == nil || !cfg.GitHub.HasApp() {
		return "", "", ""
	}
	info, err := os.Stat(githubAppTokenCachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return hub.GitHubAppTokenStatusMissing, "", detail
		}
		return hub.GitHubAppTokenStatusError, "", err.Error()
	}
	lastMintAt = info.ModTime().UTC().Format(time.RFC3339)
	if time.Since(info.ModTime()) > hub.GitHubAppTokenStaleAfter {
		return hub.GitHubAppTokenStatusStale, lastMintAt, detail
	}
	return hub.GitHubAppTokenStatusOK, lastMintAt, ""
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
		out.Client = github.NewClient(ghToken, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.ResolvedAPIURL())
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

// reporterName identifies this spoke PROCESS to the hub (HeartbeatPayload
// Reporter) as "<hostname>/<pid>". In-cluster the hostname IS the pod name;
// the PID suffix exists because two hive processes were observed beating from
// ONE pod (#2453, #2496) — bare os.Hostname() made them indistinguishable, so
// the hub's duplicate-spoke detector (noteReporter) stayed silent through 11+
// alternating beats while the dashboard flipped state every beat. With the
// PID attached, same-pod duplicates alternate as "pod-x/123 ↔ pod-x/456" and
// the detector names the exact culprits. The hub compares the whole string as
// an opaque identity, so old spokes sending bare hostnames keep working; a
// hostname failure yields empty, which the hub reads as "too old / cannot
// report", never as data.
var reporterName = func() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return ""
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}()

// ── Process singleton (#2453, #2496) ────────────────────────────────────
//
// A spoke pod ran TWO hive processes concurrently: startedAt alternated
// between two values beat to beat, the hub saw 4-5 heartbeat senders behind
// one pod name, and one process's stale App-auth snapshot flipped the
// dashboard every beat. The per-process StartHeartbeat guard (#2462) cannot
// see across processes; only a kernel-level mutual exclusion can. main()
// takes an exclusive flock before doing anything else — the second process
// logs the holder's PID and exits instead of becoming a shadow sender. The
// flock releases automatically on process death, so a crashed holder never
// blocks a legitimate restart.
const (
	// singletonLockEnv overrides the lock file location (tests, unusual
	// container layouts). Empty means the default resolution below.
	singletonLockEnv = "HIVE_SINGLETON_LOCK"
	// singletonLockDisable is the env value that skips the guard entirely —
	// an escape hatch for deliberately running two instances on one host
	// (local development with distinct configs).
	singletonLockDisable = "off"
	// singletonLockDir is the preferred lock directory: container-local tmpfs
	// created by the entrypoint, shared by every process in the container but
	// by NOTHING outside it. Deliberately NOT /data — the PVC is shared
	// across PODS during a rolling update (maxSurge=1), and a pod-spanning
	// lock would deadlock the surge pod against the terminating one.
	singletonLockDir = "/var/run/hive-metrics"
	// singletonLockName is the lock file's basename in whichever directory is
	// chosen.
	singletonLockName = "hive.singleton.lock"
	// duplicateProcessExitCode marks an exit caused by refusing to run beside
	// an already-running hive process. Distinct from 0 (clean) and 17
	// (selfUpgradeFailureExitCode) so the refusal is legible in the
	// container's termination state.
	duplicateProcessExitCode = 18
)

// singletonLockPath resolves where the process singleton lock lives. Every
// process in a container resolves the same path (the filesystem is shared),
// so the choice is deterministic where it matters; the temp-dir fallback
// covers bare-metal/dev runs where the entrypoint never created the
// container dir.
func singletonLockPath() string {
	if p := os.Getenv(singletonLockEnv); p != "" {
		return p
	}
	if st, err := os.Stat(singletonLockDir); err == nil && st.IsDir() {
		return filepath.Join(singletonLockDir, singletonLockName)
	}
	return filepath.Join(os.TempDir(), singletonLockName)
}

// spokeRestartMinUptime is how old this process must be before it acts on a
// hub-delivered restart. The hub delivers the instruction to every beat in a
// multi-minute window so all instances hear it; without this guard the
// restarted process would come back inside the same window and restart again.
const spokeRestartMinUptime = 10 * time.Minute

// init applies the container CPU quota to GOMAXPROCS with a silent logger. See
// the go.uber.org/automaxprocs/maxprocs import comment for why the banner the
// blank import would print is not acceptable in this binary. A failure here is
// deliberately ignored: it only means GOMAXPROCS keeps the Go default, which is
// the pre-existing behaviour and must never block startup.
func init() {
	_, _ = maxprocs.Set(maxprocs.Logger(func(string, ...interface{}) {}))
}
