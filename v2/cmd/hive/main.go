package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	gh "github.com/google/go-github/v72/github"

	"github.com/kubestellar/hive/v2/pkg/advisory"
	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/classify"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/dashboard"
	"github.com/kubestellar/hive/v2/pkg/defsrc"
	"github.com/kubestellar/hive/v2/pkg/discord"
	"github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/governor"
	"github.com/kubestellar/hive/v2/pkg/hub"
	"github.com/kubestellar/hive/v2/pkg/knowledge"
	"github.com/kubestellar/hive/v2/pkg/logscrub"
	"github.com/kubestellar/hive/v2/pkg/notify"
	"github.com/kubestellar/hive/v2/pkg/policies"
	"github.com/kubestellar/hive/v2/pkg/promptsrc"
	"github.com/kubestellar/hive/v2/pkg/proxy"
	"github.com/kubestellar/hive/v2/pkg/scheduler"
	"github.com/kubestellar/hive/v2/pkg/snapshot"
	"github.com/kubestellar/hive/v2/pkg/tokens"
	"github.com/kubestellar/hive/v2/pkg/trajectory"
)

var (
	gitHash   = "unknown"
	gitShort  = "unknown"
	gitBranch = "unknown"
)

// GitHub App private-key locations on a spoke, and how the two differ.
//
//   - spokeProvisionedAppKeyPath is a read-only Kubernetes Secret mount, written
//     at PROVISIONING time from a key an operator supplied for THIS hive
//     specifically. Its presence is the marker of a deliberate per-hive
//     credential, which the hub's cluster-wide reconcile must never overwrite.
//   - spokeAppKeyPath is on the PVC and is where a hub-delivered (cluster
//     default) key lands. It is also what cfg.GitHub.KeyFile is repointed at
//     once the hub delivers one, so it takes effect over the provisioned mount.
// Vars rather than consts so tests can point them at a temp dir and exercise
// the real resolution order; production never reassigns them.
var (
	spokeProvisionedAppKeyPath = "/secrets/gh-app-key.pem"
	spokeAppKeyPath            = "/data/gh-app-key.pem"
)

// spokeAppKeyFileMode is rw------- : signing material must never be readable by
// anything else sharing the PVC or the pod.
const spokeAppKeyFileMode = 0o600

// reportedAppKeyFingerprint returns the non-secret fingerprint of the App key
// this spoke is ACTUALLY using, for the heartbeat payload. It fingerprints the
// resolved key file rather than a hard-coded path so the hub compares against
// the key that would really sign a JWT.
//
// Returns "" whenever there is no usable key — no file, empty file, or
// unparseable contents. All three mean the same thing to the hub ("this spoke
// cannot authenticate") and are repaired identically. The private key itself is
// never returned, and never enters the payload.
func reportedAppKeyFingerprint(keyFile string) string {
	// Lead with the same path resolveAppKeyFile would sign with, so the hub is
	// told about the key actually in effect and never about a shadowed one.
	candidates := []string{
		resolveAppKeyFile(keyFile, os.Getenv("GH_APP_KEY_FILE")),
		keyFile, spokeAppKeyPath, spokeProvisionedAppKeyPath,
	}
	for _, p := range candidates {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if fp, err := config.AppKeyFingerprintFromFile(p); err == nil && fp != "" {
			return fp
		}
	}
	return ""
}

// hasPerHiveAppKey reports whether this spoke's key came from a per-hive
// provisioning secret rather than the cluster default. The provisioned mount is
// read-only, so its mere existence with real PEM content is the signal — a
// hub-delivered key can never create or alter it.
//
// When BOTH exist the hub-delivered PVC key is the one in effect (the callback
// repoints cfg.GitHub.KeyFile at it), so this only claims an override while the
// provisioned key is genuinely the one being used.
func hasPerHiveAppKey(keyFile string) bool {
	fp, err := config.AppKeyFingerprintFromFile(spokeProvisionedAppKeyPath)
	if err != nil || fp == "" {
		return false
	}
	// The provisioned key exists. It is the effective credential only if the
	// resolved key file still points at it — resolveAppKeyFile is the single
	// authority on that, so an unconfigured hive that has already taken delivery
	// of a /data key correctly stops claiming a per-hive override.
	return resolveAppKeyFile(keyFile, os.Getenv("GH_APP_KEY_FILE")) == spokeProvisionedAppKeyPath
}

