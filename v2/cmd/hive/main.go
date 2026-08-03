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
//
// Vars rather than consts so tests can point them at a temp dir and exercise
// the real resolution order; production never reassigns them.
var (
	spokeProvisionedAppKeyPath = "/secrets/gh-app-key.pem"
	spokeAppKeyPath            = "/data/gh-app-key.pem"
	// spokeAppKeyDir is where per-app-id keys the hub delivers land, one file per
	// App the fleet knows: gh-app-key-<appid>.pem. It is the PVC directory that
	// already holds spokeAppKeyPath, so both survive restarts. A var so tests can
	// redirect it; production never reassigns it.
	spokeAppKeyDir = "/data"
	// spokeProvisionedAppKeyDir is the read-only projected-Secret mount where
	// PROVISIONING places per-app-id keys (gh-app-key-<appid>.pem), mirroring
	// spokeAppKeyDir on the PVC. A hive provisioned with the fleet's full key set
	// holds them here from its very first boot — before any heartbeat has run — so
	// a forge switch never has to wait a beat for the target forge's key. The
	// mount is readOnly, so nothing ever writes here; it is a lookup source only.
	spokeProvisionedAppKeyDir = "/secrets"
)

// spokeAppKeyFileMode is rw------- : signing material must never be readable by
// anything else sharing the PVC or the pod.
const spokeAppKeyFileMode = 0o600

// perAppIDKeyPath returns the on-disk path of the key file a spoke uses for a
// specific app_id — /data/gh-app-key-<appid>.pem. This is what lets one spoke
// hold BOTH the github.com App key AND its cluster's GitHub Enterprise App key
// at once and sign with whichever matches its configured app_id.
//
// Returns "" for a non-positive app_id (0 = unknown/unset, the placeholder
// sentinel, or a hand-corrupted value): there is no meaningful per-app file for
// those, and the caller falls back to the existing single-file behaviour.
// agentActivityFor gathers the per-agent liveness evidence the hub needs to
// tell a deliberately-paused agent from one that is running but unable to
// work. Shared by both heartbeat build sites so the ordinary beat and the
// upgrade beat can never report different pictures of the same agent.
func agentActivityFor(mgr *agent.Manager, name string, proc *agent.AgentProcess) hub.AgentActivity {
	act := hub.AgentActivity{
		Paused:         proc.Paused,
		NeedsLogin:     proc.NeedsLogin,
		LastActivityAt: proc.LastPaneChange,
		// A missing tmux session is only meaningful for an agent the manager
		// believes is running; SessionMissing enforces that itself.
		SessionMissing: mgr.SessionMissing(name),
	}
	if proc.StartedAt != nil {
		act.StartedAt = *proc.StartedAt
	}
	return act
}

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

func perAppIDKeyPath(appID int64) string {
	if appID <= 0 {
		return ""
	}
	return filepath.Join(spokeAppKeyDir, fmt.Sprintf("gh-app-key-%d.pem", appID))
}

// deliveredKeyPath is where a hub-delivered private key for appID is stored.
//
// The filename NAMES the App, so a key can only ever be found under the App it
// was delivered for. The generic /data/gh-app-key.pem carries no such evidence:
// a key written there for one App silently becomes "the key" for whatever
// app_id the config later claims, which is how all 33 vllm-d spokes ended up
// signing as the public App with the GHE key and getting
// 404 Integration not found.
//
// Falls back to the generic path only when the delivery names no App, so a key
// is never dropped on the floor.
func deliveredKeyPath(appID int64) string {
	if p := perAppIDKeyPath(appID); p != "" {
		return p
	}
	return spokeAppKeyPath
}

// perAppIDProvisionedKeyPath is perAppIDKeyPath's read-only twin: the same
// per-app-id filename under the provisioning Secret mount. It is consulted only
// when the PVC has no usable key for the app_id, so a heartbeat-delivered key
// (which can be rotated) always wins over the one baked in at provision time.
func perAppIDProvisionedKeyPath(appID int64) string {
	if appID <= 0 {
		return ""
	}
	return filepath.Join(spokeProvisionedAppKeyDir, fmt.Sprintf("gh-app-key-%d.pem", appID))
}

// perAppIDKeyFilePrefix / Suffix bracket the per-app-id key filename so a scan
// can recover the app_id from the name. Named so the format lives in exactly one
// place alongside perAppIDKeyPath.
const (
	perAppIDKeyFilePrefix = "gh-app-key-"
	perAppIDKeyFileSuffix = ".pem"
)

// heldPerAppIDKeyFingerprints scans the PVC for per-app-id key files
// (gh-app-key-<appid>.pem) and returns app_id (decimal string) → fingerprint for
// every one that holds a usable key. It is what the spoke reports so the hub
// delivers the fleet's additional keys idempotently: a key already present with
// the right fingerprint is not re-sent.
//
// It never returns key material — only fingerprints. A missing directory,
// unreadable file, or unparseable key is silently skipped: the worst case is the
// hub re-delivers a key the spoke already writes idempotently, never a crash.
func heldPerAppIDKeyFingerprints() map[string]string {
	entries, err := os.ReadDir(spokeAppKeyDir)
	if err != nil {
		return nil
	}
	var held map[string]string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, perAppIDKeyFilePrefix) || !strings.HasSuffix(name, perAppIDKeyFileSuffix) {
			continue
		}
		idStr := strings.TrimSuffix(strings.TrimPrefix(name, perAppIDKeyFilePrefix), perAppIDKeyFileSuffix)
		id, convErr := strconv.ParseInt(idStr, 10, 64)
		if convErr != nil || id <= 0 {
			continue
		}
		fp, fpErr := config.AppKeyFingerprintFromFile(filepath.Join(spokeAppKeyDir, name))
		if fpErr != nil || fp == "" {
			continue
		}
		if held == nil {
			held = make(map[string]string)
		}
		held[idStr] = fp
	}
	return held
}