// resolveAppKeyFile picks which App private key this process will actually sign
// with, given the configured key_file and the GH_APP_KEY_FILE env override.
//
// WHY THE /data PREFERENCE MATTERS
//
// A hub-delivered key lands at spokeAppKeyPath (/data, on the PVC) and the
// heartbeat callback repoints cfg.GitHub.KeyFile at it — but only in memory, for
// the life of that process. A hive whose config carries NO key_file (which is
// the state of the three live GHE hives this repairs) used to fall straight
// through to the read-only /secrets provisioning mount. That mount holds the
// stale, wrong key, and the spoke cannot write to it. So on every restart the
// hive would silently go back to signing with the key that cannot work, and the
// hub — seeing the wrong fingerprint reported again — would redeliver forever.
// The key would be delivered and never used: a fault that reads as fixed.
//
// So when nothing is explicitly configured, a key already present on the PVC is
// preferred over the provisioning mount. An EXPLICIT key_file or env override
// still wins outright: those are deliberate, and this must not silently redirect
// an operator who named a path.
func resolveAppKeyFile(configured, envOverride string) string {
	if v := strings.TrimSpace(envOverride); v != "" {
		return v
	}
	if v := strings.TrimSpace(configured); v != "" {
		return v
	}
	// Nothing configured. Prefer a usable hub-delivered key on the PVC; fall
	// back to the provisioning mount only when /data has no parseable key. The
	// fingerprint check (not mere existence) keeps an empty or truncated /data
	// file from shadowing a good provisioned key.
	if fp, err := config.AppKeyFingerprintFromFile(spokeAppKeyPath); err == nil && fp != "" {
		return spokeAppKeyPath
	}
	return spokeProvisionedAppKeyPath
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
		fmt.Sprintf("PVC fallback %s", spokeAppKeyPath),
		fmt.Sprintf("provisioning mount %s", spokeProvisionedAppKeyPath),
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
	appKeyFile := resolveAppKeyFile(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"))

	// HasUsableApp() rejects config.PlaceholderAppID. A placeholder paired with
	// a real installation_id — exactly what happens the instant an owner
	// installs the App on a pre-provisioned hive — used to satisfy a bare
	// `AppID != 0` test and commit this process to App auth it could never
	// perform.
	if cfg.GitHub.HasUsableApp() {
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
		// Correct a stale/wrong installation_id BEFORE building the client, so
		// the very first token this process mints is scoped to the right org
		// rather than 403ing on every write until the self-heal tick runs.
		healGitHubAppInstallation(ctx, out.AppAuth, cfg, logger)
		out.Client = github.NewClientFromApp(out.AppAuth, cfg.Project.Org, cfg.Project.Repos, logger)
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
	case out.Failure != "":
		// Real App, unusable key. Already logged; leave Client nil so nothing
		// tries to act on GitHub with credentials that do not work.
	case cfg.GitHub.IsPlaceholderApp():
		// User-actionable: this hive was provisioned ahead of its App, and
		// installing the App is exactly what resolves it.
		out.Failure = "This hive carries a placeholder github.app_id and is not yet linked to a GitHub App. Install the GitHub App on your org to enable agents."
		out.State = github.AppStateNotInstalled
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
	if _, err := docsAuth.Token(ctx); err != nil {
		logger.Warn("failed to generate initial docs org token", "error", err)
	} else {
		logger.Info("docs org token cached", "installation_id", cfg.GitHub.DocsInstallationID)
	}
	go func() {
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

// processStartedAt is when this hive process began. Reported over the heartbeat
// so the hub can show an uptime pill — a hive that is 1/1 Running but restarting
// every couple of minutes looks healthy in a pod listing and in My Hives, and a
// short uptime that keeps resetting is the only visible tell.
var processStartedAt = time.Now()

func main() {
	startTime := time.Now()
	defaultConfig := "/etc/hive/hive.yaml"
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" {
		defaultConfig = envCfg
	}
	configPath := flag.String("config", defaultConfig, "path to hive.yaml config file")
	flag.Parse()
	// Canonicalize gitShort to the standard 7-char short SHA the hub stores and
	// compares against. The Dockerfile builds it with `--short=7`, but git can
	// still return more chars when 7 isn't unique; trim so what we report to the
	// hub is always the same length it stores (no short-vs-full mismatch).
	if len(gitShort) > 7 {
		gitShort = gitShort[:7]
	}
	dashboard.SetGitVersion(gitHash, gitShort)
	dashboard.SetGitBranch(gitBranch)

	logger := slog.New(logscrub.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.SetDefault(logger)

	// Clear stale upgrade marker if the current SHA differs from the marker's
	// current_sha — this means the upgrade succeeded and the marker is from a
	// previous version.
	const upgradeMarkerStartupPath = "/data/upgrade-requested"
	if markerData, err := os.ReadFile(upgradeMarkerStartupPath); err == nil {
		if !strings.Contains(string(markerData), fmt.Sprintf(`"current_sha":"%s"`, gitShort)) {
			os.Remove(upgradeMarkerStartupPath)
			logger.Info("cleared stale upgrade marker (SHA changed)", "current", gitShort)
		}
	}

	if os.Getenv("HIVE_MODE") == "hub" {
		runHub(logger)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Reconfigure logger with rolling file output
	logger = setupLogger(cfg.Governor.Logging.Dir, cfg.Governor.Logging.MaxSizeMB,
		cfg.Governor.Logging.MaxAgeDays, cfg.Governor.Logging.MaxBackups,
		cfg.Governor.Logging.Compress, cfg.Governor.Logging.Level)
	slog.SetDefault(logger)

	// Load or generate a unique Hive ID for this instance
	cfg.HiveID = loadOrGenerateHiveID(logger)
	os.Setenv("HIVE_ID", cfg.HiveID)

	// Surface config provenance: when the dashboard-save backup exists, init
	// containers restore it over the ConfigMap seed on restart, so edits made
	// only to the seed (or only to the live file) silently lose to the backup.
	bakPath := *configPath + ".bak"
	if _, statErr := os.Stat(bakPath); statErr == nil {
		logger.Info("config backup present — restored over the seed on pod restart; fixes must land in the live config so the next save refreshes it",
			"path", bakPath,
			"github_installation_id", cfg.GitHub.InstallationID,
		)
	}

	logger.Info("hive starting",
		"org", cfg.Project.Org,
		"repos", cfg.Project.Repos,
		"agents", len(cfg.Agents),
		"hive_id", cfg.HiveID,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	ghAuth := initGitHubAuth(ctx, cfg, logger)
	ghClient, appAuth := ghAuth.Client, ghAuth.AppAuth
	// appAuthFailure, when non-empty, is the operator-facing reason GitHub auth
	// is unavailable. It is surfaced through the existing
	// GitHubAppRequired/PermIssue banner rather than killing the process.
	appAuthFailure := ghAuth.Failure
	// appAuthState classifies that failure so the banner and the hub's journey
	// nudges can tell an operator-side fault (no key was ever delivered) from a
	// user-actionable one (the App is not installed).
	appAuthState := ghAuth.State
	if ghClient != nil && len(cfg.Governor.Labels.Exempt) > 0 {
		ghClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
	}
	// Load user token for advisory posting (comments on issues as the logged-in user)
	var userGHClient atomic.Pointer[github.Client]
	if tokenData, err := os.ReadFile("/data/gh-user-token"); err == nil {
		userToken := strings.TrimSpace(string(tokenData))
		if userToken != "" {
			// Identity check goes to github.com (the user token is a github.com
			// OAuth token); the repo client stays on the per-hive host.
			if username, err := github.ValidateToken(userToken, cfg.GitHub.OAuthAPIURL()); err == nil {
				uc := github.NewClient(userToken, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.ResolvedAPIURL())
				userGHClient.Store(uc)
				logger.Info("user GitHub token loaded for advisory posting", "username", username)
			} else {
				logger.Warn("persisted user token is invalid or expired", "error", err)
			}
		}
	}

	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	sched := scheduler.New(cfg, logger)

	// Wire the GitHub prompt-source resolver so agents may source their kick
	// prompt from a repo (agent.prompt_source). Fetching reuses the hive's App
	// token via ghClient and is gated to the seed-only allowlist — the closure
	// captures cfg so a live config reload updates the allowlist on the next kick.
	// A nil ghClient must be passed as a nil Fetcher interface (not a typed-nil
	// *github.Client) so the resolver's nil-fetcher fallback path triggers.
	var promptFetcher promptsrc.Fetcher
	if ghClient != nil {
		promptFetcher = ghClient
	}
	sched.SetGitHubPromptResolver(promptsrc.NewResolver(
		promptFetcher,
		func(slug string) bool { return cfg.GitHubPromptAllowed(slug) },
		logger,
	))

	// Wire the whole-agent definition_source resolver so agents imported with
	// "keep linked" re-fetch their portable AgentDefinition from the repo on
	// reload/kick and re-apply its operator-safe fields (never security/seed-only
	// fields — see pkg/defsrc). Same seed-only allowlist gate and graceful
	// fallback as the prompt resolver.
	var defFetcher defsrc.Fetcher
	if ghClient != nil {
		defFetcher = ghClient
	}
	definitionResolver := defsrc.NewResolver(
		defFetcher,
		func(slug string) bool { return cfg.GitHubDefinitionAllowed(slug) },
		logger,
	)
	// Apply live definitions once at startup so a repo edit made while the hive
	// was down is reflected before the first kick.
	defsrc.ApplyToConfig(context.Background(), cfg, definitionResolver, logger)

	// Restore sparkline history from disk so it survives container restarts
	const sparklinePath = "/data/sparkline-history.json"
	if sparkData, err := os.ReadFile(sparklinePath); err == nil {
		var snapshots []governor.EvalSnapshot
		if err := json.Unmarshal(sparkData, &snapshots); err == nil && len(snapshots) > 0 {
			gov.SeedEvalHistory(snapshots)
			logger.Info("sparkline history restored", "entries", len(snapshots))
		}
	}

	// Restore mode history from disk so the mode timeline survives container restarts
	const modeHistoryPath = "/data/mode-history.json"
	if modeData, err := os.ReadFile(modeHistoryPath); err == nil {
		var changes []governor.ModeChange
		if err := json.Unmarshal(modeData, &changes); err == nil && len(changes) > 0 {
			gov.SeedModeHistory(changes)
			logger.Info("mode history restored", "entries", len(changes))
		}
	}

	// Restore token sparkline history from disk so token charts survive container restarts
	const tokenSparklinePath = "/data/token-sparkline-history.json"
	var pendingTokenSeed []dashboard.TokenSparklineEntry
	if tokenSparkData, err := os.ReadFile(tokenSparklinePath); err == nil {
		if err := json.Unmarshal(tokenSparkData, &pendingTokenSeed); err == nil && len(pendingTokenSeed) > 0 {
			logger.Info("token sparkline history loaded", "entries", len(pendingTokenSeed))
		}
	}

	// Restore fact count history from disk so the knowledge sparkline survives restarts
	const factHistoryPath = "/data/fact-history.json"
	var pendingFactSeed []dashboard.FactHistoryEntry
	if factData, err := os.ReadFile(factHistoryPath); err == nil {
		if err := json.Unmarshal(factData, &pendingFactSeed); err == nil && len(pendingFactSeed) > 0 {
			logger.Info("fact history loaded", "entries", len(pendingFactSeed))
		}
	}

	// Restore estimated-cost history from disk so the cost sparkline survives restarts
	const costHistoryPath = "/data/cost-history.json"
	var pendingCostSeed []dashboard.CostHistoryEntry
	if costData, err := os.ReadFile(costHistoryPath); err == nil {
		if err := json.Unmarshal(costData, &pendingCostSeed); err == nil && len(pendingCostSeed) > 0 {
			logger.Info("cost history loaded", "entries", len(pendingCostSeed))
		}
	}

	// Restore governor/repo/beads/system trend history from disk so those
	// sparklines survive restarts and render for any viewer (previously kept
	// only in the browser's localStorage).
	const trendHistoryPath = "/data/trend-history.json"
	var pendingTrendSeed []dashboard.TrendHistoryEntry
	if trendData, err := os.ReadFile(trendHistoryPath); err == nil {
		if err := json.Unmarshal(trendData, &pendingTrendSeed); err == nil && len(pendingTrendSeed) > 0 {
			logger.Info("trend history loaded", "entries", len(pendingTrendSeed))
		}
	}

	if cfg.Knowledge.Enabled {
		layers := convertKnowledgeLayers(cfg.Knowledge.Layers)
		primerCfg := knowledge.PrimerConfig{
			MaxFacts:      cfg.Knowledge.Primer.MaxFacts,
			Priority:      cfg.Knowledge.Primer.Priority,
			MergeStrategy: cfg.Knowledge.Primer.MergeStrategy,
		}
		primer := knowledge.NewPrimer(layers, primerCfg, logger)
		sched.SetPrimer(primer)
		logger.Info("knowledge primer enabled",
			"layers", len(cfg.Knowledge.Layers),
			"max_facts", primerCfg.MaxFacts,
		)
	}

	notifier := notify.New(cfg.Notifications, logger)
	notifier.SetHiveID(cfg.HiveID)
	acmmLevel := inferACMMLevel(cfg)
	// A hive that booted without usable GitHub credentials raises the banner
	// immediately, seeded with the classification made at startup. Otherwise
	// these stay empty and are filled in later by the live probes below.
	githubAppRequired := appAuthFailure != ""
	// githubAppDiag/githubAppState carry the classified reason App auth failed,
	// so the banner can name the true cause (and the hub can avoid escalating
	// against a hive whose credentials the operator never delivered).
	githubAppDiag := appAuthFailure
	githubAppState := appAuthState

	// Find or create the pinned advisory issue. Any level can have advisory
	// agents whose findings should be posted to this issue.
	advisoryIssues := map[string]int{}
	if acmmLevel > 0 && ghClient != nil {
		primaryRepo := cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
			primaryRepo = cfg.Project.Repos[0]
		}
		if primaryRepo != "" {
			num, err := ghClient.EnsureAdvisoryIssue(ctx, primaryRepo)
			if err != nil {
				logger.Error("failed to ensure advisory issue", "repo", primaryRepo, "error", err)
				// GitHub returns 403 for rate limiting too — a transient
				// condition that must not raise the "App Not Installed"
				// banner (matches the guard on the repo-change path).
				if strings.Contains(err.Error(), "rate limit") {
					logger.Warn("GitHub API rate limit hit during advisory issue ensure", "repo", primaryRepo)
				} else if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "401") {
					githubAppRequired = true
					// Classify WHY before the banner renders. A bare 401/403
					// here used to become "GitHub App Not Installed", which is
					// wrong — and actively misleading — when the real cause is
					// a key the operator has not delivered. Classification is
					// on the live API response, and a missing key file
					// short-circuits without any API call.
					githubAppDiag, githubAppState = diagnoseGitHubApp(ctx, ghClient.AppAuth(), cfg.Project.Org)
					logger.Warn("GitHub App authentication failed at startup",
						"state", githubAppState.String(),
						"operator_actionable", githubAppState.OperatorActionable(),
						"error", err)
				}
			} else {
				advisoryIssues[primaryRepo] = num
				os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num))
				logger.Info("advisory issue ready", "repo", primaryRepo, "number", num)
			}
		}
	}

	advisoryStore := advisory.NewStore()

	policyDir := cfg.Policies.LocalDir
	if policyDir == "" {
		policyDir = "/data/policies"
	}
	if cfg.Policies.Path != "" {
		policyDir = policyDir + "/" + cfg.Policies.Path
	}

	// Write brainstorm policy to disk so the agent can find it.
	// The policy is embedded in the binary but the agent searches the filesystem.
	brainstormPolicyDir := policyDir
	if brainstormPolicyDir == "" {
		brainstormPolicyDir = "/data/policies/examples/kubestellar/agents"
	}
	os.MkdirAll(brainstormPolicyDir, 0o755)
	if policyData, err := policies.DefaultPolicies.ReadFile("defaults/brainstorm-advisory.md"); err == nil {
		policyPath := filepath.Join(brainstormPolicyDir, "brainstorm-advisory.md")
		// Always overwrite — the embedded policy may have been updated
		// (e.g., inception reaping guard added in bug #113 fix).
		if err := os.WriteFile(policyPath, policyData, 0o644); err != nil {
			logger.Warn("failed to write brainstorm policy", "path", policyPath, "error", err)
		} else {
			logger.Info("wrote brainstorm policy to disk", "path", policyPath)
		}
	}

	projectCtx := agent.ProjectContext{
		Org:            cfg.Project.Org,
		Repos:          cfg.Project.Repos,
		ACMMLevel:      acmmLevel,
		PRsAllowed:     cfg.Project.PRsAllowed(),
		PolicyDir:      policyDir,
		AppAuthoredPRs: cfg.GitHub.AppAuthoredPRsEnabled(),
	}
	agentMgr := agent.NewManager(cfg.EnabledAgents(), logger, projectCtx)
	// Resolve the bob API key at LAUNCH time, not here: cfg is the live config
	// pointer (the config watcher swaps its contents in place on reload), so a
	// key added via the Secret mount, the PVC file, or a config edit takes
	// effect on the next agent launch with no hive restart. Only the key's
	// LOCATION is ever in cfg; the value is read from file/env on each call and
	// is never logged.
	agentMgr.SetBobAPIKeyResolver(func() string {
		return cfg.Governor.Bob.ResolveAPIKey()
	})
	// The launch path also needs to know WHICH FILE the key came from, so it can
	// check that file is readable by the agent UID rather than only by the hive
	// process. Returns a loggable source string, never the key value.
	agentMgr.SetBobKeySourceResolver(func() string {
		return cfg.Governor.Bob.ResolveAPIKeySource()
	})
	// Log only WHERE the key came from (or that none is set) so a misconfigured
	// hive is diagnosable without the value ever reaching the logs.
	if src := cfg.Governor.Bob.ResolveAPIKeySource(); src != "" {
		logger.Info("bob api key detected", "source", src)
	} else {
		logger.Info("no bob api key configured; agents with backend \"bob\" will not launch",
			"remedy", "set governor.bob.api_key_file or the "+config.DefaultBobAPIKeyEnv+" env var")
	}
	if appAuth != nil {
		agentMgr.SetAppAuth(appAuth)
		go agentMgr.StartAgentTokenRefresh(ctx)
	}

	go agent.StartPermissionsWatcher(logger)

	const statePath = "/data/hive-state.json"
	var savedIssueCosts map[string]int64
	saved, stateErr := snapshot.LoadState(statePath, logger)
	if stateErr != nil {
		logger.Warn("failed to load persisted state", "error", stateErr)
	} else if saved != nil {
		savedIssueCosts = saved.IssueCosts
		for name, as := range saved.Agents {
			if _, inConfig := cfg.Agents[name]; !inConfig {
				logger.Info("skipping saved state for agent not in config", "agent", name)
				continue
			}
			if as.Paused {
				reason := as.PausedReason
				if reason == "" {
					reason = "persisted pause state"
				}
				trigger := as.PausedTrigger
				if trigger == "" {
					trigger = "state-restore"
				}
				_ = agentMgr.Pause(name, trigger, reason)
				if as.PausedAt != nil {
					agentMgr.SeedPauseState(name, *as.PausedAt, trigger, reason)
				}
			}
			if as.PinnedCLI != "" {
				_ = agentMgr.PinCLI(name, as.PinnedCLI)
			}
			if as.PinnedModel != "" {
				_ = agentMgr.PinModel(name, as.PinnedModel)
			}
			if as.ModelOverride != "" {
				agentMgr.SetModelOverride(name, as.ModelOverride)
				logger.Info("model override restored", "agent", name, "model", as.ModelOverride)
			}
			if as.BackendOverride != "" {
				agentMgr.SetBackendOverride(name, as.BackendOverride)
				logger.Info("backend override restored", "agent", name, "backend", as.BackendOverride)
			}
			if as.RestartCount > 0 {
				agentMgr.SeedRestartCount(name, as.RestartCount)
			}
			if as.LastKick != nil {
				agentMgr.SeedLastKick(name, *as.LastKick)
			}
			if len(as.KickHistory) > 0 {
				records := make([]agent.KickRecord, len(as.KickHistory))
				for i, ke := range as.KickHistory {
					records[i] = agent.KickRecord{Timestamp: ke.Timestamp, Agent: ke.Agent, Snippet: ke.Snippet}
				}
				agentMgr.SeedKickHistory(name, records)
			}
			if agentCfg, ok := cfg.Agents[name]; ok {
				if as.DisplayName != "" && agentCfg.DisplayName == "" {
					agentCfg.DisplayName = as.DisplayName
				}
				if as.Description != "" && agentCfg.Description == "" {
					agentCfg.Description = as.Description
				}
				if as.Enabled != nil {
					agentCfg.Enabled = *as.Enabled
				}
				if as.ClearOnKick != nil {
					agentCfg.ClearOnKick = *as.ClearOnKick
				}
				if as.StaleTimeout != nil {
					agentCfg.StaleTimeout = *as.StaleTimeout
				}
				if as.RestartStrategy != "" {
					agentCfg.RestartStrategy = as.RestartStrategy
				}
				cfg.Agents[name] = agentCfg
				_ = agentMgr.UpdateConfig(name, agentCfg)
			}
		}
		if saved.BudgetLimit > 0 {
			gov.SetBudgetLimit(saved.BudgetLimit)
		}
		if saved.BudgetIgnoreAll {
			gov.SetBudgetIgnoreAll(true)
		}
		if len(saved.BudgetIgnored) > 0 {
			gov.SetBudgetIgnored(saved.BudgetIgnored)
		}
		if len(saved.CadenceOverrides) > 0 {
			for modeName, agentCadences := range saved.CadenceOverrides {
				mode, ok := cfg.Governor.Modes[modeName]
				if !ok {
					continue
				}
				if mode.Cadences == nil {
					mode.Cadences = make(map[string]string)
				}
				for agentName, cadence := range agentCadences {
					mode.Cadences[agentName] = cadence
				}
				cfg.Governor.Modes[modeName] = mode
			}
			logger.Info("cadence overrides restored", "modes", len(saved.CadenceOverrides))
		}
		if saved.GovernorMode != "" {
			gov.SetMode(governor.Mode(saved.GovernorMode))
			logger.Info("governor mode restored", "mode", saved.GovernorMode)
		}
		if len(saved.LastKicks) > 0 {
			gov.SeedLastKicks(saved.LastKicks)
			logger.Info("governor last kicks restored", "agents", len(saved.LastKicks))
		}
		if saved.BudgetSpend > 0 || !saved.BudgetResetAt.IsZero() || len(saved.BudgetByAgent) > 0 {
			gov.SeedBudget(saved.BudgetSpend, saved.BudgetByAgent, saved.BudgetByModel, saved.BudgetResetAt)
			gov.SeedBudgetWindowBaseline(saved.BudgetWindowBaseline)
			logger.Info("budget state restored", "spend", saved.BudgetSpend, "reset_at", saved.BudgetResetAt, "window_baseline", saved.BudgetWindowBaseline)
		}
		if len(saved.KickHistory) > 0 {
			records := make([]governor.KickRecord, len(saved.KickHistory))
			for i, ke := range saved.KickHistory {
				records[i] = governor.KickRecord{Timestamp: ke.Timestamp, Agent: ke.Agent}
			}
			gov.SeedKickHistory(records)
			logger.Info("kick history restored", "entries", len(records))
		}
		if !saved.LastEval.IsZero() {
			gov.SeedLastEval(saved.LastEval)
		}
		if saved.ACMMLevel != nil && cfg.ACMMLevel == nil {
			cfg.ACMMLevel = saved.ACMMLevel
			logger.Info("ACMM level restored", "level", *saved.ACMMLevel)
		}
		if saved.ConfigOverrides != nil {
			applyConfigOverrides(cfg, saved.ConfigOverrides)
			ghClient.SetRepos(cfg.Project.Repos)
			if len(cfg.Governor.Labels.Exempt) > 0 {
				ghClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
			}
			if uc := userGHClient.Load(); uc != nil {
				uc.SetRepos(cfg.Project.Repos)
			}
			logger.Info("migrated config overrides from state to hive.yaml",
				"repos", cfg.Project.Repos)

			// Write merged config to hive.yaml so overrides become the base config
			if err := cfg.Save(); err != nil {
				logger.Error("failed to save migrated config", "error", err)
			}

			// Strip config_overrides from state and re-save
			saved.ConfigOverrides = nil
			if err := snapshot.SaveState(statePath, saved, logger); err != nil {
				logger.Error("failed to re-save state after migration", "error", err)
			}
		}
	}

	if gov.GetBudget().WeeklyLimit == 0 && cfg.Governor.Budget.TotalTokens > 0 {
		gov.SetBudgetLimit(cfg.Governor.Budget.TotalTokens)
	}

	dashSrv := dashboard.NewServerWithAuth(cfg.Dashboard.Port, cfg.Dashboard.AuthToken, logger)

	// Persist per-user dashboard sessions on the PVC (/data) so direct-route
	// users aren't logged out by pod restarts. NOTE: use /data explicitly, NOT
	// filepath.Dir(configPath) — the config lives at /etc/hive/hive.yaml, which
	// is an ephemeral emptyDir (the ConfigMap seed mount), so a sessions file
	// there is wiped on every pod roll. That was the "re-login on every visit"
	// bug on direct-route spokes. /data is the CephFS PVC (same place cost/fact
	// history persist).
	dashSrv.EnableSessionPersistence("/data/dashboard-sessions.json")

	// Seed token sparkline history now that the dashboard server exists
	if len(pendingTokenSeed) > 0 {
		dashSrv.SeedTokenSparklineHistory(pendingTokenSeed)
		logger.Info("token sparkline history restored", "entries", len(pendingTokenSeed))
	}

	if len(pendingFactSeed) > 0 {
		dashSrv.SeedFactHistory(pendingFactSeed)
		logger.Info("fact history restored", "entries", len(pendingFactSeed))
	}

	if len(pendingCostSeed) > 0 {
		dashSrv.SeedCostHistory(pendingCostSeed)
		logger.Info("cost history restored", "entries", len(pendingCostSeed))
	}

	if len(pendingTrendSeed) > 0 {
		dashSrv.SeedTrendHistory(pendingTrendSeed)
		logger.Info("trend history restored", "entries", len(pendingTrendSeed))
	}

	beadStores := make(map[string]*beads.Store)
	for name, agentCfg := range cfg.EnabledAgents() {
		store, err := beads.NewStore(agentCfg.BeadsDir)
		if err != nil {
			logger.Warn("failed to init beads store", "agent", name, "error", err)
			continue
		}
		store.SetHiveID(cfg.HiveID)
		beadStores[name] = store
		logger.Info("beads store initialized", "agent", name, "count", store.Count())
	}

	// Scan /data/beads/ for agent directories that have beads.json files on
	// disk but are not covered by the enabled-agent loop above. This handles
	// agents that were disabled between restarts or added by a previous ACMM
	// pack that is no longer active.
	const beadsRootDir = "/data/beads"
	if entries, err := os.ReadDir(beadsRootDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if _, exists := beadStores[name]; exists {
				continue // already loaded from config
			}
			agentBeadsDir := filepath.Join(beadsRootDir, name)
			beadsFile := filepath.Join(agentBeadsDir, "beads.json")
			if _, statErr := os.Stat(beadsFile); statErr != nil {
				continue // no beads.json in this directory
			}
			store, err := beads.NewStore(agentBeadsDir)
			if err != nil {
				logger.Warn("failed to load orphan beads store", "agent", name, "error", err)
				continue
			}
			store.SetHiveID(cfg.HiveID)
			beadStores[name] = store
			logger.Info("orphan beads store loaded from disk", "agent", name, "count", store.Count())
		}
	}

	initAgentConfigDrivenSystems(cfg)

	tokenCollector := tokens.NewCollector(cfg.Data.MetricsDir, logger)
	tokenCollector.SetClaudeSessionsDir(cfg.Data.ClaudeSessionsDir)
	tokenCollector.SetCopilotSessionsDir(cfg.Data.CopilotSessionsDir)
	if len(savedIssueCosts) > 0 {
		tokenCollector.SeedIssueCosts(savedIssueCosts)
		logger.Info("issue costs restored", "entries", len(savedIssueCosts))
	}
	tokenStop := make(chan struct{})
	go tokenCollector.Start(tokenStop)
	defer close(tokenStop)

	badgeURL := os.Getenv("HIVE_COVERAGE_BADGE_URL")
	if badgeURL == "" {
		badgeURL = "https://gist.githubusercontent.com/clubanderson/b9a9ae8469f1897a22d5a40629bc1e82/raw/coverage-badge.json"
	}
	primaryRepo := cfg.Project.PrimaryRepo
	if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
		primaryRepo = cfg.Project.Repos[0]
	}
	metricsCollector := dashboard.NewMetricsCollector(ghClient, cfg.Project.Org, primaryRepo, badgeURL, cfg.Project.AIAuthor, cfg.Project.Name, logger)
	go metricsCollector.Start(ctx)

	// Fleet-stats collector: computes this hive's AI-author contribution counts
	// (merged/rejected PRs, CVE-referencing PRs) across its org on a slow timer
	// and caches them, so each heartbeat can attach a fresh-but-cheap snapshot
	// the hub aggregates into the public landing page's live fleet-stats strip.
	// ai_author is optional config and hosted hives are provisioned without it,
	// so most spokes had an empty author — which silently disabled the collector
	// entirely (Start() returns early) and left the public fleet-stats strip
	// blank. Fall back to the bot token's own GitHub login: that IS the account
	// the agents open PRs as, so it is the correct author to count. Never fall
	// back to an org-wide search with no author filter — that would sweep in
	// human PRs and overstate what the fleet's agents actually did.
	fleetStatsAuthor := cfg.Project.AIAuthor
	fleetStatsToken := cfg.GitHub.Token
	if fleetStatsToken == "" {
		fleetStatsToken = os.Getenv("HIVE_GITHUB_TOKEN")
	}
	if fleetStatsAuthor == "" && fleetStatsToken != "" {
		if botUser, err := github.ValidateToken(fleetStatsToken, cfg.GitHub.ResolvedAPIURL()); err == nil && botUser.Login != "" {
			fleetStatsAuthor = botUser.Login
			logger.Info("fleet stats: ai_author unset, using bot token identity",
				"author", fleetStatsAuthor)
		} else if err != nil {
			logger.Warn("fleet stats: ai_author unset and bot identity lookup failed; "+
				"this hive will not contribute to the public fleet-stats total",
				"error", err)
		}
	}
	if fleetStatsAuthor == "" || cfg.Project.Org == "" {
		logger.Warn("fleet stats collector disabled: author or org is empty; "+
			"set project.ai_author in hive.yaml so this hive contributes to the fleet total",
			"author", fleetStatsAuthor, "org", cfg.Project.Org)
	}
	fleetStatsCollector := dashboard.NewFleetStatsCollector(ghClient, fleetStatsAuthor, cfg.Project.Org, logger)
	go fleetStatsCollector.Start(ctx)

	var lastActionable atomic.Pointer[github.ActionableResult]
	refreshDashboard := func() {
		actionable := lastActionable.Load()
		govState := gov.GetState()
		agentStatuses := agentMgr.AllStatuses()
		payload := dashboard.BuildFrontendStatus(
			govState,
			actionable,
			agentStatuses,
			cfg,
			tokenCollector,
			gov,
			beadStores,
			ghClient,
			ctx,
			metricsCollector,
		)
		if d := dashSrv.GetAdvisoryDigest(); d != nil {
			payload.AdvisoryDigest = d
		}
		dashSrv.UpdateStatus(payload)
	}

	const cachedActionablePath = "/data/last-actionable.json"
	if data, err := os.ReadFile(cachedActionablePath); err == nil {
		var cached github.ActionableResult
		if err := json.Unmarshal(data, &cached); err == nil {
			lastActionable.Store(&cached)
			gov.SeedQueueState(cached.Issues.Count, cached.PRs.Count, cached.Hold.Total, cached.Issues.SLAViolations)
			refreshDashboard()
			logger.Info("restored cached actionable data", "issues", cached.Issues.Count, "prs", cached.PRs.Count, "age", time.Since(cached.GeneratedAt).Round(time.Second))
		}
	}

	var knowledgeAPI *knowledge.KnowledgeAPI
	if cfg.Knowledge.Enabled {
		layers := convertKnowledgeLayers(cfg.Knowledge.Layers)
		knowledgeAPI = knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{
			Enabled: cfg.Knowledge.Enabled,
			Engine:  cfg.Knowledge.Engine,
		}, logger)
	}

	// Auto-connect configured vaults and start git-sync for Obsidian Git integration
	gitSyncer := knowledge.NewGitSyncer(logger)
	const seedDataDir = "/opt/hive/seed-data/wiki"
	for _, vc := range cfg.Knowledge.Vaults {
		if err := knowledge.InitVaultRepo(vc.Path, logger); err != nil {
			logger.Warn("failed to init vault directory", "name", vc.Name, "path", vc.Path, "error", err)
			continue
		}
		if err := knowledge.SeedVaultContent(vc.Path, seedDataDir, logger); err != nil {
			logger.Warn("failed to seed vault content", "name", vc.Name, "error", err)
		}
		if knowledgeAPI != nil {
			if err := knowledgeAPI.ConnectVault(vc.Path, vc.Name); err != nil {
				logger.Warn("failed to connect vault", "name", vc.Name, "path", vc.Path, "error", err)
				continue
			}
			logger.Info("vault auto-connected", "name", vc.Name, "path", vc.Path, "auto_index", vc.AutoIndex)
			if primer := sched.GetPrimer(); primer != nil {
				store := knowledgeAPI.GetVaultStore(vc.Path)
				if store != nil {
					primer.AddFileStore(vc.Name, store, knowledge.LayerPersonal)
					logger.Info("vault registered with primer", "name", vc.Name)
				}
			}
		}
		if vc.GitSync {
			// Find the store we just connected so the syncer can trigger reindex
			for _, vi := range knowledgeAPI.Vaults() {
				if vi.Name == vc.Name {
					// Re-fetch the FileStore by connecting info — the syncer needs it
					// to call Reindex() after each pull
					store := knowledgeAPI.GetVaultStore(vc.Path)
					if store != nil {
						gitSyncer.Add(vc.Name, vc.Path, store)
					}
					break
				}
			}
		}
	}

	// Auto-connect configured git sources (remote repos indexed as knowledge)
	for _, gsc := range cfg.Knowledge.GitSources {
		if knowledgeAPI == nil {
			// Knowledge not enabled but git sources configured — auto-enable
			knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
				Enabled: true,
				Engine:  "file",
			}, logger)
			logger.Info("auto-enabled knowledge API for git sources")
		}
		gsConfig := knowledge.GitSourceConfig{
			Name:    gsc.Name,
			URL:     gsc.URL,
			Branch:  gsc.Branch,
			Subpath: gsc.Subpath,
			Layer:   knowledge.LayerType(gsc.Layer),
		}
		if err := knowledgeAPI.ConnectGitSource(ctx, gsConfig); err != nil {
			logger.Warn("failed to connect git source",
				"name", gsc.Name,
				"url", gsc.URL,
				"subpath", gsc.Subpath,
				"error", err,
			)
		} else {
			logger.Info("git source connected",
				"name", gsc.Name,
				"url", gsc.URL,
				"subpath", gsc.Subpath,
				"layer", gsc.Layer,
			)
			// Register the FileStore with the scheduler's primer so agents
			// get primed with facts from this git source during kicks.
			if primer := sched.GetPrimer(); primer != nil {
				for _, gs := range knowledgeAPI.GitSources() {
					if gs.Name == gsc.Name && gs.Ready {
						store := knowledgeAPI.GetGitSourceStore(gsc.Name)
						if store != nil {
							primer.AddFileStore(gsc.Name, store, knowledge.LayerType(gsc.Layer))
						}
						break
					}
				}
			}
		}
	}

	// Auto-import configured document sources (PDFs, URLs as knowledge)
	for _, doc := range cfg.Knowledge.Documents {
		if knowledgeAPI == nil {
			knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
				Enabled: true,
				Engine:  "file",
			}, logger)
			logger.Info("auto-enabled knowledge API for document sources")
		}
		docConfig := knowledge.DocSourceConfig{
			Name:     doc.Name,
			URL:      doc.URL,
			FilePath: doc.FilePath,
			Layer:    knowledge.LayerType(doc.Layer),
		}
		meta, err := knowledgeAPI.ImportDocument(ctx, docConfig)
		if err != nil {
			logger.Warn("failed to import document source",
				"name", doc.Name,
				"error", err,
			)
		} else {
			logger.Info("document source imported",
				"name", doc.Name,
				"facts", meta.FactCount,
				"content_type", meta.ContentType,
			)
		}
	}

	go gitSyncer.Start(ctx)

	// Auto-enable knowledge API when not explicitly configured.
	// Both bead-synth-wiki and inception require it.
	if knowledgeAPI == nil {
		knowledgeAPI = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{
			Enabled: true,
			Engine:  "file",
		}, logger)
		logger.Info("auto-enabled file-based knowledge API")
	}

	var beadSynth *knowledge.BeadSynthesizer
	if len(beadStores) > 0 {
		synthVaultPath := cfg.Knowledge.BeadSynthesizer.VaultPath
		if synthVaultPath == "" {
			synthVaultPath = "/data/vaults/bead-synth-wiki"
		}
		if err := os.MkdirAll(synthVaultPath, 0o755); err != nil {
			logger.Warn("failed to create bead-synth vault dir", "path", synthVaultPath, "error", err)
		}
		if knowledgeAPI != nil {
			if connErr := knowledgeAPI.ConnectVault(synthVaultPath, "bead-synth-wiki"); connErr != nil {
				logger.Warn("failed to auto-connect bead-synth vault", "path", synthVaultPath, "error", connErr)
			} else {
				logger.Info("auto-connected bead-synth vault", "path", synthVaultPath)
				if primer := sched.GetPrimer(); primer != nil {
					store := knowledgeAPI.GetVaultStore(synthVaultPath)
					if store != nil {
						beadLayer := knowledge.LayerType(cfg.Knowledge.BeadSynthesizer.TargetLayer)
						if beadLayer == "" {
							beadLayer = knowledge.LayerPersonal
						}
						primer.AddFileStore("bead-synth-wiki", store, beadLayer)
						logger.Info("bead-synth vault registered with primer", "layer", beadLayer)
					}
				}
			}
		}
		var rawGH *gh.Client
		if ghClient != nil {
			rawGH = ghClient.GoGitHub()
		}

		var kRetention *knowledge.RetentionPolicy
		if rp := cfg.Knowledge.BeadSynthesizer.RetentionPolicy; rp != nil {
			kRetention = &knowledge.RetentionPolicy{
				MaxBeads:               rp.MaxBeads,
				ArchiveAfterSynthDays:  rp.ArchiveAfterSynthDays,
				HighPriorityRetainDays: rp.HighPriorityRetainDays,
				PreserveWithDeps:       rp.PreserveWithDeps,
			}
		} else {
			kRetention = &knowledge.RetentionPolicy{
				PreserveWithDeps: true,
			}
		}

		beadSynth = knowledge.NewBeadSynthesizer(beadStores, knowledgeAPI, knowledge.BeadSynthesizerConfig{
			Schedule:         cfg.Knowledge.BeadSynthesizer.Schedule,
			MinConfidence:    cfg.Knowledge.BeadSynthesizer.MinConfidence,
			TargetLayer:      cfg.Knowledge.BeadSynthesizer.TargetLayer,
			MaxFactsPerCycle: cfg.Knowledge.BeadSynthesizer.MaxFactsPerCycle,
			VaultPath:        synthVaultPath,
			Org:              cfg.Project.Org,
			Repos:            cfg.Project.Repos,
			RetentionPolicy:  kRetention,
		}, logger, rawGH)

		if cleaned, err := beadSynth.CleanupVault(); err != nil {
			logger.Warn("vault cleanup failed", "error", err)
		} else if cleaned > 0 {
			logger.Info("cleaned up low-quality bead-synth facts", "removed", cleaned)
		}

		if cfg.Knowledge.BeadSynthesizer.IsEnabled() && knowledgeAPI != nil {
			beadSynth.StartBackground(ctx)
			logger.Info("bead-to-wiki synthesizer started",
				"schedule", cfg.Knowledge.BeadSynthesizer.Schedule,
				"target_layer", cfg.Knowledge.BeadSynthesizer.TargetLayer,
				"vault_path", synthVaultPath,
				"bead_stores", len(beadStores),
			)
		}
	}

	// Open the graph store in a background goroutine. NewGraphStore acquires
	// a SQLite file lock that blocks if the old pod still holds it. Deferring
	// this lets the HTTP server start so the readiness probe passes, which
	// tells Kubernetes to terminate the old pod and release the lock.
	const graphStorePath = "/data/graph/knowledge.db"
	go func() {
		graphStore, graphErr := knowledge.NewGraphStore(graphStorePath, logger)
		if graphErr != nil {
			logger.Warn("failed to open knowledge graph store", "path", graphStorePath, "error", graphErr)
			return
		}
		logger.Info("knowledge graph store opened", "path", graphStorePath)
		if primer := sched.GetPrimer(); primer != nil {
			primer.SetGraphStore(graphStore)
		}
		if knowledgeAPI != nil {
			knowledgeAPI.SetGraphStore(graphStore)
			if primer := sched.GetPrimer(); primer != nil {
				knowledgeAPI.WireContext7Suggester(primer)
			}
		}
		if beadSynth != nil {
			beadSynth.SetGraphStore(graphStore)
		}
		if knowledgeAPI != nil {
			for _, ls := range knowledgeAPI.FileStores() {
				if n, err := graphStore.SyncFromFileStore(ls); err != nil {
					logger.Warn("graph sync failed", "store", ls.Name(), "error", err)
				} else if n > 0 {
					logger.Info("graph synced from vault", "store", ls.Name(), "triples", n)
				}
			}
		}
	}()

	go dashboard.StartWorkspaceCleanup(ctx, logger, dashSrv.GetAudit())

	os.MkdirAll(nousSnapshotDir, 0o755)
	os.MkdirAll(nousGovernorDir, 0o755)
	nousState := loadNousState(logger)
	nousState.SnapshotDir = nousSnapshotDir

	inceptionEngine := knowledge.NewInceptionEngine("/data", knowledgeAPI, logger)
	sched.SetInception(inceptionEngine)

	// Brainstorm is on-demand only. Only restart with bootstrap during
	// capture phase — structure/scaffold phases don't need a fresh kick
	// and restarting would revert the phase back to capture.
	// Skip stale inceptions (> 10 min old) — these are leftovers from
	// previous runs that would interfere with new inceptions.
	const staleInceptionThreshold = 10 * time.Minute
	if state := inceptionEngine.GetState(); state != nil &&
		state.Phase != knowledge.PhaseComplete &&
		state.Phase != knowledge.PhaseScaffold {
		if time.Since(state.StartedAt) < staleInceptionThreshold {
			msg := sched.BuildAgentMessage("brainstorm", nil, nil)
			if err := agentMgr.RestartWithBootstrap(ctx, "brainstorm", msg); err != nil {
				logger.Warn("failed to resume brainstorm for active inception", "error", err)
			} else {
				logger.Info("brainstorm resumed for active inception", "phase", state.Phase)
			}
		} else {
			logger.Info("skipping stale inception resume — resetting",
				"phase", state.Phase,
				"age", time.Since(state.StartedAt).Round(time.Second),
			)
			_ = inceptionEngine.Reset()
			if err := agentMgr.Pause("brainstorm", "startup", "stale inception cleared — on-demand only"); err != nil {
				logger.Debug("brainstorm pause on startup", "error", err)
			}
		}
	} else {
		if err := agentMgr.Pause("brainstorm", "startup", "on-demand agent — triggered by inception only"); err != nil {
			logger.Debug("brainstorm pause on startup", "error", err)
		}
	}

	dashSrv.RegisterAPI(&dashboard.Dependencies{
		Config:           cfg,
		AgentMgr:         agentMgr,
		Governor:         gov,
		GHClient:         ghClient,
		GHAppAuth:        appAuth,
		Tokens:           tokenCollector,
		Knowledge:        knowledgeAPI,
		Inception:        inceptionEngine,
		Nous:             nousState,
		Scheduler:        sched,
		MetricsCollector: metricsCollector,
		BeadSynthesizer:  beadSynth,
		BeadStores:       beadStores,
		Logger:           logger,
		Ctx:              ctx,
		RefreshFunc:      refreshDashboard,
		PersistFunc: func() {
			persistState(agentMgr, gov, cfg, tokenCollector, statePath, logger, dashSrv)
		},
		ReInitFunc: func() {
			initAgentConfigDrivenSystems(cfg)
		},
		SetUserClient: func(token string) {
			uc := github.NewClient(token, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.ResolvedAPIURL())
			userGHClient.Store(uc)
			logger.Info("user GitHub client updated via device flow")
		},
		EnumerateFunc: func() {
			runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, &userGHClient, nil, logger)
		},
		AdvisoryResetFunc: func(newPrimaryRepo string) {
			logger.Info("advisory reset: primary repo changed, creating new advisory issue", "repo", newPrimaryRepo)
			if ghClient != nil {
				num, err := ghClient.EnsureAdvisoryIssue(ctx, newPrimaryRepo)
				if err != nil {
					logger.Error("failed to create advisory issue on new primary repo", "repo", newPrimaryRepo, "error", err)
					if strings.Contains(err.Error(), "rate limit") {
						logger.Warn("GitHub API rate limit hit during advisory issue creation", "repo", newPrimaryRepo)
					} else if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "401") {
						dashSrv.SetGitHubAppRequired(true)
					}
				} else {
					advisoryIssues[newPrimaryRepo] = num
					os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num))
					dashSrv.SetGitHubAppRequired(false)
					dashSrv.ClearPendingGitHubAppInstall()
					logger.Info("advisory issue ready on new primary repo", "repo", newPrimaryRepo, "number", num)
				}
			}
		},
		ReinitGitHubFunc: func(newAppID, newInstallationID int64, keyFile string) error {
			newAppAuth, err := github.NewAppAuth(newAppID, newInstallationID, keyFile, logger, cfg.GitHub.ResolvedAPIURL())
			if err != nil {
				return fmt.Errorf("initializing app auth: %w", err)
			}
			newClient := github.NewClientFromApp(newAppAuth, cfg.Project.Org, cfg.Project.Repos, logger)
			if len(cfg.Governor.Labels.Exempt) > 0 {
				newClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
			}

			ghClient = newClient
			appAuth = newAppAuth
			agentMgr.SetAppAuth(newAppAuth)
			dashSrv.UpdateGitHubClient(newClient, newAppAuth)
			logger.Info("github client reinitialized via config API", "app_id", newAppID, "installation_id", newInstallationID)

			primaryRepo := cfg.Project.PrimaryRepo
			if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
				primaryRepo = cfg.Project.Repos[0]
			}
			if primaryRepo != "" {
				num, advErr := ghClient.EnsureAdvisoryIssue(ctx, primaryRepo)
				if advErr != nil {
					logger.Warn("advisory issue creation failed after reinit", "repo", primaryRepo, "error", advErr)
				} else {
					advisoryIssues[primaryRepo] = num
					os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num))
					logger.Info("advisory issue ready after reinit", "repo", primaryRepo, "number", num)
				}
			}
			return nil
		},
	})

	dashSrv.SetGitHubAppRequired(githubAppRequired)
	// Order matters: SetGitHubAppRequired(false) clears both fields, so the
	// classified state is applied only after it, and only when a failure was
	// actually detected.
	if githubAppRequired {
		dashSrv.SetGitHubAppState(githubAppState.String())
		if githubAppDiag != "" {
			dashSrv.SetGitHubAppPermIssue(githubAppDiag)
		}
	}

	// Wire up the manual re-check callback for the dashboard button.
	{
		recheckRepo := cfg.Project.PrimaryRepo
		if recheckRepo == "" && len(cfg.Project.Repos) > 0 {
			recheckRepo = cfg.Project.Repos[0]
		}
		if recheckRepo != "" {
			dashSrv.SetGitHubAppRecheckFn(func() bool {
				// The Re-check button is the first thing an owner clicks on a
				// degraded hive. Report the real cause instead of the generic
				// "not accessible" — there is no client to check WITH, so the
				// credentials themselves are what must be fixed.
				if ghClient == nil {
					logger.Warn("github app recheck: hive is running without GitHub credentials", "detail", appAuthFailure)
					dashSrv.AuditLog("system", "github_app_check", "result=no GitHub client: "+appAuthFailure, "")
					return false
				}
				num, err := ghClient.EnsureAdvisoryIssue(ctx, recheckRepo)
				if err != nil {
					logger.Debug("github app recheck: not accessible", "repo", recheckRepo, "error", err)
					dashSrv.AuditLog("system", "github_app_check", "result=not accessible (app not installed / no read)", "")
					return false
				}
				advisoryIssues[recheckRepo] = num
				os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num))
				// Finding the advisory issue only proves the app is installed
				// (reads succeed on public repos even with a token from the
				// wrong installation). Verify write capability before letting
				// the handler clear the banner, so Re-check can't produce a
				// clears-then-returns flip-flop.
				// Before reporting a wrong-account installation, try to fix it:
				// this is the exact case rediscovery exists for. Cached by TTL,
				// so a repeated re-check does not re-hit the API.
				healGitHubAppInstallation(ctx, ghClient.AppAuth(), cfg, logger)
				if diag, state := diagnoseGitHubApp(ctx, ghClient.AppAuth(), cfg.Project.Org); diag != "" {
					dashSrv.SetGitHubAppPermIssue(diag)
					dashSrv.SetGitHubAppState(state.String())
					logger.Warn("github app recheck: app detected but write not verified",
						"repo", recheckRepo, "state", state.String(),
						"operator_actionable", state.OperatorActionable(), "detail", diag)
					dashSrv.AuditLog("system", "github_app_check", "result=installed but write NOT verified: "+diag, "")
					return false
				}
				logger.Info("github app recheck: app detected, write verified", "repo", recheckRepo, "number", num)
				dashSrv.AuditLog("system", "github_app_check", "result=OK (installed, write verified)", "")
				return true
			})
		}
	}

	// Self-heal the "GitHub App not installed" banner. This handles:
	// 1. GitHub App credentials arrived after startup (via heartbeat/webhook)
	// 2. ReinitGitHubFunc succeeded but cleared githubAppRequired before
	//    EnsureAdvisoryIssue could run against the new client
	// 3. A TRANSIENT startup/runtime 4xx (rate-limit blip, brief token-refresh
	//    window, momentary permission propagation delay) latched the banner even
	//    though the app is really installed and can write. Previously the retry
	//    loop exited permanently after the first advisory-issue READ succeeded,
	//    so a later transient write failure that re-set the flag was never
	//    re-evaluated — the banner stuck until the pod was restarted.
	//
	// The loop therefore runs for the lifetime of the process (it does NOT
	// return after the first success) and, whenever the banner is currently
	// showing, re-runs the SAME read+write verification as the manual "Re-check"
	// button (githubAppRecheckFn, which calls diagnoseGitHubAppWrite) and clears
	// the flag on success. When the banner is not showing there is nothing to do,
	// so the tick is a cheap no-op that makes no GitHub API calls.
	{
		primaryRepo := cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
			primaryRepo = cfg.Project.Repos[0]
		}
		if primaryRepo != "" {
			// githubAppSelfHealInterval mirrors the heartbeat cadence so a stale
			// banner clears within one heartbeat window of the app becoming
			// healthy, without adding meaningful GitHub API load (the check only
			// runs while the banner is actually showing).
			const githubAppSelfHealInterval = 2 * time.Minute
			go func() {
				ticker := time.NewTicker(githubAppSelfHealInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						// Nothing to heal unless the banner is showing.
						if !dashSrv.IsGitHubAppRequired() {
							continue
						}
						// Re-run the same read+write verification the manual
						// Re-check button uses. It clears the flag on success
						// (installed AND write-verified) and leaves it set on a
						// genuine failure (not installed / insufficient perms).
						if dashSrv.RecheckGitHubApp() {
							if num, exists := advisoryIssues[primaryRepo]; exists {
								os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num))
							}
							logger.Info("github app self-heal: banner cleared, app installed and write verified", "repo", primaryRepo)
						} else {
							logger.Debug("github app self-heal: still not verified, banner remains", "repo", primaryRepo)
						}
					}
				}
			}()
		}
	}

	if brainstormBeads, ok := beadStores["brainstorm"]; ok {
		inceptionWatcher := dashboard.NewInceptionWatcher(brainstormBeads, inceptionEngine, sched, agentMgr, gov, logger)
		go inceptionWatcher.Run(ctx)
	}

	if saved == nil {
		if levelStr := os.Getenv("HIVE_LEVEL"); levelStr != "" {
			const maxACMMLevel = 6
			level, err := strconv.Atoi(levelStr)
			if err != nil || level < 1 || level > maxACMMLevel {
				logger.Warn("invalid HIVE_LEVEL, skipping auto-apply", "value", levelStr)
			} else {
				logger.Info("first start detected, auto-applying ACMM pack", "level", level)
				result, err := dashSrv.ApplyPack(level)
				if err != nil {
					logger.Error("failed to auto-apply ACMM pack", "level", level, "error", err)
				} else {
					logger.Info("ACMM pack auto-applied",
						"level", level,
						"name", result.Name,
						"created", result.Created,
						"skipped", result.Skipped,
						"paused", result.Paused,
						"resumed", result.Resumed,
					)
				}
			}
		}
	} else {
		// Config file is authoritative on restarts; HIVE_LEVEL env var is
		// only a fallback for initial provisioning when no level is persisted.
		const maxACMMLevel = 6
		level := 0
		if cfg.ACMMLevel != nil && *cfg.ACMMLevel >= 1 && *cfg.ACMMLevel <= maxACMMLevel {
			level = *cfg.ACMMLevel
		} else if saved.ACMMLevel != nil && *saved.ACMMLevel >= 1 && *saved.ACMMLevel <= maxACMMLevel {
			level = *saved.ACMMLevel
		} else if levelStr := os.Getenv("HIVE_LEVEL"); levelStr != "" {
			if parsed, err := strconv.Atoi(levelStr); err == nil && parsed >= 1 && parsed <= maxACMMLevel {
				level = parsed
			} else {
				logger.Warn("invalid HIVE_LEVEL, skipping auto-apply", "value", levelStr)
			}
		}
		if level > 0 {
			action := "merging pack updates"
			if saved.ACMMLevel == nil || *saved.ACMMLevel != level {
				action = "re-applying pack (level changed)"
			}
			logger.Info("audit: "+action, "level", level, "saved_level", saved.ACMMLevel, "trigger", "startup")
			result, err := dashSrv.ApplyPack(level)
			if err != nil {
				logger.Error("failed to apply ACMM pack", "level", level, "error", err)
			} else {
				logger.Info("ACMM pack applied on startup",
					"level", level,
					"name", result.Name,
					"created", result.Created,
					"updated", result.Updated,
					"skipped", result.Skipped,
					"paused", result.Paused,
					"resumed", result.Resumed,
				)
			}
		}
	}

	if cfg.Policies.Repo != "" {
		localDir := cfg.Policies.LocalDir
		if localDir == "" {
			localDir = "/data/policies"
		}
		watcher := policies.NewWatcher(
			cfg.Policies.Repo,
			cfg.Policies.Branch,
			cfg.Policies.Path,
			localDir,
			cfg.Policies.PollInterval,
			logger,
		)
		if err := watcher.Start(ctx); err != nil {
			logger.Warn("policy watcher failed to start", "error", err)
		}
	}

	// Watch hive.yaml for external changes and reload config when modified
	configWatcher := config.NewWatcher(*configPath, func(newCfg *config.Config) {
		// Preserve runtime-only fields that are not in the YAML
		newCfg.HiveID = cfg.HiveID

		// Preserve ACMM level from the agent manager — it is the
		// authoritative source. The file may have a stale value if
		// a watcher reload races with a level-switch saveConfig().
		if cfg.ACMMLevel != nil {
			newCfg.ACMMLevel = cfg.ACMMLevel
		}

		// Capture the outgoing GitHub App identity before the swap so we can
		// tell whether the reload changed it.
		prevGitHub := cfg.GitHub

		// Swap the in-memory config pointer contents
		*cfg = *newCfg

		// Re-sync subsystems that cache config values
		ghClient.SetRepos(cfg.Project.Repos)
		if uc := userGHClient.Load(); uc != nil {
			uc.SetRepos(cfg.Project.Repos)
		}
		gov.UpdateConfig(cfg.Governor)

		// Re-apply live agent definitions (definition_source) on reload so an
		// operator's edit to a linked repo propagates. Merges only operator-safe
		// fields; a fetch failure keeps each agent's baked definition. Runs before
		// initAgentConfigDrivenSystems so downstream systems see the merged config.
		defsrc.ApplyToConfig(context.Background(), cfg, definitionResolver, logger)

		initAgentConfigDrivenSystems(cfg)

		// Rebuild GitHub App auth when its identity changed. AppAuth captures
		// app_id/installation_id at construction, so without this a corrected
		// installation_id in hive.yaml keeps minting tokens for the OLD
		// installation until the pod restarts.
		if prevGitHub.AppID != cfg.GitHub.AppID ||
			prevGitHub.InstallationID != cfg.GitHub.InstallationID ||
			prevGitHub.KeyFile != cfg.GitHub.KeyFile ||
			prevGitHub.APIURL != cfg.GitHub.APIURL {
			if cfg.GitHub.HasUsableApp() && cfg.GitHub.KeyFile != "" {
				newAppAuth, appErr := github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, cfg.GitHub.KeyFile, logger, cfg.GitHub.ResolvedAPIURL())
				if appErr != nil {
					logger.Error("github app auth rebuild after config reload failed", "error", appErr)
				} else {
					newClient := github.NewClientFromApp(newAppAuth, cfg.Project.Org, cfg.Project.Repos, logger)
					if len(cfg.Governor.Labels.Exempt) > 0 {
						newClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
					}
					ghClient = newClient
					appAuth = newAppAuth
					agentMgr.SetAppAuth(newAppAuth)
					dashSrv.UpdateGitHubClient(newClient, newAppAuth)
					logger.Info("github app auth rebuilt after config reload",
						"app_id", cfg.GitHub.AppID,
						"installation_id", cfg.GitHub.InstallationID,
					)
				}
			}
		}

		refreshDashboard()
	}, logger)
	dashSrv.SetSkipReloadFunc(configWatcher.SkipNext)
	go configWatcher.Start(ctx)

	// Persist operator pause/resume into the on-disk config so it survives
	// restarts and upgrades. Without this, a pod restart rebuilt every agent
	// un-paused, silently undoing an operator's pause on the next upgrade.
	// Concurrent pauses (e.g. an ACMM pack pausing several agents in a loop,
	// or login-detector firing while the operator pauses) each did an
	// unsynchronized cfg.Agents map write + cfg.Save(). The saves clobbered
	// each other (last writer wins), so only some pauses reached the PVC and
	// the rest were silently lost on the next restart. Serialize the
	// read-modify-save under a dedicated mutex so every pause transition is
	// durably persisted.
	agentMgr.SetPersistPauseCallback(func(name string, paused bool) {
		configWatcher.SkipNext() // don't let our own write trigger a reload
		changed, err := cfg.SetAgentPausedAndSave(name, paused)
		if err != nil {
			logger.Warn("failed to persist agent pause state", "agent", name, "paused", paused, "error", err)
		}
		_ = changed
	})

	// Persist the fully-expanded prompt text of every kick so owners can review
	// what their agents were actually told, over a day/week window, in the
	// per-agent "Prompts" tab. Redaction and truncation happen inside the
	// store, before anything is written to the PVC.
	agentMgr.SetRecordPromptCallback(dashSrv.RecordPrompt)

	// Register custom GHE hostnames with the proxy allowlist so mode
	// enforcement applies to GitHub Enterprise API and web requests.
	for _, rawURL := range []string{cfg.GitHub.ResolvedAPIURL(), cfg.GitHub.ResolvedBaseURL()} {
		if parsed, err := url.Parse(rawURL); err == nil && parsed.Host != "" {
			proxy.RegisterGitHubHost(parsed.Host)
		}
	}

	dashboard.SetBackendAuthProvider(agentMgr.BackendAuthAvailable)

	githubProxy, err := proxy.NewGitHubProxy(logger, cfg.Project.Org, cfg.Project.Repos)
	if err != nil {
		logger.Error("failed to create github proxy", "error", err)
	} else {
		dashboard.SetProxyViolationsProvider(githubProxy.Violations)
		// Lets the dashboard narrow the LiteLLM model dropdown to the set the
		// configured key is entitled to, learned by the proxy from a key-info
		// probe or a "team not allowed" 403.
		dashboard.SetEntitledModelsProvider(githubProxy.EntitledModels)

		// Wire the inference token sink so the translator records per-agent
		// usage (from the gateway's OpenAI usage block) into the same metrics
		// dir the token collector scans. Without this, bare-mode inference
		// agents (litellm/vllm/llm-d) never write a scannable session file and
		// their consumption reads as zero.
		githubProxy.SetTokenSink(tokens.NewInferenceSink(cfg.Data.MetricsDir, logger))

		// With the sink active, the proxy also MITMs the Copilot completion host
		// (api.githubcopilot.com) to record Copilot token usage live per
		// response — so Copilot cost shows up while an agent runs instead of only
		// tallying at session shutdown. Tell the collector to defer Copilot token
		// accrual to the sink ONLY for sessions active from NOW on (the moment
		// live capture starts). Sessions that ended earlier were never sniffed by
		// the proxy, so the scanner keeps counting their shutdown tokens —
		// otherwise all pre-existing Copilot spend would vanish.
		tokenCollector.SetCopilotLiveCapture(time.Now().UnixMilli())

		vllmEndpoints := parseEndpointList(envOrDefault("HIVE_VLLM_ENDPOINT", "http://hive-vllm-svc.hive-inference.svc.cluster.local:8000"))
		llmdEndpoints := parseEndpointList(envOrDefault("HIVE_LLMD_ENDPOINT", "http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000"))
		inferenceEndpoints := map[string][]string{
			"vllm":  vllmEndpoints,
			"llm-d": llmdEndpoints,
		}
		// litellm has no in-cluster default: register it only when an
		// endpoint is configured (yaml or HIVE_LITELLM_ENDPOINT), so an
		// unconfigured backend doesn't show up in model discovery. A URL
		// saved later from the governor LiteLLM tab is registered at
		// runtime via dashSrv.UpdateInferenceEndpoint.
		if cfg.Governor.LiteLLM.LocalProxy {
			inferenceEndpoints["litellm"] = []string{litellmLocalProxyURL()}
		} else if litellmEndpoint := cfg.Governor.LiteLLM.ResolveEndpoint(); litellmEndpoint != "" {
			inferenceEndpoints["litellm"] = parseEndpointList(litellmEndpoint)
		}
		// Register every explicitly-configured named gateway's endpoint by
		// gateway NAME so the Model Gateways tab's per-gateway model discovery
		// and per-gateway routing resolve on boot (the legacy "litellm" block
		// is already registered above; ResolvedGateways only synthesizes it
		// when no explicit gateways are set, so this loop never double-adds it).
		for _, gw := range cfg.Governor.Gateways {
			if ep := strings.TrimSpace(gw.Endpoint); ep != "" {
				inferenceEndpoints[gw.Name] = parseEndpointList(ep)
			}
		}
		dashSrv.SetInferenceEndpoints(inferenceEndpoints)
		// Treat any configured gateway name as an inference-routable backend so
		// an agent with backend: <gateway> routes through it. Resolution is live
		// (reads cfg on each call) so gateways added from the Model Gateways tab
		// take effect without a restart.
		agentMgr.SetGatewayBackendChecker(func(backend string) bool {
			return cfg.Governor.ResolveGateway(backend) != nil &&
				!strings.EqualFold(backend, "") // empty is the default, not a named backend
		})
		agentMgr.SetInferenceCallbacks(
			func(agentName, backend, model string) {
				// Named model gateway (OpenRouter, a second LiteLLM, etc.): resolve
				// endpoint/key/model from the gateway and route through it. Built-in
				// backend names (litellm/vllm/llm-d) are handled below; a gateway
				// literally named "litellm" resolves here to the same legacy block
				// via ResolvedGateways, so behavior is identical.
				if gw := cfg.Governor.ResolveGateway(backend); gw != nil && !config.IsInferenceBackend(backend) {
					endpoint := gw.Endpoint
					if endpoint == "" {
						logger.Warn("gateway backend selected but no endpoint configured",
							"agent", agentName, "gateway", backend, "model", model)
						return
					}
					if model == "" {
						model = gw.DefaultModel
					}
					githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:  backend,
						Endpoint: endpoint,
						Model:    model,
						APIKey:   gw.ResolveAPIKey(),
						CABundle: gw.CABundle,
					})
					return
				}
				if backend == "litellm" {
					// Resolve endpoint/key at call time so a URL saved from
					// the governor LiteLLM tab (or a rotated key) takes
					// effect without a hive restart. cfg is the live config
					// pointer — the config watcher swaps its contents in
					// place on reload.
					lc := cfg.Governor.LiteLLM
					endpoint := lc.ResolveEndpoint()
					if lc.LocalProxy {
						// Local fallback: the Go translator forwards to the
						// bundled litellm proxy on loopback instead of the
						// remote endpoint.
						endpoint = litellmLocalProxyURL()
					}
					if endpoint == "" {
						logger.Warn("litellm backend selected but no endpoint configured",
							"agent", agentName, "model", model)
						return
					}
					if model == "" {
						model = lc.DefaultModel
					}
					githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:  backend,
						Endpoint: endpoint,
						Model:    model,
						APIKey:   lc.ResolveAPIKey(),
						CABundle: lc.CABundle,
					})
					return
				}
				endpoints := vllmEndpoints
				if backend == "llm-d" {
					endpoints = llmdEndpoints
				}
				// vllm/llm-d endpoints are unauthenticated with a public
				// or in-cluster CA — no bearer key or custom CA bundle.
				endpoint := proxy.FindEndpointForModel(endpoints, model, "", "")
				if endpoint == "" {
					logger.Warn("no endpoint serves model, using first endpoint",
						"agent", agentName, "model", model, "backend", backend)
					endpoint = endpoints[0]
				}
				githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
					Backend:  backend,
					Endpoint: endpoint,
					Model:    model,
				})
			},
			func(agentName string) {
				githubProxy.ClearInferenceRoute(agentName)
			},
		)

		go func() {
			if err := githubProxy.Start(); err != nil {
				logger.Error("github proxy failed", "error", err)
			}
		}()
		go func() {
			if err := githubProxy.StartInferenceTranslator(); err != nil {
				logger.Error("inference translation server failed", "error", err)
			}
		}()
		if cfg.Governor.LiteLLM.LocalProxy {
			go superviseLocalLiteLLM(ctx, logger)
		}
		logger.Info("github proxy started", "addr", githubProxy.ListenAddr())
	}

	go func() {
		if err := dashSrv.Start(); err != nil {
			logger.Error("dashboard server failed", "error", err)
		}
	}()

	if cfg.Notifications.Discord != nil && cfg.Notifications.Discord.BotToken != "" && cfg.Notifications.Discord.ChannelID != "" {
		discordBot := discord.NewBot(discord.Config{
			Token:          cfg.Notifications.Discord.BotToken,
			ChannelID:      cfg.Notifications.Discord.ChannelID,
			DashboardURL:   fmt.Sprintf("http://localhost:%d", cfg.Dashboard.Port),
			DashboardToken: os.Getenv("HIVE_DASHBOARD_TOKEN"),
		}, logger)
		var agentNameList []string
		for name := range cfg.EnabledAgents() {
			agentNameList = append(agentNameList, name)
		}
		discordBot.SetAgentNames(agentNameList)
		if err := discordBot.Start(ctx); err != nil {
			logger.Warn("discord bot failed to start", "error", err)
		} else {
			logger.Info("discord bot started", "channel", cfg.Notifications.Discord.ChannelID)
		}
	}

	onDemandFromPack := config.OnDemandAgentsFromPacks()
	if len(onDemandFromPack) > 0 {
		logger.Info("on-demand agents from pack definitions", "agents", onDemandFromPack)
	}
	// One visible "hive restarted" marker per boot, so the audit log shows a
	// restart happened (and at what build) instead of only a burst of
	// per-agent agent_start rows. Include a count of persisted pauses being
	// restored so the operator can confirm pause state survived the restart.
	pausedCount := 0
	for _, ac := range cfg.EnabledAgents() {
		if ac.Paused {
			pausedCount++
		}
	}
	dashSrv.AuditLog("system", "hive_restart",
		fmt.Sprintf("build=%s version=%s; restoring %d paused agent(s)", gitShort, "3.0.0", pausedCount), "")

	const agentLaunchDelaySec = 15
	agentIndex := 0
	for name, ac := range cfg.EnabledAgents() {
		isOnDemand := ac.OnDemand || onDemandFromPack[name]
		if isOnDemand {
			logger.Info("skipping on-demand agent at startup", "name", name)
			continue
		}
		if agentIndex > 0 {
			logger.Info("staggering agent launch", "name", name, "delay_sec", agentLaunchDelaySec)
			time.Sleep(time.Duration(agentLaunchDelaySec) * time.Second)
		}
		logger.Info("audit: starting agent", "name", name, "trigger", "startup")
		if err := agentMgr.Start(ctx, name); err != nil {
			logger.Warn("failed to start agent", "name", name, "error", err)
		} else {
			// Surface whether a persisted operator pause was honored on this
			// restart, so the audit log shows pause state survived (or didn't).
			detail := "trigger=startup"
			if ac.Paused {
				detail = "trigger=startup; restored paused (persisted)"
			}
			dashSrv.AuditLog("system", "agent_start", detail, name)
		}
		agentIndex++
	}

	// Start hub heartbeat push if configured (env var or config)
	hubURL := cfg.Hub.URL
	if envHub := os.Getenv("HIVE_HUB_URL"); envHub != "" {
		hubURL = envHub
		cfg.Hub.Enabled = true
		cfg.Hub.URL = envHub
	}
	if envCluster := os.Getenv("HIVE_CLUSTER_ID"); envCluster != "" {
		cfg.Hub.ClusterID = envCluster
	}
	if cfg.Hub.Enabled && hubURL != "" {
		// Heartbeat cadence is INDEPENDENT of the governor eval interval. It was
		// previously tied to cfg.Governor.EvalIntervalS, so a low-ACMM hive
		// (which evaluates infrequently by design — e.g. ~10 min at L2) beat the
		// hub only every ~10 min. The hub marks a hive stale after
		// heartbeatHealthStaleness (5 min), so such hives showed a gray/stale
		// dot for half of every cycle despite being perfectly healthy. Beat on a
		// fixed interval comfortably under that 5-min threshold so every hive,
		// regardless of ACMM level, stays fresh on the hub.
		const heartbeatSendInterval = 2 * time.Minute
		go hub.StartHeartbeat(ctx, hubURL, func() *hub.HeartbeatPayload {
			if !cfg.Hub.Enabled {
				return nil
			}
			statuses := agentMgr.AllStatuses()
			govState := gov.GetState()
			agents := make([]hub.AgentSummary, 0, len(statuses))
			for name, proc := range statuses {
				as := hub.AgentSummary{Name: name, State: string(proc.State)}
				if ac, ok := cfg.Agents[name]; (ok && ac.OnDemand) || onDemandFromPack[name] {
					as.Mode = "on_demand"
				}
				agents = append(agents, as)
			}
			acmmLvl := 0
			if cfg.ACMMLevel != nil {
				acmmLvl = *cfg.ACMMLevel
			}
			// Attach cached fleet-stat counts only once the collector has done a
			// successful compute — nil pointers until then, so the hub never
			// aggregates a not-yet-computed zero into the public fleet total.
			var prsMerged, prsRejected, cvesClosed *int
			if fc, ok := fleetStatsCollector.Snapshot(); ok {
				m, rj, cv := fc.PRsMerged, fc.PRsRejected, fc.CVEsClosed
				prsMerged, prsRejected, cvesClosed = &m, &rj, &cv
			}
			// Count agents with a method/model assigned for the hub's
			// user-journey stage detection. Always a non-nil pointer from a
			// spoke new enough to compute it, so the hub can distinguish
			// "genuinely zero agents configured" from "old spoke, unknown".
			agentsWithModel := agentMgr.CountAgentsWithModel()
			return &hub.HeartbeatPayload{
				AgentsWithModel: &agentsWithModel,
				HiveID:          cfg.HiveID,
				Org:             cfg.Project.Org,
				AIAuthor:          cfg.Project.AIAuthor,
				AIAuthorEffective: cfg.EffectiveAIAuthor(),
				StartedAt:         processStartedAt.UTC().Format(time.RFC3339),
				Repos:           cfg.Project.Repos,
				PrimaryRepo:     cfg.Project.PrimaryRepo,
				ACMMLevel:       acmmLvl,
				Agents:          agents,
				Governor:        hub.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs},
				// Tokens carries the spoke's authoritative cumulative token
				// total (same store the dashboard token panel and governor
				// budget read). It flows to the hub's My Hives token column so
				// heartbeat-only hives (reached via heartbeat, not hub-kubectl)
				// display real consumption. Refreshed each heartbeat, so the
				// column is as fresh as the last heartbeat. Despite the
				// "24h"-suffixed field name this is a lifetime/window total,
				// consistent with what the spoke dashboard already shows.
				Tokens24h: func() int64 {
					if tokenCollector == nil {
						return 0
					}
					if summary := tokenCollector.Summary(); summary != nil {
						return summary.TotalTokens
					}
					return 0
				}(),
				Contributors: func() hub.ContributorSummary {
					reg, active := dashSrv.ContributorSummary()
					return hub.ContributorSummary{Registered: reg, Active: active}
				}(),
				Leaderboard: func() []hub.LeaderboardEntry {
					lb := dashSrv.LeaderboardForHub()
					out := make([]hub.LeaderboardEntry, len(lb))
					for i, e := range lb {
						out[i] = hub.LeaderboardEntry{
							GitHubUsername: e.GitHubUsername,
							AvatarURL:      e.AvatarURL,
							TrustTier:      e.TrustTier,
							TasksCompleted: e.TasksCompleted,
							TasksFailed:    e.TasksFailed,
							Active:         e.Active,
							CurrentTask:    e.CurrentTask,
						}
					}
					return out
				}(),
				Owner: func() string {
					if td, err := os.ReadFile("/data/gh-user-token"); err == nil {
						tok := strings.TrimSpace(string(td))
						if tok != "" {
							// gh-user-token is a github.com OAuth token — validate its
							// identity against github.com, not the (possibly GHE) repo host.
							if u, err := github.ValidateToken(tok, cfg.GitHub.OAuthAPIURL()); err == nil {
								return u.Login
							}
						}
					}
					return ""
				}(),
				// Report the API URL we are actually running against so the hub
				// can see whether a GitHub Enterprise API URL it delivered has
				// landed. Resolved (never empty) so the hub can distinguish
				// "public github.com" from "spoke too old to report this".
				GitHubAPIURL: cfg.GitHub.ResolvedAPIURL(),
				Health:       dashSrv.HealthSummary(),
				DashboardURL: func() string {
					if cfg.Hub.DashboardURL != "" {
						return cfg.Hub.DashboardURL
					}
					if cfg.HiveID != "" && cfg.Hub.URL != "" {
						if u, err := url.Parse(cfg.Hub.URL); err == nil && u.Host != "" {
							return fmt.Sprintf("https://%s.%s", cfg.HiveID, u.Host)
						}
					}
					return fmt.Sprintf("http://localhost:%d", cfg.Dashboard.Port)
				}(),
				SnapshotURL: cfg.Hub.SnapshotURL,
				HiveType:    cfg.Hub.HiveType,
				ClusterID:   cfg.Hub.ClusterID,
				IsPublic:    cfg.Hub.IsPublic,
				Version:     "3.0.0",
				GitHash:     gitShort,
				GitBranch:   gitBranch,
				// The image ref the Deployment tracks, read in-cluster and
				// cached. The hub cannot see it for firewalled spokes, and it
				// is the only way to distinguish a hive pinned to an immutable
				// SHA tag (which can never receive a rolling upgrade) from one
				// riding <branch>-latest. Empty off-cluster — never guessed.
				ImageRef:                hub.SelfDeploymentImage(),
				// The GitHub instance this spoke actually runs against. Only
				// the spoke knows this for certain: a hive's GitHub can differ
				// from its cluster's default, so the hub cannot infer it.
				// Reported as a bare hostname; empty base_url means public
				// github.com, which githubHostLabel renders as such.
				GitHubHost:              hub.GitHubHostLabel(cfg.GitHub.BaseURL),
				GitHubAppRequired:       dashSrv.IsGitHubAppRequired(),
				GitHubAppPermIssue:      dashSrv.GetGitHubAppPermIssue(),
				GitHubAppState:          dashSrv.GetGitHubAppState(),
				PendingGitHubAppInstall: dashSrv.IsPendingGitHubAppInstall(),
				AutoUpgrade:             cfg.Hub.AutoUpgrade,
				ClusterHealth: func() *hub.HeartbeatClusterHealthReport {
					if os.Getenv("HIVE_CLUSTER_ID") == "" {
						return nil
					}
					return hub.CollectClusterHealth(logger)
				}(),
				PRsMerged90d:   prsMerged,
				PRsRejected90d: prsRejected,
				CVEsClosed:     cvesClosed,
				// Report WHICH App key we hold, never the key. The hub compares
				// this against its per-cluster key and pushes a correction only
				// on a mismatch, so a spoke already holding the right key costs
				// nothing and a spoke holding the wrong one self-heals.
				GitHubAppKeyFingerprint: reportedAppKeyFingerprint(cfg.GitHub.KeyFile),
				GitHubAppKeyPerHive:     hasPerHiveAppKey(cfg.GitHub.KeyFile),
				// Report the App this hive believes it authenticates as. The hub
				// pairs it with the fingerprint above to tell a per-hive key that
				// is WRONG for this App from one that is deliberately for another.
				GitHubAppID: cfg.GitHub.AppID,
			}
		}, heartbeatSendInterval, logger, hub.UpgradeCallback(func(targetSHA string) {
			const upgradeMarkerPath = "/data/upgrade-requested"

			// Never self-upgrade to the commit we are already running. The hub
			// may instruct an upgrade to a short SHA that is a prefix of our
			// full gitShort (or vice-versa); treating that as "behind" caused a
			// crash-loop (patch → 403 → os.Exit → repeat) on hives sitting
			// exactly at HEAD. Prefix-compare so same-commit is a no-op.
			if sha1, sha2 := targetSHA, gitShort; sha1 != "" && sha2 != "" {
				n := len(sha1)
				if len(sha2) < n {
					n = len(sha2)
				}
				if strings.EqualFold(sha1[:n], sha2[:n]) {
					logger.Info("self-upgrade skipped: target is the running commit",
						"target", targetSHA, "current", gitShort)
					return
				}
			}

			// If a previous process already attempted an upgrade and we booted
			// with the same git hash, the image tag didn't actually change.
			// Skip to avoid an infinite restart loop.
			if markerData, err := os.ReadFile(upgradeMarkerPath); err == nil {
				if strings.Contains(string(markerData), fmt.Sprintf(`"current_sha":"%s"`, gitShort)) {
					logger.Info("self-upgrade skipped: already attempted from this SHA (image unchanged)",
						"target", targetSHA,
						"current", gitShort,
					)
					os.Remove(upgradeMarkerPath)
					return
				}
			}

			// Minimum uptime before allowing self-upgrade to avoid restart loops.
			const minUptimeBeforeUpgrade = 5 * time.Minute
			uptime := time.Since(startTime)
			if uptime < minUptimeBeforeUpgrade {
				logger.Warn("self-upgrade deferred: minimum uptime not reached",
					"target", targetSHA,
					"current", gitShort,
					"uptime", uptime.Round(time.Second),
					"min_uptime", minUptimeBeforeUpgrade,
				)
				return
			}

			marker := fmt.Sprintf(`{"target_sha":"%s","current_sha":"%s","requested_at":"%s"}`,
				targetSHA, gitShort, time.Now().UTC().Format(time.RFC3339))
			if err := os.WriteFile(upgradeMarkerPath, []byte(marker), 0o644); err != nil {
				logger.Warn("failed to write upgrade marker", "path", upgradeMarkerPath, "error", err)
			}

			logger.Info("self-upgrade triggered: sending upgrading heartbeat then exiting",
				"current", gitShort,
				"latest", targetSHA,
				"uptime", uptime.Round(time.Second),
			)

			hub.SendUpgradingHeartbeat(hubURL, func() *hub.HeartbeatPayload {
				if !cfg.Hub.Enabled {
					return nil
				}
				statuses := agentMgr.AllStatuses()
				agents := make([]hub.AgentSummary, 0, len(statuses))
				for name, proc := range statuses {
					as := hub.AgentSummary{Name: name, State: string(proc.State)}
					if ac, ok := cfg.Agents[name]; (ok && ac.OnDemand) || onDemandFromPack[name] {
						as.Mode = "on_demand"
					}
					agents = append(agents, as)
				}
				acmmLvl := 0
				if cfg.ACMMLevel != nil {
					acmmLvl = *cfg.ACMMLevel
				}
				return &hub.HeartbeatPayload{
					HiveID:    cfg.HiveID,
					Org:       cfg.Project.Org,
					ACMMLevel: acmmLvl,
					Agents:    agents,
					GitHash:   gitShort,
					ClusterID: cfg.Hub.ClusterID,
					HiveType:  cfg.Hub.HiveType,
					IsPublic:  cfg.Hub.IsPublic,
					Version:   "3.0.0",
				}
			}, targetSHA, logger)

			// A plain rollout restart only advances a deployment tracking a
			// MUTABLE tag. On a SHA-pinned deployment it relaunches the very
			// same image, so the hive reports the old hash and the hub re-sends
			// this upgrade every heartbeat — a restart loop that never lands.
			// UpgradeSelfToSHA patches the image instead when we are pinned.
			needsRestart, err := hub.UpgradeSelfToSHA(logger, targetSHA)
			if err != nil {
				logger.Warn("pinned-image upgrade failed, falling back to rolling restart",
					"target", targetSHA, "error", err)
				needsRestart = true
			}
			if needsRestart {
				if err := hub.RolloutRestartSelf(logger); err != nil {
					logger.Warn("rolling restart failed, falling back to os.Exit",
						"error", err,
					)
					os.Exit(0)
				}
			}
			// Rolling restart initiated — K8s will start a new pod and
			// send SIGTERM to this one once the replacement is Ready.
			// Block here so the process stays alive until terminated.
			logger.Info("waiting for SIGTERM after rolling restart")
			<-ctx.Done()
		}), hub.GitHubAppConfigCallback(func(ghCfg *hub.HeartbeatGitHubAppConfig) {
			logger.Info("received github app config via heartbeat",
				"app_id", ghCfg.AppID,
				"installation_id", ghCfg.InstallationID,
				"has_key", ghCfg.PrivateKey != "",
			)

			keyPath := spokeAppKeyPath
			// keyChanged gates dropping the cached installation token below: a
			// token minted under the previous key is invalid the moment the key
			// is replaced, but a redelivery of the SAME key must not throw away a
			// perfectly good token every heartbeat.
			keyChanged := false
			if ghCfg.PrivateKey != "" {
				// Fingerprint before and after so the key rotation is auditable
				// from the spoke's own logs. Fingerprints only — the key itself
				// is never logged.
				beforeFP, _ := config.AppKeyFingerprintFromFile(keyPath)
				if err := os.WriteFile(keyPath, []byte(ghCfg.PrivateKey), spokeAppKeyFileMode); err != nil {
					logger.Error("failed to write github app key from heartbeat", "error", err)
					return
				}
				// os.WriteFile does NOT re-apply the mode to a file that already
				// exists, so a key written by an older build (or restored from a
				// looser-moded source) would keep its old permissions forever.
				// Chmod unconditionally so every path converges on 0600.
				if err := os.Chmod(keyPath, spokeAppKeyFileMode); err != nil {
					logger.Warn("could not tighten github app key permissions", "path", keyPath, "error", err)
				}
				afterFP, _ := config.AppKeyFingerprintFromFile(keyPath)
				keyChanged = afterFP != "" && afterFP != beforeFP
				logger.Info("github app private key written via heartbeat",
					"path", keyPath,
					"from_fingerprint", beforeFP,
					"to_fingerprint", afterFP,
					"key_changed", keyChanged,
				)
				if keyChanged {
					// Invalidate before the new client is built, so nothing can
					// read the dead token out of the shared on-disk cache in
					// between. Agents read that file directly.
					appAuth.DropCachedToken()
				}
			}

			// Adopt a hub-delivered app_id only when it names a REAL App. Zero
			// means "not speaking to this field"; the placeholder sentinel is
			// what a pre-provisioned hive already carries, so re-adopting it
			// would overwrite a good app_id with a non-App on any heartbeat
			// that echoed the original seed back.
			if ghCfg.AppID != 0 && ghCfg.AppID != config.PlaceholderAppID {
				cfg.GitHub.AppID = ghCfg.AppID
			}
			// A zero installation_id means "the hub is not speaking to this
			// field", not "clear it". The cluster-wide key reconcile repairs the
			// KEY on hives whose installation_id is already correct (and which
			// the hub does not track); assigning zero here would blank a working
			// value and turn a key-only fault into a total auth outage.
			if ghCfg.InstallationID != 0 {
				cfg.GitHub.InstallationID = ghCfg.InstallationID
			}
			if ghCfg.PrivateKey != "" {
				cfg.GitHub.KeyFile = keyPath
			}
			// Same "empty means unchanged" contract as installation_id: adopting
			// an empty slug would blank a working install link.
			if ghCfg.AppSlug != "" && cfg.GitHub.AppSlug != ghCfg.AppSlug {
				logger.Info("adopting github app slug from hub",
					"was", cfg.GitHub.AppSlug, "now", ghCfg.AppSlug)
				cfg.GitHub.AppSlug = ghCfg.AppSlug
			}

			if cfg.GitHub.HasUsableApp() && cfg.GitHub.KeyFile != "" {
				newAppAuth, err := github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, cfg.GitHub.KeyFile, logger, cfg.GitHub.ResolvedAPIURL())
				if err != nil {
					logger.Error("github app auth init via heartbeat failed", "error", err)
					return
				}
				// Hub-delivered creds can carry a wrong installation_id just as
				// easily as a hand-edited config; correct (and persist) it
				// before building a client that would 403 on every write.
				healGitHubAppInstallation(ctx, newAppAuth, cfg, logger)
				newClient := github.NewClientFromApp(newAppAuth, cfg.Project.Org, cfg.Project.Repos, logger)
				if len(cfg.Governor.Labels.Exempt) > 0 {
					newClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
				}
				ghClient = newClient
				appAuth = newAppAuth
				agentMgr.SetAppAuth(newAppAuth)
				dashSrv.UpdateGitHubClient(newClient, newAppAuth)
				dashSrv.SetGitHubAppRequired(false)
				dashSrv.ClearPendingGitHubAppInstall()
				logger.Info("github app configured via heartbeat delivery",
					"app_id", cfg.GitHub.AppID,
					"installation_id", cfg.GitHub.InstallationID,
				)
			}
		}), hub.HubBannerCallback(func(banner *hub.HubBanner) {
			if banner == nil {
				dashSrv.ClearHubBanner()
				return
			}
			dashSrv.SetHubBanner(banner.ID, banner.Message, banner.Color)
		}), hub.VisibilityCallback(func(isPublic bool) {
			if cfg.Hub.IsPublic != isPublic {
				logger.Info("hub overrode visibility via heartbeat",
					"was", cfg.Hub.IsPublic, "now", isPublic)
				cfg.Hub.IsPublic = isPublic
			}
		}), hub.SwitchBranchCallback(func(tag string) {
			// Branch switch delivered via heartbeat (the hub couldn't reach
			// this cluster over kubectl). Patch our OWN deployment image via
			// the in-cluster K8s API — the pod has no kubectl binary, but its
			// SA holds the hive-self-upgrade role (patch on deployment/hive).
			// K8s then rolls the pod onto the new tag.
			image := "ghcr.io/kubestellar/hive:" + tag
			if err := hub.SwitchImageSelf(logger, image); err != nil {
				logger.Warn("branch switch via heartbeat failed", "tag", tag, "image", image, "error", err)
				return
			}
		}), hub.AuthorizedUsersCallback(func(users []string) {
			// The hub delivered its authoritative access list. Reconcile our
			// login allowlist so Manage Access grants take effect on this
			// heartbeat-only spoke without any kubectl push. The dashboard reads
			// cfg.Dashboard.AuthorizedUsers live on each login, so updating it in
			// place is enough. Only log when it actually changes to avoid noise.
			if !sameStringSlice(cfg.Dashboard.AuthorizedUsers, users) {
				logger.Info("authorized users updated from hub heartbeat",
					"was", len(cfg.Dashboard.AuthorizedUsers), "now", len(users))
				cfg.Dashboard.AuthorizedUsers = users
			}
		}), hub.ProjectConfigCallback(func(pc *hub.HeartbeatProjectConfig) {
			// The hub assigned this (previously placeholder) hive a real project.
			// Reconcile our running project config so agents work the claimed
			// org/repos at the claimed maturity level. This is the ONLY delivery
			// channel on heartbeat-only clusters (vllm-d) — no kubectl push is
			// possible. The hub keeps sending this every beat until we report the
			// matching project back, so an idempotent no-op when already matched
			// is expected and cheap.
			if pc == nil || pc.Org == "" {
				return
			}
			curACMM := 0
			if cfg.ACMMLevel != nil {
				curACMM = *cfg.ACMMLevel
			}
			// Adopt the vanity dashboard URL delivered on claim, if any. We
			// report cfg.Hub.DashboardURL in our heartbeats, so once set the hub
			// registry's dashboardUrl becomes the vanity URL (not the placeholder
			// host). Track it in the already-reconciled check so a URL-only change
			// still gets applied and persisted.
			vanityMatched := pc.DashboardURL == "" || cfg.Hub.DashboardURL == pc.DashboardURL
			authorMatched := pc.AIAuthor == "" || cfg.Project.AIAuthor == pc.AIAuthor
			apiURLMatched := pc.GitHubAPIURL == "" || cfg.GitHub.APIURL == pc.GitHubAPIURL
			if cfg.Project.Org == pc.Org &&
				sameStringSlice(cfg.Project.Repos, pc.Repos) &&
				cfg.Project.PrimaryRepo == pc.PrimaryRepo &&
				curACMM == pc.ACMMLevel &&
				authorMatched &&
				apiURLMatched &&
				vanityMatched {
				return // already reconciled
			}
			if pc.DashboardURL != "" && cfg.Hub.DashboardURL != pc.DashboardURL {
				logger.Info("adopting vanity dashboard URL from hub heartbeat",
					"was", cfg.Hub.DashboardURL, "now", pc.DashboardURL)
				cfg.Hub.DashboardURL = pc.DashboardURL
			}
			logger.Info("project config updated from hub heartbeat (placeholder claimed)",
				"was_org", cfg.Project.Org, "now_org", pc.Org,
				"repos", pc.Repos, "primary_repo", pc.PrimaryRepo,
				"acmm_level", pc.ACMMLevel)
			cfg.Project.Org = pc.Org
			cfg.Project.Repos = pc.Repos
			cfg.Project.PrimaryRepo = pc.PrimaryRepo
			// Only adopt a non-empty author. The hub echoes this struct back on
			// every beat, so assigning unconditionally would reset a locally
			// configured ai_author to "" each time — which is precisely what
			// kept the fleet-stats collector disabled on every hive.
			if pc.AIAuthor != "" {
				cfg.Project.AIAuthor = pc.AIAuthor
			}
			// Adopt a GitHub Enterprise API URL when the hub sends one. Empty
			// means "leave mine alone" — the spoke's own default is already
			// api.github.com, so this never clobbers a working config.
			if pc.GitHubAPIURL != "" && cfg.GitHub.APIURL != pc.GitHubAPIURL {
				logger.Info("adopting GitHub API URL from hub heartbeat",
					"was", cfg.GitHub.APIURL, "now", pc.GitHubAPIURL)
				cfg.GitHub.APIURL = pc.GitHubAPIURL
			}
			level := pc.ACMMLevel
			cfg.ACMMLevel = &level

			// Re-sync the GitHub clients that cache the repo list (mirrors the
			// config-watcher reload path).
			ghClient.SetRepos(cfg.Project.Repos)
			if uc := userGHClient.Load(); uc != nil {
				uc.SetRepos(cfg.Project.Repos)
			}

			// Persist to the PVC overlay so the claim survives a pod restart
			// (config save writes the overlay hive.yaml, same as level switches).
			if err := cfg.Save(); err != nil {
				logger.Error("failed to save claimed project config", "error", err)
			}
		}), hub.GatewayConfigCallback(func(gw *hub.HeartbeatGatewayConfig) {
			// The hub funded an OpenRouter gateway on this hive's behalf (scan-to-
			// fund from My Hives) and delivered it over the heartbeat channel — the
			// only path that reaches a firewalled/heartbeat-only spoke (vllm-d). We
			// store the key in our OWN per-gateway secret-file store and create the
			// "openrouter" gateway. The hub drains the delivery after sending, so
			// this fires once per fund; the key value is never logged.
			if gw == nil || gw.Key == "" {
				return
			}
			if err := dashSrv.ApplyDeliveredGateway(gw.Name, gw.Kind, gw.Endpoint, gw.DefaultModel, gw.Key); err != nil {
				logger.Error("failed to apply hub-delivered gateway", "gateway", gw.Name, "error", err)
			}
		}))

		go hub.StartTaskStatusPush(ctx, hubURL, func() *hub.TaskStatusPayload {
			reg, active := dashSrv.ContributorSummary()
			lb := dashSrv.LeaderboardForHub()
			out := make([]hub.LeaderboardEntry, len(lb))
			for i, e := range lb {
				out[i] = hub.LeaderboardEntry{
					GitHubUsername: e.GitHubUsername,
					AvatarURL:      e.AvatarURL,
					TrustTier:      e.TrustTier,
					TasksCompleted: e.TasksCompleted,
					TasksFailed:    e.TasksFailed,
					Active:         e.Active,
					CurrentTask:    e.CurrentTask,
				}
			}
			return &hub.TaskStatusPayload{
				HiveID:       cfg.HiveID,
				Leaderboard:  out,
				Contributors: hub.ContributorSummary{Registered: reg, Active: active},
			}
		}, logger)
	}

	// Trajectory-review lane (opt-in): a second-model check that reads each
	// running agent's recent transcript and pauses on goal drift. Built once;
	// runs off the governor tick, gated by its own cadence. If the reviewer
	// cannot be constructed (no LiteLLM endpoint/model), the lane is disabled
	// with a single warning rather than erroring every tick.
	var trajLane *trajectory.Lane
	if cfg.Governor.Trajectory.IsEnabled() {
		reviewEndpoint, reviewKey, reviewModel := cfg.Governor.ResolveReviewer()
		reviewer, terr := trajectory.NewReviewer(trajectory.Config{
			Endpoint:        reviewEndpoint,
			APIKey:          reviewKey,
			Model:           reviewModel,
			TranscriptLines: cfg.Governor.Trajectory.TranscriptLines,
		})
		if terr != nil {
			// Enabled but not runnable — surface it as a dashboard alert, not
			// just a log line, so a safety control is never silently inert.
			// (Reconciled below so it also clears when the lane is disabled.)
			logger.Warn("trajectory-review lane enabled but not running", "reason", terr.Error())
		} else {
			trajLane = trajectory.NewLane(reviewer, agentMgr,
				dashboard.NewTrajectorySink(dashSrv, notifier),
				trajectory.LaneConfig{
					IntervalS:    cfg.Governor.Trajectory.IntervalS,
					OnDivergence: cfg.Governor.Trajectory.OnDivergence,
					ExemptAgents: cfg.Governor.Trajectory.ExemptAgents,
				}, logger)
			logger.Info("trajectory-review lane enabled",
				"model", reviewModel,
				"interval_s", cfg.Governor.Trajectory.IntervalS,
				"on_divergence", cfg.Governor.Trajectory.OnDivergence)
		}
	}
	// Reconcile the not-configured alert from actual state (raises only when
	// enabled AND no reviewer resolves; clears when off or configured).
	dashSrv.ReconcileTrajectoryAlert(&cfg.Governor)

	logger.Info("entering governor loop", "interval_seconds", cfg.Governor.EvalIntervalS)
	lastEvalInterval := cfg.Governor.EvalIntervalS
	ticker := time.NewTicker(time.Duration(cfg.Governor.EvalIntervalS) * time.Second)
	defer ticker.Stop()

	var agentTicker *time.Ticker
	if cfg.Dashboard.AgentPollIntervalS > 0 {
		agentTicker = time.NewTicker(time.Duration(cfg.Dashboard.AgentPollIntervalS) * time.Second)
		defer agentTicker.Stop()
		logger.Info("fast agent status enabled", "interval_seconds", cfg.Dashboard.AgentPollIntervalS)
	}

	dashSrv.MarkReady()

	const cliStartupDelay = 10 * time.Second
	logger.Info("waiting for CLI startup before first eval", "delay", cliStartupDelay)
	select {
	case <-time.After(cliStartupDelay):
	case <-ctx.Done():
		return
	}

	gov.ClearLastKicks()
	logger.Info("cleared last kicks for startup — all eligible agents will be kicked on first eval")
	runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, &userGHClient, nil, logger)
	persistState(agentMgr, gov, cfg, tokenCollector, statePath, logger, dashSrv)

	agentTickCh := func() <-chan time.Time {
		if agentTicker != nil {
			return agentTicker.C
		}
		return nil
	}()

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down, persisting state")
			persistState(agentMgr, gov, cfg, tokenCollector, statePath, logger, dashSrv)
			return
		case <-ticker.C:
			restarted := agentMgr.CheckAndRestartCrashedAgents(ctx)
			for _, name := range restarted {
				dashSrv.AuditLog("system", "restart", "trigger=crash-recovery", name)
			}
			// If brainstorm crashed during inception, re-kick via SendKick.
			// SendKick waits for the CLI to be ready and sends the message
			// If brainstorm crashed during inception, re-kick with bootstrap.
			// The table parser in the watcher will catch questions from the
			// agent's output even if bd create doesn't execute.
			for _, name := range restarted {
				if name == "brainstorm" && inceptionEngine != nil {
					if state := inceptionEngine.GetState(); state != nil && state.Phase == knowledge.PhaseCapture {
						msg := sched.BuildAgentMessage("brainstorm", nil, sched.GetLastActionable())
						if err := agentMgr.RestartWithBootstrap(ctx, "brainstorm", msg); err != nil {
							logger.Warn("inception re-kick after crash failed", "error", err)
						} else {
							logger.Info("brainstorm re-kicked after crash", "phase", state.Phase)
							dashSrv.AuditLog("system", "kick", "trigger=inception-crash-recovery", "brainstorm")
						}
						gov.RecordKick("brainstorm")
					}
				}
			}
			runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, &userGHClient, restarted, logger)
			// Trajectory review runs after the eval cycle (so kicks/intents are
			// current) on its own cadence, gated by Due().
			if trajLane != nil && trajLane.Due(time.Now()) {
				trajLane.Run(ctx)
			}
			persistState(agentMgr, gov, cfg, tokenCollector, statePath, logger, dashSrv)
			if cfg.Governor.EvalIntervalS != lastEvalInterval && cfg.Governor.EvalIntervalS > 0 {
				logger.Info("eval interval changed, resetting ticker",
					"from", lastEvalInterval, "to", cfg.Governor.EvalIntervalS)
				ticker.Reset(time.Duration(cfg.Governor.EvalIntervalS) * time.Second)
				lastEvalInterval = cfg.Governor.EvalIntervalS
			}
		case <-agentTickCh:
			govState := gov.GetState()
			agentStatuses := agentMgr.AllStatuses()
			payload := dashboard.BuildAgentOnlyStatus(govState, agentStatuses, cfg)
			dashSrv.BroadcastAgentStatus(payload)
		}
	}
}

// Dashboard system-alert IDs for the budget thresholds.
const (
	budgetWarnAlertID      = "budget-warn"
	budgetExhaustedAlertID = "budget-exhausted"
)

// applyBudgetAlerts turns budget threshold crossings into dashboard system
// alerts and notifications. Crossings fire once per window (governor tracks
// the one-shot flags); alerts are cleared when the threshold no longer
// applies (window rolled, limit raised, or budgeting disabled).
func applyBudgetAlerts(gov *governor.Governor, trans governor.BudgetTransitions, dashSrv *dashboard.Server, notifier *notify.Notifier) {
	if !trans.WarnActive {
		dashSrv.ClearSystemAlert(budgetWarnAlertID)
	}
	if !trans.ExhaustedActive {
		dashSrv.ClearSystemAlert(budgetExhaustedAlertID)
	}

	budget := gov.GetBudget()
	if trans.WarnCrossed {
		msg := fmt.Sprintf("token budget at %d%%+ of weekly limit: %d of %d tokens used",
			governor.BudgetWarnPct, budget.CurrentSpend, budget.WeeklyLimit)
		dashSrv.AddSystemAlert(budgetWarnAlertID, "warning", msg)
		notifier.Send("Budget warning", msg, notify.PriorityDefault)
	}
	if trans.ExhaustedCrossed {
		windowEnd := budget.ResetAt.Add(governor.BudgetWindowDuration)
		msg := fmt.Sprintf("token budget exhausted: %d of %d tokens used — agent kicks suspended until %s (exempt agents keep running)",
			budget.CurrentSpend, budget.WeeklyLimit, windowEnd.Format(time.RFC1123))
		dashSrv.AddSystemAlert(budgetExhaustedAlertID, "error", msg)
		notifier.Send("Budget exhausted", msg, notify.PriorityHigh)
	}
}