// writePerAppIDKey persists a hub-delivered per-app-id key to its PVC file
// atomically (temp file in the same dir, then rename) with a restrictive 0600
// mode from creation, so a spoke can never sign with a half-written key. Returns
// the resulting fingerprint (never the key) for auditable logging, or an error.
func writePerAppIDKey(appID int64, pemData string) (string, error) {
	path := perAppIDKeyPath(appID)
	if path == "" {
		return "", fmt.Errorf("refusing to write key for non-positive app_id %d", appID)
	}
	trimmed := strings.TrimSpace(pemData)
	if !strings.HasPrefix(trimmed, "-----BEGIN") {
		return "", fmt.Errorf("app key for app_id %d is not PEM", appID)
	}
	fp, err := config.AppKeyFingerprint(trimmed)
	if err != nil {
		return "", fmt.Errorf("app key for app_id %d is unusable: %w", appID, err)
	}
	if err := os.MkdirAll(spokeAppKeyDir, 0o700); err != nil {
		return "", fmt.Errorf("create app key dir: %w", err)
	}
	tmp, err := os.CreateTemp(spokeAppKeyDir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return "", fmt.Errorf("create temp app key file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds
	if err := tmp.Chmod(spokeAppKeyFileMode); err != nil {
		tmp.Close()
		return "", fmt.Errorf("chmod temp app key file: %w", err)
	}
	if _, err := tmp.WriteString(trimmed + "\n"); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write temp app key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("sync temp app key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temp app key file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("rename app key into place: %w", err)
	}
	return fp, nil
}

// reportedAppKeyFingerprint returns the non-secret fingerprint of the App key
// this spoke is ACTUALLY using, for the heartbeat payload. It fingerprints the
// resolved key file rather than a hard-coded path so the hub compares against
// the key that would really sign a JWT.
//
// Returns "" whenever there is no usable key — no file, empty file, or
// unparseable contents. All three mean the same thing to the hub ("this spoke
// cannot authenticate") and are repaired identically. The private key itself is
// never returned, and never enters the payload.
func reportedAppKeyFingerprint(keyFile string, appID int64) string {
	// Lead with the same path resolveAppKeyFile would sign with, so the hub is
	// told about the key actually in effect and never about a shadowed one.
	candidates := []string{
		resolveAppKeyFile(keyFile, os.Getenv("GH_APP_KEY_FILE"), appID),
		perAppIDKeyPath(appID),
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
func hasPerHiveAppKey(keyFile string, appID int64) bool {
	fp, err := config.AppKeyFingerprintFromFile(spokeProvisionedAppKeyPath)
	if err != nil || fp == "" {
		return false
	}
	// The provisioned key exists. It is the effective credential only if the
	// resolved key file still points at it — resolveAppKeyFile is the single
	// authority on that, so an unconfigured hive that has already taken delivery
	// of a /data key (or a per-app-id key) correctly stops claiming a per-hive
	// override.
	return resolveAppKeyFile(keyFile, os.Getenv("GH_APP_KEY_FILE"), appID) == spokeProvisionedAppKeyPath
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
//
// PER-APP-ID SELECTION (the both-keys fix)
//
// appID is the App this process is configured to authenticate AS
// (cfg.GitHub.AppID). When the hub has delivered a per-app-id key for exactly
// that App — /data/gh-app-key-<appID>.pem — it is preferred over the generic
// single-file paths, because it is provably the RIGHT key for the app_id we
// claim. This is what lets a github.com hive on a GitHub-Enterprise cluster sign
// with the github.com key even though its cluster default (and its single
// /data/gh-app-key.pem) is the GHE key. It sits just below an explicit
// key_file/env override — an operator who named a path still wins — and above
// the generic fallbacks. appID <= 0 disables it entirely, so nothing changes for
// a hive that reports no app_id.
func resolveAppKeyFile(configured, envOverride string, appID int64) string {
	if v := strings.TrimSpace(envOverride); v != "" {
		return v
	}
	if v := strings.TrimSpace(configured); v != "" {
		// MIGRATION. A configured key_file that is the GENERIC path is not an
		// operator's choice — it is a value older builds wrote automatically on
		// every key delivery, and it does not name the App it holds. When we can
		// see a per-app-id key for the app_id we actually claim, that key is
		// correct by construction and the generic pin is stale, so ignore it.
		//
		// Without this, the ~33 spokes already carrying
		// key_file: /data/gh-app-key.pem keep signing with whichever App's key
		// happens to sit there — the live 404 Integration not found — because an
		// explicit value short-circuits the per-app-id lookup below.
		//
		// Deliberately narrow: only the exact generic path is overridden, and
		// only when a usable per-app-id key exists. Any other path is a genuine
		// operator override (a hive on a third App with a bespoke key location)
		// and still wins outright.
		if v == spokeAppKeyPath {
			if p := perAppIDKeyPath(appID); p != "" {
				if fp, err := config.AppKeyFingerprintFromFile(p); err == nil && fp != "" {
					return p
				}
			}
		}
		return v
	}
	// Nothing explicitly configured. Prefer a per-app-id key matching the App we
	// claim — the only key that is CORRECT-by-construction for this app_id — over
	// the generic cluster/provisioned files. The fingerprint check (not mere
	// existence) keeps an empty or truncated per-app file from shadowing a good
	// generic key.
	if p := perAppIDKeyPath(appID); p != "" {
		if fp, err := config.AppKeyFingerprintFromFile(p); err == nil && fp != "" {
			return p
		}
	}
	// Same idea, but from the read-only provisioning mount: a hive provisioned
	// with the fleet's full key set can sign as its configured App on its very
	// first boot, before any heartbeat has delivered anything to the PVC. Ranked
	// BELOW the PVC copy so a rotated key delivered by heartbeat always wins over
	// the one frozen into the Secret at provision time.
	if p := perAppIDProvisionedKeyPath(appID); p != "" {
		if fp, err := config.AppKeyFingerprintFromFile(p); err == nil && fp != "" {
			return p
		}
	}
	// Prefer a usable hub-delivered key on the PVC; fall back to the provisioning
	// mount only when /data has no parseable key.
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
		fmt.Sprintf("per-app-id PVC key %s/gh-app-key-<app_id>.pem", spokeAppKeyDir),
		fmt.Sprintf("per-app-id provisioning key %s/gh-app-key-<app_id>.pem", spokeProvisionedAppKeyDir),
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
	appKeyFile := resolveAppKeyFile(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID)

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

// reporterName identifies this spoke process to the hub (HeartbeatPayload
// Reporter). In-cluster the hostname IS the pod name; on failure it stays
// empty, which the hub reads as "too old / cannot report", never as data.
var reporterName, _ = os.Hostname()

// spokeRestartMinUptime is how old this process must be before it acts on a
// hub-delivered restart. The hub delivers the instruction to every beat in a
// multi-minute window so all instances hear it; without this guard the
// restarted process would come back inside the same window and restart again.
const spokeRestartMinUptime = 10 * time.Minute

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
		m := parseUpgradeMarker(markerData)
		if m.CurrentSHA != gitShort {
			// We booted on a different SHA than the one that requested the
			// upgrade: it landed. Drop the marker so the attempt budget resets.
			os.Remove(upgradeMarkerStartupPath)
			logger.Info("upgrade landed, cleared marker",
				"current", gitShort, "previous", m.CurrentSHA, "target", m.TargetSHA)
		} else {
			// Same SHA as the attempt that ran before this boot: the image never
			// changed, so that attempt FAILED. Say so at startup — previously
			// this restart looked completely routine in the logs.
			logger.Error("previous self-upgrade attempt did not land (still on the same image)",
				"current", gitShort,
				"target", m.TargetSHA,
				"attempts", m.Attempts,
				"last_error", m.LastError,
			)
		}
	}

	if os.Getenv("HIVE_MODE") == "hub" {
		runHub(logger)
		return
	}

	// Use LoadWithDashboardOverlay (not plain Load) so the dashboard overlay's
	// removed_agents tombstones are populated into cfg.RemovedAgents at boot —
	// BEFORE the startup ApplyPack below reconciles the ACMM roster. Plain Load
	// never reads the overlay, so on restart the tombstone was invisible and
	// ApplyPack re-added deleted pack agents (brainstorm/guide) every time
	// (#2439). Same return signature as Load; falls back to the seed when no
	// overlay exists or the pod is not in Kubernetes.
	cfg, err := config.LoadWithDashboardOverlay(*configPath)
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

	// Observability (#2439): report the removed-agents tombstone LoadWithDashboardOverlay
	// adopted from the dashboard overlay at boot, BEFORE the startup ApplyPack below. On
	// a non-sticking-removal report this line is the first check — an empty set here on a
	// hive that removed an agent means the tombstone did not persist across the restart.
	logger.Info("boot: loaded removed-agents tombstone",
		"hive_id", cfg.HiveID,
		"count", len(cfg.RemovedAgents),
		"agents", cfg.RemovedAgents,
	)

	// Surface config provenance: when the persisted runtime config exists, init
	// containers restore it over the ConfigMap seed on restart, so edits made
	// only to the seed (or only to the live file) silently lose to it.
	//
	// Checks the legacy name too: during the migration a hive may still carry
	// only /data/hive.yaml.bak, and the whole point of this log line is to warn
	// that such a file is shadowing the seed. Note this path was previously
	// built as *configPath + ".bak", which only ever resolved to the real
	// location when HIVE_CONFIG happened to live under /data — a literal grep
	// for "hive.yaml.bak" could not find it either.
	for _, runtimePath := range []string{config.RuntimeConfigFile, config.RuntimeConfigFileLegacy} {
		if _, statErr := os.Stat(runtimePath); statErr == nil {
			logger.Info("persisted runtime config present — restored over the seed on pod restart; fixes must land in the live config so the next save refreshes it",
				"path", runtimePath,
				"github_installation_id", cfg.GitHub.InstallationID,
			)
			break
		}
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
	// Config truth outranks live probes: an App with no installation cannot
	// mint, period. A cached token can keep clients green for up to an hour
	// after an installation is cleared, and waiting for the first failed mint
	// left the banner down and the hub green exactly when the operator needed
	// the opposite (the fast-model-actuation incident).
	if cfg.GitHub.ConfiguredButUninstalled() {
		githubAppRequired = true
		githubAppState = github.AppStateNotInstalled
		githubAppDiag = "GitHub App " + strconv.FormatInt(cfg.GitHub.AppID, 10) +
			" has no installation for this org — install it (the spoke adopts the installation automatically)"
	}

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
				if isGitHubRateLimitText(err) {
					logger.Warn("GitHub API rate limit hit during advisory issue ensure", "repo", primaryRepo)
				} else {
					// Do NOT decide from the error string. This call site used
					// to raise the banner on a substring match for "403"/"401"
					// and set githubAppRequired=true BEFORE classifying, then
					// never lower it again when classification came back OK or
					// inconclusive — which is why the banner showed on boot and
					// vanished on the first Re-check with nothing fixed.
					// classifyGitHubAppFailure is the same verdict Re-check
					// uses, and it declines to raise on AppStateUnknown.
					raise, diag, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger)
					if raise {
						githubAppRequired = true
						githubAppDiag, githubAppState = diag, state
						logger.Warn("GitHub App authentication failed at startup",
							"state", state.String(),
							"operator_actionable", state.OperatorActionable(),
							"error", err)
					} else {
						logger.Warn("advisory issue ensure failed but GitHub App auth verified healthy — not raising the App banner",
							"repo", primaryRepo, "state", state.String(), "error", err)
					}
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

	// PR-open-as-the-App-bot: agents push their branch (App-token credential
	// helper) then drop a request file; the hive opens the PR here with the App
	// token so it is authored by "<slug>[bot]", not the Copilot login user.
	// Gated on a real client + usable App — with no App there is no bot to author
	// as, and requests simply accumulate rather than opening under a wrong
	// identity. ghClient uses the App installation token (see ghAuth wiring).
	if ghClient != nil && cfg.GitHub.HasUsableApp() {
		// authz enforces the SAME per-agent ACMM write-gate + forge-resistance as
		// the direct `gh pr create` path — the request-file route grants no extra
		// privilege. A denied request is quarantined, never opened.
		ghClient.StartPRRequestWatcher(ctx, agentMgr.AuthorizePROpen, nil)
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
	//
	// Use EffectiveAIAuthor(), not the raw Project.AIAuthor field. App-authored
	// hives deliberately leave ai_author EMPTY and derive their identity from
	// the installed App ("<slug>[bot]") — that is what keeps App-bot mode
	// durable across restarts. Reading the raw field saw "" for every one of
	// them and disabled the collector fleet-wide, while the PAT fallback below
	// could not rescue it either: those hives authenticate as a GitHub App and
	// have github.token empty, so there was no token to identify. The result
	// was a fleet where essentially no spoke ever attempted a collect.
	fleetStatsAuthor := cfg.EffectiveAIAuthor()
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
	// Persist the collected counts on the /data PVC (same store as sessions and
	// cost/fact history) so a restart resumes from the last-known counts instead
	// of nil. Without this, a fleet-wide upgrade clears every spoke's in-memory
	// counts and the public landing-page total collapses until all spokes
	// re-collect (#2329, building on the hub-side #2328 defensive aging fix).
	fleetStatsCollector.EnablePersistence("/data/fleet-stats.json")
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
					if isGitHubRateLimitText(err) {
						logger.Warn("GitHub API rate limit hit during advisory issue creation", "repo", newPrimaryRepo)
					} else {
						// #2224 replaced error-string classification everywhere
						// else but missed this site, which raised the banner on
						// a bare "403"/"401" substring and recorded no state at
						// all — so the UI fell back to "App Not Installed" even
						// for an operator-side key fault. Classify properly.
						raise, diag, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger)
						if raise {
							dashSrv.SetGitHubAppRequired(true)
							dashSrv.SetGitHubAppState(state.String())
							if diag != "" {
								dashSrv.SetGitHubAppPermIssue(diag)
							}
							logger.Warn("GitHub App authentication failed creating advisory issue",
								"repo", newPrimaryRepo, "state", state.String(),
								"operator_actionable", state.OperatorActionable())
						}
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
		// Same key resolution as boot (initGitHubAuth) and the heartbeat apply
		// path: without it, the dashboard Set ID handler gated reinit on the
		// raw key_file, which is deliberately empty on hub-delivered per-app-id
		// keys (#2459).
		ResolveAppKeyFileFunc: func(configured string, appID int64) string {
			return resolveAppKeyFile(configured, os.Getenv("GH_APP_KEY_FILE"), appID)
		},
	})

	// Forge App tab inventory: the resolved active key path and the per-app-id
	// PVC keys live here in cmd/hive, so they are injected as a provider (the
	// SetGitHubAppRecheckFn pattern). Fingerprints and paths only — the
	// provider never touches key material.
	dashSrv.SetForgeAppInventoryFn(func() dashboard.ForgeAppInventory {
		held := heldPerAppIDKeyFingerprints()
		keys := make([]dashboard.ForgeAppKey, 0, len(held))
		for idStr, fp := range held {
			keys = append(keys, dashboard.ForgeAppKey{
				AppID:       idStr,
				Path:        filepath.Join(spokeAppKeyDir, perAppIDKeyFilePrefix+idStr+perAppIDKeyFileSuffix),
				Fingerprint: fp,
			})
		}
		return dashboard.ForgeAppInventory{
			ActiveKeyFile: resolveAppKeyFile(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID),
			HeldKeys:      keys,
		}
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
				// Shared verdict with the boot and advisory-digest paths. Re-check
				// previously branched on `diag != ""` while boot branched on the
				// error string, which is how the two came to disagree about the
				// same hive; routing both through classifyGitHubAppFailure means
				// they cannot drift again.
				if raise, diag, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger); raise {
					dashSrv.SetGitHubAppPermIssue(diag)
					dashSrv.SetGitHubAppState(state.String())
					logger.Warn("github app recheck: app detected but write not verified",
						"repo", recheckRepo, "state", state.String(),
						"operator_actionable", state.OperatorActionable(), "detail", diag)
					dashSrv.AuditLog("system", "github_app_check", "result=installed but write NOT verified: "+diag, "")
					return false
				}
				// #2353: the classifier above only proves the installation
				// authenticates and grants issues:write — NOT that this repo can
				// actually be written. Finding the advisory issue is a READ, which
				// succeeds even when the repo is not in the App installation's
				// selected repos. Perform a REAL write probe before clearing the
				// banner, so re-check cannot falsely "verify write" for a repo the
				// App can only read (the recheck false-positive).
				if werr := ghClient.ProbeIssueWrite(ctx, recheckRepo, num); werr != nil {
					if strings.Contains(werr.Error(), "403") && strings.Contains(werr.Error(), "Resource not accessible by integration") {
						msg, state := classifyGitHubAppWriteForbidden(ctx, ghClient.AppAuth(), cfg.Project.Org, recheckRepo)
						dashSrv.SetGitHubAppPermIssue(msg)
						dashSrv.SetGitHubAppState(state.String())
						logger.Warn("github app recheck: write probe returned 403 — not clearing the banner",
							"repo", recheckRepo, "state", state.String(), "detail", msg)
						dashSrv.AuditLog("system", "github_app_check", "result=write probe FORBIDDEN: "+msg, "")
						return false
					}
					// A non-403 probe failure is inconclusive (rate limit,
					// transient network). Do NOT clear the banner on a write we
					// could not confirm, but also do NOT accuse anyone.
					logger.Warn("github app recheck: write probe inconclusive — leaving banner as-is",
						"repo", recheckRepo, "error", werr)
					dashSrv.AuditLog("system", "github_app_check", "result=write probe inconclusive", "")
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

		// Preserve removed-agent tombstones across the swap as a union of the
		// live cfg and the incoming reload. LoadWithDashboardOverlay now carries
		// the overlay's tombstones into newCfg, but a removal that landed in the
		// live cfg after this reload's snapshot (or an overlay too short/stale to
		// echo it back yet) must not be lost — otherwise the next persistState
		// saver rewrites every layer tombstone-free and the deleted agents
		// reappear (#2439). Union keeps any tombstone present in either side.
		for _, name := range cfg.RemovedAgents {
			newCfg.MarkAgentRemoved(name)
		}
		newCfg.PruneRemovedAgents()

		// Observability (#2439): this is the ~2-min interval reload path, so keep it
		// at DEBUG to avoid spamming a healthy hive. When a removal is not sticking,
		// enabling DEBUG shows the tombstone surviving each swap — an empty count here
		// while the agent keeps reappearing localizes the leak to this union-preserve.
		logger.Debug("reload: preserved removed-agents",
			"hive_id", cfg.HiveID,
			"count", len(newCfg.RemovedAgents),
			"agents", newCfg.RemovedAgents,
		)

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
					// Key source must MATCH the entitlement/probe path (gateways.go,
					// cost.go, openrouter.go), which resolve the key from the gateway
					// via ResolveGateway(backend).ResolveAPIKey(). When an EXPLICIT
					// `gateways:` block names this backend, that gateway carries its
					// own api_key_file (e.g. the key saved from the Model Gateways
					// tab). Reading the legacy Governor.LiteLLM key file here instead
					// would send a DIFFERENT (often stale) key than entitlement
					// validated, causing inference 401s after a key rotation done via
					// the Gateways tab. Resolve from the same gateway so inference and
					// entitlement always agree on one key source.
					//
					// Only explicit gateways override: ResolvedGateways synthesizes an
					// implicit "litellm" gateway from the legacy block when no
					// `gateways:` are set, but that synthetic gateway lacks the
					// multi-location file fallback of LiteLLMConfig.ResolveAPIKey
					// (k8s Secret mount + PVC copy). For no-gateway hives we therefore
					// keep the legacy resolver to preserve today's behavior.
					apiKey := cfg.Governor.ResolveLiteLLMInferenceKey(backend)
					caBundle := lc.CABundle
					if len(cfg.Governor.Gateways) > 0 {
						if gw := cfg.Governor.ResolveGateway(backend); gw != nil {
							caBundle = gw.CABundle
						}
					}
					githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:  backend,
						Endpoint: endpoint,
						Model:    model,
						APIKey:   apiKey,
						CABundle: caBundle,
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

	// Mark the dashboard READY as soon as the HTTP server can serve requests —
	// which is NOW: config is loaded, GitHub client/App auth are wired, the
	// dashboard deps are set, and the listener (go dashSrv.Start() above) is up.
	// None of /api/*, /sso, /open, /api/livez or /api/health depend on the agent
	// fleet being up; the frontend already handles agents appearing over time.
	//
	// This MUST precede the staggered agent-launch loop below. That loop sleeps
	// ~15s per agent (× the whole fleet = several minutes) and previously ran
	// BEFORE MarkReady, so /api/livez returned 503 "starting" for the entire
	// launch window. The liveness probe (period 30s × failureThreshold 3 ≈ 90s)
	// then SIGKILLed the container (exit 137) before readiness was ever reached
	// on cold start, and rolling upgrades left the Service with no Ready endpoint
	// for minutes → 503s on /open and /sso. Flipping ready here makes the pod
	// Ready in seconds and moves the fleet spin-up entirely off the critical path.
	dashSrv.MarkReady()

	// Launch the persistent (non-on-demand) agents in the BACKGROUND so the
	// staggered start no longer gates pod readiness. The loop honors ctx: on
	// shutdown the ctx-aware stagger returns immediately instead of leaking a
	// goroutine parked in a bare time.Sleep.
	go func() {
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
				select {
				case <-time.After(time.Duration(agentLaunchDelaySec) * time.Second):
				case <-ctx.Done():
					logger.Info("aborting staggered agent launch: shutting down")
					return
				}
			}
			// Bail before starting another agent if we are already shutting down,
			// so a SIGTERM during the launch window doesn't spawn fresh processes.
			if ctx.Err() != nil {
				logger.Info("aborting staggered agent launch: shutting down")
				return
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
	}()

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
				mode := ""
				if ac, ok := cfg.Agents[name]; (ok && ac.OnDemand) || onDemandFromPack[name] {
					mode = "on_demand"
				}
				agents = append(agents, hub.NewAgentSummary(name, string(proc.State), mode,
					agentActivityFor(agentMgr, name, proc)))
			}
			acmmLvl := 0
			if cfg.ACMMLevel != nil {
				acmmLvl = *cfg.ACMMLevel
			}
			// Attach cached fleet-stat counts only once the collector has done a
			// successful compute — nil pointers until then, so the hub never
			// aggregates a not-yet-computed zero into the public fleet total.
			var prsMerged, prsRejected, cvesClosed *int
			fleetStatsCollectedAt := ""
			if fc, ok := fleetStatsCollector.Snapshot(); ok {
				m, rj, cv := fc.PRsMerged, fc.PRsRejected, fc.CVEsClosed
				prsMerged, prsRejected, cvesClosed = &m, &rj, &cv
				// Report WHEN these were computed, not when this beat was sent.
				// The hub carries counts forward across spoke restarts, so this
				// timestamp is the only way it can tell a fresh contribution
				// from one frozen by a collector that has started failing.
				if t := fleetStatsCollector.CollectedAt(); !t.IsZero() {
					fleetStatsCollectedAt = t.UTC().Format(time.RFC3339)
				}
			}
			// Count agents with a method/model assigned for the hub's
			// user-journey stage detection. Always a non-nil pointer from a
			// spoke new enough to compute it, so the hub can distinguish
			// "genuinely zero agents configured" from "old spoke, unknown".
			agentsWithModel := agentMgr.CountAgentsWithModel()
			return &hub.HeartbeatPayload{
				AgentsWithModel: &agentsWithModel,
				// Read-back for hub-funded gateways: the hub clears its pending
				// record only when it sees the gateway named here, so a lost
				// delivery is re-offered rather than dropped. Names only — the
				// key never leaves the spoke.
				GatewayNames:      dashSrv.ConfiguredGatewayNames(),
				HiveID:            cfg.HiveID,
				Org:               cfg.Project.Org,
				AIAuthor:          cfg.Project.AIAuthor,
				AIAuthorEffective: cfg.EffectiveAIAuthor(),
				StartedAt:         processStartedAt.UTC().Format(time.RFC3339),
				// Reporter names THIS process (the pod) so the hub can tell two
				// instances reporting as one hive apart — the pod name is the
				// hostname inside the container.
				Reporter: reporterName,
				// Advisory-staleness signal (mirrors StartedAt/uptime). Report the
				// last successful digest-post time only if the spoke has actually
				// posted one — a zero time is left as an empty string so the hub
				// reads it as UNKNOWN (not-advisory-mode / old spoke), never a
				// false stale alarm. The last post error rides alongside so a
				// working-App-but-failing-post hive can be flagged with its cause.
				AdvisoryLastPostedAt: func() string {
					postedAt, _, _ := dashSrv.AdvisoryState()
					if postedAt.IsZero() {
						return ""
					}
					return postedAt.UTC().Format(time.RFC3339)
				}(),
				AdvisoryError: func() string {
					_, _, errMsg := dashSrv.AdvisoryState()
					return errMsg
				}(),
				Repos:       cfg.Project.Repos,
				PrimaryRepo: cfg.Project.PrimaryRepo,
				ACMMLevel:   acmmLvl,
				Agents:      agents,
				Governor:    hub.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs},
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
				// Report who has a live dashboard session so the hub can accumulate
				// per-user "time in hive". Bare usernames only — never session
				// ids/tokens (ActiveSessionUsernames guarantees this).
				ActiveSessionUsers: dashSrv.ActiveSessionUsernames(),
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
				ImageRef: hub.SelfDeploymentImage(),
				// The GitHub instance this spoke actually runs against. Only
				// the spoke knows this for certain: a hive's GitHub can differ
				// from its cluster's default, so the hub cannot infer it.
				// Reported as a bare hostname via HostLabel(), which reads BOTH
				// base_url and api_url — a GHE placeholder with base_url:"" but
				// api_url: github.ibm.com must report github.ibm.com, not be
				// silently rendered as github.com in the spokes table.
				GitHubHost:              cfg.GitHub.HostLabel(),
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
				PRsMerged90d:          prsMerged,
				PRsRejected90d:        prsRejected,
				CVEsClosed:            cvesClosed,
				FleetStatsCollectedAt: fleetStatsCollectedAt,
				// Report WHICH App key we hold, never the key. The hub compares
				// this against its per-cluster key and pushes a correction only
				// on a mismatch, so a spoke already holding the right key costs
				// nothing and a spoke holding the wrong one self-heals.
				GitHubAppKeyFingerprint: reportedAppKeyFingerprint(cfg.GitHub.KeyFile, cfg.GitHub.AppID),
				GitHubAppKeyPerHive:     hasPerHiveAppKey(cfg.GitHub.KeyFile, cfg.GitHub.AppID),
				// Report the App this hive believes it authenticates as. The hub
				// pairs it with the fingerprint above to tell a per-hive key that
				// is WRONG for this App from one that is deliberately for another.
				GitHubAppID: cfg.GitHub.AppID,
				// Report the REST of the identity set too. app_id alone cannot
				// distinguish a correctly-delivered identity from a
				// half-applied one: a GHE app_id with an empty api_url looks
				// identical to the hub, and 404s on every token request. All
				// four together let the hub see the whole set.
				GitHubAppSlug:        cfg.GitHub.AppSlug,
				GitHubInstallationID: cfg.GitHub.InstallationID,
				GitHubBaseURL:        cfg.GitHub.BaseURL,
				// Report the fingerprint of every ADDITIONAL per-app-id key already
				// on the PVC, so the hub delivers the fleet's other App keys once
				// and then stops re-sending them.
				GitHubAppKeysHeld: heldPerAppIDKeyFingerprints(),
			}
		}, heartbeatSendInterval, logger, hub.RestartSpokeCallback(func() {
			if up := time.Since(processStartedAt); up < spokeRestartMinUptime {
				logger.Info("hub requested a spoke restart; ignoring — this process just started",
					"uptime", up.Round(time.Second))
				return
			}
			logger.Warn("hub requested a spoke restart — rolling this deployment",
				"reporter", reporterName)
			if err := hub.RolloutRestartSelf(logger); err != nil {
				// Do NOT exit here: without deployment-patch RBAC an exit would
				// restart onto the same state every delivery and look like a
				// crash-loop. The error names the missing Role instead.
				logger.Error("spoke restart failed: could not patch own Deployment",
					"error", err,
					"hint", "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding)")
			}
		}), hub.UpgradeCallback(func(targetSHA string) {
			const upgradeMarkerPath = "/data/upgrade-requested"

			// attemptCount carries the number of PREVIOUS failed attempts for this
			// (current_sha → target_sha) pair, read from the marker below.
			attemptCount := 0

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

			// A previous process attempted an upgrade and we booted with the same
			// git hash, so the image did not actually change: the attempt FAILED.
			// Back off rather than retrying instantly (that was a crash-loop), but
			// do NOT latch forever — "image unchanged" is the signature of a failed
			// upgrade, not a reason to stop trying. The latch is keyed on
			// (current_sha → target_sha) and bounded to selfUpgradeMaxAttempts, so a
			// NEW target always gets a fresh budget and a transient failure (an RBAC
			// Role that showed up late, a registry blip) still converges.
			if markerData, err := os.ReadFile(upgradeMarkerPath); err == nil {
				m := parseUpgradeMarker(markerData)
				if m.CurrentSHA == gitShort && sameUpgradeTarget(m.TargetSHA, targetSHA) {
					if m.Attempts >= selfUpgradeMaxAttempts {
						// Terminal: report it LOUDLY and tell the hub, so the UI stops
						// claiming "Upgrading" forever and a human sees the real cause.
						logger.Error("self-upgrade FAILED: giving up after repeated attempts (image never changed)",
							"target", targetSHA,
							"current", gitShort,
							"attempts", m.Attempts,
							"max_attempts", selfUpgradeMaxAttempts,
							"last_error", m.LastError,
							"hint", "the spoke must be able to get/patch its own Deployment; check the hive-self-upgrade Role/RoleBinding in this namespace",
						)
						hub.ReportUpgradeFailure(hubURL, cfg.HiveID, targetSHA, gitShort,
							fmt.Sprintf("self-upgrade failed after %d attempts: %s", m.Attempts, m.LastError), logger)
						return
					}
					// Exponential backoff between attempts so a hard failure does not
					// spin every heartbeat while a recoverable one still retries.
					backoff := selfUpgradeBaseBackoff << (m.Attempts - 1)
					if backoff > selfUpgradeMaxBackoff {
						backoff = selfUpgradeMaxBackoff
					}
					if since := time.Since(m.RequestedAt); since < backoff {
						logger.Warn("self-upgrade retry deferred: backing off after a failed attempt",
							"target", targetSHA,
							"current", gitShort,
							"attempts", m.Attempts,
							"retry_in", (backoff - since).Round(time.Second),
							"last_error", m.LastError,
						)
						return
					}
					logger.Warn("self-upgrade retrying after a failed attempt (image unchanged)",
						"target", targetSHA,
						"current", gitShort,
						"attempt", m.Attempts+1,
						"max_attempts", selfUpgradeMaxAttempts,
						"last_error", m.LastError,
					)
					attemptCount = m.Attempts
				} else {
					// Different SHA or a different target — the old marker is stale.
					os.Remove(upgradeMarkerPath)
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

			// Record the attempt BEFORE acting: if the process dies mid-upgrade the
			// next boot must still see an incremented count, otherwise a crash loop
			// would retry without ever exhausting the budget.
			writeUpgradeMarker(upgradeMarkerPath, upgradeMarker{
				TargetSHA:   targetSHA,
				CurrentSHA:  gitShort,
				RequestedAt: time.Now().UTC(),
				Attempts:    attemptCount + 1,
			}, logger)

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
					mode := ""
					if ac, ok := cfg.Agents[name]; (ok && ac.OnDemand) || onDemandFromPack[name] {
						mode = "on_demand"
					}
					agents = append(agents, hub.NewAgentSummary(name, string(proc.State), mode,
						agentActivityFor(agentMgr, name, proc)))
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
				recordUpgradeError(upgradeMarkerPath, err, logger)
				needsRestart = true
			}
			if needsRestart {
				if err := hub.RolloutRestartSelf(logger); err != nil {
					// This is the wedge. os.Exit here restarts the pod onto the
					// SAME image, so the upgrade silently never lands. It is an
					// ERROR, not a Warn, and the cause (typically a 403 because
					// the spoke lacks patch on its own Deployment) must be both
					// persisted for the next attempt and reported to the hub so
					// the UI stops showing a permanent "Upgrading".
					logger.Error("self-upgrade FAILED: could not patch own Deployment, restarting onto the same image",
						"target", targetSHA,
						"current", gitShort,
						"error", err,
						"hint", "grant get/patch on deployments/hive in this namespace (hive-self-upgrade Role/RoleBinding)",
					)
					recordUpgradeError(upgradeMarkerPath, err, logger)
					hub.ReportUpgradeFailure(hubURL, cfg.HiveID, targetSHA, gitShort, err.Error(), logger)
					// Exit NON-ZERO. Exiting 0 on a failed upgrade told Kubernetes
					// the process had completed successfully, so the restart looked
					// routine and nothing — not the pod's exit code, not an event,
					// not a probe — recorded that an upgrade had just failed. A
					// non-zero code makes the failure visible in the pod's
					// lastState.terminated and in `kubectl describe`.
					os.Exit(selfUpgradeFailureExitCode)
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

			// WRITE-PATH GUARD. Compute the identity this push WOULD produce and
			// refuse the whole delivery if it is internally inconsistent.
			//
			// This is the guard the 2026-07-31 incident needed. That push carried
			// the GHE app_id and slug with no api_url; the adoption below applies
			// each field independently under an "empty means unchanged" contract,
			// so seven public-GitHub hives took the GHE App ID, kept api_url: "",
			// and every token request 404'd. Refusing the whole delivery leaves
			// those hives on their previous, working identity instead of a half of
			// two identities.
			//
			// Rejection is loud and repeats on every beat: the hub keeps pushing
			// until the spoke reports back, so a silent skip would be an invisible
			// permanent stall. There is no auto-repair here — the fix is on the
			// hub, in clusters.json.
			if prospective := prospectiveGitHubIdentity(cfg.GitHub, ghCfg); prospective != nil {
				if err := config.RejectIdentitySet(*prospective); err != nil {
					logger.Error("REFUSING hub github app config: the pushed identity set is inconsistent and would half-apply — nothing was changed",
						"error", err,
						"pushed_app_id", ghCfg.AppID,
						"pushed_app_slug", ghCfg.AppSlug,
						"current_app_id", cfg.GitHub.AppID,
						"current_api_url", cfg.GitHub.APIURL,
						"current_base_url", cfg.GitHub.BaseURL,
						"remedy", "correct github_app_id/github_app_slug/github_api_url/github_base_url for this cluster on the hub",
					)
					return
				}
			}

			// Write the delivered key to the path that NAMES the App it belongs
			// to, not to a generic filename.
			//
			// /data/gh-app-key.pem carries no evidence of which App signed it, so
			// a key delivered for one App silently becomes "the key" for whatever
			// app_id the config later claims. On 2026-07-31 that is exactly what
			// happened: all 33 vllm-d spokes had key_file pinned to the generic
			// path holding the GHE key, so correcting app_id to the public App
			// still produced 404 Integration not found — the right key was
			// already on disk at gh-app-key-3568013.pem and unreachable, because
			// an explicit key_file short-circuits resolveAppKeyFile before the
			// per-app-id lookup runs.
			//
			// Deriving the filename from the app_id makes that mismatch
			// unrepresentable: a key can only be found under the App it was
			// delivered for. Falls back to the generic path when the delivery
			// names no App, so a key is never dropped on the floor.
			keyPath := deliveredKeyPath(ghCfg.AppID)
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

			// Persist the fleet's ADDITIONAL App keys — every OTHER App's key,
			// keyed by app_id — so this spoke can sign for the App it is actually
			// configured as even when that is NOT its cluster's default. This is
			// the both-keys fix: a github.com hive on a GHE cluster now receives
			// and stores the github.com key here, and resolveAppKeyFile selects it
			// by matching cfg.GitHub.AppID.
			//
			// Written to distinct /data/gh-app-key-<appid>.pem files, so they never
			// collide with the primary /data/gh-app-key.pem above. Writing one that
			// matches our OWN app_id must take effect immediately: flip keyChanged
			// so the client is rebuilt below, exactly as a primary-key change does.
			for _, ak := range ghCfg.AdditionalKeys {
				if ak.PrivateKey == "" || ak.AppID <= 0 {
					continue
				}
				perAppPath := perAppIDKeyPath(ak.AppID)
				beforeFP, _ := config.AppKeyFingerprintFromFile(perAppPath)
				fp, err := writePerAppIDKey(ak.AppID, ak.PrivateKey)
				if err != nil {
					logger.Error("failed to write additional github app key from heartbeat",
						"app_id", ak.AppID, "error", err)
					continue
				}
				changed := fp != "" && fp != beforeFP
				logger.Info("additional github app private key written via heartbeat",
					"app_id", ak.AppID,
					"path", perAppPath,
					"from_fingerprint", beforeFP,
					"to_fingerprint", fp,
					"key_changed", changed,
				)
				// If this additional key is for the App we ourselves authenticate
				// as, it is now the key resolveAppKeyFile will pick — treat it like
				// a primary-key rotation so the client rebuild below uses it.
				if changed && ak.AppID == cfg.GitHub.AppID {
					keyChanged = true
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
			if next, cleared := nextInstallationID(cfg.GitHub.InstallationID, ghCfg); cleared {
				// The banner and the hub must flip to not-installed NOW, not
				// when the cached token dies an hour from now. Same config-truth
				// rule as startup.
				dashSrv.SetGitHubAppRequired(true)
				dashSrv.SetGitHubAppState(github.AppStateNotInstalled.String())
				logger.Info("clearing github app installation_id on operator request",
					"was", cfg.GitHub.InstallationID)
				cfg.GitHub.InstallationID = next
			} else {
				cfg.GitHub.InstallationID = next
			}
			// Deliberately NOT `cfg.GitHub.KeyFile = keyPath`.
			//
			// key_file is DERIVABLE from app_id (resolveAppKeyFile prefers
			// /data/gh-app-key-<app_id>.pem, the only key correct by
			// construction). Persisting the path turns a derived value into a
			// stored one that outlives the App it was derived for: once written,
			// it short-circuits resolveAppKeyFile on every later boot, so a
			// corrected app_id keeps signing with the previous App's key. That is
			// what left all 33 vllm-d spokes pinned to the GHE key.
			//
			// Leaving it empty lets derivation run every time, so the key always
			// tracks the App actually in effect. An operator-set key_file still
			// wins — that override is intentional, for a hive whose App this
			// build does not know (e.g. a hive on a third App ID with a key at a
			// bespoke path).
			// Same "empty means unchanged" contract as installation_id: adopting
			// an empty slug would blank a working install link.
			if ghCfg.AppSlug != "" && cfg.GitHub.AppSlug != ghCfg.AppSlug {
				logger.Info("adopting github app slug from hub",
					"was", cfg.GitHub.AppSlug, "now", ghCfg.AppSlug)
				cfg.GitHub.AppSlug = ghCfg.AppSlug
			}
			// Adopt the forge URLs from the SAME delivery as the App above.
			// prospectiveGitHubIdentity already validated all four fields
			// together, so reaching here means the complete set is coherent —
			// applying the App without its URLs would undo that check by leaving
			// the spoke pointed at the previous forge.
			//
			// Same "empty means unchanged" contract: empty is the correct steady
			// state for a public hive, so it can never be read as "blank this".
			if ghCfg.APIURL != "" && cfg.GitHub.APIURL != ghCfg.APIURL {
				logger.Info("adopting github api url from hub",
					"was", cfg.GitHub.APIURL, "now", ghCfg.APIURL)
				cfg.GitHub.APIURL = ghCfg.APIURL
			}
			if ghCfg.BaseURL != "" && cfg.GitHub.BaseURL != ghCfg.BaseURL {
				logger.Info("adopting github base url from hub",
					"was", cfg.GitHub.BaseURL, "now", ghCfg.BaseURL)
				cfg.GitHub.BaseURL = ghCfg.BaseURL
			}

			// Persist the adopted App IDENTITY to the PVC overlay, exactly as the
			// claimed-project-config callback persists what it adopts.
			//
			// Without this the adoption lived only in memory. The key files are
			// written to /data (durable), but app_id/app_slug/installation_id were
			// not, so on every pod restart the entrypoint re-merged the ConfigMap
			// seed and the spoke reverted to whatever App it was provisioned with —
			// silently undoing a completed repair and making the hub's push look
			// like it had never happened. That is how a GHE hive kept re-appearing
			// with the github.com app_id and an empty slug across restarts.
			//
			// Saved even when the App is not yet usable (installation_id still 0):
			// the corrected app_id and slug are precisely what the owner needs on
			// disk so the dashboard renders a working install link BEFORE they have
			// installed anything.
			if err := cfg.Save(); err != nil {
				logger.Error("failed to persist github app config from heartbeat", "error", err)
			}

			// Resolve the key file the same way startup does, so a hive whose
			// only correct key arrived as an ADDITIONAL per-app-id key (no
			// primary key_file configured — the exact vllm-d state) still finds
			// it: resolveAppKeyFile prefers /data/gh-app-key-<appid>.pem for the
			// app_id we now claim. An explicit key_file still wins outright.
			rebuildKeyFile := resolveAppKeyFile(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID)
			if cfg.GitHub.HasUsableApp() && rebuildKeyFile != "" {
				newAppAuth, err := github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, rebuildKeyFile, logger, cfg.GitHub.ResolvedAPIURL())
				if err != nil {
					logger.Error("github app auth init via heartbeat failed", "error", err)
					return
				}
				// Deliberately NOT persisted. rebuildKeyFile is the RESOLVED
				// path, and writing a resolved value back into config is what
				// converts a derivation into a pin: the next boot reads it as an
				// explicit key_file, short-circuits resolveAppKeyFile, and keeps
				// using this App's key even after app_id changes. Re-resolving on
				// every use costs a stat and cannot go stale.
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
			if pc == nil {
				return
			}
			// A URL-only push (org empty, dashboard_url set) delivers the vanity
			// dashboard URL to an already-claimed hive whose meta project is stale/
			// empty on the hub — we must still adopt+report it, or the hub keeps
			// showing the raw placeholder host forever. Handle it BEFORE the
			// org-empty bail below, which exists so an empty project never blanks a
			// working config: with no org there is nothing to reconcile except the
			// URL, so adopt it, persist, and return without touching the project.
			if pc.Org == "" {
				if pc.DashboardURL != "" && cfg.Hub.DashboardURL != pc.DashboardURL {
					logger.Info("adopting vanity dashboard URL from hub heartbeat (url-only push)",
						"was", cfg.Hub.DashboardURL, "now", pc.DashboardURL)
					cfg.Hub.DashboardURL = pc.DashboardURL
					if err := cfg.Save(); err != nil {
						logger.Error("failed to save adopted vanity dashboard URL", "error", err)
					}
				}
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
				// WRITE-PATH GUARD, mirroring the App-config callback: an api_url
				// that names a different forge than our app_id is the same
				// half-applied identity arriving from the other direction. Skip
				// only this field — the org/repos/ACMM adoption around it is
				// unrelated and must still land.
				prospective := cfg.GitHub
				prospective.APIURL = pc.GitHubAPIURL
				if err := config.RejectIdentitySet(prospective); err != nil {
					logger.Error("REFUSING hub GitHub API URL: it does not match this hive's app_id and would half-apply an identity — api_url left unchanged",
						"error", err,
						"pushed_api_url", pc.GitHubAPIURL,
						"current_api_url", cfg.GitHub.APIURL,
						"current_app_id", cfg.GitHub.AppID,
						"remedy", "correct github_api_url/github_app_id for this cluster on the hub",
					)
				} else {
					logger.Info("adopting GitHub API URL from hub heartbeat",
						"was", cfg.GitHub.APIURL, "now", pc.GitHubAPIURL)
					cfg.GitHub.APIURL = pc.GitHubAPIURL
				}
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
	// Clear any legacy "not configured" banner alert persisted by an older
	// build. The half-configured state is shown inline in Governor Config →
	// General, not in the top banner.
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

	// NOTE: dashSrv.MarkReady() was previously HERE, after the staggered agent
	// launch and the heartbeat/trajectory/ticker setup. It has been moved to
	// immediately after the HTTP listener starts (before the agent-launch loop),
	// so the pod becomes Ready in seconds instead of minutes. See the MarkReady
	// call and comment above the agent-launch goroutine.

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

// githubRateLimitErrText is the substring GitHub's client surfaces on a rate or
// abuse limit. Matching text is acceptable ONLY here: a rate limit is a reason
// to skip classification entirely, never a reason to accuse anyone of anything,
// so a false negative costs one extra (correct) classification round-trip.
const githubRateLimitErrText = "rate limit"

// isGitHubRateLimitText reports whether an error looks like a GitHub rate limit.
func isGitHubRateLimitText(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), githubRateLimitErrText)
}

// githubAppBannerAttempts is how many times classifyGitHubAppFailure probes
// before accepting an unclassifiable (AppStateUnknown) verdict. A cold start
// races the cluster's DNS/egress-proxy readiness, so the FIRST App call a pod
// makes is the one most likely to fail for reasons that have nothing to do
// with the App. One retry converts that transient into a correct verdict.
const githubAppBannerAttempts = 2

// githubAppBannerRetryDelay spaces those attempts. Short enough not to stall
// boot, long enough for an egress proxy or DNS cache to come up.
const githubAppBannerRetryDelay = 3 * time.Second

// classifyGitHubAppFailure is the SINGLE decision point for "should the GitHub
// App banner be raised?" — used by the boot path, the advisory-digest path and
// the manual Re-check button alike, so those three can never again disagree
// about the same hive.
//
// It exists because they DID disagree. The boot path used to raise the banner
// on a substring match for "403"/"401" in a formatted error string (the exact
// pattern #2224 replaced), set githubAppRequired=true UNCONDITIONALLY, and
// then classify — never lowering the flag again when classification came back
// healthy or inconclusive. Re-check ran the very same diagnoseGitHubApp probe
// but treated an empty diagnosis as success and cleared the banner. Same
// evidence, opposite conclusion: the banner appeared on every cold start whose
// first advisory-issue call blipped, and vanished the moment the user clicked
// Re-check without anything having been fixed.
//
// Two rules make the verdict trustworthy:
//
//  1. Only a state that is genuinely actionable raises the banner. AppStateOK
//     obviously does not, and neither does AppStateUnknown — #2224 defines it
//     as "we could not reach a conclusion", and escalating on it is precisely
//     the false accusation that design was meant to prevent.
//  2. An unknown verdict is retried before it is accepted, so a transient
//     startup network failure is not mistaken for a definitive one.
//
// Returns raise=false with an empty message when the App is fine or when we
// simply cannot tell.
func classifyGitHubAppFailure(ctx context.Context, appAuth *github.AppAuth, expectedOwner string, logger *slog.Logger) (raise bool, msg string, state github.AppAuthState) {
	for attempt := 1; attempt <= githubAppBannerAttempts; attempt++ {
		msg, state = diagnoseGitHubApp(ctx, appAuth, expectedOwner)
		if state != github.AppStateUnknown {
			break
		}
		if attempt < githubAppBannerAttempts {
			logger.Debug("github app classification inconclusive — retrying before accepting a verdict",
				"attempt", attempt, "owner", expectedOwner)
			select {
			case <-ctx.Done():
				return false, "", github.AppStateUnknown
			case <-time.After(githubAppBannerRetryDelay):
			}
		}
	}

	// AppStateUnknown must never raise the banner: we did not get an answer
	// from GitHub, and a hive whose App is perfectly healthy would otherwise
	// be told to reinstall it because of a momentary network fault. The
	// self-heal loop and the next eval cycle both re-probe, so deferring the
	// verdict costs nothing but a delay on a hive that IS genuinely broken.
	if state == github.AppStateUnknown {
		logger.Warn("github app state could not be determined — leaving the banner down rather than guessing",
			"owner", expectedOwner)
		return false, "", github.AppStateUnknown
	}
	if state == github.AppStateOK {
		return false, "", github.AppStateOK
	}
	return true, msg, state
}

// classifyGitHubAppWriteForbidden (#2353) is the verdict for a REAL write that
// returned 403 "Resource not accessible by integration". Authentication-only
// health checks (classifyGitHubAppFailure / diagnoseGitHubApp) inspect the
// installation's granted PERMISSIONS but never whether the target repo is in
// the installation's `selected` repositories — so they return AppStateOK for a
// repo the App cannot write, and the write failure stayed invisible to health
// (githubAppState=None) or, worse, got hard-overridden into a false "lacks
// Issues: Read & Write" banner.
//
// The rule here keeps attribution honest:
//
//   - If diagnoseGitHubApp finds a genuine, classifiable App-auth problem
//     (wrong installation, missing key, insufficient PERMISSION, etc.), report
//     THAT — it is the real cause and its copy is already accurate.
//   - If diagnoseGitHubApp reports the installation is healthy (AppStateOK:
//     right owner, issues:write granted) OR could not reach a verdict
//     (AppStateUnknown), the write 403 is still real and must NOT be silently
//     healthy. Report AppStateWriteForbidden with copy that names the repo and
//     the likeliest cause (repo not in the installation's selected repos),
//     WITHOUT inventing a permission gap that the diagnosis just proved absent.
//
// It always raises: a write that returned 403 is a genuine, standing failure to
// surface, distinct from the transient/unknown probe failures
// classifyGitHubAppFailure guards against.
func classifyGitHubAppWriteForbidden(ctx context.Context, appAuth *github.AppAuth, expectedOwner, repo string) (msg string, state github.AppAuthState) {
	diagMsg, diagState := diagnoseGitHubApp(ctx, appAuth, expectedOwner)
	if diagState != github.AppStateOK && diagState != github.AppStateUnknown {
		// A real, classifiable App-auth problem — its message is the accurate
		// one (e.g. wrong-installation, key-missing, or a genuine permission
		// gap). Use it as-is rather than masking it with the repo-scope story.
		return diagMsg, diagState
	}
	// Installation authenticates and (per the diagnosis) holds issues:write, yet
	// the write was forbidden. Attribute it to the one thing the permission
	// check cannot see — repo scope — and name the repo so the fix is concrete.
	d := github.AppAuthDiagnosis{
		State:           github.AppStateWriteForbidden,
		ExpectedAccount: expectedOwner,
		Repo:            repo,
	}
	return d.Message(), github.AppStateWriteForbidden
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
						postedViaFallback := false
						if uc := userGHClient.Load(); uc != nil {
							if uerr := uc.PostAdvisoryDigest(ctx, primaryRepo, issueNum, md); uerr == nil {
								logger.Info("posted advisory digest", "repo", primaryRepo, "issue", issueNum, "findings", digest.TotalCount, "via", "user-fallback")
								postedViaFallback = true
							}
						}
						// Record the outcome for the heartbeat's advisory-staleness
						// signal. A user-fallback success still counts as a fresh
						// digest post (the issue DID get updated); otherwise record the
						// App error so the hub can flag the digest as stale with its
						// specific cause. err.Error() is the same string logged just
						// below — log-safe, never key material.
						if postedViaFallback {
							dashSrv.RecordAdvisoryPost(digest.TotalCount)
						} else {
							dashSrv.RecordAdvisoryError(err.Error())
						}
						logger.Warn("failed to post advisory digest via app", "repo", primaryRepo, "issue", issueNum, "error", err)
						if strings.Contains(err.Error(), "403") && strings.Contains(err.Error(), "Resource not accessible by integration") {
							// App is installed (we found the issue) but a real
							// WRITE was forbidden. #2353: attribute this honestly.
							// diagnoseGitHubApp only inspects installation-level
							// PERMISSIONS, so when it comes back healthy (issues:write
							// granted, right owner) the previous code hard-overrode
							// that "OK" into a false "lacks Issues: Read & Write"
							// banner — the exact misattribution #2353 reports. When
							// the diagnosis is genuinely a permission/installation
							// problem, use it; otherwise surface a DISTINCT
							// write-forbidden state naming the likeliest real cause
							// (the repo is not in the App installation's selected
							// repos), instead of leaving health at None or faking a
							// permission gap the diagnosis just disproved.
							msg, state := classifyGitHubAppWriteForbidden(ctx, ghClient.AppAuth(), cfg.Project.Org, primaryRepo)
							dashSrv.SetGitHubAppRequired(true)
							dashSrv.SetGitHubAppPermIssue(msg)
							dashSrv.SetGitHubAppState(state.String())
							logger.Warn("GitHub App write failed — cannot write issue comments",
								"repo", primaryRepo, "state", state.String(),
								"operator_actionable", state.OperatorActionable(), "detail", msg)
						} else if isGitHubRateLimitText(err) {
							logger.Warn("GitHub API rate limit hit, skipping advisory digest post", "repo", primaryRepo)
						} else {
							// Same verdict function as boot and Re-check, so a
							// healthy or unclassifiable probe cannot raise the
							// banner here either.
							raise, msg, state := classifyGitHubAppFailure(ctx, ghClient.AppAuth(), cfg.Project.Org, logger)
							if raise {
								dashSrv.SetGitHubAppRequired(true)
								if msg != "" {
									dashSrv.SetGitHubAppPermIssue(msg)
								}
								dashSrv.SetGitHubAppState(state.String())
								logger.Warn("GitHub App authentication failed posting advisory digest",
									"repo", primaryRepo, "state", state.String(),
									"operator_actionable", state.OperatorActionable())
							}
						}
					} else {
						logger.Info("posted advisory digest", "repo", primaryRepo, "issue", issueNum, "findings", digest.TotalCount, "via", "app")
						// Record the fresh, successful digest post so the hub's
						// advisory-staleness gate stays satisfied for this hive.
						dashSrv.RecordAdvisoryPost(digest.TotalCount)
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
const (
	// selfUpgradeMaxAttempts bounds how many times a spoke retries an upgrade
	// that keeps leaving the image unchanged. Bounded rather than unlimited so a
	// genuinely broken hive (e.g. missing RBAC) stops thrashing its pod, and
	// bounded rather than "never again" so a transient failure still converges.
	selfUpgradeMaxAttempts = 5
	// selfUpgradeBaseBackoff is the delay before retry #2; it doubles per
	// attempt up to selfUpgradeMaxBackoff.
	selfUpgradeBaseBackoff = 2 * time.Minute
	// selfUpgradeMaxBackoff caps the exponential backoff between retries.
	selfUpgradeMaxBackoff = 30 * time.Minute
	// selfUpgradeFailureExitCode marks a process exit caused by a FAILED
	// self-upgrade. Distinct from 0 so the failure is visible in the container's
	// termination state instead of looking like a clean shutdown.
	selfUpgradeFailureExitCode = 17
)

// upgradeMarker is the on-PVC record at /data/upgrade-requested. It survives
// pod restarts (that is the whole point: the process exits as part of an
// upgrade), so it is the only place attempt bookkeeping can live.
type upgradeMarker struct {
	TargetSHA   string    `json:"target_sha"`
	CurrentSHA  string    `json:"current_sha"`
	RequestedAt time.Time `json:"requested_at"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error,omitempty"`
}

// parseUpgradeMarker decodes a marker, tolerating the legacy format that had no
// attempts/last_error fields. A legacy marker counts as one prior attempt so an
// already-wedged hive gets retries under the new budget instead of being
// treated as fresh.
func parseUpgradeMarker(data []byte) upgradeMarker {
	var m upgradeMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return upgradeMarker{}
	}
	if m.Attempts < 1 {
		m.Attempts = 1
	}
	return m
}

// sameUpgradeTarget reports whether two target SHAs refer to the same commit,
// tolerating short/full SHA length mismatch the way the hub's sameCommit does.
// A DIFFERENT target must reset the attempt budget, so this comparison is what
// keeps the latch from outliving the upgrade it was created for.
func sameUpgradeTarget(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	return strings.EqualFold(a[:n], b[:n])
}

func writeUpgradeMarker(path string, m upgradeMarker, logger *slog.Logger) {
	data, err := json.Marshal(m)
	if err != nil {
		logger.Warn("failed to encode upgrade marker", "error", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		logger.Warn("failed to write upgrade marker", "path", path, "error", err)
	}
}

// recordUpgradeError annotates the existing marker with the cause of the failed
// attempt so the NEXT boot can log why the previous one did not land — without
// it the reason dies with the process and the failure is invisible.
func recordUpgradeError(path string, upgradeErr error, logger *slog.Logger) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	m := parseUpgradeMarker(data)
	m.LastError = upgradeErr.Error()
	writeUpgradeMarker(path, m, logger)
}

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

// mergeableJSONUnknown is the explicit wire value for "mergeability was never
// determined". It is spelled out rather than left as "" so a consumer reading
// merge-eligible.json cannot mistake an unpopulated field for a definitive
// "no" — the failure mode that made every PR read as unmergeable.
const mergeableJSONUnknown = "unknown"

// mergeableJSON renders a tri-state mergeability verdict for the
// merge-eligible.json marker, mapping the unknown zero value to an explicit
// "unknown" rather than an empty string.
func mergeableJSON(m github.Mergeable) string {
	if m == github.MergeableUnknown {
		return mergeableJSONUnknown
	}
	return string(m)
}

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
		Number int      `json:"number"`
		Repo   string   `json:"repo"`
		Title  string   `json:"title"`
		Author string   `json:"author"`
		Labels []string `json:"labels,omitempty"`
		// Mergeable is a tri-state string ("yes"/"no"/"unknown"), not a bool.
		// A bool here defaulted to false for every PR, because the value was
		// read from a list endpoint that never returns it.
		Mergeable string `json:"mergeable"`
		DCO       string `json:"dco"`
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
			Mergeable: mergeableJSON(pr.Mergeable),
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