// diagnoseGitHubAppWrite returns "" when the configured GitHub App
// installation belongs to expectedOwner and grants issues:write. Otherwise it
// returns a banner-ready diagnosis distinguishing the two write-failure causes
// that produce identical 403s: an installation_id pointing at a different
// org's installation, and a permission update the org owner hasn't approved
// yet. A nil appAuth (token-authenticated hive) yields "" — nothing to check.
// healGitHubAppInstallation self-heals a hive whose github.installation_id
// points at the WRONG account — the failure mode diagnoseGitHubAppWrite
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

// diagnoseGitHubApp classifies this hive's GitHub App credential state and
// returns both the machine-readable state and banner-ready copy.
//
// It supersedes a substring match on the formatted error ("403"/"401"), which
// could not tell a user-side failure from an operator-side one and so showed
// every hive the same "GitHub App Not Installed" banner. The most damaging
// case that fixes: a spoke holding the WRONG private key (the hub's key push
// has not landed, or delivered a public github.com key to a GitHub Enterprise
// hive) gets `401 A JSON web token could not be decoded`. The user cannot see,
// supply, or correct that key — telling them to install the App or check
// github.installation_id sends them to redo work they already did correctly.
//
// The candidate key paths are passed so a MISSING key is detected without any
// API round-trip at all.
//
// Returns ("", AppStateOK) when App auth is healthy, and ("", state) for a nil
// appAuth (a token-authenticated hive has nothing to check).
func diagnoseGitHubApp(ctx context.Context, appAuth *github.AppAuth, expectedOwner string) (string, github.AppAuthState) {
	if appAuth == nil {
		return "", github.AppStateOK
	}
	d := appAuth.DiagnoseAppAuth(ctx, expectedOwner, spokeAppKeyPath, spokeProvisionedAppKeyPath)
	return d.Message(), d.State
}

// diagnoseGitHubAppWrite is the string-only wrapper retained for callers that
// only need banner copy.
func diagnoseGitHubAppWrite(ctx context.Context, appAuth *github.AppAuth, expectedOwner string) string {
	msg, _ := diagnoseGitHubApp(ctx, appAuth, expectedOwner)
	return msg
}

func runEvalCycle(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	gov *governor.Governor,
	sched *scheduler.Scheduler,
	agentMgr *agent.Manager,
	dashSrv *dashboard.Server,
	notifier *notify.Notifier,
	beadStores map[string]*beads.Store,
	tokenCollector *tokens.Collector,
	metricsCollector *dashboard.MetricsCollector,
	nousState *dashboard.NousState,
	lastActionable *atomic.Pointer[github.ActionableResult],
	advisoryStore *advisory.Store,
	advisoryIssues map[string]int,
	userGHClient *atomic.Pointer[github.Client],
	restartedAgents []string,
	logger *slog.Logger,
) {
	// A hive running without GitHub credentials (placeholder app_id, or a real
	// App whose key could not be read) has nothing to enumerate. Return before
	// the first API call rather than logging a misleading enumeration failure
	// once per eval interval — the dashboard banner already states the cause.
	if ghClient == nil {
		logger.Debug("skipping eval cycle: hive is running without GitHub credentials")
		return
	}

	if dashSrv.IsGitHubAppRequired() {
		primaryRepo := cfg.Project.PrimaryRepo
		if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
			primaryRepo = cfg.Project.Repos[0]
		}
		if primaryRepo != "" && ghClient != nil {
			num, retryErr := ghClient.EnsureAdvisoryIssue(ctx, primaryRepo)
			if retryErr == nil {
				advisoryIssues[primaryRepo] = num
				os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num))
				logger.Info("advisory issue created on retry", "repo", primaryRepo, "number", num)
			}
		}
	}

	actionable, err := ghClient.EnumerateActionable(ctx)
	if err != nil {
		logger.Error("failed to enumerate actionable items", "error", err)
		return
	}

	ghClient.EnrichCIStatus(ctx, actionable.PRs.Items)

	// Duplicate-PR guard: drop issues an open hive-authored PR already claims,
	// before the governor counts the queue or the scheduler builds kicks. A
	// restart storm otherwise re-offers the same issue on every fresh agent
	// start, and the agent — having no memory of the PR it just filed — files
	// another. Backed by a PVC ledger so it survives those restarts, and fails
	// closed (keeps the last known claims) when the GitHub API is unavailable.
	applyDuplicatePRGuard(ctx, cfg, ghClient, actionable, logger)

	lastActionable.Store(actionable)
	if data, err := json.Marshal(actionable); err == nil {
		atomicWrite("/data/last-actionable.json", data)
	}

	writeMergeEligible(actionable, actionable.Hold, cfg.Project.Org, logger)

	shaResult, shaErr := ghClient.EnforceSHAHold(ctx, github.SHAHoldConfig{
		PrimaryRepo:     cfg.Project.PrimaryRepo,
		AIAuthor:        cfg.Project.AIAuthor,
		InternalAuthors: []string{"kubestellar-hive[bot]", "github-actions[bot]", "dependabot[bot]", "copilot-swe-agent[bot]"},
	})
	if shaErr != nil {
		logger.Warn("SHA hold enforcement failed", "error", shaErr)
	} else {
		logger.Info("SHA hold enforcement complete",
			"held", shaResult.Held,
			"unheld", shaResult.Unheld,
			"skipped", shaResult.Skipped,
		)
	}

	// Refresh budget spend from lifetime token totals before Evaluate so
	// the kick gate sees current-window numbers.
	if tokenCollector != nil {
		if summary := tokenCollector.Summary(); summary != nil {
			trans := gov.UpdateBudgetFromTotals(summary.TotalTokens, summary.ByAgent, summary.ByModel)
			applyBudgetAlerts(gov, trans, dashSrv, notifier)
		}
	}

	agentsDue := gov.Evaluate(
		actionable.Issues.Count,
		actionable.PRs.Count,
		actionable.Hold.Total,
		actionable.Issues.SLAViolations,
	)

	// Restarted agents need a kick even if the governor wouldn't schedule one this cycle.
	if len(restartedAgents) > 0 {
		dueSet := make(map[string]bool, len(agentsDue))
		for _, a := range agentsDue {
			dueSet[a] = true
		}
		for _, a := range restartedAgents {
			if !dueSet[a] {
				agentsDue = append(agentsDue, a)
				logger.Info("adding restarted agent to kick list", "agent", a)
			}
		}
	}

	govState := gov.GetState()
	logger.Info("governor eval complete",
		"mode", govState.Mode,
		"issues", govState.QueueIssues,
		"prs", govState.QueuePRs,
		"agents_due", agentsDue,
	)

	// cadence.Paused (cadence: "pause" in config) means "don't kick this agent
	// in this mode" — it does NOT force-pause the agent. Manual pause/resume
	// via the dashboard is always respected; the governor only controls kicks.

	// Filter out on-demand agents — they are only triggered explicitly
	onDemandSet := config.OnDemandAgentsFromPacks()
	var filteredDue []string
	for _, name := range agentsDue {
		if ac, ok := cfg.Agents[name]; ok && ac.OnDemand {
			continue
		}
		if onDemandSet[name] {
			continue
		}
		filteredDue = append(filteredDue, name)
	}
	agentsDue = filteredDue

	sched.SetLastActionable(actionable)
	if len(agentsDue) > 0 {
		messages := sched.BuildKickMessages(actionable, agentsDue)
		for _, msg := range messages {
			logger.Info("audit: governor kicking agent", "agent", msg.Agent, "trigger", "governor-eval")
			if err := agentMgr.SendKick(msg.Agent, msg.Message); err != nil {
				logger.Warn("failed to send kick", "agent", msg.Agent, "error", err)
				continue
			}
			gov.RecordKick(msg.Agent)
			dashSrv.AuditLog("governor", "kick", "trigger=governor-eval", msg.Agent)

			// Log token state at time of kick for cost attribution
			if tokenCollector != nil {
				if summary := tokenCollector.Summary(); summary != nil {
					agentTokens := summary.ByAgent[msg.Agent]
					logger.Info("kick token snapshot",
						"agent", msg.Agent,
						"agent_tokens", agentTokens,
						"total_tokens", summary.TotalTokens,
						"total_sessions", summary.SessionCount,
					)
				}
			}
		}
	}

	if actionable.Issues.SLAViolations > 0 {
		const doubleSLAMinutes = 60
		const maxSLANotificationsPerCycle = 3
		sent := 0
		for _, issue := range actionable.Issues.Items {
			if issue.AgeMinutes > doubleSLAMinutes {
				if sent >= maxSLANotificationsPerCycle {
					logger.Info("SLA notification cap reached, skipping remaining", "remaining", actionable.Issues.SLAViolations-sent)
					break
				}
				notifier.Send(
					"SLA 2x breach",
					fmt.Sprintf("%s#%d age %dm: %s\n%s", issue.Repo, issue.Number, issue.AgeMinutes, issue.Title, issue.URL),
					notify.PriorityHigh,
				)
				sent++
			}
		}
	}

	// Scan agent panes for login-required patterns and pause + notify if detected
	scanForLoginRequired(cfg, agentMgr, notifier, dashSrv, logger)

	agentStatuses := agentMgr.AllStatuses()

	statusPayload := dashboard.BuildFrontendStatus(
		govState,
		actionable,
		agentStatuses,
		cfg,
		tokenCollector,
		gov,
		beadStores,
		ghClient,
		ctx,
		metricsCollector,
	)
	// Ingest any JSONL findings agents wrote and persist them as beads.
	if advisoryStore != nil {
		findings, err := advisoryStore.ReadNewFindings()
		if err != nil {
			logger.Warn("failed to read advisory findings", "error", err)
		} else if len(findings) > 0 {
			// Log each new finding for the audit trail
			for _, f := range findings {
				logger.Info("advisory finding ingested",
					"agent", f.Agent,
					"severity", f.Severity,
					"type", f.Type,
					"title", f.Title,
					"file", f.File,
					"line", f.Line,
				)
			}
			if persisted := advisory.PersistAsBeads(findings, beadStores); persisted > 0 {
				logger.Info("advisory findings persisted as beads", "count", persisted)
			}
		}
	}

	// Reload bead stores from disk before building the digest. Agents write
	// beads via the bd CLI which persists directly to disk, so the in-memory
	// stores can become stale between eval cycles.
	for name, store := range beadStores {
		if err := store.Reload(); err != nil {
			logger.Warn("failed to reload beads from disk", "agent", name, "error", err)
		}
	}

	// Advisory digest: build from beads (the source of truth) before status broadcast.
	if len(beadStores) > 0 {
		digest := advisory.BuildDigestFromBeads(beadStores, string(govState.Mode))
		if advisoryStore != nil {
			advisoryStore.SetLatestDigest(digest)
		}
		dashSrv.SetAdvisoryDigest(digest)
		statusPayload.AdvisoryDigest = digest

		if digest.TotalCount > 0 {
			// Log severity breakdown and contributing agents
			bySeverity := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0, "info": 0}
			agentNames := make([]string, 0, len(digest.ByAgent))
			for agentName, findings := range digest.ByAgent {
				agentNames = append(agentNames, fmt.Sprintf("%s(%d)", agentName, len(findings)))
				for _, f := range findings {
					bySeverity[strings.ToLower(f.Severity)]++
				}
			}
			logger.Info("advisory digest built",
				"total_findings", digest.TotalCount,
				"critical", bySeverity["critical"],
				"high", bySeverity["high"],
				"medium", bySeverity["medium"],
				"low", bySeverity["low"],
				"agents", strings.Join(agentNames, ", "),
				"resolved_count", len(digest.RecentlyResolved),
			)

			primaryRepo := cfg.Project.PrimaryRepo
			if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
				primaryRepo = cfg.Project.Repos[0]
			}
			// Repo entries may be org-qualified ("org/repo"); the digest
			// linkifier needs the bare repo name alongside the org.
			org, repoName := cfg.Project.Org, primaryRepo
			if parts := strings.SplitN(primaryRepo, "/", 2); len(parts) == 2 {
				org, repoName = parts[0], parts[1]
			}
			md := advisory.FormatDigestMarkdown(digest, org, repoName)
			if md != "" {
				if issueNum, ok := advisoryIssues[primaryRepo]; ok && issueNum > 0 {
					// Prefer the App client as the PRIMARY poster. The App
					// authored the advisory-digest comment and always holds
					// issues:write, so it is the correct identity to edit it.
					// The App banner must be driven ONLY by the App's own
					// error — never by a user-token failure. Otherwise a
					// user-token problem (kellyaa: expired token → 401;
					// kalantar: valid token but not repo-admin → 403 editing
					// the bot's own comment) would false-flag the App as "Not
					// Installed" even though the App itself works fine.
					if err := ghClient.PostAdvisoryDigest(ctx, primaryRepo, issueNum, md); err != nil {
						// App failed. Try the user client as a FALLBACK ONLY so
						// the digest still gets posted. A user-fallback success
						// does NOT clear the App banner — the App error below is
						// what decides the banner state.
						if uc := userGHClient.Load(); uc != nil {
							if uerr := uc.PostAdvisoryDigest(ctx, primaryRepo, issueNum, md); uerr == nil {
								logger.Info("posted advisory digest", "repo", primaryRepo, "issue", issueNum, "findings", digest.TotalCount, "via", "user-fallback")
							}
						}
						logger.Warn("failed to post advisory digest via app", "repo", primaryRepo, "issue", issueNum, "error", err)
						if strings.Contains(err.Error(), "403") && strings.Contains(err.Error(), "Resource not accessible by integration") {
							// App is installed (we found the issue) but can't write.
							// Resolve the actual cause — pending permission approval
							// vs. an installation_id pointing at the wrong org — so
							// the banner tells the user what to actually fix.
							msg, state := diagnoseGitHubApp(ctx, ghClient.AppAuth(), cfg.Project.Org)
							if msg == "" {
								msg = "The GitHub App is installed but lacks Issues: Read & Write permission. The org owner must approve updated permissions at the app installation settings page."
								state = github.AppStateInsufficientPerms
							}
							dashSrv.SetGitHubAppRequired(true)
							dashSrv.SetGitHubAppPermIssue(msg)
							dashSrv.SetGitHubAppState(state.String())
							logger.Warn("GitHub App write failed — cannot write issue comments",
								"repo", primaryRepo, "state", state.String(),
								"operator_actionable", state.OperatorActionable(), "detail", msg)
						} else if strings.Contains(err.Error(), "rate limit") {
							logger.Warn("GitHub API rate limit hit, skipping advisory digest post", "repo", primaryRepo)
						} else if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "401") {
							// Same reasoning as the startup path: classify
							// before raising a banner, so an operator-side key
							// failure is never rendered as "not installed".
							msg, state := diagnoseGitHubApp(ctx, ghClient.AppAuth(), cfg.Project.Org)
							dashSrv.SetGitHubAppRequired(true)
							if msg != "" {
								dashSrv.SetGitHubAppPermIssue(msg)
							}
							dashSrv.SetGitHubAppState(state.String())
							logger.Warn("GitHub App authentication failed posting advisory digest",
								"repo", primaryRepo, "state", state.String(),
								"operator_actionable", state.OperatorActionable())
						}
					} else {
						logger.Info("posted advisory digest", "repo", primaryRepo, "issue", issueNum, "findings", digest.TotalCount, "via", "app")
						// A successful write proves the app is installed AND has
						// write access — clear BOTH the perm issue and the
						// app-required banner flag. Previously only the perm
						// issue was cleared, so githubAppRequired (set true at
						// startup or on an early transient failure) stuck on
						// forever and the "GitHub App Not Installed" banner
						// never went away despite tokens working.
						dashSrv.SetGitHubAppPermIssue("")
						dashSrv.SetGitHubAppRequired(false)
						dashSrv.ClearPendingGitHubAppInstall()
					}
				}
			}
		}
	} else if d := dashSrv.GetAdvisoryDigest(); d != nil {
		statusPayload.AdvisoryDigest = d
	}

	dashSrv.UpdateStatus(statusPayload)

	if agentStats := dashboard.CollectAgentStats(statusPayload); len(agentStats) > 0 {
		gov.AttachAgentStats(agentStats)
	}

	if repoSnaps := dashboard.CollectRepoSnapshots(statusPayload); len(repoSnaps) > 0 {
		gov.AttachRepoSnapshots(repoSnaps)
	}

	if nousState != nil {
		var tokenSummary *tokens.AggregateSummary
		if tokenCollector != nil {
			tokenSummary = tokenCollector.Summary()
		}
		if err := nousState.RecordSnapshot(govState, actionable, agentsDue, agentStatuses, tokenSummary); err != nil {
			logger.Warn("failed to record nous snapshot", "error", err)
		}
	}
}

// loginCommandForBackend returns the login instruction for a given CLI backend.
func loginCommandForBackend(backend string) string {
	switch backend {
	case "claude":
		return "Run: claude login"
	case "copilot":
		return "Run: copilot auth login"
	case "gemini":
		return "Run: gemini auth login"
	case "goose":
		return "Run: goose auth login"
	default:
		return "Run the login command for " + backend
	}
}

// scanForLoginRequired checks each running agent's tmux pane output for login-required
// patterns. When a match is found, the agent is paused and a notification is sent.
func scanForLoginRequired(
	cfg *config.Config,
	agentMgr *agent.Manager,
	notifier *notify.Notifier,
	dashSrv *dashboard.Server,
	logger *slog.Logger,
) {
	patterns := cfg.Governor.Sensing.LoginPatterns
	if len(patterns) == 0 {
		return
	}

	// Compile regex patterns, skipping empty and invalid ones
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			logger.Warn("invalid login pattern regex", "pattern", p, "error", err)
			continue
		}
		compiled = append(compiled, re)
	}
	if len(compiled) == 0 {
		return
	}

	const paneLines = 50 // number of recent lines to scan
	statuses := agentMgr.AllStatuses()
	for name, proc := range statuses {
		if proc.State != "running" {
			continue
		}

		output, err := agentMgr.GetOutput(name, paneLines)
		if err != nil || len(output) == 0 {
			continue
		}

		joined := strings.Join(output, "\n")
		for _, re := range compiled {
			if re.MatchString(joined) {
				logger.Warn("login required detected",
					"agent", name,
					"pattern", re.String(),
				)

				// Pause the agent instead of restarting
				if pauseErr := agentMgr.Pause(name, "login-detector", "login required detected"); pauseErr != nil {
					logger.Warn("failed to pause agent after login detection",
						"agent", name, "error", pauseErr)
				} else {
					dashSrv.AuditLog("system", "pause", "trigger=login-detector", name)
				}

				// Determine the login instruction based on the agent's backend
				backend := cfg.Agents[name].Backend
				loginCmd := loginCommandForBackend(backend)

				notifier.Send(
					fmt.Sprintf("\U0001F511 Login required: %s", name),
					fmt.Sprintf(
						"Agent '%s' needs authentication. Open the agent's terminal "+
							"(tmux attach -t hive-%s) and run the login command for the CLI (%s). %s",
						name, name, backend, loginCmd,
					),
					notify.PriorityHigh,
				)

				break // one match per agent is enough
			}
		}
	}
}

func convertKnowledgeLayers(cfgLayers []config.KnowledgeLayer) []knowledge.LayerConfig {
	layers := make([]knowledge.LayerConfig, len(cfgLayers))
	for i, l := range cfgLayers {
		layers[i] = knowledge.LayerConfig{
			Type:   knowledge.LayerType(l.Type),
			Path:   l.Path,
			URL:    l.URL,
			Shared: l.Shared,
		}
	}
	return layers
}

// hiveIDFilePath is the persistent file where the Hive ID is stored across restarts.
const hiveIDFilePath = "/data/hive-id"

// loadOrGenerateHiveID reads the Hive ID from disk, or generates and persists a new one.
func loadOrGenerateHiveID(logger *slog.Logger) string {
	if envID := os.Getenv("HIVE_ID"); envID != "" {
		if err := os.WriteFile(hiveIDFilePath, []byte(envID+"\n"), 0o644); err == nil {
			logger.Info("hive ID set from HIVE_ID env var", "id", envID)
		}
		return envID
	}

	if data, err := os.ReadFile(hiveIDFilePath); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			logger.Info("hive ID loaded from disk", "id", id)
			return id
		}
	}

	id := "hive-" + randomName()

	if err := os.WriteFile(hiveIDFilePath, []byte(id+"\n"), 0o644); err != nil {
		logger.Warn("failed to persist hive ID", "error", err)
	} else {
		logger.Info("generated new hive ID", "id", id)
	}

	return id
}

// randomName generates a Docker-style adjective-noun name.
func randomName() string {
	adjectives := []string{
		"bold", "calm", "cool", "dark", "deep", "fair", "fast", "keen",
		"kind", "loud", "mild", "neat", "pale", "pure", "rare", "rich",
		"safe", "slim", "soft", "tall", "thin", "true", "vast", "warm",
		"wise", "able", "busy", "easy", "epic", "free", "glad", "good",
		"idle", "just", "lazy", "lean", "live", "long", "lost", "main",
		"next", "open", "real", "sure", "wild", "worn", "zero", "blue",
	}
	nouns := []string{
		"ant", "ape", "bat", "bee", "cow", "doe", "eel", "elk",
		"fox", "gnu", "hen", "jay", "kit", "lark", "moth", "newt",
		"owl", "pug", "ram", "ray", "seal", "swan", "toad", "wren",
		"bear", "colt", "crow", "deer", "dove", "duck", "fawn", "frog",
		"goat", "gull", "hare", "hawk", "ibis", "lynx", "mink", "mole",
		"orca", "pike", "puma", "slug", "stag", "wolf", "yak", "wasp",
	}

	buf := make([]byte, 2)
	if _, err := rand.Read(buf); err != nil {
		return "bold-ant"
	}
	adj := adjectives[int(buf[0])%len(adjectives)]
	noun := nouns[int(buf[1])%len(nouns)]
	return adj + "-" + noun
}

func persistState(agentMgr *agent.Manager, gov *governor.Governor, cfg *config.Config, tc *tokens.Collector, path string, logger *slog.Logger, dashSrv *dashboard.Server) {
	statuses := agentMgr.AllStatuses()
	agents := make(map[string]snapshot.AgentState, len(statuses))
	for name, proc := range statuses {
		as := snapshot.AgentState{
			Paused:          proc.Paused,
			PinnedCLI:       proc.PinnedCLI,
			PinnedModel:     proc.PinnedModel,
			ModelOverride:   proc.ModelOverride,
			BackendOverride: proc.BackendOverride,
			RestartCount:    proc.RestartCount,
			LastKick:        proc.LastKick,
			PausedReason:    proc.PausedReason,
			PausedTrigger:   proc.PausedTrigger,
		}
		if !proc.PausedAt.IsZero() {
			t := proc.PausedAt
			as.PausedAt = &t
		}
		if len(proc.KickHistory) > 0 {
			as.KickHistory = make([]snapshot.AgentKickEntry, len(proc.KickHistory))
			for i, kr := range proc.KickHistory {
				as.KickHistory[i] = snapshot.AgentKickEntry{
					Timestamp: kr.Timestamp,
					Agent:     kr.Agent,
					Snippet:   kr.Snippet,
				}
			}
		}
		if agentCfg, ok := cfg.Agents[name]; ok {
			as.DisplayName = agentCfg.DisplayName
			as.Description = agentCfg.Description
			enabled := agentCfg.Enabled
			as.Enabled = &enabled
			clearOnKick := agentCfg.ClearOnKick
			as.ClearOnKick = &clearOnKick
			staleTimeout := agentCfg.StaleTimeout
			as.StaleTimeout = &staleTimeout
			as.RestartStrategy = agentCfg.RestartStrategy
			as.LaunchCmd = agentCfg.LaunchCmd
		}
		agents[name] = as
	}

	cadenceOverrides := make(map[string]map[string]string)
	for modeName, mode := range cfg.Governor.Modes {
		if len(mode.Cadences) > 0 {
			cadenceOverrides[modeName] = make(map[string]string, len(mode.Cadences))
			for agentName, cadence := range mode.Cadences {
				cadenceOverrides[modeName][agentName] = cadence
			}
		}
	}

	budget := gov.GetBudget()
	govState := gov.GetState()

	govKickHistory := gov.KickHistory()
	kickEntries := make([]snapshot.GovKickEntry, len(govKickHistory))
	for i, kr := range govKickHistory {
		kickEntries[i] = snapshot.GovKickEntry{Timestamp: kr.Timestamp, Agent: kr.Agent}
	}

	var issueCosts map[string]int64
	if tc != nil {
		issueCosts = tc.IssueCosts()
	}

	state := &snapshot.PersistedState{
		Agents:               agents,
		GovernorMode:         string(govState.Mode),
		BudgetLimit:          budget.WeeklyLimit,
		BudgetIgnored:        budget.IgnoredAgents,
		BudgetIgnoreAll:      budget.IgnoreAll,
		CadenceOverrides:     cadenceOverrides,
		LastKicks:            govState.LastKick,
		BudgetSpend:          budget.CurrentSpend,
		BudgetResetAt:        budget.ResetAt,
		BudgetByAgent:        budget.ByAgent,
		BudgetByModel:        budget.ByModel,
		BudgetWindowBaseline: budget.WindowBaseline,
		KickHistory:          kickEntries,
		IssueCosts:           issueCosts,
		LastEval:             govState.LastEval,
		ACMMLevel:            cfg.ACMMLevel,
	}

	if err := snapshot.SaveState(path, state, logger); err != nil {
		logger.Error("failed to persist state", "error", err)
	}

	// Reconcile the persisted pause field from the authoritative live manager
	// state and save, atomically under the config save mutex. persistState runs
	// async (go PersistFunc()) on every pause/resume; doing the c.Agents update
	// and Save under saveMu (via ReconcilePausedAndSave) means it can neither
	// race the pause callback's map write nor clobber its file write with a
	// stale paused=false. livePaused is built from AllStatuses(), read above.
	livePaused := make(map[string]bool, len(agents))
	for name, as := range agents {
		livePaused[name] = as.Paused
	}
	if err := cfg.ReconcilePausedAndSave(livePaused); err != nil {
		logger.Error("failed to persist config to yaml", "error", err)
		if dashSrv != nil {
			dashSrv.AddSystemAlert("config-save-failed", "error",
				"Config save failed — runtime state (ACMM level, agent config) will be lost on restart: "+err.Error())
		}
	} else if dashSrv != nil {
		dashSrv.ClearSystemAlert("config-save-failed")
	}

	history := gov.EvalHistory()
	if len(history) > 0 {
		historyData, err := json.Marshal(history)
		if err == nil {
			atomicWrite("/data/sparkline-history.json", historyData)
		}
	}

	modeHistory := gov.ModeHistory()
	if len(modeHistory) > 0 {
		modeData, err := json.Marshal(modeHistory)
		if err == nil {
			atomicWrite("/data/mode-history.json", modeData)
		}
	}

	if dashSrv != nil {
		tokenHistory := dashSrv.TokenSparklineHistory()
		if len(tokenHistory) > 0 {
			tokenData, err := json.Marshal(tokenHistory)
			if err == nil {
				atomicWrite("/data/token-sparkline-history.json", tokenData)
			}
		}

		factHist := dashSrv.FactHistory()
		if len(factHist) > 0 {
			factData, err := json.Marshal(factHist)
			if err == nil {
				atomicWrite("/data/fact-history.json", factData)
			}
		}

		costHist := dashSrv.CostHistory()
		if len(costHist) > 0 {
			costData, err := json.Marshal(costHist)
			if err == nil {
				atomicWrite("/data/cost-history.json", costData)
			}
		}

		trendHist := dashSrv.TrendHistory()
		if len(trendHist) > 0 {
			trendData, err := json.Marshal(trendHist)
			if err == nil {
				atomicWrite("/data/trend-history.json", trendData)
			}
		}
	}
}

const mergeEligiblePath = "/var/run/hive-metrics/merge-eligible.json"
const ciFailingPath = "/var/run/hive-metrics/ci-failing.json"

// claimLedger holds the duplicate-PR guard's persisted issue→PR claim mapping
// across eval cycles. It is loaded lazily on first use (and retried on a load
// failure) rather than at startup, so a missing or corrupt /data ledger can
// never block the hive from booting.
var (
	claimLedgerOnce sync.Once
	claimLedger     *github.ClaimLedger
)

// hiveIdentity determines which PR authors count as "this hive", so only our
// own PRs suppress work. Two accounts can open PRs on our behalf:
//   - project.ai_author — the account agents push and open PRs as
//   - the GitHub App bot login ("<app-slug>[bot]") when the hive authenticates
//     as an installation, which is what actually authors PRs in that mode
func hiveIdentity(cfg *config.Config) github.HiveIdentity {
	id := github.HiveIdentity{AIAuthor: cfg.Project.AIAuthor}
	if slug := cfg.GitHub.ResolvedAppSlug(); slug != "" {
		id.AppLogin = slug + "[bot]"
	}
	return id
}

// applyDuplicatePRGuard filters issues already claimed by an open hive-authored
// PR out of the actionable set. Failures are logged, never fatal: the guard is
// a safety net, and a broken net must not take the hive down with it.
func applyDuplicatePRGuard(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	actionable *github.ActionableResult,
	logger *slog.Logger,
) {
	claimLedgerOnce.Do(func() {
		ledger, err := github.LoadClaimLedger(github.ClaimLedgerPath, logger)
		if err != nil {
			// LoadClaimLedger always returns a usable (possibly empty) ledger
			// alongside the error, so we keep it and just report the problem.
			logger.Warn("duplicate-PR guard: could not load persisted claim ledger, starting empty",
				"path", github.ClaimLedgerPath, "error", err)
		}
		claimLedger = ledger
	})
	if claimLedger == nil {
		return
	}
	github.ApplyDuplicatePRGuard(ctx, ghClient, claimLedger, hiveIdentity(cfg), actionable, logger)
}

func writeMergeEligible(actionable *github.ActionableResult, hold github.HoldResult, org string, logger *slog.Logger) {
	holdSet := make(map[string]bool)
	for _, h := range hold.Items {
		key := fmt.Sprintf("%s/%d", h.Repo, h.Number)
		holdSet[key] = true
	}

	type eligiblePR struct {
		Number    int      `json:"number"`
		Repo      string   `json:"repo"`
		Title     string   `json:"title"`
		Author    string   `json:"author"`
		Labels    []string `json:"labels,omitempty"`
		Mergeable bool     `json:"mergeable"`
		DCO       string   `json:"dco"`
	}

	type failingPR struct {
		Number  int    `json:"number"`
		Repo    string `json:"repo"`
		Title   string `json:"title"`
		Author  string `json:"author"`
		HeadSHA string `json:"head_sha,omitempty"`
	}

	var eligible []eligiblePR
	var failing []failingPR
	for _, pr := range actionable.PRs.Items {
		if pr.Draft {
			continue
		}
		key := fmt.Sprintf("%s/%d", pr.Repo, pr.Number)
		if holdSet[key] {
			continue
		}
		fullRepo := pr.Repo
		if !strings.Contains(fullRepo, "/") && org != "" {
			fullRepo = org + "/" + fullRepo
		}

		if pr.CIStatus == "failure" {
			failing = append(failing, failingPR{
				Number:  pr.Number,
				Repo:    fullRepo,
				Title:   pr.Title,
				Author:  pr.Author,
				HeadSHA: pr.HeadSHA,
			})
			continue
		}

		dco := "unknown"
		for _, l := range pr.Labels {
			if l == "dco-signoff: yes" {
				dco = "yes"
			} else if l == "dco-signoff: no" {
				dco = "no"
			}
		}
		eligible = append(eligible, eligiblePR{
			Number:    pr.Number,
			Repo:      fullRepo,
			Title:     pr.Title,
			Author:    pr.Author,
			Labels:    pr.Labels,
			Mergeable: pr.Mergeable,
			DCO:       dco,
		})
	}

	_ = os.MkdirAll("/var/run/hive-metrics", 0o755)

	payload := map[string]any{
		"generated_at":   time.Now().UTC().Format(time.RFC3339),
		"merge_eligible": eligible,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		logger.Warn("failed to marshal merge-eligible", "error", err)
		return
	}
	atomicWrite(mergeEligiblePath, data)
	logger.Info("merge-eligible.json updated", "eligible", len(eligible), "ci_failing", len(failing), "total_prs", len(actionable.PRs.Items))

	failPayload := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"ci_failing":   failing,
	}
	failData, err := json.Marshal(failPayload)
	if err != nil {
		logger.Warn("failed to marshal ci-failing", "error", err)
		return
	}
	atomicWrite(ciFailingPath, failData)
}

func atomicWrite(path string, data []byte) {
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

func applyConfigOverrides(cfg *config.Config, o *snapshot.ConfigOverrides) {
	if len(o.ProjectRepos) > 0 {
		cfg.Project.Repos = o.ProjectRepos
	}
	if o.EvalIntervalS != nil {
		cfg.Governor.EvalIntervalS = *o.EvalIntervalS
	}
	if len(o.Thresholds) > 0 {
		for name, threshold := range o.Thresholds {
			if mode, ok := cfg.Governor.Modes[name]; ok {
				mode.Threshold = threshold
				cfg.Governor.Modes[name] = mode
			}
		}
	}
	if len(o.SensingGHRate) > 0 {
		cfg.Governor.Sensing.GHRatePatterns = o.SensingGHRate
	}
	if len(o.SensingCLIExclude) > 0 {
		cfg.Governor.Sensing.CLIExcludePatterns = o.SensingCLIExclude
	}
	if len(o.SensingLogin) > 0 {
		cfg.Governor.Sensing.LoginPatterns = o.SensingLogin
	}
	if o.SensingTTL != nil {
		cfg.Governor.Sensing.TTLSeconds = *o.SensingTTL
	}
	if o.SensingPullback != nil {
		cfg.Governor.Sensing.PullbackSeconds = *o.SensingPullback
	}
	if len(o.ExemptLabels) > 0 {
		cfg.Governor.Labels.Exempt = o.ExemptLabels
	}
	if o.NtfyServer != "" || o.NtfyTopic != "" {
		if cfg.Notifications.Ntfy == nil {
			cfg.Notifications.Ntfy = &config.NtfyConfig{}
		}
		if o.NtfyServer != "" {
			cfg.Notifications.Ntfy.Server = o.NtfyServer
		}
		if o.NtfyTopic != "" {
			cfg.Notifications.Ntfy.Topic = o.NtfyTopic
		}
	}
	if o.DiscordWebhook != "" {
		if cfg.Notifications.Discord == nil {
			cfg.Notifications.Discord = &config.DiscordConfig{}
		}
		cfg.Notifications.Discord.Webhook = o.DiscordWebhook
	}
	if o.HealthcheckInterval != nil {
		cfg.Governor.Health.HealthcheckInterval = *o.HealthcheckInterval
	}
	if o.RestartCooldown != nil {
		cfg.Governor.Health.RestartCooldown = *o.RestartCooldown
	}
	if o.ModelLock != nil {
		cfg.Governor.Health.ModelLock = *o.ModelLock
	}
	if o.LogMaxSizeMB != nil {
		cfg.Governor.Logging.MaxSizeMB = *o.LogMaxSizeMB
	}
	if o.LogMaxAgeDays != nil {
		cfg.Governor.Logging.MaxAgeDays = *o.LogMaxAgeDays
	}
	if o.LogMaxBackups != nil {
		cfg.Governor.Logging.MaxBackups = *o.LogMaxBackups
	}
	if o.LogCompress != nil {
		cfg.Governor.Logging.Compress = *o.LogCompress
	}
	if o.LogLevel != "" {
		cfg.Governor.Logging.Level = o.LogLevel
	}
}

const (
	nousGovernorDir = "/var/run/nous/governor"
	nousSnapshotDir = "/data/nous/snapshots"
)

func loadNousState(logger *slog.Logger) *dashboard.NousState {
	state := &dashboard.NousState{
		Mode:   "observe",
		Scope:  "governor",
		Phase:  "collecting",
		Status: make(map[string]interface{}),
		Config: make(map[string]interface{}),
	}

	if ledgerData, err := os.ReadFile(nousGovernorDir + "/ledger.json"); err == nil {
		var ledger struct {
			Iterations []map[string]interface{} `json:"iterations"`
		}
		if err := json.Unmarshal(ledgerData, &ledger); err == nil {
			state.Ledger = ledger.Iterations
			logger.Info("nous ledger loaded", "iterations", len(state.Ledger))
		}
	}

	if principlesData, err := os.ReadFile(nousGovernorDir + "/principles.json"); err == nil {
		var pFile struct {
			Principles []json.RawMessage `json:"principles"`
		}
		if err := json.Unmarshal(principlesData, &pFile); err == nil {
			for _, raw := range pFile.Principles {
				var p map[string]interface{}
				if json.Unmarshal(raw, &p) == nil {
					state.Principles = append(state.Principles, dashboard.NousPrinciple{
						ID:         stringFromMap(p, "id"),
						Text:       stringFromMap(p, "statement"),
						Confidence: confidenceToFloat(stringFromMap(p, "confidence")),
						Source:     stringFromMap(p, "category"),
					})
				}
			}
			logger.Info("nous principles loaded", "count", len(state.Principles))
		}
	}

	snapshotCount := 0
	if entries, err := os.ReadDir(nousSnapshotDir); err == nil {
		snapshotCount = len(entries)
	}

	iterationCount := len(state.Ledger)
	if iterationCount > 0 {
		state.Phase = "observing"
	}

	state.Status = map[string]interface{}{
		"status":          "active",
		"mode":            state.Mode,
		"scope":           state.Scope,
		"phase":           state.Phase,
		"snapshots":       snapshotCount,
		"snapshotCount":   snapshotCount,
		"iterations":      iterationCount,
		"principles":      len(state.Principles),
		"principleCount":  len(state.Principles),
		"baseline_target": dashboard.NousBaselineTarget,
		"snapshotTarget":  dashboard.NousBaselineTarget,
		"baseline_pct":    float64(snapshotCount) * 100 / dashboard.NousBaselineTarget,
	}

	return state
}

func stringFromMap(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func confidenceToFloat(s string) float64 {
	switch s {
	case "high":
		return 0.9
	case "medium":
		return 0.7
	case "low":
		return 0.4
	default:
		return 0.5
	}
}

const logFilename = "hive.log"

func setupLogger(dir string, maxSizeMB, maxAgeDays, maxBackups int, compress bool, level string) *slog.Logger {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("failed to create log directory, falling back to stdout only", "dir", dir, "error", err)
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLogLevel(level)}))
	}

	lj := &lumberjack.Logger{
		Filename:   filepath.Join(dir, logFilename),
		MaxSize:    maxSizeMB,
		MaxAge:     maxAgeDays,
		MaxBackups: maxBackups,
		Compress:   compress,
	}

	tee := io.MultiWriter(os.Stdout, lj)
	return slog.New(slog.NewJSONHandler(tee, &slog.HandlerOptions{Level: parseLogLevel(level)}))
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// initAgentConfigDrivenSystems wires up config-driven agent metadata to subsystems
// that previously relied on hardcoded agent name maps (classifier, discord, token detector).
func initAgentConfigDrivenSystems(cfg *config.Config) {
	var lanes []classify.LaneConfig
	var agentNames []string
	detectKeywords := make(map[string][]string)
	discordIdentities := make(map[string]discord.AgentIdentity)
	discordAliases := make(map[string]string)

	for name, agent := range cfg.Agents {
		agentNames = append(agentNames, name)

		if len(agent.LaneKeywords) > 0 {
			lanes = append(lanes, classify.LaneConfig{
				Name:     name,
				Keywords: agent.LaneKeywords,
			})
		}
		if len(agent.DetectKeywords) > 0 {
			detectKeywords[name] = agent.DetectKeywords
		}
		if agent.Emoji != "" || agent.Color != "" {
			discordIdentities[name] = discord.AgentIdentity{
				Emoji: agent.Emoji,
				Color: parseColorInt(agent.Color),
			}
		}
		for _, alias := range agent.Aliases {
			discordAliases[alias] = name
		}
	}

	if len(lanes) > 0 {
		classify.SetLanes(lanes)
	}
	if len(detectKeywords) > 0 {
		tokens.SetDetectKeywords(detectKeywords)
	}
	tokens.SetAgentNames(agentNames)
	discord.SetAgentIdentities(discordIdentities)
	if len(discordAliases) > 0 {
		discord.SetAgentAliases(discordAliases)
	}
}

// inferACMMLevel returns the configured ACMM level, defaulting to L1 (advisory-only).
// sameStringSlice reports whether two string slices have identical contents in
// the same order. Used to skip no-op authorized-users updates from heartbeats.
func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func inferACMMLevel(cfg *config.Config) int {
	if cfg.ACMMLevel != nil {
		return *cfg.ACMMLevel
	}
	return 1
}

// parseColorInt converts a hex color string like "#3498db" to an int.
func parseColorInt(color string) int {
	color = strings.TrimPrefix(color, "#")
	if color == "" {
		return 0x95a5a6
	}
	var result int
	fmt.Sscanf(color, "%x", &result)
	return result
}

func runHub(logger *slog.Logger) {
	port := 3001
	if p := os.Getenv("HIVE_HUB_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	logger.Info("starting in HUB mode", "port", port)

	hubSrv := hub.NewHubServer(port, logger, gitShort, gitBranch)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("hub received signal, shutting down gracefully", "signal", sig)
		const shutdownTimeout = 10 * time.Second
		if err := hubSrv.Shutdown(shutdownTimeout); err != nil {
			logger.Error("hub graceful shutdown failed", "error", err)
		}
	}()

	if err := hubSrv.Start(port); err != nil && err != http.ErrServerClosed {
		logger.Error("hub server failed", "error", err)
		os.Exit(1)
	}
	logger.Info("hub server stopped")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseEndpointList splits a comma-separated list of URLs into a slice.
// A single URL is returned as a one-element slice.
const (
	// litellmLocalProxyPort is the loopback port the bundled litellm proxy
	// listens on when governor.litellm.local_proxy is enabled. Distinct from
	// proxy.InferenceTranslatePort (18444): agents always talk to the Go
	// translator, which forwards to this local litellm instance.
	litellmLocalProxyPort = 18445
	// litellmLocalConfigPath is the user-provided litellm proxy config
	// (model list, upstream keys) on the /data volume.
	litellmLocalConfigPath = "/data/litellm/config.yaml"
	// litellmRestartDelay is the pause before restarting a crashed local
	// litellm proxy, to avoid a tight crash loop.
	litellmRestartDelay = 5 * time.Second
)

// litellmLocalProxyURL is the endpoint the Go inference translator forwards
// to when the local litellm proxy fallback is enabled.
func litellmLocalProxyURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", litellmLocalProxyPort)
}

// superviseLocalLiteLLM runs the bundled litellm binary as a local
// Anthropic-compat translator fallback (governor.litellm.local_proxy: true),
// restarting it on exit like StartInferenceTranslator's supervision.
// Agents never talk to it directly — the Go translator stays in front so
// per-agent attribution, mode enforcement, and the MITM proxy path are
// preserved.
func superviseLocalLiteLLM(ctx context.Context, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		cmd := exec.CommandContext(ctx, "litellm",
			"--host", "127.0.0.1",
			"--port", strconv.Itoa(litellmLocalProxyPort),
			"--config", litellmLocalConfigPath)
		logger.Info("starting local litellm proxy",
			"port", litellmLocalProxyPort, "config", litellmLocalConfigPath)
		if err := cmd.Run(); err != nil {
			logger.Warn("local litellm proxy exited", "error", err)
		} else {
			logger.Warn("local litellm proxy exited cleanly; restarting")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(litellmRestartDelay):
		}
	}
}

func parseEndpointList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{raw}
	}
	return out
}
