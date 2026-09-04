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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

	"gopkg.in/natefinch/lumberjack.v2"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/discord"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/mint"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/proclock"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/proxy"
	"github.com/hivecommons/hive/pkg/pushbroker"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/tracing"
	"github.com/hivecommons/hive/pkg/trajectory"
	"github.com/hivecommons/hive/pkg/watchdog"
	"github.com/hivecommons/hive/pkg/watsonx"
	"github.com/hivecommons/hive/pkg/worksource"
	"go.opentelemetry.io/otel/attribute"
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
func agentActivityFor(mgr *agent.Manager, cfg *config.Config, govState governor.State, currentMode, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) hub.AgentActivity {
	act := hub.AgentActivity{
		Paused: proc.Paused,
		// Pause provenance (#4041): ride WHO/WHY/WHEN to the hub so the
		// fleet view can tell a deliberate owner quiesce from a malfunction.
		PausedTrigger:  proc.PausedTrigger,
		PausedReason:   proc.PausedReason,
		PausedBy:       proc.PausedBy,
		PausedAt:       proc.PausedAt,
		NeedsLogin:     proc.NeedsLogin,
		QuotaExhausted: proc.QuotaExhausted,
		LastActivityAt: proc.LastPaneChange,
		// A missing tmux session is only meaningful for an agent the manager
		// believes is running; SessionMissing enforces that itself.
		SessionMissing: mgr.SessionMissing(name),
	}
	if proc.StartedAt != nil {
		act.StartedAt = *proc.StartedAt
	}
	act.KickInterval = heartbeatKickInterval(govState, name, proc, onDemandFromPack)

	// EXPECTED leg: does the governor's current mode schedule this agent on a
	// kicking cadence right now? Shared with the dashboard's offByCadence via
	// config.ExpectedActive so the two never disagree.
	if cfg != nil {
		onDemandAgent := false
		enabled := false
		if ac, ok := cfg.Agents[name]; ok {
			onDemandAgent = ac.OnDemand
			enabled = ac.Enabled
		}
		act.ExpectedActive = cfg.ExpectedActive(name, currentMode, onDemandAgent, onDemandFromPack)
		act.Enabled = enabled
	}

	// ABLE leg: the exact ACMM capability gates the spoke enforces. ok=false
	// (unknown agent) leaves all three false, read hub-side as UNKNOWN.
	if canIssue, canPR, canMerge, ok := mgr.AgentCapabilities(name); ok {
		act.CanOpenIssue = canIssue
		act.CanOpenPR = canPR
		act.CanMerge = canMerge
	}

	// Backend lets the hub interpret NeedsLogin (interactive vs inference).
	if backend, ok := mgr.EffectiveBackend(name); ok {
		act.Backend = backend
	}

	// #5958: an agent the spoke has stopped relaunching must say so, with the
	// reason. Without this the hub sees state=failed and renders the same
	// "restart needed" that sent operators clicking a button which could not
	// fix a login prompt or a rejected key.
	if reason, _, blocked, ok := mgr.StartFailureState(name); ok && blocked {
		act.StartBlockedReason = reason
	}

	return act
}

func heartbeatKickInterval(govState governor.State, name string, proc *agent.AgentProcess, onDemandFromPack map[string]bool) time.Duration {
	if proc == nil || !proc.Config.UsesGovernorKick() || proc.Config.OnDemand || onDemandFromPack[name] {
		return 0
	}
	cadence, ok := govState.Cadences[name]
	if !ok || cadence.Paused || cadence.Interval <= 0 {
		return 0
	}
	return cadence.Interval
}

func quotaExhaustedAgentCount(agents []hub.AgentSummary) int {
	count := 0
	for _, a := range agents {
		if a.QuotaExhausted && !a.Paused &&
			!strings.EqualFold(a.State, "paused") &&
			strings.EqualFold(a.State, "running") {
			count++
		}
	}
	return count
}

func quotaExhaustedProcessCount(statuses map[string]*agent.AgentProcess) int {
	count := 0
	for _, proc := range statuses {
		if proc != nil && proc.QuotaExhausted && !proc.Paused && proc.State == agent.StateRunning {
			count++
		}
	}
	return count
}

func quotaExhaustedAgentReason(count int) string {
	if count <= 0 {
		return ""
	}
	return fmt.Sprintf("%d agent(s) out of provider quota", count)
}

func providerLimitHeartbeatFields(agents []hub.AgentSummary) (reason string, rebuffs int) {
	errMsg, _, _, rebuffs := dashboard.InferenceBudgetExceeded()
	if errMsg != "" {
		if rebuffs > 1 {
			return fmt.Sprintf("provider spending limit reached — %d refused calls: %s", rebuffs, errMsg), rebuffs
		}
		return "provider spending limit reached — " + errMsg, rebuffs
	}
	return quotaExhaustedAgentReason(quotaExhaustedAgentCount(agents)), 0
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
// app_id the config later claims, which is how all 33 heartbeat-only-cluster spokes ended up
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
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename below succeeds
	if err := tmp.Chmod(spokeAppKeyFileMode); err != nil {
		_ = tmp.Close() // best-effort cleanup; the chmod error is what's returned
		return "", fmt.Errorf("chmod temp app key file: %w", err)
	}
	if _, err := tmp.WriteString(trimmed + "\n"); err != nil {
		_ = tmp.Close() // best-effort cleanup; the write error is what's returned
		return "", fmt.Errorf("write temp app key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // best-effort cleanup; the sync error is what's returned
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
		//
		// BOTH generic paths qualify. /data/gh-app-key.pem is what older builds
		// wrote on every key delivery; /secrets/gh-app-key.pem is what the
		// PROVISIONING TEMPLATE hardcodes for every App-using hive
		// (saas_provision.go). Neither names the App it holds, and neither was
		// typed by an operator. Until /secrets was included here, a provisioned
		// hive could never change forges: the hub could correct app_id all it
		// liked and the spoke kept signing with the provisioned key, which on
		// the spoke-cluster pool was a placeholder matching NEITHER real App.
		if v == spokeAppKeyPath || v == spokeProvisionedAppKeyPath {
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
	appKeyFile := resolveAppKeyFile(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID)

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

// processStartedAt is when this hive process began. Reported over the heartbeat
// so the hub can show an uptime pill — a hive that is 1/1 Running but restarting
// every couple of minutes looks healthy in a pod listing and in My Hives, and a
// short uptime that keeps resetting is the only visible tell.
var processStartedAt = time.Now()

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

func main() {
	// --version fast path, before any flag parsing or startup work: the CI
	// smoke test (and operators) probe the binary with `hive --version`; the
	// standard flag set would reject it ("flag provided but not defined").
	// dd's full CLI dispatcher handles this via a version subcommand; this is
	// the minimal equivalent for the v4 line.
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Printf("hive %s (commit %s, branch %s)\n", version, gitShort, gitBranch)
		return
	}
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
	// Channel-delivered spokes ("stable" retag of a v4 build) label their
	// version badge with the channel; "" outside a cluster or on branch/SHA
	// tags, in which case the badge stays branch-only.
	dashboard.SetReleaseChannel(hub.SelfImageReleaseChannel())

	logger := slog.New(logscrub.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.SetDefault(logger)

	// Process singleton: refuse to become a second hive process in this
	// container (#2453, #2496). Two concurrent processes beat as the same pod,
	// alternate registry state every beat, and are invisible to both the
	// in-process StartHeartbeat guard and the hub's duplicate-spoke detector.
	// The flock releases on process death, so this never blocks a restart.
	if os.Getenv(singletonLockEnv) != singletonLockDisable {
		lockPath := singletonLockPath()
		procLock, lockErr := proclock.Acquire(lockPath)
		if lockErr != nil {
			logger.Error("another hive process is already running in this container — refusing to start a duplicate (#2453, #2496)",
				"lock", lockPath,
				"pid", os.Getpid(),
				"error", lockErr.Error(),
			)
			os.Exit(duplicateProcessExitCode)
		}
		// Held for the process lifetime; the kernel releases it on exit. Kept
		// referenced so the *os.File is never garbage-collected (a collected
		// file closes its descriptor, which would silently drop the flock).
		defer procLock.Release()
	}

	// Clear stale upgrade marker if the current SHA differs from the marker's
	// current_sha — this means the upgrade succeeded and the marker is from a
	// previous version.
	const upgradeMarkerStartupPath = "/data/upgrade-requested"
	if markerData, err := os.ReadFile(upgradeMarkerStartupPath); err == nil {
		m := parseUpgradeMarker(markerData)
		if m.CurrentSHA != gitShort {
			// We booted on a different SHA than the one that requested the
			// upgrade: it landed. Drop the marker so the attempt budget resets.
			if err := os.Remove(upgradeMarkerStartupPath); err != nil && !os.IsNotExist(err) {
				logger.Warn("failed to clear stale upgrade marker", "path", upgradeMarkerStartupPath, "error", err)
			}
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
		runHub(logger, *configPath)
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
	_ = os.Setenv("HIVE_ID", cfg.HiveID) // valid key/value; Setenv cannot fail on Unix

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

	// HIVE_CONFIG names a DIFFERENT file from the one we actually loaded.
	//
	// This is only ever the entrypoint's read-only escape hatch failing to
	// land. When the config path cannot be written, entrypoint.sh exports
	// HIVE_CONFIG=/data/hive.yaml.runtime and logs "config path is read-only —
	// using ... directly"; HIVE_CONFIG is read above only as the DEFAULT of
	// -config, and the image's CMD passes that flag explicitly
	// (Dockerfile: CMD ["--config", "/etc/hive/hive.yaml"]), so the explicit
	// value won and the redirect did nothing.
	//
	// That is #4973, and it is silently destructive rather than merely wrong:
	// the stale file loads, then Config.Save() writes the whole in-memory
	// config back over /data/hive.yaml.runtime, destroying the state the
	// operator had persisted there. An ACMM level set from the dashboard came
	// back at its provisioned value after a restart, twice, with /data intact.
	//
	// entrypoint.sh now appends `--config "$HIVE_CONFIG"` to the argv so the
	// last-occurrence-wins rule in flag.Parse carries the redirect, which means
	// this branch should be unreachable. It is kept — at WARN, naming both
	// paths — because the failure it reports is invisible from every other
	// vantage point: /api/config/provenance reads HIVE_CONFIG directly, so it
	// reports the file the entrypoint chose while the process runs on the one
	// it did not, and the two disagree with no way to tell from the outside.
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" && envCfg != *configPath {
		logger.Warn("config path disagreement: HIVE_CONFIG names a different file than the one loaded — an explicit -config (the image CMD) outranked the entrypoint's redirect; persisted state in HIVE_CONFIG may be overwritten by the next save",
			"hive_config_env", envCfg,
			"loaded_config", *configPath,
		)
	}

	logger.Info("hive starting",
		"org", cfg.Project.Org,
		"repos", cfg.Project.Repos,
		"agents", len(cfg.Agents),
		"hive_id", cfg.HiveID,
	)
	startupRepoTargetIssue := config.ValidateRepoTargets(cfg)
	if startupRepoTargetIssue != nil {
		logger.Warn("repo target misconfigured — owner action required",
			"issue", startupRepoTargetIssue.Message,
			"hive_id", cfg.HiveID,
			"org", cfg.Project.Org,
			"repos", cfg.Project.Repos,
			"primary_repo", cfg.Project.PrimaryRepo,
		)
	}
	repoTargetMisconfigured := func() bool {
		return config.ValidateRepoTargets(cfg) != nil
	}
	repoTargetIssueMessage := func() string {
		if issue := config.ValidateRepoTargets(cfg); issue != nil {
			return issue.Message
		}
		return ""
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize OpenTelemetry tracing. Off by default: with no otel block
	// (or otel.enabled=false) this installs a no-op provider with zero export
	// overhead. Never fatal — a tracing setup error must not stop hive.
	otelCfg := cfg.EffectiveOTel()
	traceShutdown, traceErr := tracing.Init(ctx, tracing.Config{
		Enabled:     otelCfg.Enabled,
		Endpoint:    otelCfg.Endpoint,
		Headers:     otelCfg.Headers,
		ServiceName: otelCfg.ServiceNameOrDefault(),
		Insecure:    otelCfg.Insecure,
		SampleRatio: otelCfg.SampleRatio,
		HiveID:      cfg.HiveID,
		Branch:      cfg.Policies.Branch,
		// Reach anchors (#3973): gitShort is the ldflags-baked commit of THIS
		// binary (already canonicalized to 7 chars above), and the image ref is
		// the Deployment-declared image (cached — warmed by the release-channel
		// read at startup; "" outside a cluster). Spans attribute to the code
		// that actually runs, not to the merge/publish event (#3816).
		Commit: gitShort,
		Image:  hub.SelfDeploymentImage(),
	})
	if traceErr != nil {
		logger.Warn("tracing init failed; continuing without tracing", "error", traceErr)
	} else if otelCfg.Enabled {
		logger.Info("otel tracing enabled", "endpoint", otelCfg.Endpoint, "service_name", otelCfg.ServiceNameOrDefault())
	}
	// Component reach counters (#3993): resume this commit's counters from the
	// PVC before any span can start. Independent of the otel block above —
	// counters increment with or without an exporter (design D2 of #3973), so
	// this runs unconditionally and a load failure only costs history, never
	// counting. Counters persisted by a DIFFERENT commit are dropped inside
	// LoadReachState: a new binary starts fresh keys naturally.
	if err := tracing.LoadReachState(reachStatePath, gitShort, logger); err != nil {
		logger.Warn("reach state load failed; starting with fresh counters", "error", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), traceShutdownTimeout)
		defer shutdownCancel()
		if err := traceShutdown(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown error", "error", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	// preShutdownHooks run in the signal handler before the context is canceled,
	// in registration order, while every connection and tmux server is still
	// live. Registrations happen later in startup, once the subsystems they
	// touch exist.
	//
	// This was a single atomic.Pointer[func()] until kubestellar/hive#5390. A
	// lone pointer makes registration DESTRUCTIVE: the second Store silently
	// discards the first hook, and the loss is invisible — nothing fails, a
	// shutdown side effect simply stops happening. That is precisely the trap
	// the WebSocket drain walked into, since the slot was already held by
	// #4296's kick-log archive. A slice makes adding a hook additive by
	// construction, so the next one cannot repeat the mistake.
	var preShutdownHooks shutdownHooks
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		preShutdownHooks.run()
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
		ghClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
	}
	// Unconditional (nil-safe, zero value = no filtering): the issue filter
	// gates which issues become actionable at all, so it must be installed
	// even when no exempt labels are configured.
	ghClient.SetIssueFilter(cfg.Project.IssueFilter)
	// The user write-token client (userGHClient) was removed: every GitHub write
	// — issues, PRs, comments, merges, and the advisory digest — now goes through
	// the hive's App installation token (ghClient / kubestellar-hive[bot]). The
	// user token only ever served as an advisory-digest fallback writer, which is
	// no longer wanted (and forced the excessive "repo" login scope, issue #1927).
	// Dashboard login now requests no scope and no user write-token is persisted.

	gov := governor.New(cfg.Governor, cfg.EnabledAgents(), logger)
	// Default mode thresholds scale with how many repos the hive watches, so
	// the mode ladder means the same thing on a 3-repo hive as on a 39-repo
	// one (#3498). Explicit thresholds are unaffected.
	gov.SetRepoCount(cfg.Project.RepoCount())
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

	// #4298: restore per-budget-window history so past resets survive a restart.
	// A missing or unparseable file is ordinary on a hive upgrading into this
	// feature — it simply starts with no history rather than failing to boot.
	const budgetWindowHistoryPath = "/data/budget-window-history.json"
	var pendingBudgetWindowSeed []dashboard.BudgetWindowEntry
	if budgetData, err := os.ReadFile(budgetWindowHistoryPath); err == nil {
		if err := json.Unmarshal(budgetData, &pendingBudgetWindowSeed); err == nil && len(pendingBudgetWindowSeed) > 0 {
			logger.Info("budget window history loaded", "entries", len(pendingBudgetWindowSeed))
		}
	}

	// #4263: restore convergence soak telemetry so a fixed-commit off/shadow/
	// enforce comparison survives restarts. Missing or unparseable is ordinary
	// on a hive that never ran with the toggle on — start empty, never fail.
	const convergenceSoakHistoryPath = "/data/convergence-soak-history.json"
	var pendingConvergenceSoakSeed []dashboard.ConvergenceSoakEntry
	if soakData, err := os.ReadFile(convergenceSoakHistoryPath); err == nil {
		if err := json.Unmarshal(soakData, &pendingConvergenceSoakSeed); err == nil && len(pendingConvergenceSoakSeed) > 0 {
			logger.Info("convergence soak history loaded", "entries", len(pendingConvergenceSoakSeed))
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

	// Invocation-attribution trail (pkg/github/attribution.go): stamp hive-
	// created PRs/issues with what the hive invoked, and audit every such
	// creation. Wired in stages as dependencies come up: the trailer gate now
	// (cfg exists, and the advisory-issue ensure just below must respect the
	// toggle), the per-agent resolver after the agent manager exists, and the
	// audit sink after the dashboard server exists. cfg is the live pointer
	// (the config watcher swaps contents in place), so the toggle is read
	// fresh per creation — a dashboard flip takes effect immediately.
	if ghClient != nil {
		ghClient.SetAttributionHooks(github.AttributionHooks{
			TrailerEnabled: func() bool { return cfg.Governor.AttributionTrailerEnabled() },
		})
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
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
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
	if err := os.MkdirAll(brainstormPolicyDir, 0o755); err != nil {
		logger.Warn("failed to create brainstorm policy dir", "path", brainstormPolicyDir, "error", err)
	}
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
		Org:             cfg.Project.Org,
		Repos:           cfg.Project.Repos,
		PrimaryRepoName: cfg.Project.PrimaryRepo,
		ACMMLevel:       acmmLevel,
		PRsAllowed:      cfg.Project.PRsAllowed(),
		PolicyDir:       policyDir,
		AppAuthoredPRs:  cfg.GitHub.AppAuthoredPRsEnabled(),
	}
	if cfg.GitHub.IsGHE() {
		projectCtx.GHHost = cfg.GitHub.HostLabel()
	}
	agentMgr := agent.NewManager(cfg.EnabledAgents(), logger, projectCtx)
	// SIGTERM (pod roll, hive upgrade) destroys every tmux server and with it
	// the in-flight kick's scrollback; archive it to /data first (#4296).
	archiveOnShutdown := func() { agentMgr.ArchiveAllKickLogs("shutdown") }
	preShutdownHooks.add("archive-kick-logs", archiveOnShutdown)
	agentMgr.SetSandboxConfig(cfg.AgentSandbox)

	// Say out loud when the sandbox opt-in is configured but inert. The gate is
	// two-part (global agent_sandbox.enabled AND a per-agent sandbox.enabled),
	// and the dashboard's Security tab writes only the global half — so an
	// owner can turn the sandbox on, be told the setting was updated, and still
	// have every agent running unconfined on the operator's own host.
	//
	// That silence is the part of #4918 that is safe to fix here. The gate
	// itself is load-bearing: a sandboxed agent runs a different execution
	// model and startSandboxKickLocked has no tmux fallback, so collapsing it
	// would convert working agents into permanently failing ones. Telling an
	// operator who believes they are covered that they are not costs nothing.
	logAgentSandboxPosture(logger, cfg)
	// Treat any configured gateway name as an inference-routable backend so an
	// agent with backend: <gateway> routes through it. Resolution is live
	// (reads cfg on each call) so gateways added from the Model Gateways tab
	// take effect without a restart.
	//
	// Wired HERE — immediately after the manager is constructed — and not in
	// the proxy/dashboard wiring further down, because SetBackendOverride
	// validates backend names against this predicate. The persisted-state
	// replay (restoreAgentRuntimeState, below) re-applies saved backend
	// overrides long before the dashboard wiring runs, and with the predicate
	// still unset every gateway-named override was rejected there — silently,
	// while the model override beside it restored fine. That is the #3961
	// asymmetric revert: an agent switched to a gateway backend came back on
	// its config backend but with the switched model still applied, producing
	// launch-dead hybrids like `pi --model gpt-5.6-luna`.
	agentMgr.SetGatewayBackendChecker(func(backend string) bool {
		return cfg.Governor.ResolveGateway(backend) != nil &&
			!strings.EqualFold(backend, "") // empty is the default, not a named backend
	})
	// Resolve the bob API key at LAUNCH time, not here: cfg is the live config
	// pointer (the config watcher swaps its contents in place on reload), so a
	// key added via the Secret mount, the PVC file, or a config edit takes
	// effect on the next agent launch with no hive restart. Only the key's
	// LOCATION is ever in cfg; the value is read from file/env on each call and
	// is never logged.
	agentMgr.SetBobAPIKeyResolver(func() string {
		return cfg.Governor.Bob.ResolveAPIKey()
	})
	// Hive-wide default explain mode, resolved per kick/launch off the live cfg
	// pointer for the same reason as the bob key above: an operator debugging a
	// misbehaving fleet turns explanation on from Settings → Governor and needs
	// it on the NEXT kick, not after a restart. Governor config wins over
	// HIVE_EXPLAIN_MODE; the env var stays as the fallback (#4712).
	agentMgr.SetExplainModeDefaultResolver(func() string {
		return cfg.Governor.ResolveExplainModeDefault()
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
		agentMgr.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: appAuth})
	}
	// Start the per-agent token refresh loop UNCONDITIONALLY. It no-ops until
	// App auth is wired, and on hosted spokes that wiring happens AFTER boot
	// (heartbeat delivery / config API reinit / config reload). Gating this on
	// appAuth != nil at boot meant those hives never refreshed per-agent token
	// caches: agent sessions outlived their scoped token, gh 401'd and printed
	// "gh auth login", and the login-detector auto-paused the agent (#4072).
	go agentMgr.StartAgentTokenRefresh(ctx)
	// Start the credential watchdog UNCONDITIONALLY. It self-gates per backend
	// on the presence of an agent using that backend each tick, so it is a
	// no-op on gateway/inference-only hives. On Copilot/Claude hives it turns a
	// missing or expired durable credential — the "stuck at login after an
	// upgrade roll" outage — into an immediate Audit Log signal instead of a
	// silent multi-hour stall.
	go agentMgr.StartCredentialWatchdog(ctx)
	// Keep the Copilot CLI's config.json copilotTokens populated from the
	// durable user token, so agents never sit stuck at "Please use /login"
	// while a valid token exists (CLI 1.0.78 does not re-populate the emptied
	// store from the injected env token on its own). Self-gates on a copilot
	// backend and only writes when the store is empty; never runs a login.
	go agentMgr.StartCopilotSessionRefresh(ctx)
	if ghClient != nil {
		agentMgr.SetSandboxPRClient(ghClient)
	}

	// PR-open-as-the-App-bot: agents push their branch (App-token credential
	// helper) then drop a request file; the hive opens the PR here with the App
	// token so it is authored by "<slug>[bot]", not the Copilot login user.
	// Gated on a real client + usable App — with no App there is no bot to author
	// as, and requests simply accumulate rather than opening under a wrong
	// identity. ghClient uses the App installation token (see ghAuth wiring).
	// Create the agent-facing request queues REGARDLESS of App state. The
	// watchers below stay gated (no App, no bot to author as), but the queues
	// must exist either way or the "requests simply accumulate" behavior above
	// is a fiction: hive-open-pr / hive-open-issue run in the AGENT's shell and
	// hard-fail on a missing directory, discarding the finding instead of
	// queueing it. App setup routinely completes after boot (operator saves the
	// installation ID, /gh-setup persists it, auto-discovery finds it later), so
	// this gap silently disarms agent writes on a hive that looks healthy.
	github.PrepareRequestDirs(logger)

	if ghClient != nil && cfg.GitHub.HasUsableApp() {
		// Attribution resolver: effective backend/model from the manager
		// (runtime overrides included), falling back to the configured values
		// for an agent the manager does not know; tool version resolved
		// lazily per backend and cached. Only launch descriptors flow here —
		// never tokens, keys, or prompt content.
		ghClient.SetAttributionResolver(func(agentName string) github.InvocationMeta {
			backend, model, effort, known := agentMgr.InvocationMetadata(agentName)
			if !known {
				if ac, inCfg := cfg.Agents[agentName]; inCfg {
					backend, model = ac.Backend, ac.Model
					// Same resolver the Manager uses, not a second copy of the
					// rule: a hardcoded default here would drift silently the
					// moment agy's default effort changed.
					effort = agent.ResolveReasoningEffort(backend, model)
				}
			}
			tool, toolVersion := github.ResolveToolVersion(backend)
			return github.InvocationMeta{
				Agent:   agentName,
				Backend: backend,
				// bob self-selects (no catalog): requested model is honestly
				// "auto" — see github.RequestedModel for the known follow-up
				// on discovering bob's internal routing.
				Model:       github.RequestedModel(backend, model),
				Effort:      effort,
				Tool:        tool,
				ToolVersion: toolVersion,
			}
		})
		// authz enforces the SAME per-agent ACMM write-gate + forge-resistance as
		// the direct `gh pr create` path — the request-file route grants no extra
		// privilege. A denied request is quarantined, never opened.
		// holdLabel (F6): at hold-gated ACMM levels (L3/L4/L5) every agent-opened
		// PR must carry the "hold" label so the merge gate holds it for human
		// approval. Outreach content is public speech on the project's behalf, so
		// it remains human-reviewed at L6 too. This is decided server-side from the
		// authenticated agent identity and authoritative hive level
		// (GetACMMLevel), NOT from a client flag — the gh-wrapper.sh tail that used
		// to add the label was dead code after `exec hive-open-pr`. L1/L2 open no
		// agent PRs (manual); non-outreach L6 PRs retain their existing automerge
		// behavior.
		holdLabel := func(agentName string) bool {
			return shouldHoldAgentPR(agentName, agentMgr.GetACMMLevel())
		}
		// #5117: tell the client which accounts are ours, so the
		// self-authorization gate recognises an issue filed under
		// project.ai_author's plain user account as hive-filed rather than
		// mistaking it for a human's. The App bot is recognised without this;
		// hiveIdentity() is the same resolver the duplicate-PR guard uses.
		ghClient.SetHiveIdentity(hiveIdentity(cfg))
		ghClient.StartPRRequestWatcher(ctx, agentMgr.AuthorizePROpen, holdLabel, nil)
		// Issue relay: agents request issue creation and comments by dropping a
		// file (hive-open-issue via the gh wrapper) instead of calling GitHub
		// from their own shell. The agent-side call used to ride the agent's
		// shell tool — one GHE secondary-rate-limit stall or mangled multiline
		// command and the finding was silently lost (root-caused live
		// 2026-08-21: sec-check's creates timed out and survived only as
		// beads). The watcher executes server-side with the App token, retries
		// with backoff, dedupes by exact open-issue title, and enforces the
		// same forge-resistance + CanCreateIssues mode gate the wrapper does.
		ghClient.StartIssueRequestWatcher(ctx, agentMgr.AuthorizeIssueOpen, nil)
		// Review relay: agents request PR reviews by dropping a file (hive-review)
		// instead of running `gh pr review` in their own shell, which the hive
		// never observes. The watcher submits the review with the App token and
		// records it on the audit/activity trail, gated by the same
		// forge-resistance + push-capability (CanPush) check as opening a PR —
		// reviewing is a PR-write, so AuthorizePROpen is the correct gate.
		ghClient.StartReviewRequestWatcher(ctx, agentMgr.AuthorizePROpen, nil)
		// Merge relay: agents request merges by dropping a file (hive-merge)
		// instead of calling the GitHub MCP merge_pull_request tool, whose GraphQL
		// mutation GitHub rejects for App tokens ("Resource not accessible by
		// integration"). The hive merges over REST with the App token, gated by
		// the same forge-resistance + a CanMerge ACMM check.
		// bindMergeAuthz layers the F4 target-binding (CWE-863) on top of the
		// manager's agent/UID/CanMerge check: the merge must name a pinned head
		// SHA (no unpinned "merge whatever HEAD is now") AND the (repo, number)
		// must appear in the governor's current merge-eligible list — so an
		// injected agent cannot land an arbitrary reachable PR of its choosing.
		// Fix #2: on a terminal merge failure caused by a failing REQUIRED check,
		// re-engage the fix loop instead of abandoning the PR. The hook records a
		// re-engagement under the escalation store's per-red-SHA cap (shared with
		// the reaper so a PR is never double-dispatched beyond its budget) and
		// returns whether the cap still allowed a dispatch. The PR is already
		// surfaced into CI_FAILING by writeMergeEligible each eval tick; the hook
		// is the loop-safety authority that decides when to STOP nudging.
		ghClient.SetMergeReEngageHook(mergeReEngageHook(cfg))
		ghClient.StartMergeRequestWatcher(ctx, bindMergeAuthz(agentMgr.AuthorizeMerge), nil)

		// SECURITY (audit F3): re-verify the merger tier inside the sweep. The
		// dashboard's queue endpoint gates on requireMergerOrOwnerRole, but the
		// sweep merges a minute later off the label + App-authored approval body
		// alone, so without this ANY actor who can get the label applied merges
		// anything, and a sockpuppet pair defeats the self-merge ban. Resolved
		// against the SAME allowlist the dashboard uses so there is one notion of
		// trust; read through cfg on every call so a config reload takes effect.
		ghClient.SetMergerAuthorizer(trustedMergerFunc(cfg))

		// commitGreen's required-checks gate (self-merge sweep, see
		// automerge_sweep.go): install the operator-declared
		// auto_merge.required_checks list, if any, so gating does not depend
		// on GitHub's branch-protection API — the Hive App token lacks
		// administration:read, so that API call reliably errors and would
		// otherwise fail closed to the coarser isMetaCheck/isIgnorableCICheck
		// allowlist. Unset/empty leaves the API/allowlist fallback chain
		// intact (SetRequiredChecks(nil) is a safe no-op).
		if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
			ghClient.SetRequiredChecks(set)
		}

		// Self-authored auto-merge: the App merges its OWN open, CI-green PRs
		// directly over the REST API, without a human "Approved ... for Hive
		// auto-merge" queue review and without waiting on tide. Prow forbids
		// self-approval (lgtm+approved must come from someone other than the
		// author), and the author here is always the App itself, so the
		// human-queue path (StartMergeRequestWatcher above / the governor
		// sweep) can never clear for the App's own PRs — this is the only
		// route that lands them. See AutoMergeConfig and
		// SweepSelfAuthoredAutoMerges for the full rationale and the safety
		// properties preserved (green required checks, head-SHA re-verified
		// immediately before merge, squash method, all tiers included).
		// Default ON; `auto_merge.self_authored: false` disables it. ALSO
		// gated on ACMM level (config.SelfMergeMinACMMLevel): l4.md/l5.md
		// both forbid the App merging its own PRs, so an L4/L5 hive must
		// never start this loop regardless of the flag above — see
		// AutoMergeConfig.SelfAuthoredAutoMergeAllowed. StartSelfAuthoredAutoMergeSweep
		// itself no-ops (with a one-time INFO log) when acmmAllowed is false.
		ghClient.StartSelfAuthoredAutoMergeSweep(ctx, cfg.AutoMerge.MaxMerges, cfg.AutoMerge.SelfAuthoredAutoMergeAllowed(cfg.ACMMLevel), cfg.ACMMLevel)
	}

	// Opt-in mint credential: when mint.enabled, build a Minter from the config
	// (signing key + issuer + TTL) and attach it so each per-agent token refresh
	// ALSO issues a scoped short-lived OIDC token alongside the GitHub App token.
	// Default off — an absent/disabled `mint:` block leaves the credential path
	// byte-identical. Fail-safe: a mint setup error is logged, never fatal.
	if cfg.Mint.Enabled {
		if agentMinter, err := buildAgentMinter(cfg, logger); err != nil {
			logger.Warn("mint enabled but minter setup failed; agents keep App token only", "error", err)
		} else {
			agentMgr.SetAgentMint(agentMinter)
			logger.Info("mint credential enabled", "issuer", cfg.Mint.Issuer, "hive_id", cfg.HiveID)
		}
	}

	go agent.StartPermissionsWatcher(logger)

	const statePath = "/data/hive-state.json"
	saved, stateErr := snapshot.LoadState(statePath, logger)
	if stateErr != nil {
		logger.Warn("failed to load persisted state", "error", stateErr)
	} else if saved != nil {
		restoreAgentRuntimeState(saved, cfg, agentMgr, logger)
		// Re-establish the fleet breaker AFTER per-agent pauses are restored
		// above: the agents it held are already back in the paused state (with
		// PausedTrigger == fleet-breaker from their persisted pause), so this
		// only re-attaches the breaker so a later release resumes exactly them.
		// An engaged breaker must REMAIN engaged across restart — it does not
		// auto-release, and it does not re-pause or resume anything here.
		if saved.Breaker != nil && saved.Breaker.Engaged {
			agentMgr.RestoreBreaker(true, saved.Breaker.Paused)
			logger.Info("fleet breaker restored from state", "held", len(saved.Breaker.Paused))
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
					mode.Cadences = make(map[string]config.Cadence)
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
				ghClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
			}
			ghClient.SetIssueFilter(cfg.Project.IssueFilter)
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
	// SIGTERM (pod roll, hive self-upgrade) kills the process and every
	// contributor WebSocket with it, and until #5390 it did so without a word:
	// the peer saw a bare 1006, indistinguishable from a network fault, which is
	// what made #5090 take days to diagnose. Send each contributor a 1012
	// (CloseServiceRestart) first so the relay knows to reconnect immediately —
	// into the replacement pod, which maxSurge=1/maxUnavailable=0 has already
	// brought to readiness before this signal was delivered.
	//
	// Registered as its OWN hook rather than folded into archiveOnShutdown: the
	// two are unrelated, and the drain must not be able to prevent the archive
	// from running. addUrgent, not add, because it is the time-critical half —
	// the sooner the frame is on the wire the sooner the relay reconnects,
	// whereas the kick-log archive does PVC I/O on NFS and nobody is waiting on
	// it. The hub is resolved lazily inside the closure because the contributor
	// hub is not constructed until registerContributeRoutes runs, below.
	preShutdownHooks.addUrgent("drain-contributor-websockets", func() {
		dashSrv.DrainContributorsForShutdown()
	})
	var beadStores map[string]*beads.Store

	// Wire ioscan input enforcement (opt-in via ioscan.enabled) to the dashboard
	// audit log so a blocked/redacted issue title surfaces in the existing
	// audit-trail UI with no new sink. The closure keeps pkg/scheduler decoupled
	// from pkg/dashboard — the scheduler only knows a func(action, detail, agent).
	sched.SetAuditFunc(func(action, detail, agent string) {
		dashSrv.AuditLog(agent, action, detail, agent)
	})
	sched.SetAdvisoryFunc(func(title, detail, agentName string) {
		store := beadStores[agentName]
		if store == nil {
			store = beadStores["scanner"]
		}
		if store == nil {
			store = beadStores["supervisor"]
		}
		if store == nil {
			for _, candidate := range beadStores {
				store = candidate
				break
			}
		}
		if store != nil {
			if b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, agentName, ""); err == nil {
				_ = store.SetMetadata(b.ID, "ioscan_classifier", detail)
			}
		}
	})
	if cfg.Ioscan.IsEnabled() && cfg.Ioscan.Classifier.Enabled {
		endpoint, apiKey, model := cfg.Governor.ResolveReviewer()
		if cfg.Ioscan.Classifier.Model != "" {
			model = cfg.Ioscan.Classifier.Model
		}
		if model == "" {
			model = ioscan.DefaultClassifierModel
		}
		classifier, cerr := ioscan.NewLLMClassifier(ioscan.LLMClassifierConfig{
			Endpoint: endpoint,
			APIKey:   apiKey,
			Model:    model,
		})
		if cerr != nil {
			logger.Warn("ioscan classifier enabled but not running", "reason", cerr.Error())
		} else {
			sched.SetClassifier(ioscan.NewCachedClassifier(classifier, ioscan.DefaultClassifierCacheEntries), ioscan.Thresholds{
				Warn:  cfg.Ioscan.Classifier.WarnThreshold,
				Block: cfg.Ioscan.Classifier.BlockThreshold,
			})
			logger.Info("ioscan semantic classifier enabled", "model", model)
		}
	}
	agentMgr.SetSandboxAuditCallback(func(agentName, action, detail string) {
		dashSrv.AuditLog(agentName, action, detail, agentName)
		if action == "sandbox_broker_rejected" {
			if store, ok := beadStores[agentName]; ok && store != nil {
				if b, err := store.Create("Sandbox push broker rejected changes", beads.TypeAdvisory, beads.PriorityHigh, agentName, ""); err == nil {
					_ = store.SetMetadata(b.ID, "sandbox_broker_rejection", detail)
				}
			}
		}
	})

	// Persist per-user dashboard sessions on the PVC (/data) so direct-route
	// users aren't logged out by pod restarts. NOTE: use /data explicitly, NOT
	// filepath.Dir(configPath) — the config lives at /etc/hive/hive.yaml, which
	// is an ephemeral emptyDir (the ConfigMap seed mount), so a sessions file
	// there is wiped on every pod roll. That was the "re-login on every visit"
	// bug on direct-route spokes. /data is the CephFS PVC (same place cost/fact
	// history persist).
	dashSrv.EnableSessionPersistence("/data/dashboard-sessions.json")

	// Lifecycle timeline journeys persist on the PVC too (#5656): the ring is
	// the panel's only memory of merged/blocked outcomes, so a pod roll must
	// not zero the fleet counters. Enabled before any producer records.
	dashSrv.EnableLifecyclePersistence("/data/lifecycle-timeline.json")

	// The scheduler's classifier pass records KindClassified journeys the
	// moment lane routing decides an issue's lane — same store, no extra work.
	sched.SetLifecycleRecorder(dashSrv.LifecycleTimeline())

	// Attribution audit sink: every hive-mediated PR/issue creation lands in
	// the dashboard audit log (audit.jsonl + ring) UNCONDITIONALLY — the
	// trailer toggle never gates this. Creations before this point (the
	// startup advisory-issue ensure) fall back to the hive log inside
	// recordCreationAudit, so no creation goes unrecorded. The same stream
	// feeds the lifecycle timeline: agent_pr_created → pr_opened and
	// pr_merged → merged (both automerge sweep paths, MergePR from the
	// dashboard queue and the merge watcher), see recordLifecycleFromAudit.
	if ghClient != nil {
		ghClient.SetAttributionAudit(func(action, detail, agent string) {
			dashSrv.AuditLog("system", action, detail, agent)
			recordLifecycleFromAudit(dashSrv, cfg.Project.Org, action, detail, agent)
		})
	}

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

	if len(pendingBudgetWindowSeed) > 0 {
		dashSrv.SeedBudgetWindowHistory(pendingBudgetWindowSeed)
		logger.Info("budget window history restored", "entries", len(pendingBudgetWindowSeed))
	}
	if len(pendingConvergenceSoakSeed) > 0 {
		dashSrv.SeedConvergenceSoak(pendingConvergenceSoakSeed)
		logger.Info("convergence soak history restored", "entries", len(pendingConvergenceSoakSeed))
	}

	if len(pendingTrendSeed) > 0 {
		dashSrv.SeedTrendHistory(pendingTrendSeed)
		logger.Info("trend history restored", "entries", len(pendingTrendSeed))
	}

	beadStores = make(map[string]*beads.Store)
	// Count stores that fail to open. They are dropped from beadStores entirely,
	// which makes an incomplete ledger indistinguishable from a smaller one — and
	// the dependency admission gate must not read a lookup miss in a truncated
	// ledger as "this candidate declared no dependencies".
	beadStoreLoadFailures := 0
	for name, agentCfg := range cfg.EnabledAgents() {
		store, err := beads.NewStore(agentCfg.BeadsDir)
		if err != nil {
			logger.Warn("failed to init beads store", "agent", name, "error", err)
			beadStoreLoadFailures++
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
				beadStoreLoadFailures++
				continue
			}
			store.SetHiveID(cfg.HiveID)
			beadStores[name] = store
			logger.Info("orphan beads store loaded from disk", "agent", name, "count", store.Count())
		}
	}

	if cfg.Retro.Enabled {
		if _, exists := beadStores[retro.Actor]; !exists {
			retroStore, err := beads.NewStore(filepath.Join(beadsRootDir, retro.Actor))
			if err != nil {
				logger.Warn("failed to init retro beads store", "error", err)
				beadStoreLoadFailures++
			} else {
				retroStore.SetHiveID(cfg.HiveID)
				beadStores[retro.Actor] = retroStore
				logger.Info("retro beads store initialized", "count", retroStore.Count())
			}
		}
	}

	initAgentConfigDrivenSystems(cfg)

	tokenCollector := tokens.NewCollector(cfg.Data.MetricsDir, logger)
	tokenCollector.SetClaudeSessionsDir(cfg.Data.ClaudeSessionsDir)
	tokenCollector.SetCopilotSessionsDir(cfg.Data.CopilotSessionsDir)
	tokenCollector.SetBobSessionsDir(cfg.Data.BobSessionsDir)
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

	// Per-repo output-activity collector: reads the local audit log (no GitHub
	// calls) and summarizes issues/PRs/comments/merges/claims/reviews per repo
	// with recency, so the hub can tell — from the heartbeat alone — whether each
	// hive is producing output back to its work source. Persisted to the /data
	// PVC so a restart resumes the last summary; the collector loop reads
	// /data/audit.jsonl every few minutes.
	activityCollector := dashboard.NewActivityCollector(dashSrv.GetAudit(), "", logger)
	activityCollector.EnablePersistence("/data/activity.json")
	go activityCollector.Start(ctx)

	// Per-repo cost collector: joins the same audited output events against
	// the token collector's per-message usage timeline, on the same ticker
	// interval as the activity collector above, and caches the result for
	// /api/repo-cost. Before this (#4943), the interval join — including
	// the same expensive audit read the activity collector does — ran on
	// every 60s dashboard poll, per open browser tab, instead of once per
	// collection interval.
	repoCostCollector := dashboard.NewRepoCostCollector(dashSrv.GetAudit(), tokenCollector, "", logger)
	repoCostCollector.EnablePersistence("/data/repo-cost.json")
	go repoCostCollector.Start(ctx)

	// Persistent hourly metrics behind the Operations + Leaderboard sparklines
	// (queue depth, tasks/hour, fleet size, per-contributor completions). The
	// store loads any prior 7-day history from the /data PVC on first use and the
	// rollup goroutine samples + buckets hourly, so a rolling upgrade resumes the
	// trend instead of flattening it. Bound to ctx so it shuts down cleanly with
	// the rest of the background loops (no goroutine leak). See contribute_metrics.go.
	dashSrv.StartContributeMetrics(ctx)

	var lastActionable atomic.Pointer[github.ActionableResult]
	refreshDashboard := func() {
		// Capture the mutation epoch BEFORE reading any state: if a mutation
		// (e.g. a restart-count or budget-window reset) lands while this
		// snapshot is being built, UpdateStatusIfFresh drops it so the stale
		// values never overwrite what the mutation's own refresh will publish
		// (#4348 — the restart-count flicker).
		buildEpoch := dashSrv.BeginStatusSnapshot()
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
		dashSrv.UpdateStatusIfFresh(payload, buildEpoch)
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
		// The curator block was previously dropped here, so NewPromoter always
		// received a zero CuratorConfig and AutoPromoteThreshold never reached
		// the promoter in production. Passing it through is what makes the
		// threshold gate real for the scheduled sweep (#5430).
		knowledgeAPI = knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{
			Enabled: cfg.Knowledge.Enabled,
			Engine:  cfg.Knowledge.Engine,
			Curator: curatorConfigFromHive(cfg.Knowledge.Curator),
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

	// Scheduled knowledge promotion (#5430). knowledge.curator.schedule used to
	// be parsed, defaulted to "daily", and never read. It now drives a real
	// sweep — but ONLY when knowledge.curator.enabled is explicitly true.
	// StartBackground is a no-op otherwise, and logs a notice if a schedule was
	// configured without the opt-in so the mismatch is visible rather than
	// silent. Do not replace the IsEnabled() guard with a schedule check: that
	// would enable unreviewed promotion on every hive that omits the key.
	if knowledgeAPI != nil && cfg.Knowledge.Curator.IsEnabled() {
		promotionScheduler := knowledge.NewPromotionScheduler(
			knowledgeAPI.Promoter(),
			curatorConfigFromHive(cfg.Knowledge.Curator),
			logger,
		)
		promotionScheduler.StartBackground(ctx)
	} else if cfg.Knowledge.Curator.Schedule != "" {
		logger.Info("knowledge.curator.schedule is set but scheduled promotion is disabled",
			"schedule", cfg.Knowledge.Curator.Schedule,
			"hint", "set knowledge.curator.enabled: true to opt in",
		)
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

	if err := os.MkdirAll(nousSnapshotDir, 0o755); err != nil {
		logger.Warn("failed to create nous snapshot dir", "path", nousSnapshotDir, "error", err)
	}
	if err := os.MkdirAll(nousGovernorDir, 0o755); err != nil {
		logger.Warn("failed to create nous governor dir", "path", nousGovernorDir, "error", err)
	}
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

	// Provider rotation (RFC #3958): opt-in automatic failover when a
	// provider's subscription/credit is exhausted. Nil when disabled.
	var rotationMgr *rotation.Manager
	if cfg.Governor.Rotation.Enabled {
		rotationMgr = rotation.NewManager(cfg.Governor.Rotation)
		rotationMgr.Start(ctx)
		logger.Info("provider rotation enabled",
			"threshold_pct", cfg.Governor.Rotation.EffectiveThreshold(),
			"providers", len(cfg.Governor.Rotation.Providers))
	}

	// Agent self-healing watchdog (RFC #4665): liveness/readiness
	// reconciliation on the governor tick. Config problems fall back to the
	// RFC defaults loudly — a typo must not disable self-healing silently.
	wdSettings, wdCfgErrs := watchdog.SettingsFrom(cfg.Governor.Watchdog)
	for _, e := range wdCfgErrs {
		logger.Warn("watchdog config problem", "error", e)
	}
	var wd *watchdog.Reconciler
	if wdSettings.Enabled() {
		wdFleet := agent.WatchdogFleet{
			M: agentMgr,
			// Queue depth for the readiness gate: an agent producing nothing
			// while nothing is queued is correct, not unhealthy. Read live
			// from the governor so it reflects the current sweep.
			Queued: func() (int, bool) {
				st := gov.GetState()
				return st.QueueIssues + st.QueuePRs, true
			},
		}
		wd = watchdog.New(wdSettings, wdFleet, dashSrv, logger,
			watchdog.WithAuthProbes(watchdogAuthProbes(cfg)))
		if saved != nil && len(saved.Watchdog) > 0 {
			wd.Restore(saved.Watchdog)
		}
		// Dead-session recovery moves under the watchdog's bounded ladder ONLY
		// when the watchdog may actually act. In observe mode the manager's
		// crash loop keeps its existing job, so there is never a window in
		// which neither component restarts a dead agent.
		agentMgr.SetDeadSessionRecoveryOwner(wdSettings.MayAct())
		logger.Info("agent watchdog enabled (RFC #4665)",
			"mode", string(wdSettings.Mode),
			"probe_interval", wdSettings.ProbeInterval,
			"crash_loop_after", wdSettings.CrashLoopAfter,
			"auth_probe", wdSettings.AuthProbe,
			"dead_session_recovery", map[bool]string{true: "watchdog", false: "crash-loop"}[wdSettings.MayAct()])
		if wdSettings.Mode == watchdog.ModeObserve {
			logger.Info("agent watchdog is in OBSERVE mode: it will classify agents, publish conditions and record what it WOULD have done, but will not restart or pause anything. Set governor.watchdog.mode: heal to enable healing.")
		}
	} else {
		logger.Info("agent watchdog disabled by config", "mode", string(wdSettings.Mode))
	}

	// Linear write credential for ISSUES_ONLY+ agents (GitHub-issue parity):
	// prefer the connected Linear agent app's OAuth token, so agent writes are
	// authored by the same "Hive" app identity that acknowledges sessions —
	// the analogue of App-bot authorship on GitHub — and fall back to the
	// work-source API key from hive.yaml. Resolved live off the dashboard's
	// install store and the cfg pointer so a workspace connected after boot
	// reaches agents on their next launch / hourly token refresh. Values are
	// never logged. Wired before RegisterAPI so the resolver is in place
	// before any agent launches.
	agentMgr.SetLinearCredentialResolver(func() agent.LinearCredential {
		if tok := dashSrv.LinearAgentAccessToken(); tok != "" {
			return agent.LinearCredential{AccessToken: tok}
		}
		if cfg.Governor.WorkSource.Type == "linear" {
			return agent.LinearCredential{APIKey: strings.TrimSpace(cfg.Governor.WorkSource.Linear.APIKey)}
		}
		return agent.LinearCredential{}
	})

	// In-flight ledger + session PR link (Linear GitHub-parity follow-ups):
	// the scheduler withholds work a Linear session is already working, and
	// the pr-request watcher narrates opened PRs into the session.
	sched.SetInflightLookup(dashSrv.LinearSessionHolder)
	if ghClient != nil {
		ghClient.SetPROpenedHook(func(agentName, repo string, number int, url string) {
			dashSrv.LinearAgentPROpened(agentName, repo, number, url)
			// Same typed hook feeds the lifecycle timeline: the watcher fires
			// it on the exact path that opened the PR, with the agent name the
			// audit stream attributes to the governor flow (#5656). The store
			// dedupes with the audit-sink bridge by (ref, kind).
			recordPROpened(dashSrv, cfg.Project.Org, agentName, repo, number, url)
		})
	}

	dashSrv.RegisterAPI(&dashboard.Dependencies{
		Config:           cfg,
		AgentMgr:         agentMgr,
		Governor:         gov,
		GHClient:         ghClient,
		GHAppAuth:        appAuth,
		GHTokenScopes:    ghAuth.TokenScopes,
		Tokens:           tokenCollector,
		Knowledge:        knowledgeAPI,
		Inception:        inceptionEngine,
		Nous:             nousState,
		Scheduler:        sched,
		MetricsCollector: metricsCollector,
		RotationMgr:      rotationMgr,
		// #3972: hand the ACMM advisor the SAME cached fleet-stats collector
		// the heartbeat reads, so its merge-success signal reuses the existing
		// 30-minute collect loop instead of issuing a second GitHub fetch.
		FleetStats:            fleetStatsCollector,
		Activity:              activityCollector,
		RepoCost:              repoCostCollector,
		BeadSynthesizer:       beadSynth,
		BeadStores:            beadStores,
		BeadStoreLoadFailures: beadStoreLoadFailures,
		Logger:                logger,
		Ctx:                   ctx,
		RefreshFunc:           refreshDashboard,
		// #3768: give the contribute queue read access to the duplicate-PR
		// claim ledger, so an issue any open PR (hive-authored or a human
		// contributor's) already claims to fix is never offered to another
		// contributor. Lazy: the ledger loads on first use, same as the
		// eval-cycle guard.
		IssueClaimed: func(repo string, number int) (github.IssueClaim, bool) {
			return getClaimLedger(logger).Lookup(repo, number)
		},
		HookFire: func(ctx context.Context, p hooks.Payload) {
			hookDispatcher().Fire(ctx, p)
		},
		PersistFunc: func() {
			persistState(agentMgr, gov, cfg, statePath, logger, dashSrv, wd)
		},
		ReInitFunc: func() {
			initAgentConfigDrivenSystems(cfg)
		},
		EnumerateFunc: func() {
			runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, nil, logger)
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
					_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
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
			newClient := github.NewClientFromAppWithBotLogin(newAppAuth, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.BotLogin())
			if len(cfg.Governor.Labels.Exempt) > 0 {
				newClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
				newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
			}
			newClient.SetIssueFilter(cfg.Project.IssueFilter)
			if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
				newClient.SetRequiredChecks(set)
			}

			ghClient = newClient
			appAuth = newAppAuth
			agentMgr.SetAppAuth(newAppAuth)
			// Deliver fresh per-agent scoped tokens to already-running agents
			// immediately — the periodic refresh loop only ticks every 40m,
			// far too long for agents whose caches are empty or stale (#4072).
			go agentMgr.RefreshAgentTokens(ctx)
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
					_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
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
				// #4360: ask about repo COVERAGE before attempting a read.
				// A repo the installation does not cover answers 404, which is
				// indistinguishable from "no such repo" and used to be reported
				// as "app not installed / no read" — sending the operator after
				// credentials that were never broken. Checking first means the
				// specific, correct message wins over the generic one.
				if raise, diag, state := classifyGitHubAppRepoCoverage(ctx, ghClient.AppAuth(), cfg.Project.Org, cfg.Project.Repos, logger); raise {
					dashSrv.SetGitHubAppPermIssue(diag)
					dashSrv.SetGitHubAppState(state.String())
					logger.Warn("github app recheck: installation does not cover every configured repo",
						"org", cfg.Project.Org, "state", state.String(), "detail", diag)
					dashSrv.AuditLog("system", "github_app_check", "result=repos not in installation: "+diag, "")
					return false
				}
				num, err := ghClient.EnsureAdvisoryIssue(ctx, recheckRepo)
				if err != nil {
					logger.Debug("github app recheck: not accessible", "repo", recheckRepo, "error", err)
					dashSrv.AuditLog("system", "github_app_check", "result=not accessible (app not installed / no read)", "")
					return false
				}
				advisoryIssues[recheckRepo] = num
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
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

	// If the App credentials are present but github.installation_id is still
	// empty, discover it automatically. This covers the delayed approval path:
	// a non-admin requests installation, an org admin approves later, and the
	// spoke adopts the installation ID without requiring anyone to paste it.
	{
		const githubAppDiscoveryInterval = 5 * time.Minute
		tryDiscover := func() {
			if cfg.GitHub.InstallationID != 0 {
				return
			}
			_, _ = dashSrv.AutoDiscoverGitHubInstallationID(ctx, false)
		}
		go func() {
			tryDiscover()
			ticker := time.NewTicker(githubAppDiscoveryInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					tryDiscover()
				}
			}
		}()
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
	// button (githubAppRecheckFn, which calls diagnoseGitHubApp) and clears
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
						_, _ = dashSrv.AutoDiscoverGitHubInstallationID(ctx, false)
						// Re-run the same read+write verification the manual
						// Re-check button uses. It clears the flag on success
						// (installed AND write-verified) and leaves it set on a
						// genuine failure (not installed / insufficient perms).
						if dashSrv.RecheckGitHubApp() {
							if num, exists := advisoryIssues[primaryRepo]; exists {
								_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
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
		gov.UpdateConfig(cfg.Governor)
		// A reload can add or archive repos, which moves every scaled default
		// threshold — re-sync it alongside the repo list above.
		gov.SetRepoCount(cfg.Project.RepoCount())
		agentMgr.SetSandboxConfig(cfg.AgentSandbox)
		// Re-run the posture check on reload, not only at boot: flipping the
		// Security tab's sandbox toggle writes the config and lands here, which
		// is the exact moment an operator forms the belief that they are now
		// sandboxed. See logAgentSandboxPosture.
		logAgentSandboxPosture(logger, cfg)

		// Hot-reload the state-triggered hooks (RFC #4001). Recompiles only
		// when the `hooks:` list actually changed, and swaps the registry in
		// place so per-hook rate-limit windows SURVIVE the reload — otherwise
		// a reload loop would be a way to clear the anti-storm ceiling.
		buildHookDispatcher(cfg, hookSinks{
			Notifier: notifier,
			AgentMgr: agentMgr,
			Timeline: dashSrv.LifecycleTimeline(),
			Audit:    dashSrv.AgentAuditSink(),
		}, logger)

		// Re-apply live agent definitions (definition_source) on reload so an
		// operator's edit to a linked repo propagates. Merges only operator-safe
		// fields; a fetch failure keeps each agent's baked definition. Runs before
		// initAgentConfigDrivenSystems so downstream systems see the merged config.
		defsrc.ApplyToConfig(context.Background(), cfg, definitionResolver, logger)
		if err := cfg.ExpandAgentReplicas(); err != nil {
			logger.Warn("failed to expand agent replicas after config reload", "error", err)
		}
		addedAgents := agentMgr.ReconcileAgents(cfg.EnabledAgents())
		for _, added := range addedAgents {
			if ac, ok := cfg.Agents[added]; ok && !ac.OnDemand {
				go func(name string) {
					logger.Info("audit: starting reconciled agent", "name", name, "trigger", "config-reload")
					if err := agentMgr.Start(ctx, name); err != nil {
						logger.Warn("failed to start reconciled agent", "name", name, "error", err)
					}
				}(added)
			}
		}
		gov.UpdateAgents(cfg.EnabledAgents())

		initAgentConfigDrivenSystems(cfg)

		// Rebuild GitHub App auth when its identity changed. AppAuth captures
		// app_id/installation_id at construction, so without this a corrected
		// installation_id in hive.yaml keeps minting tokens for the OLD
		// installation until the pod restarts.
		//
		// RESOLVE the key file rather than reading cfg.GitHub.KeyFile raw. An
		// unset key_file is the CORRECT steady state on a hosted spoke — the
		// heartbeat apply path deliberately does not persist one, because the
		// path is derivable from app_id and a stored value outlives the App it
		// was derived for. Gating on the raw field therefore skipped the rebuild
		// entirely on exactly the hives that need it: a corrected
		// installation_id saved to hive.yaml kept minting tokens for the old
		// installation until the pod restarted. Startup (resolveAppKeyFile
		// above), the heartbeat rebuild, and the dashboard's Set ID handler
		// (#2459) all already resolve here; this was the last raw reader.
		//
		// Comparing RESOLVED paths also catches a change the raw comparison
		// cannot see: a per-app-id key arriving on the PVC changes which key
		// this process should sign with while cfg.GitHub.KeyFile stays "".
		prevKeyFile := resolveAppKeyFile(prevGitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), prevGitHub.AppID)
		nextKeyFile := resolveAppKeyFile(cfg.GitHub.KeyFile, os.Getenv("GH_APP_KEY_FILE"), cfg.GitHub.AppID)
		if prevGitHub.AppID != cfg.GitHub.AppID ||
			prevGitHub.InstallationID != cfg.GitHub.InstallationID ||
			prevKeyFile != nextKeyFile ||
			prevGitHub.APIURL != cfg.GitHub.APIURL {
			if cfg.GitHub.HasUsableApp() && nextKeyFile != "" {
				newAppAuth, appErr := github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.InstallationID, nextKeyFile, logger, cfg.GitHub.ResolvedAPIURL())
				if appErr != nil {
					logger.Error("github app auth rebuild after config reload failed", "error", appErr)
				} else {
					newClient := github.NewClientFromAppWithBotLogin(newAppAuth, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.BotLogin())
					if len(cfg.Governor.Labels.Exempt) > 0 {
						newClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
						newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
					}
					newClient.SetIssueFilter(cfg.Project.IssueFilter)
					if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
						newClient.SetRequiredChecks(set)
					}
					ghClient = newClient
					appAuth = newAppAuth
					agentMgr.SetAppAuth(newAppAuth)
					// Immediate per-agent token delivery — see #4072.
					go agentMgr.RefreshAgentTokens(ctx)
					agentMgr.SetSandboxPushMinter(pushbroker.GitHubAppMinter{Auth: newAppAuth})
					agentMgr.SetSandboxPRClient(newClient)
					dashSrv.UpdateGitHubClient(newClient, newAppAuth)
					logger.Info("github app auth rebuilt after config reload",
						"app_id", cfg.GitHub.AppID,
						"installation_id", cfg.GitHub.InstallationID,
						"key_file", nextKeyFile,
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

	// Feed agent lifecycle events (start, stop, launch FAILURE, backend/model
	// change) into the durable audit store behind the dashboard's Audit Log.
	// Injected as an interface because pkg/dashboard already imports pkg/agent,
	// so the manager cannot reach the audit store directly without an import
	// cycle. Motivating case: an agent configured with a backend its hive image
	// did not support failed at every launch for a day, visible only as a WARN
	// line inside the pod.
	agentMgr.SetAuditSink(dashSrv.AgentAuditSink())

	// Compile the operator's state-triggered hooks (RFC #4001). Every sink the
	// vetted actions act through exists by this point: the notifier, the agent
	// manager's AUDITED pause, the lifecycle timeline, and the same audit store
	// the dashboard writes. The approvals sink stays nil until #4000's queue
	// lands — an enqueue-approval hook then reports a wiring failure per firing
	// rather than silently dropping the request.
	//
	// Fail-closed: an invalid hooks list logs and leaves the previous set
	// armed; it never crashes the process or silently disarms working hooks.
	buildHookDispatcher(cfg, hookSinks{
		Notifier: notifier,
		AgentMgr: agentMgr,
		Timeline: dashSrv.LifecycleTimeline(),
		Audit:    dashSrv.AgentAuditSink(),
	}, logger)

	// Emit the governor_mode_change transition post-commit. Installed once:
	// the observer reads the dispatcher through hookDispatcher() on each
	// firing, so a later config reload that arms or disarms hooks is picked up
	// without re-registering.
	installGovernorModeChangeEmitter(gov)
	installAgentPauseEmitter(agentMgr)

	// Register custom GHE hostnames with the proxy allowlist so mode
	// enforcement applies to GitHub Enterprise API and web requests.
	for _, rawURL := range []string{cfg.GitHub.ResolvedAPIURL(), cfg.GitHub.ResolvedBaseURL()} {
		if parsed, err := url.Parse(rawURL); err == nil && parsed.Host != "" {
			proxy.RegisterGitHubHost(parsed.Host)
		}
	}

	dashboard.SetBackendAuthProvider(agentMgr.BackendAuthAvailable)
	// Per-agent probe supersedes the backend-level one: under the per-agent-UID
	// layout each agent has its own HOME, so the shared credential path is empty
	// even for authenticated agents (see pkg/agent/authprobe.go).
	dashboard.SetAgentAuthProvider(agentMgr.AgentAuthAvailable)

	canaryLeakHandler := func(leak ioscan.CanaryLeak) {
		detail := fmt.Sprintf("rule=%s, agent=%s, source=%s", ioscan.CanaryLeakRule, leak.Agent, leak.Source)
		dashSrv.AuditLog(leak.Agent, "ioscan_canary_leak", detail, leak.Agent)
		if store, ok := beadStores[leak.Agent]; ok && store != nil {
			if b, berr := store.Create("Canary token leaked via "+leak.Source, beads.TypeAdvisory, beads.PriorityCritical, leak.Agent, ""); berr == nil {
				_ = store.SetMetadata(b.ID, "rule", ioscan.CanaryLeakRule)
				_ = store.SetMetadata(b.ID, "source", leak.Source)
			}
		}
	}
	if ghClient != nil {
		ghClient.SetCanaryScanner(cfg.Ioscan.IsEnabled() && cfg.Ioscan.Canaries, cfg.Ioscan.FailClosed(), ioscan.DefaultCanaries, canaryLeakHandler)
	}

	githubProxy, err := proxy.NewGitHubProxy(logger, cfg.Project.Org, cfg.Project.Repos)
	if err != nil {
		logger.Error("failed to create github proxy", "error", err)
	} else {
		githubProxy.SetCanaryScanner(cfg.Ioscan.IsEnabled() && cfg.Ioscan.Canaries, cfg.Ioscan.FailClosed(), ioscan.DefaultCanaries, canaryLeakHandler)
		dashboard.SetProxyViolationsProvider(githubProxy.Violations)
		// Lets the dashboard narrow the LiteLLM model dropdown to the set the
		// configured key is entitled to, learned by the proxy from a key-info
		// probe or a "team not allowed" 403.
		dashboard.SetEntitledModelsProvider(githubProxy.EntitledModels)
		// Surface a stale/invalid inference gateway key (repeated 401s on every
		// inference call) as a hive health signal: the proxy latches the failure
		// after several consecutive rejections and clears it on the next success,
		// and the heartbeat builder reports it to the hub (both as an immediate
		// advisory-staleness cause and as a dedicated inference-auth alert).
		dashboard.SetInferenceAuthProvider(githubProxy.InferenceAuthError)
		// #4294: the provider spending-limit signal, read by the eval cycle to
		// raise an advisory and stop kicking agents at a gateway that is
		// refusing on a money limit.
		dashboard.SetInferenceBudgetProvider(githubProxy.InferenceBudgetExceeded)
		dashboard.SetGatewayHealthProvider(githubProxy.GatewayHealth)

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
		// The gateway-name predicate (SetGatewayBackendChecker) is wired right
		// after the manager is constructed — see the comment there (#3961): it
		// must be live before the persisted-state replay re-applies saved
		// backend overrides, which happens well before this point.
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
					// watsonx authenticates the OpenAI-compatible model gateway
					// with a short-lived IAM bearer minted from the IBM Cloud API
					// key (NOT the raw key), and scopes billing/limits by a
					// project id sent as X-IBM-Project-ID. Mint (cached) and set
					// both here; every other kind sends the resolved key verbatim.
					apiKey, extraHeaders := resolveGatewayAuth(gw, agentName, backend, logger)
					githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:      backend,
						Endpoint:     endpoint,
						Model:        model,
						APIKey:       apiKey,
						CABundle:     gw.CABundle,
						ExtraHeaders: extraHeaders,
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
					// Endpoint/model resolution lives in a pure function so the
					// decision tree (local proxy / legacy block / explicit-gateway
					// fallback / no route at all) is unit-testable — it is not
					// reachable from a test while inline in main(). See #5460.
					endpoint, resolvedModel, ok := resolveLiteLLMInferenceRoute(cfg, backend, model)
					if !ok {
						logger.Warn("litellm backend selected but no endpoint configured",
							"agent", agentName, "model", model)
						return
					}
					model = resolvedModel
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
				if backend == config.GatewayKindWatsonx {
					// Built-in "watsonx" backend: the operator set
					// `backend: watsonx` without a gateway literally NAMED
					// watsonx (a named one is handled by the gateway branch
					// above). Resolve the watsonx gateway by KIND so the
					// endpoint, IBM Cloud key, project id and region all come
					// from the existing `gateways:` plumbing rather than being
					// re-derived here.
					gw := resolveWatsonxGateway(cfg)
					if gw == nil {
						logger.Warn("watsonx backend selected but no watsonx gateway is configured; add one under the Model Gateways tab",
							"agent", agentName, "model", model)
						return
					}
					// Region-only gateways are legal (the guided form can save a
					// region without an endpoint), so fall back to the shared
					// region template — the same helper the dashboard preset uses.
					endpoint := strings.TrimSpace(gw.Endpoint)
					if endpoint == "" {
						endpoint = watsonx.EndpointForRegion(gw.Region)
					}
					if model == "" {
						model = gw.DefaultModel
					}
					apiKey, extraHeaders := resolveGatewayAuth(gw, agentName, backend, logger)
					githubProxy.SetInferenceRoute(agentName, &proxy.InferenceRoute{
						Backend:      backend,
						Endpoint:     endpoint,
						Model:        model,
						APIKey:       apiKey,
						CABundle:     gw.CABundle,
						ExtraHeaders: extraHeaders,
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
			AllowedUsers:   cfg.Notifications.Discord.AllowedUsers,
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
	// per-agent agent_start rows. Include the persisted pauses being restored
	// so the operator can confirm pause state survived the restart — broken
	// down by trigger, and EXCLUDING agents that are startup-paused by design
	// (on-demand agents like brainstorm), whose inclusion turned "restoring 9
	// paused agent(s)" into a false systemic-incident signal on every upgrade
	// restart of a deliberately owner-quiesced fleet (#4041).
	dashSrv.AuditLog("system", "hive_restart",
		fmt.Sprintf("build=%s version=%s; %s", gitShort, version,
			pausedRestoreDetail(cfg.EnabledAgents(), onDemandFromPack, agentMgr.AllStatuses())), "")

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
		// Publish the collect-independent identity BEFORE the loop starts, so
		// this spoke can report liveness even if its very first collects time
		// out. collect() below reaches api.github.com (owner-token validation,
		// and it shares the pass that enumerates issues/PRs for MTTR), which on
		// a hive with real repos routinely exceeds the collect budget right
		// after a restart. Without this, such a spoke sent NOTHING and read
		// OFFLINE on the hub while being perfectly healthy.
		hub.PublishHeartbeatIdentity(
			cfg.HiveID,
			cfg.Project.Org,
			cfg.Project.PrimaryRepo,
			cfg.Project.Repos,
			reporterName,
			processStartedAt.UTC().Format(time.RFC3339),
			gitShort,
		)
		go hub.StartHeartbeat(ctx, hubURL, func() *hub.HeartbeatPayload {
			if !cfg.Hub.Enabled {
				return nil
			}
			statuses := agentMgr.AllStatuses()
			govState := gov.GetState()
			currentMode := strings.ToLower(string(govState.Mode))
			agents := make([]hub.AgentSummary, 0, len(statuses))
			for name, proc := range statuses {
				mode := ""
				if ac, ok := cfg.Agents[name]; (ok && ac.OnDemand) || onDemandFromPack[name] {
					mode = "on_demand"
				}
				agents = append(agents, hub.NewAgentSummary(name, string(proc.State), mode,
					agentActivityFor(agentMgr, cfg, govState, currentMode, name, proc, onDemandFromPack)))
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
			// Per-repo output-activity summary (hive-health): map the dashboard
			// collector's snapshot into the plain hub wire structs. Nil when the
			// collector hasn't produced a snapshot yet, so the hub carries the
			// last one forward rather than seeing a fabricated empty summary.
			var repoActivity []hub.RepoActivityWire
			repoActivityCollectedAt := ""
			repoActivityWindowHours := 0
			repoActivityCountWindowHours := 0
			if asnap, ok := activityCollector.Snapshot(); ok {
				repoActivity = buildRepoActivityWire(asnap.Repos)
				repoActivityWindowHours = asnap.WindowHours
				repoActivityCountWindowHours = asnap.CountWindowHours
				if t := activityCollector.CollectedAt(); !t.IsZero() {
					repoActivityCollectedAt = t.UTC().Format(time.RFC3339)
				}
			}
			// Count agents with a method/model assigned for the hub's
			// user-journey stage detection. Always a non-nil pointer from a
			// spoke new enough to compute it, so the hub can distinguish
			// "genuinely zero agents configured" from "old spoke, unknown".
			agentsWithModel := agentMgr.CountAgentsWithModel()

			// --- Quadrant signals ------------------------------------------
			// All read from state this spoke already maintains on an existing
			// timer: ZERO new GitHub API calls, which matters because the whole
			// fleet shares one search quota. Every one stays nil unless its
			// source has actually produced a measurement — the hub's scorer
			// reads nil as absent evidence and a zero as a genuine low score,
			// so emitting a zero for missing data would silently misinform
			// operators rather than merely lose precision.

			// Budget spend is uninterpretable without its window bounds (zero
			// equally means "window just rolled" and "nothing consumed"), so
			// the three travel together or not at all.
			var budgetSpend *int64
			var budgetLimit *int64
			var budgetIgnored *bool
			var budgetWindowStartsAt, budgetWindowEndsAt string
			budget := gov.GetBudget()
			limit := budget.WeeklyLimit
			ignored := budget.IgnoreAll
			budgetLimit = &limit
			budgetIgnored = &ignored
			if start, end, ok := gov.BudgetWindow(); ok {
				spend := budget.CurrentSpend
				budgetSpend = &spend
				budgetWindowStartsAt = start.UTC().Format(time.RFC3339)
				budgetWindowEndsAt = end.UTC().Format(time.RFC3339)
			}
			// BudgetExhausted and SLAViolations are both plain (non-pointer)
			// governor state, so their zero values are indistinguishable from
			// "never evaluated" at the source. LastEval is the only thing that
			// tells the two apart: before the first eval — or before a restart
			// restores one — false/0 are struct defaults, not readings. Gate
			// both on it so a spoke still booting reports nil rather than
			// asserting a healthy budget and a clean SLA it has not checked.
			var budgetExhausted *bool
			var slaViolations *int
			if !govState.LastEval.IsZero() {
				exhausted := govState.BudgetExhausted
				violations := govState.SLAViolations
				budgetExhausted, slaViolations = &exhausted, &violations
			}

			// Hold comes from the cached actionable result rather than
			// govState.QueueHold: both carry the same number, but the cache is
			// a nilable pointer, so a spoke that has not yet completed (or
			// restored) a scan reports nil instead of an int zero that is
			// indistinguishable from "nothing is on hold".
			var holdTotal *int
			if act := lastActionable.Load(); act != nil {
				total := act.Hold.Total
				holdTotal = &total
			}

			// Planning is unavailable below ACMM L5, where AwaitingReview is
			// structurally zero rather than measured — report nil so the hub
			// does not read "no plans are blocked on a human" into a hive that
			// has no planning subsystem at all.
			//
			// architectPaused is passed false rather than resolved from agent
			// statuses: it feeds only FrontendPlanning.ArchitectPaused, which
			// this heartbeat does not send, and the resolver is unexported to
			// pkg/dashboard. Passing false cannot perturb AwaitingReview.
			var awaitingReview *int
			if planning := dashboard.BuildPlanning(beadStores, false, acmmLvl); planning.Available {
				n := planning.AwaitingReview
				awaitingReview = &n
			}

			// Contributor-relay tasks over the trailing 7d, summed from the
			// spoke's own 168 hourly buckets. nil until the store exists; a
			// zero from an existing store is a real "no contributor finished
			// anything" reading.
			var tasksCompleted7d *int
			if n, ok := dashSrv.TasksCompleted7d(); ok {
				tasksCompleted7d = &n
			}

			providerLimitReason, providerLimitRebuffs := providerLimitHeartbeatFields(agents)

			// Remediation-hint detectors (#5577). All three read state the
			// spoke already maintains — no new GitHub calls, no new file
			// scans on the beat path. AgentErrorStreaks is nil until the
			// token collector's first bob-recording scan completes ("not
			// measured", hub carries forward); the other two are always live
			// measurements and send [] to clear a stale carry-forward.
			agentErrorStreaks := tokenCollector.AgentErrorStreaks()
			consentWedged := agentMgr.ConsentWedgedAgents()
			noCadenceAgents := gov.NoCadenceAgents()

			return &hub.HeartbeatPayload{
				AgentsWithModel:      &agentsWithModel,
				BudgetCurrentSpend:   budgetSpend,
				BudgetLimit:          budgetLimit,
				BudgetWindowStartsAt: budgetWindowStartsAt,
				BudgetWindowEndsAt:   budgetWindowEndsAt,
				BudgetExhausted:      budgetExhausted,
				BudgetIgnored:        budgetIgnored,
				HoldTotal:            holdTotal,
				AwaitingReview:       awaitingReview,
				SLAViolations:        slaViolations,
				TasksCompleted7d:     tasksCompleted7d,
				AgentErrorStreaks:    agentErrorStreaks,
				ConsentWedged:        consentWedged,
				NoCadenceAgents:      noCadenceAgents,
				// Read-back for hub-funded gateways: the hub clears its pending
				// record only when it sees the gateway named here, so a lost
				// delivery is re-offered rather than dropped. Names only — the
				// key never leaves the spoke.
				GatewayNames:  dashSrv.ConfiguredGatewayNames(),
				GatewayHealth: dashSrv.GatewayHealthState(),
				// Hash only, never the raw token: lets the hub verify this
				// spoke's upgrade-proof credential without reading the
				// hive-secrets secret from a cluster it may not reach
				// (pull-only). Empty when no token is configured.
				DashboardTokenHash: func() string {
					if cfg.Dashboard.AuthToken == "" {
						return ""
					}
					return hub.HashDashboardToken(cfg.Dashboard.AuthToken)
				}(),
				HiveID:            cfg.HiveID,
				Org:               cfg.Project.Org,
				AIAuthor:          cfg.Project.AIAuthor,
				AIAuthorEffective: cfg.EffectiveAIAuthor(),
				StartedAt:         processStartedAt.UTC().Format(time.RFC3339),
				// FD gauge (#3875): a socket leak reached 92,962 FDs and
				// self-DoSed spokes with nothing surfacing it. Report the count
				// and its rlimit every beat so the next leak is a climbing
				// number on the hub, not a manual /proc excavation.
				OpenFDs:     hub.OpenFDCount(),
				FDSoftLimit: hub.FDSoftLimit(),
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
				// Digest SHAPE: how many findings went out, and how many the
				// top-N cap withheld. The hub renders the pair so a capped
				// digest never reads as the complete picture.
				AdvisoryFindingCount: func() int {
					findings, _ := dashSrv.AdvisoryCounts()
					return findings
				}(),
				AdvisoryOverflowCount: func() int {
					_, overflow := dashSrv.AdvisoryCounts()
					return overflow
				}(),
				// Inference-backend auth-failure signal (repeated 401s from a
				// stale gateway key). Reported as its own field so the hub can
				// raise a dedicated inference-auth alert whose ROOT cause an
				// operator sees directly — distinct from the advisory-staleness
				// pill AdvisoryError also trips. Empty when inference auth is
				// healthy or the hive routes to no inference backend; self-clears
				// on the next successful inference call.
				InferenceAuthError: func() string {
					errMsg, _ := dashSrv.InferenceAuthState()
					return errMsg
				}(),
				ProviderLimitReason:     providerLimitReason,
				ProviderLimitRebuffs:    providerLimitRebuffs,
				RepoTargetMisconfigured: repoTargetMisconfigured(),
				RepoTargetIssue:         repoTargetIssueMessage(),
				Repos:                   cfg.Project.Repos,
				PrimaryRepo:             cfg.Project.PrimaryRepo,
				ACMMLevel:               acmmLvl,
				Agents:                  agents,
				Governor: hub.GovernorSummary{Mode: string(govState.Mode), Issues: govState.QueueIssues, PRs: govState.QueuePRs, WorkSource: func() string {
					if t := cfg.Governor.WorkSource.Type; t != "" && t != "github" {
						return t
					}
					return ""
				}()},
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
				// The honest subset of the above: users whose browser reported
				// focused, recent-input presence (see dashboard/presence.go).
				// An idle open tab appears in ActiveSessionUsers but not here.
				EngagedSessionUsers: dashSrv.EngagedSessionUsernames(),
				// Per-user last audit-logged real action, so the hub can tell
				// users who DO things from users who merely stay logged in.
				UserLastActions: dashSrv.UserLastActions(),
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
					// Prefer the host our OWN Route/Ingress actually serves.
					//
					// The synthesised "<hiveID>.<hub host>" below is only
					// correct when this spoke is fronted by the hub's wildcard
					// domain, i.e. co-located with the hub. On any other
					// cluster that name resolves — via that same wildcard — to
					// the HUB's router, which has no backend for it and returns
					// 503, so the hub linked users at a hostname that could
					// never work while our real Route served fine. Reading the
					// live object is the only source that is right on every
					// cluster, and on a pull-only cluster the hub cannot read
					// it, so the spoke must report it.
					if host := hub.SpokeServedHost(ctx); host != "" {
						return "https://" + host
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
				Version:     version,
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
				GitHubHost:         cfg.GitHub.HostLabel(),
				GitHubAppRequired:  dashSrv.IsGitHubAppRequired(),
				GitHubAppPermIssue: dashSrv.GetGitHubAppPermIssue(),
				GitHubAppState:     dashSrv.GetGitHubAppState(),
				GitHubAppTokenStatus: func() string {
					status, _, _ := githubAppTokenHeartbeatFields(cfg, dashSrv.GetGitHubAppPermIssue())
					return status
				}(),
				GitHubAppTokenLastMintAt: func() string {
					_, lastMintAt, _ := githubAppTokenHeartbeatFields(cfg, dashSrv.GetGitHubAppPermIssue())
					return lastMintAt
				}(),
				GitHubAppTokenError: func() string {
					_, _, errMsg := githubAppTokenHeartbeatFields(cfg, dashSrv.GetGitHubAppPermIssue())
					return errMsg
				}(),
				PendingGitHubAppInstall: dashSrv.IsPendingGitHubAppInstall(),
				AutoUpgrade:             cfg.Hub.AutoUpgrade,
				ClusterHealth: func() *hub.HeartbeatClusterHealthReport {
					if os.Getenv("HIVE_CLUSTER_ID") == "" {
						return nil
					}
					return hub.CollectClusterHealth(logger)
				}(),
				PRsMerged90d:                 prsMerged,
				PRsRejected90d:               prsRejected,
				CVEsClosed:                   cvesClosed,
				FleetStatsCollectedAt:        fleetStatsCollectedAt,
				RepoActivity:                 repoActivity,
				RepoActivityCollectedAt:      repoActivityCollectedAt,
				RepoActivityWindowHours:      repoActivityWindowHours,
				RepoActivityCountWindowHours: repoActivityCountWindowHours,
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
				// Component reach counters (#3993, phase 2a of #3973): per
				// (component, running commit) span counts aggregated in-process,
				// exporter or not (D2) — the heartbeat is the only channel that
				// reaches every spoke, pull-only ones included (D1). nil until
				// the first span, which the hub reads as "no data", never as
				// zero reach. Capped at tracing.MaxReachComponents entries.
				ComponentReach: tracing.ReachSnapshot(),
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
							upgradeFailureSummary(m.Attempts, m.LastError), logger)
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
					if err := os.Remove(upgradeMarkerPath); err != nil && !os.IsNotExist(err) {
						logger.Warn("failed to clear stale upgrade marker", "path", upgradeMarkerPath, "error", err)
					}
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
				govState := gov.GetState()
				currentMode := strings.ToLower(string(govState.Mode))
				agents := make([]hub.AgentSummary, 0, len(statuses))
				for name, proc := range statuses {
					mode := ""
					if ac, ok := cfg.Agents[name]; (ok && ac.OnDemand) || onDemandFromPack[name] {
						mode = "on_demand"
					}
					agents = append(agents, hub.NewAgentSummary(name, string(proc.State), mode,
						agentActivityFor(agentMgr, cfg, govState, currentMode, name, proc, onDemandFromPack)))
				}
				acmmLvl := 0
				if cfg.ACMMLevel != nil {
					acmmLvl = *cfg.ACMMLevel
				}
				providerLimitReason, providerLimitRebuffs := providerLimitHeartbeatFields(agents)
				return &hub.HeartbeatPayload{
					HiveID: cfg.HiveID,
					Org:    cfg.Project.Org,
					// Project identity rides even this minimal beat. The hub
					// rebuilds the registry entry from each payload VERBATIM
					// (no carry-forward for these fields), and this beat is
					// the LAST one the hub holds for the whole restart window
					// that follows — omitting repos/primary_repo here blanked
					// the entry (org set, primaryRepo "", repos []) until the
					// new process's first successful collect, breaking the
					// public-directory row (no repo link) and rendering the
					// hive name as "org/". Both values are plain config reads,
					// exactly as cheap as Org above.
					Repos:                   cfg.Project.Repos,
					PrimaryRepo:             cfg.Project.PrimaryRepo,
					ACMMLevel:               acmmLvl,
					Agents:                  agents,
					GitHash:                 gitShort,
					ClusterID:               cfg.Hub.ClusterID,
					HiveType:                cfg.Hub.HiveType,
					IsPublic:                cfg.Hub.IsPublic,
					Version:                 version,
					RepoTargetMisconfigured: repoTargetMisconfigured(),
					RepoTargetIssue:         repoTargetIssueMessage(),
					ProviderLimitReason:     providerLimitReason,
					ProviderLimitRebuffs:    providerLimitRebuffs,
					// Remediation-hint detectors (#5577): all three are
					// cheap in-memory reads, so even this minimal upgrading
					// beat carries them — the pod is about to restart, and
					// carrying the last real measurement across the roll keeps
					// a live wedge visible instead of blanking it.
					AgentErrorStreaks: tokenCollector.AgentErrorStreaks(),
					ConsentWedged:     agentMgr.ConsentWedgedAgents(),
					NoCadenceAgents:   gov.NoCadenceAgents(),
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
			// happened: all 33 heartbeat-only-cluster spokes had key_file pinned to the generic
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
			// applyDeliveredPerAppKey writes ONE (app_id, key) pair to its
			// per-app-id file and reports whether that changed the key we
			// ourselves sign with. Shared by the (now-inert) AdditionalKeys loop
			// and the targeted SecondaryKey delivery below so both write through
			// identical code — the alternative is two copies of an atomic 0600
			// write, one of which eventually loses a guard.
			applyDeliveredPerAppKey := func(kind string, appID int64, privateKey string) {
				if privateKey == "" || appID <= 0 {
					return
				}
				perAppPath := perAppIDKeyPath(appID)
				beforeFP, _ := config.AppKeyFingerprintFromFile(perAppPath)
				fp, err := writePerAppIDKey(appID, privateKey)
				if err != nil {
					logger.Error("failed to write "+kind+" github app key from heartbeat",
						"app_id", appID, "error", err)
					return
				}
				changed := fp != "" && fp != beforeFP
				logger.Info(kind+" github app private key written via heartbeat",
					"app_id", appID,
					"path", perAppPath,
					"from_fingerprint", beforeFP,
					"to_fingerprint", fp,
					"key_changed", changed,
				)
				// If this key is for the App we ourselves authenticate as, it is
				// now the key resolveAppKeyFile will pick — treat it like a
				// primary-key rotation so the client rebuild below uses it.
				if changed && appID == cfg.GitHub.AppID {
					keyChanged = true
					appAuth.DropCachedToken()
				}
			}
			for _, ak := range ghCfg.AdditionalKeys {
				applyDeliveredPerAppKey("additional", ak.AppID, ak.PrivateKey)
			}

			// The OPTIONAL SECOND App key (#4815), delivered targeted at this
			// hive alone rather than broadcast. It lands in the same
			// /data/gh-app-key-<appid>.pem namespace the spoke has always used,
			// so heldPerAppIDKeyFingerprints reports it back on the next beat
			// (which is what stops the hub re-pushing it) and the Forge App tab
			// renders it, both with no further change. nil for every hive with no
			// second App.
			if ghCfg.SecondaryKey != nil {
				applyDeliveredPerAppKey("secondary", ghCfg.SecondaryKey.AppID, ghCfg.SecondaryKey.PrivateKey)
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
			// what left all 33 heartbeat-only-cluster spokes pinned to the GHE key.
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
			// primary key_file configured — the exact heartbeat-only-cluster state) still finds
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
				newClient := github.NewClientFromAppWithBotLogin(newAppAuth, cfg.Project.Org, cfg.Project.Repos, logger, cfg.GitHub.BotLogin())
				if len(cfg.Governor.Labels.Exempt) > 0 {
					newClient.SetExemptLabels(cfg.Governor.Labels.Exempt)
					newClient.SetAutoMergeLabel(normalizedAutoMergeLabel(cfg.Governor.Labels.AutoMerge))
				}
				newClient.SetIssueFilter(cfg.Project.IssueFilter)
				if set, ok := cfg.AutoMerge.RequiredCheckSet(); ok {
					newClient.SetRequiredChecks(set)
				}
				ghClient = newClient
				appAuth = newAppAuth
				agentMgr.SetAppAuth(newAppAuth)
				// Immediate per-agent token delivery: hosted spokes get their
				// App creds via this heartbeat path AFTER agents have already
				// launched (with empty 0-byte caches), so waiting for the next
				// 40-minute tick guarantees a window of gh 401s (#4072).
				go agentMgr.RefreshAgentTokens(ctx)
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
			image := "ghcr.io/hivecommons/hive:" + tag
			if err := hub.SwitchImageSelf(logger, image); err != nil {
				logger.Warn("branch switch via heartbeat failed", "tag", tag, "image", image, "error", err)
				return
			}
		}), hub.AuthorizedUsersCallback(func(users []string, names map[string]string) {
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
			// AuthorizedUserNames is purely cosmetic (see its doc) — it never
			// gates sign-in, so it's fine to just take whatever the hub sent
			// (including nil, which means "no names known") without the
			// same-value guard above.
			cfg.Dashboard.AuthorizedUserNames = names
		}), hub.ProjectConfigCallback(func(pc *hub.HeartbeatProjectConfig) {
			// The hub assigned this (previously placeholder) hive a real project.
			// Reconcile our running project config so agents work the claimed
			// org/repos at the claimed maturity level. This is the ONLY delivery
			// channel on heartbeat-only clusters (the heartbeat-only cluster) — no kubectl push is
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
			if issue := config.ValidateProjectRepoTargets(pc.Org, pc.Repos, pc.PrimaryRepo, cfg.GitHub.HostLabel()); issue != nil {
				logger.Error("REFUSING hub project config: repo target is misconfigured — project left unchanged",
					"error", issue.Message,
					"pushed_org", pc.Org,
					"pushed_repos", pc.Repos,
					"pushed_primary_repo", pc.PrimaryRepo,
				)
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
			// Issue filter: nil means "the hub is not speaking to this field"
			// (mirrors AIAuthor's empty-means-keep), so the hub's every-beat
			// echo can never blank a locally configured filter.
			issueFilterMatched := pc.IssueFilter == nil || cfg.Project.IssueFilter.Equal(*pc.IssueFilter)
			if cfg.Project.Org == pc.Org &&
				sameStringSlice(cfg.Project.Repos, pc.Repos) &&
				cfg.Project.PrimaryRepo == pc.PrimaryRepo &&
				curACMM == pc.ACMMLevel &&
				authorMatched &&
				apiURLMatched &&
				issueFilterMatched &&
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
			// Adopt a hub-delivered issue filter only when the hub actually
			// sent one (non-nil). A push without the field leaves the spoke's
			// locally configured filter untouched — the org/repos assignments
			// above never wipe it either, so a local filter SURVIVES claim
			// delivery. A non-nil but EMPTY filter is an explicit clear.
			if pc.IssueFilter != nil && !cfg.Project.IssueFilter.Equal(*pc.IssueFilter) {
				logger.Info("adopting issue filter from hub heartbeat",
					"require_labels", pc.IssueFilter.RequireLabels)
				cfg.Project.IssueFilter = *pc.IssueFilter
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

			// Re-sync the GitHub client that caches the repo list (mirrors the
			// config-watcher reload path). The issue filter is cached the same
			// way, so re-install it too — a hub-delivered filter must take
			// effect on the next enumeration, not the next restart.
			ghClient.SetRepos(cfg.Project.Repos)
			ghClient.SetIssueFilter(cfg.Project.IssueFilter)

			// Persist to the PVC overlay so the claim survives a pod restart
			// (config save writes the overlay hive.yaml, same as level switches).
			if err := cfg.Save(); err != nil {
				logger.Error("failed to save claimed project config", "error", err)
			}
		}), hub.GatewayConfigCallback(func(gw *hub.HeartbeatGatewayConfig) {
			// The hub funded an OpenRouter gateway on this hive's behalf (scan-to-
			// fund from My Hives) and delivered it over the heartbeat channel — the
			// only path that reaches a firewalled/heartbeat-only spoke (the heartbeat-only cluster). We
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
	// build. The half-configured state is shown inline in Settings →
	// General, not in the top banner.
	dashSrv.ReconcileTrajectoryAlert(&cfg.Governor)

	// Stall-replan lane (Phase 3 planning intelligence): periodically detects
	// approved plans whose sub-tasks have stopped progressing and re-kicks the
	// architect to revise them, bounded by a per-plan replan cap. It runs off the
	// governor tick, gated by its own Due() cadence (no goroutine of its own), and
	// drives the architect only through SendKick (agentKicker) from this tick —
	// never from the agent-launch path — so it cannot touch the manager lock
	// unsafely. On by default; a no-op when there are no approved plans.
	var replanLane *planning.ReplanLane
	if cfg.Governor.Replan.IsEnabled() {
		rc := cfg.Governor.Replan
		replanLane = planning.NewReplanLane(
			beadStores,
			agentKicker{mgr: agentMgr},
			gov,
			dashboard.NewReplanSink(dashSrv, notifier),
			planning.ReplanLaneConfig{
				IntervalS: rc.IntervalS,
				Stall: planning.StallConfig{
					StallThreshold: time.Duration(rc.StallThresholdS) * time.Second,
					MaxReplans:     rc.MaxReplans,
				},
			}, logger)
		logger.Info("stall-replan lane enabled",
			"interval_s", rc.IntervalS,
			"stall_threshold_s", rc.StallThresholdS,
			"max_replans", rc.MaxReplans)
	}

	var retroLane *retro.Lane
	if cfg.Retro.Enabled {
		retroStore := beadStores[retro.Actor]
		escalationStoreOnce.Do(func() {
			escalationStore = escalation.Load(escalationLedgerPath)
		})
		retroLane = retro.NewLane(beadStores, retroStore, dashSrv.LifecycleTimeline(), escalationStore, retro.Config{
			Enabled:             cfg.Retro.Enabled,
			ScanIntervalS:       cfg.Retro.ScanIntervalS,
			MaxFixAttempts:      cfg.Retro.MaxFixAttempts,
			MaxKicks:            cfg.Retro.MaxKicks,
			LongStallDays:       cfg.Retro.LongStallDays,
			RecentClosedWindowS: cfg.Retro.RecentClosedWindowS,
			AnalysisModel:       cfg.Retro.AnalysisModel,
			AnalysisEndpoint:    cfg.Governor.LiteLLM.ResolveEndpoint(),
			AnalysisAPIKey:      cfg.Governor.LiteLLM.ResolveAPIKey(),
		}, logger)
		if knowledgeAPI != nil {
			retroLane.SetKnowledgeSink(knowledgeAPI)
		}
		logger.Info("retro lane enabled",
			"scan_interval_s", cfg.Retro.ScanIntervalS,
			"max_fix_attempts", cfg.Retro.MaxFixAttempts,
			"max_kicks", cfg.Retro.MaxKicks,
			"long_stall_days", cfg.Retro.LongStallDays,
			"analysis_enabled", cfg.Retro.AnalysisModel != "")
	}

	logger.Info("entering governor loop", "interval_seconds", cfg.Governor.EvalIntervalS)
	lastEvalInterval := cfg.Governor.EvalIntervalS
	ticker := time.NewTicker(time.Duration(cfg.Governor.EvalIntervalS) * time.Second)
	defer ticker.Stop()
	var lastAutoMergeSweep time.Time

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

	// #2573: startup must NOT clear persisted last-kick timestamps. It used to
	// (gov.ClearLastKicks) so that every eligible agent was kicked on the first
	// eval — "one kick per agent per pod boot, by design". But on hosted hives
	// the hub rolls the Deployment for every auto-upgrade, and each roll is a
	// brand-new pod with container restart count 0, so agents on 4h/6h cadences
	// were re-kicked at roll frequency — burning backend tokens ("Bob coins")
	// far beyond any configured cadence, while "next run" (recomputed from the
	// wiped timestamps) showed sooner than the cadence implied. LastKick state
	// is persisted to /data (PVC) after every eval and restored above via
	// SeedLastKicks, so this first eval kicks exactly the agents whose cadence
	// has actually elapsed — including everything after downtime longer than an
	// interval. A hive with no persisted state (fresh install) has no LastKick
	// entries, and every cadenced agent is still kicked here, unchanged.
	logger.Info("startup honors persisted cadence state — first eval kicks only agents whose cadence has elapsed")
	runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, nil, logger)
	runRotationCheck(ctx, cfg, rotationMgr, gov, agentMgr, logger)
	if wd != nil {
		wd.Tick(ctx)
	}
	runAutoMergeSweepIfDue(ctx, ghClient, dashSrv, &lastAutoMergeSweep, logger)
	persistState(agentMgr, gov, cfg, statePath, logger, dashSrv, wd)

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
			persistState(agentMgr, gov, cfg, statePath, logger, dashSrv, wd)
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
			// Watchdog sweep (RFC #4665): synchronous but bounded — every
			// probe carries a deadline and restarts run detached under a hard
			// timeout, so a wedged agent can never stall this tick. Tick
			// self-gates to watchdog.probe_interval_s.
			//
			// It runs BEFORE runEvalCycle so agents it revived join this
			// cycle's resume-kick list rather than waiting a full eval
			// interval. Restarts are detached, so a given sweep's completions
			// are usually collected on the next pass — TakeRestarted drains
			// whatever has finished, and the governor gate gets the final say
			// either way.
			if wd != nil {
				// Re-resolve the mode each sweep so a change saved from the
				// dashboard (or the fleet-wide kill switch being engaged)
				// takes effect without a restart — and so dead-session
				// ownership moves with it. Without this, leaving heal via the
				// settings page would stop the watchdog restarting while the
				// manager's crash loop was still standing down: a window in
				// which NEITHER recovers a dead agent.
				if s, errs := watchdog.SettingsFrom(cfg.Governor.Watchdog); s.Mode != wd.Mode() {
					for _, e := range errs {
						logger.Warn("watchdog config problem", "error", e)
					}
					logger.Info("watchdog mode changed", "from", string(wd.Mode()), "to", string(s.Mode))
					dashSrv.AuditLog("system", "watchdog-mode", "from="+string(wd.Mode())+", to="+string(s.Mode), "")
					wd.SetSettings(s)
					agentMgr.SetDeadSessionRecoveryOwner(s.MayAct())
				}
				wd.Tick(ctx)
				for _, name := range wd.TakeRestarted() {
					dashSrv.AuditLog("system", "restart", "trigger=watchdog", name)
					restarted = append(restarted, name)
				}
			}
			runEvalCycle(ctx, cfg, ghClient, gov, sched, agentMgr, dashSrv, notifier, beadStores, tokenCollector, metricsCollector, nousState, &lastActionable, advisoryStore, advisoryIssues, restarted, logger)
			runRotationCheck(ctx, cfg, rotationMgr, gov, agentMgr, logger)
			runAutoMergeSweepIfDue(ctx, ghClient, dashSrv, &lastAutoMergeSweep, logger)
			// Trajectory review runs after the eval cycle (so kicks/intents are
			// current) on its own cadence, gated by Due().
			if trajLane != nil && trajLane.Due(time.Now()) {
				trajLane.Run(ctx)
			}
			// Stall-replan runs on the same tick, gated by its own Due() cadence.
			// It is synchronous and adds no goroutine; kicks go through the same
			// out-of-band SendKick path as the eval cycle above.
			if replanLane != nil && replanLane.Due(time.Now()) {
				if n := replanLane.Run(ctx); n > 0 {
					logger.Info("stall-replan lane re-kicked stalled plans", "replans", n)
				}
			}
			if retroLane != nil && retroLane.Due(time.Now()) {
				if n := retroLane.Run(ctx); n > 0 {
					logger.Info("retro lane filed advisory beads", "findings", n)
				}
			}
			persistState(agentMgr, gov, cfg, statePath, logger, dashSrv, wd)
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
	// noCadenceAlertID is the never-kicked cause+fix banner (#5577): enabled
	// agents with no cadence in any mode and no kick ever.
	noCadenceAlertID = "agent-no-cadence"
	// providerBudgetAlertID is the PROVIDER spend rebuff (#4294), kept distinct
	// from the two token-budget alerts above so an operator can tell "we used
	// our token allowance" from "the gateway will not spend more money".
	providerBudgetAlertID = "provider-budget-exceeded"
)

// buildRepoActivityWire maps the dashboard activity collector's per-repo
// snapshot into the plain hub wire structs the heartbeat carries. Kept here (in
// the one package that imports both hub and dashboard) so pkg/hub never has to
// import pkg/dashboard back — that would be an import cycle, since dashboard
// already imports hub. A field-by-field copy, mirroring how the fleet-stat
// scalars are lifted out of their snapshot at the beat's build site.
func buildRepoActivityWire(repos []dashboard.RepoActivity) []hub.RepoActivityWire {
	if len(repos) == 0 {
		return nil
	}
	stat := func(s dashboard.ActivityActionStat) hub.ActivityStatWire {
		return hub.ActivityStatWire{Count: s.Count, NewestAt: s.NewestAt}
	}
	out := make([]hub.RepoActivityWire, 0, len(repos))
	for _, r := range repos {
		agents := make([]hub.AgentRepoActivityWire, 0, len(r.Agents))
		for _, a := range r.Agents {
			agents = append(agents, hub.AgentRepoActivityWire{
				Agent:      a.Agent,
				Issues:     stat(a.Issues),
				PRs:        stat(a.PRs),
				Comments:   stat(a.Comments),
				Merges:     stat(a.Merges),
				Claims:     stat(a.Claims),
				Reviews:    stat(a.Reviews),
				Advisory:   stat(a.Advisory),
				Reconciled: stat(a.Reconciled),
			})
		}
		out = append(out, hub.RepoActivityWire{
			Repo:       r.Repo,
			Issues:     stat(r.Issues),
			PRs:        stat(r.PRs),
			Comments:   stat(r.Comments),
			Merges:     stat(r.Merges),
			Claims:     stat(r.Claims),
			Reviews:    stat(r.Reviews),
			Advisory:   stat(r.Advisory),
			Reconciled: stat(r.Reconciled),
			Agents:     agents,
		})
	}
	return out
}

// providerBudgetNotify is the one-shot guard for the provider spend-rebuff
// notification (#4294). runEvalCycle sees the CONDITION every cycle for as long
// as the provider stays clipped, but an operator only needs to be paged on the
// CROSSING — the dashboard banner is what carries the ongoing state. Keyed on
// the latch time (which does not move forward while latched) so a fresh clip
// after a recovery pages again, while the same clip never pages twice.
//
// Package-level because runEvalCycle is a function called once per tick with no
// state of its own; mutex-guarded because it also runs from the startup and
// restart call sites.
var providerBudgetNotify providerBudgetNotifyState

type providerBudgetNotifyState struct {
	mu sync.Mutex
	// notifiedSince is the latch time already notified about; zero when the
	// provider is serving or the current latch has not been notified yet.
	notifiedSince time.Time
}

// shouldSend reports whether this latch still owes the operator a notification,
// and records that it has been sent. Returns true at most once per latch.
func (p *providerBudgetNotifyState) shouldSend(since time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.notifiedSince.IsZero() && p.notifiedSince.Equal(since) {
		return false
	}
	p.notifiedSince = since
	return true
}

// reset forgets the notified latch so the next clip pages again, and reports
// whether a notified latch was in force — true means this cycle is the
// RECOVERY crossing, the one cycle that owes the operator the "serving again"
// notification (the counterpart of shouldSend's entering crossing; every later
// healthy cycle returns false). Called on every cycle where the provider is
// serving.
func (p *providerBudgetNotifyState) reset() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	wasNotified := !p.notifiedSince.IsZero()
	p.notifiedSince = time.Time{}
	return wasNotified
}

// providerBudgetSuppresses reports whether a latched spend rebuff should
// withhold this cycle's kicks, or whether the cycle is a PROBE that must be let
// through.
//
// The latch alone is not enough to keep suppressing, and that is the whole
// subtlety: the only thing that clears the latch is a successful inference
// call, and the only thing that produces inference calls is a kick. Suppressing
// purely on the latch is therefore self-sustaining — on a cadence-only hive it
// would mute the hive permanently while its alert promised recovery at the
// provider's next window reset. Freshness breaks that loop: evidence of being
// clipped expires, and when it does one cycle is spent finding out whether it
// is still true.
func providerBudgetSuppresses(latched bool, lastRebuff, now time.Time, probeInterval time.Duration) bool {
	if !latched {
		return false
	}
	// A latched signal with no stamp (an older snapshot, or a state restored
	// without one) probes immediately rather than suppressing indefinitely:
	// spending one run is recoverable, muting the hive forever is not.
	if lastRebuff.IsZero() {
		return false
	}
	return now.Sub(lastRebuff) < probeInterval
}

// providerBudgetProbe remembers when the last probe kick was RELEASED, which
// providerBudgetSuppresses alone cannot know. Without it, the moment the last
// rebuff went stale EVERY following cycle would release kicks until the probe's
// run reached its first inference call and rebuffed — and agent runs take
// minutes to get there, which at a five-minute eval cadence re-creates a slice
// of the very burn this feature exists to stop. Stamping the release re-arms
// suppression immediately: exactly one probe flies per interval, measured from
// whichever is later — the last observed rebuff or the last released probe.
//
// Package-level for the same reason as providerBudgetNotify: runEvalCycle has
// no state of its own, and this mirrors the process-wide latch it gates.
var providerBudgetProbe providerBudgetProbeState

type providerBudgetProbeState struct {
	mu sync.Mutex
	// lastProbe is when a probe kick was last released; zero when the provider
	// is serving or no probe has flown for the current latch.
	lastProbe time.Time
}

// freshest returns the later of the last observed rebuff and the last released
// probe — the stamp suppression freshness is measured from. A probe whose run
// has not yet reached its first inference call must still hold suppression.
func (p *providerBudgetProbeState) freshest(lastRebuff time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastProbe.After(lastRebuff) {
		return p.lastProbe
	}
	return lastRebuff
}

// markReleased records that a probe kick actually went out this cycle. Only
// called when a kick was really delivered — a probe window that happened to
// find nothing due gathers no evidence and must not re-arm the timer.
func (p *providerBudgetProbeState) markReleased(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastProbe = now
}

// reset forgets the probe stamp. Called on every cycle where the provider is
// serving, so a new latch starts its probe clock from its own rebuffs.
func (p *providerBudgetProbeState) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastProbe = time.Time{}
}

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

// applyNoCadenceAlert keeps the never-kicked cause+fix banner (#5577) in sync
// with the governor's view: raised (warning, not error — the hive is not
// broken, it is unconfigured) while any enabled, governor-kickable agent has
// no cadence in any mode and has never been kicked; cleared the moment the
// operator sets a cadence or any kick path reaches the agent. This is the
// spoke-side parity for the hub verdict's no-cadence amber: the same
// governor-derived signal, rendered where the operator can act on it, with no
// hub round-trip.
func applyNoCadenceAlert(gov *governor.Governor, dashSrv *dashboard.Server) {
	agents := gov.NoCadenceAgents()
	if len(agents) == 0 {
		dashSrv.ClearSystemAlert(noCadenceAlertID)
		return
	}
	dashSrv.AddSystemAlert(noCadenceAlertID, "warning", noCadenceAlertMessage(agents))
}

// noCadenceAlertMessage renders the banner line: symptom, cause AND fix — the
// exact gap the RFC calls out in the dashboard's not-producing warnings,
// which name only the symptom.
func noCadenceAlertMessage(agents []string) string {
	return fmt.Sprintf("agent(s) %s enabled but never kicked — no cadence configured; set cadences on the agent card",
		strings.Join(agents, ", "))
}

// agentKicker adapts *agent.Manager to planning.Kicker for the Phase 3
// stall-replan lane. Kick delegates to SendKick, which takes the manager lock
// ITSELF and is only ever called here from the governor tick (never from the
// agent-launch path), so it cannot re-enter a held manager lock. This is the
// same out-of-band kick path the eval loop already uses for governor kicks.
type agentKicker struct{ mgr *agent.Manager }

func (k agentKicker) Kick(agent, message string) error {
	return k.mgr.SendKick(agent, message)
}

// planFromLabeledIssues is Phase 4 Part B: for each actionable issue carrying a
// `plan`/`epic` label, mint an epic (idempotent) and hand it to the architect,
// RESPECTING the architect's pause. It is a plain synchronous call on the eval
// tick — no goroutine — and only ever touches the manager via SendKick/IsPaused,
// exactly like every governor kick, so it never re-enters the launch-path mutex.
// Epics are minted into the architect store (falling back to any store) so the
// dashboard plan-review flow and replan lane find them the same way.
//
// A minted epic is decompose_pending (plan_status=draft). While the architect is
// paused, the request stays queued and visible (the PLANNING tile shows it as
// pending) — we never force-unpause. Once the architect is available, we kick it
// each cycle until it decomposes and clears the pending marker.
func planFromLabeledIssues(
	actionable *github.ActionableResult,
	beadStores map[string]*beads.Store,
	agentMgr *agent.Manager,
	gov *governor.Governor,
	dashSrv *dashboard.Server,
	logger *slog.Logger,
	acmmLevel int,
) {
	if actionable == nil || len(beadStores) == 0 {
		return
	}
	store, ok := beadStores[planning.ArchitectAgentName]
	if !ok {
		for name := range beadStores {
			store = beadStores[name]
			break
		}
	}
	if store == nil {
		return
	}

	sink := labelPlanSink{gov: gov, dashSrv: dashSrv, logger: logger}
	planning.PlanIssuesFromLabels(store, agentMgr, actionable.Issues.Items, sink,
		func(ref string, err error) {
			logger.Warn("plan-from-label: minting epic failed", "issue", ref, "error", err)
		}, acmmLevel)
}

// labelPlanSink adapts the governor/dashboard/logger to planning.LabelPlanSink so
// the label-trigger core lives (and is tested) in pkg/planning.
type labelPlanSink struct {
	gov     *governor.Governor
	dashSrv *dashboard.Server
	logger  *slog.Logger
}

func (s labelPlanSink) KickedPlan(epic *beads.Bead) {
	s.gov.RecordKick(planning.ArchitectAgentName)
	if s.dashSrv != nil {
		s.dashSrv.AuditLog("planning", "plan_from_label", "epic="+epic.ID+" ref="+epic.ExternalRef, planning.ArchitectAgentName)
	}
	s.logger.Info("audit: plan requested from labeled issue", "epic", epic.ID, "ref", epic.ExternalRef)
}

func (s labelPlanSink) QueuedPlan(epic *beads.Bead, paused bool) {
	if paused {
		// Architect deliberately paused — queue, do not unpause, log once.
		s.logger.Info("plan-from-label: architect paused, plan queued", "epic", epic.ID, "ref", epic.ExternalRef)
		return
	}
	s.logger.Warn("plan-from-label: architect unavailable, plan queued", "epic", epic.ID, "ref", epic.ExternalRef)
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
	d := diagnoseGitHubAppFull(ctx, appAuth, expectedOwner)
	return d.Message(), d.State
}

// diagnoseGitHubAppFull is diagnoseGitHubApp without the lossy projection to
// (message, state). Callers that only need the banner should keep using the
// wrapper above; this exists for the one caller that also reports the granted
// Actions and Commit-statuses permissions (#4030), which the projection drops.
func diagnoseGitHubAppFull(ctx context.Context, appAuth *github.AppAuth, expectedOwner string) github.AppAuthDiagnosis {
	if appAuth == nil {
		return github.AppAuthDiagnosis{State: github.AppStateOK, ExpectedAccount: expectedOwner}
	}
	return appAuth.DiagnoseAppAuth(ctx, expectedOwner, spokeAppKeyPath, spokeProvisionedAppKeyPath)
}

// maxTimelineEnumeratePerCycle bounds how many enumerated-issue events a single
// eval cycle records into the lifecycle timeline, keeping the recording loop
// O(1)-bounded so it never slows the eval cycle. Since #5656 the store dedupes
// by (ref, kind) — re-enumeration refreshes the journey instead of appending —
// so the cap no longer protects the ring from eviction floods; it matches the
// endpoint's default journey limit so every renderable journey gets refreshed.
const maxTimelineEnumeratePerCycle = 200

// lifecycleRecorder narrows *dashboard.Server to just the timeline accessor the
// recording helpers need, so they stay trivially testable with a fake and never
// depend on the rest of the Server surface.
type lifecycleRecorder interface {
	LifecycleTimeline() *timeline.Store
}

// recordEnumeratedIssues records a KindEnumerated event for each enumerated
// actionable issue, bounded by maxTimelineEnumeratePerCycle. The store dedupes
// by (ref, kind), so each cycle refreshes the journeys' enumerated stage
// rather than appending a flood (#5656). It is fully guarded: a nil recorder,
// nil store, or nil actionable set is a no-op, and Record itself is nil-safe.
// This must never slow or break the eval loop, so it does no blocking I/O
// (journey persistence is throttled and atomic inside the store).
//
// PR-open/merge lifecycle spans are emitted by the same tracing mapper when
// callers record those timeline events; this helper only has enumerated issues.
func recordEnumeratedIssues(ctx context.Context, rec lifecycleRecorder, actionable *github.ActionableResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rec == nil || actionable == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	limit := maxTimelineEnumeratePerCycle
	if len(actionable.Issues.Items) < limit {
		limit = len(actionable.Issues.Items)
	}
	for i := 0; i < limit; i++ {
		issue := actionable.Issues.Items[i]
		event := timeline.Event{
			IssueRef: issueRef(issue.Repo, issue.Number),
			Kind:     timeline.KindEnumerated,
		}
		_, span := tracing.StartTimelineSpan(ctx, event)
		store.Record(event)
		span.End()
	}
}

// recordKick records KindKicked events for the given agent. When issue refs are
// supplied it records one issue-scoped event per ref so post-completion lanes
// can reconstruct per-bead kick counts. With no refs, it records one
// agent-scoped event. Guarded and nil-safe; no I/O.
func recordKick(ctx context.Context, rec lifecycleRecorder, agent string, issueRefs ...string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rec == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	if len(issueRefs) > 0 {
		for _, ref := range issueRefs {
			if ref == "" {
				continue
			}
			event := timeline.Event{
				IssueRef: ref,
				Kind:     timeline.KindKicked,
				Agent:    agent,
			}
			_, span := tracing.StartTimelineSpan(ctx, event)
			store.Record(event)
			span.End()
		}
		return
	}
	event := timeline.Event{
		Kind:  timeline.KindKicked,
		Agent: agent,
	}
	_, span := tracing.StartTimelineSpan(ctx, event)
	store.Record(event)
	span.End()
}

// issueRef renders the canonical "repo#number" reference the timeline uses to
// group events by issue. An empty repo yields an empty ref (the store tolerates
// it).
func issueRef(repo string, number int) string {
	if repo == "" {
		return ""
	}
	return fmt.Sprintf("%s#%d", repo, number)
}

func actionableIssueRef(issue github.Issue) string {
	ref := worksource.Ref{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: issue.ExternalID,
		Number:     issue.Number,
		URL:        issue.URL,
	}
	if key := ref.Key(); key != "" {
		return key
	}
	if issue.Repo != "" {
		return issue.Repo
	}
	return issue.ExternalID
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
	var d github.AppAuthDiagnosis
	for attempt := 1; attempt <= githubAppBannerAttempts; attempt++ {
		d = diagnoseGitHubAppFull(ctx, appAuth, expectedOwner)
		msg, state = d.Message(), d.State
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

	// #4030: record the Actions and Commit-statuses grants alongside the
	// verdict. These are NOT required of the Hive App and their absence is not
	// a fault — the optional Visual Hive App exists so they never have to be.
	// But an installation that has not approved them is otherwise
	// indistinguishable from one that has, both reporting "ok", which is what
	// would make a half-approved fleet invisible during any later
	// consolidation.
	//
	// It is deliberately emitted for EVERY verdict, including AppStateOK.
	// Gating it on a fault would defeat the purpose: the half-approved
	// installation is the one that looks healthy. It costs no extra API call —
	// the diagnosis above already fetched the installation.
	//
	// Note this runs where verdicts are computed, not on every eval cycle:
	// every caller reaches here from a failed GitHub call or from the
	// dashboard's Re-check. Re-check is therefore the operator-invokable way to
	// read a specific installation's grants.
	// #5774: record the write-path grants on the SAME line, for the same
	// reason and with the same posture. The App migration that blocked every
	// agent PR flow was invisible here because this verdict read Issues and
	// nothing else: a hive that could file issues and could not push a branch
	// reported "ok", and so did a healthy one. Contents/Pull-requests/Workflows
	// are recorded, never enforced — see GrantsAgentPushFlow for why requiring
	// them would misreport the read-only advisory tier — and, like the grants
	// above, they are emitted for EVERY verdict including AppStateOK, because
	// the installation that looks healthy is precisely the one worth counting.
	logger.Info("github app credential verdict",
		"owner", expectedOwner, "state", state.String(),
		"grants", d.ExecutionGrants(),
		"visual_hive_execution_grants", d.GrantsVisualHiveExecution(),
		"push_flow_grants", d.PushFlowGrants(),
		"agent_push_flow_grants", d.GrantsAgentPushFlow())
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

// classifyGitHubAppRepoCoverage (#4360) asks the deterministic question the
// other classifiers cannot: does this installation actually COVER the repos
// this hive is configured to work on?
//
// Everything else here reasons from a failed call. diagnoseGitHubApp inspects
// installation-level PERMISSIONS and never repo scope, and
// classifyGitHubAppWriteForbidden infers scope from a 403 after a write has
// already failed. Neither can see the case that prompted this: a hive pointed
// at a second repo in the right org, on the right installation, simply not
// ticked in the App's selected repos. GitHub answers 404 for that — the same
// answer it gives for a repo that does not exist — so the read path reported
// "app not installed / no read" and the dashboard blamed an undelivered
// private key that had in fact arrived. The operator was sent to re-upload a
// key, which could not possibly help.
//
// An error here is NOT a verdict. If the listing cannot be fetched the
// credentials themselves are the more likely story, and the existing checks
// tell it better; this returns raise=false and lets them run.
func classifyGitHubAppRepoCoverage(ctx context.Context, appAuth *github.AppAuth, org string, repos []string, logger *slog.Logger) (raise bool, msg string, state github.AppAuthState) {
	if appAuth == nil || len(repos) == 0 {
		return false, "", github.AppStateUnknown
	}

	cov, err := appAuth.InstallationCoverage(ctx)
	if err != nil {
		logger.Debug("github app repo coverage: could not list installation repositories — deferring to the credential checks",
			"org", org, "error", err)
		return false, "", github.AppStateUnknown
	}

	missing := cov.Missing(org, repos)
	if len(missing) == 0 {
		return false, "", github.AppStateOK
	}

	// #5774: a coverage miss whose shape is an org TRANSFER gets its own
	// verdict, checked first because the not-covered copy is actively wrong for
	// it. "Tick this repo in the installation's repository access" cannot be
	// followed when the repository has left that account — there is nothing
	// there to tick — and this classifier exists in the first place because
	// sending an operator to a fix that cannot work costs them real debugging
	// time. MovedTo returns nothing unless the shape is unambiguous (see its
	// three clauses), so the not-covered verdict below remains the default.
	if moves := cov.MovedTo(org, repos); len(moves) > 0 {
		d := github.AppAuthDiagnosis{
			State:           github.AppStateRepoMoved,
			ExpectedAccount: org,
			InstallationID:  appAuth.InstallationID(),
			APIURL:          appAuth.APIURL(),
			RepoMoves:       moves,
		}
		logger.Warn("github app repo coverage: configured repositories were transferred to another account",
			"configured_org", org, "now_under", github.MovedOwner(moves), "repos", len(moves))
		return true, d.Message(), github.AppStateRepoMoved
	}

	d := github.AppAuthDiagnosis{
		State:           github.AppStateRepoNotCovered,
		ExpectedAccount: org,
		InstallationID:  appAuth.InstallationID(),
		APIURL:          appAuth.APIURL(),
		Repos:           missing,
	}
	return true, d.Message(), github.AppStateRepoNotCovered
}

// primaryAdvisoryRepo returns the repo the advisory digest is posted to: the
// configured primary repo, falling back to the first listed repo. Shared by the
// boot ensure, the per-cycle re-ensure, and the post path so all three can never
// disagree about WHICH repo's pinned issue the digest belongs to.
func primaryAdvisoryRepo(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.Project.PrimaryRepo != "" {
		return cfg.Project.PrimaryRepo
	}
	if len(cfg.Project.Repos) > 0 {
		return cfg.Project.Repos[0]
	}
	return ""
}

// advisoryIssueUnresolved reports whether the pinned advisory issue for repo is
// still unknown, i.e. the digest has nowhere to go. A recorded 0 counts as
// unresolved: it is the zero value a failed ensure would leave behind, and
// posting to issue 0 is not a thing.
func advisoryIssueUnresolved(advisoryIssues map[string]int, repo string) bool {
	num, ok := advisoryIssues[repo]
	return !ok || num <= 0
}

func advisoryIssueNumber(advisoryIssues map[string]int, repo string) (int, bool) {
	num, ok := advisoryIssues[repo]
	return num, ok && num > 0
}

func shouldBuildAdvisoryDigest(beadStores map[string]*beads.Store, ghClient *github.Client, hasExistingPinnedIssue bool) bool {
	if len(beadStores) > 0 {
		return true
	}
	return ghClient != nil && hasExistingPinnedIssue
}

func shouldPostAdvisoryDigest(digest *advisory.Digest, ghClient *github.Client, hasPinnedIssue bool) bool {
	if digest == nil {
		return false
	}
	if digest.TotalCount > 0 || len(digest.RecentlyResolved) > 0 {
		return true
	}
	return ghClient != nil && hasPinnedIssue
}

// advisoryPostGate tracks, per repo, when the digest was last SUCCESSFULLY
// posted, so governor.advisory.update_interval_s (#4820) can throttle the
// GitHub round-trip. Package-level because runEvalCycle carries no state of
// its own, and mutex-guarded because startup/restart call sites exist besides
// the ticker. clampLogged makes the "interval clamped" warning a one-shot
// instead of a per-cycle drone.
var advisoryPostGate = struct {
	mu          sync.Mutex
	lastSuccess map[string]time.Time
	clampLogged bool
}{lastSuccess: map[string]time.Time{}}

// advisoryPostDue reports whether the update-interval gate is open for a post
// attempt to repo, logging (once) if the configured value was clamped. An
// interval of 0 (unset knob) means the gate is ALWAYS open — the digest posts
// every eval cycle, exactly the pre-#4820 cadence — and a repo with no
// successful post since process start is open too, so the first post is never
// delayed. The gate advances only on SUCCESS (recordAdvisoryPostSuccess,
// mirroring how the #4818 skip-guard records its hash): a failed attempt is
// retried on the very next cycle instead of waiting out the interval, keeping
// error recovery — and the hub's staleness signal — as prompt as today.
func advisoryPostDue(advCfg config.AdvisoryConfig, repo string, now time.Time, logger *slog.Logger) bool {
	interval := advCfg.UpdateInterval()
	advisoryPostGate.mu.Lock()
	defer advisoryPostGate.mu.Unlock()
	if raw := advCfg.UpdateIntervalS; raw > 0 && time.Duration(raw)*time.Second != interval && !advisoryPostGate.clampLogged {
		advisoryPostGate.clampLogged = true
		logger.Warn("advisory update_interval_s outside allowed bounds — clamped",
			"configured_s", raw, "effective_s", int(interval.Seconds()),
			"min_s", config.MinAdvisoryUpdateIntervalS, "max_s", config.MaxAdvisoryUpdateIntervalS)
	}
	if interval <= 0 {
		return true
	}
	last, ok := advisoryPostGate.lastSuccess[repo]
	return !ok || now.Sub(last) >= interval
}

// recordAdvisoryPostSuccess advances the update-interval gate for repo after a
// successful digest write. Skip-if-unchanged cycles count too: pkg/github
// returns nil for them by design so freshness advances (#4818/#4821), and an
// unchanged digest is exactly the case the throttle exists to quiet.
func recordAdvisoryPostSuccess(repo string, now time.Time) {
	advisoryPostGate.mu.Lock()
	defer advisoryPostGate.mu.Unlock()
	advisoryPostGate.lastSuccess[repo] = now
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

// actionableAfterGitHubEnumerate decides whether an eval cycle survives a
// failed GitHub enumeration. On the default (GitHub) work source the answer
// is no: an all-repos failure usually means a rate limit or outage, and a
// zero-count result would idle the agents, so the cycle keeps prior state.
//
// On a non-default work source (e.g. Linear) the GitHub call is only there
// for PR maintenance; the backlog comes from the work-source overlay that
// runs next. Aborting here meant a Linear-sourced hive whose GitHub App could
// not list issues (403 "Resource not accessible by integration", an Issues
// permission a Linear hive should not need) never enumerated its Linear
// backlog at all and sat at queue 0. Such a hive continues with whatever
// partial result GitHub returned (nil becomes an empty result; PRs are kept
// when obtainable) and lets the overlay populate issues.
func actionableAfterGitHubEnumerate(cfg *config.Config, actionable *github.ActionableResult, err error, logger *slog.Logger) (*github.ActionableResult, bool) {
	if err == nil {
		return actionable, true
	}
	wsType := cfg.Governor.WorkSource.Type
	if wsType == "" || wsType == "github" {
		logger.Error("failed to enumerate actionable items", "error", err)
		return nil, false
	}
	logger.Warn("GitHub enumeration failed; continuing so the configured work source can still populate issues",
		"work_source", wsType, "error", err)
	if actionable == nil {
		actionable = &github.ActionableResult{GeneratedAt: time.Now()}
	}
	return actionable, true
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
	restartedAgents []string,
	logger *slog.Logger,
) {
	// Governor eval-cycle span. When tracing is disabled (the default) this is
	// a no-op span with no allocation of note and no export — see pkg/tracing.
	ctx, span := tracing.StartSpan(ctx, "governor.eval_cycle",
		attribute.String("hive.id", cfg.HiveID),
		attribute.Int(tracing.AttrHiveACMMLevel, inferACMMLevel(cfg)))
	defer span.End()

	// A hive running without GitHub credentials (placeholder app_id, or a real
	// App whose key could not be read) has nothing to enumerate. Return before
	// the first API call rather than logging a misleading enumeration failure
	// once per eval interval — the dashboard banner already states the cause.
	if ghClient == nil {
		logger.Debug("skipping eval cycle: hive is running without GitHub credentials")
		return
	}

	// Re-ensure the pinned advisory issue whenever it is still unresolved, not
	// only while the App banner is up (#4167). The startup ensure can fail for
	// reasons that deliberately do NOT raise that banner — a rate limit, a 5xx,
	// a search-API blip — and the old gate meant such a hive kept an empty
	// advisoryIssues map for the rest of the process lifetime: every later
	// digest silently found no issue to post to, so the pinned comment froze at
	// whatever it last said. Retrying here is cheap (one search per eval cycle
	// only while unresolved, nothing once resolved) and is the difference
	// between a transient boot error and a permanently wedged digest.
	//
	// advisoryEnsureErr keeps this cycle's ensure failure so the post-path
	// error recorded below can name the CAUSE (e.g. Issues disabled on a fork,
	// #4329) instead of only the symptom.
	var advisoryEnsureErr error
	primaryRepoAtCycleStart := primaryAdvisoryRepo(cfg)
	_, hadPinnedAdvisoryIssueAtCycleStart := advisoryIssueNumber(advisoryIssues, primaryRepoAtCycleStart)
	if primaryRepoAtCycleStart != "" && ghClient != nil {
		if advisoryIssueUnresolved(advisoryIssues, primaryRepoAtCycleStart) {
			num, retryErr := ghClient.EnsureAdvisoryIssue(ctx, primaryRepoAtCycleStart)
			if retryErr == nil {
				advisoryIssues[primaryRepoAtCycleStart] = num
				_ = os.Setenv("HIVE_ADVISORY_ISSUE", fmt.Sprintf("%d", num)) // valid key/value; Setenv cannot fail on Unix
				logger.Info("advisory issue resolved on retry", "repo", primaryRepoAtCycleStart, "number", num)
			} else {
				advisoryEnsureErr = retryErr
				logger.Warn("advisory issue still unresolved — digest cannot be posted this cycle",
					"repo", primaryRepoAtCycleStart, "error", retryErr)
			}
		}
	}

	enumCtx, enumSpan := tracing.StartSpan(ctx, "governor.enumerate_actionable")
	actionable, err := ghClient.EnumerateActionable(enumCtx)
	enumSpan.End()
	actionable, ok := actionableAfterGitHubEnumerate(cfg, actionable, err, logger)
	if !ok {
		return
	}

	// If a non-default work source is configured, overlay its issues onto
	// the actionable result. PRs always come from GitHub.
	if wsType := cfg.Governor.WorkSource.Type; wsType != "" && wsType != "github" {
		ghToken := cfg.GitHub.Token
		if ghToken == "" {
			ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
		}
		ws, wsErr := worksource.FromConfig(cfg.Governor.WorkSource, ghClient, ghToken, cfg.Project.Org, logger)
		actionable.Issues = workSourceIssuesForCycle(ctx, ws, wsErr, cfg.Governor.Labels.Exempt, cfg.Project.IssueFilter, logger)
	}

	ghClient.EnrichCIStatus(ctx, actionable.PRs.Items)

	// Fold this pass's CI state into the fix-loop staleness clock BEFORE any
	// consumer reads it, so the claim-suppression guard (#3), the merge watcher
	// (#2), and the stuck-PR reaper (#4) all key off a consistent, current
	// signal within the same tick. This only records first-seen-red times; the
	// distinct-SHA attempt counting still happens in runEscalationSweep below.
	recordRedStaleness(cfg, actionable)

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

	// Record enumerated issues into the lifecycle timeline so the dashboard's
	// lifecycle view has real data. Cheap and fully guarded: a nil dashboard or
	// nil store is a no-op (timeline.Store.Record is nil-safe), the loop is
	// bounded by maxTimelineEnumeratePerCycle, and the store dedupes by
	// (ref, kind) so this per-cycle sweep refreshes journeys instead of
	// flooding them (#5656).
	recordEnumeratedIssues(ctx, dashSrv, actionable)

	escalatedPRs := runEscalationSweep(ctx, cfg, governorForge(cfg, ghClient, logger), actionable, notifier, dashSrv, logger)

	intentVerdicts := writeIntentVerdicts(ctx, cfg, ghClient, actionable, beadStores, logger)
	refreshReviewVerdicts(cfg, logger)
	requiredCheckSet, _ := cfg.AutoMerge.RequiredCheckSet()
	writeMergeEligible(actionable, actionable.Hold, cfg.Project.Org, escalatedPRs, cfg.Intent.Enforce, intentVerdicts, cfg.Review.RequireApproval, requiredCheckSet, logger)

	// Stuck-PR reaper (backstop): re-dispatch a fix for any hive-authored PR
	// that is red on a required check AND stale (its red head SHA unchanged past
	// RedPRStaleAfter). writeMergeEligible already surfaces every red PR into
	// ci-failing.json (the CI_FAILING kick block), so the PR is already in the
	// work list; the reaper's job is to guarantee a STALE one is not silently
	// abandoned, to dedup the dispatch via the escalation store's re-engagement
	// cap (so a permanently-red PR is not re-nudged every tick forever), and to
	// stand down for PRs already escalated to a human. Composes with the merge
	// watcher (#2): both go through the same TryReEngage cap, so the same PR is
	// never double-dispatched within a red-SHA's budget.
	reapStuckRedPRs(cfg, actionable, escalatedPRs, logger)

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

	// Cause+fix banner for the never-kicked class (#5577): the dashboard's
	// not-producing warnings name the SYMPTOM (agent idle, zero tokens); this
	// names the cause — enabled agent, no cadence in any mode, never kicked —
	// and the fix. Computed from the spoke's own governor config, no hub
	// round-trip; self-clears the moment a cadence is set or any kick lands.
	applyNoCadenceAlert(gov, dashSrv)

	agentsDue := gov.Evaluate(
		actionable.Issues.Count,
		actionable.PRs.Count,
		actionable.Hold.Total,
		actionable.Issues.SLAViolations,
	)

	// Crash-restarted agents may get a "resume" kick ahead of their cadence
	// slot so work interrupted mid-task resumes promptly — but ONLY through
	// the governor's gate (#2573). Unconditionally kicking every restarted
	// agent meant a crash-looping CLI was kicked on every eval cycle,
	// burning backend tokens far faster than any configured cadence and
	// bypassing the budget gate; AllowResumeKick bounds resume kicks to one
	// per cadence interval and respects mode pauses and the budget.
	agentsDue = mergeResumeKicks(agentsDue, restartedAgents, gov.AllowResumeKick, logger)

	govState := gov.GetState()
	span.SetAttributes(
		attribute.String(tracing.AttrHiveGovernorMode, string(govState.Mode)),
		attribute.Int("hive.queue.issues", govState.QueueIssues),
		attribute.Int("hive.queue.prs", govState.QueuePRs),
		attribute.Int("hive.queue.hold", govState.QueueHold),
	)
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
	// Operator-paused agents must consume NOTHING (#2573); see
	// filterKickableAgents for the full gate.
	agentsDue = filterKickableAgents(agentsDue, cfg.Agents, config.OnDemandAgentsFromPacks(), agentMgr.IsPaused)

	// PROVIDER SPEND REBUFF (#4294). When the inference gateway is refusing on a
	// money limit, every kick launched this cycle is a run that cannot buy a
	// single token — precisely the failure this addresses: a hive that kept
	// firing its cadence into a gateway rejecting 100% of requests all day,
	// silently, until the provider's spend window happened to roll over.
	//
	// The alert is raised HERE, before kick assembly, so an operator is told
	// even on a cycle where nothing happened to be due. The actual suppression
	// happens after every kick source has contributed — see below.
	providerBudgetCause, providerBudgetSince, providerBudgetLastRebuff, providerBudgetRebuffs := dashboard.InferenceBudgetExceeded()
	// Suppress only while the latch is FRESH. Withholding kicks also withholds
	// the inference calls that clear the latch, so a hive whose only kick source
	// is the governor cadence would never learn the provider's window reset and
	// would stay muted forever — the exact topology in the field report. Once
	// the last rebuff is older than the probe interval, this cycle's kicks go
	// through as a probe: still clipped re-freshens the stamp and suppression
	// resumes, served clears the latch outright.
	providerBudgetProbeInterval := cfg.Governor.ProviderBudget.EffectiveProbeInterval()
	providerBudgetLatched := providerBudgetCause != ""
	suppressKicks := providerBudgetSuppresses(providerBudgetLatched,
		providerBudgetProbe.freshest(providerBudgetLastRebuff), time.Now(), providerBudgetProbeInterval)
	if providerBudgetLatched {
		state := "agent kicks suspended"
		if !suppressKicks {
			state = "probing with a single agent kick to test whether the provider window has reset"
		}
		msg := fmt.Sprintf("provider spending limit reached — %s: %s", state, providerBudgetCause)
		if providerBudgetRebuffs > 1 {
			msg = fmt.Sprintf("provider spending limit reached (%d refused calls since %s) — %s: %s",
				providerBudgetRebuffs, providerBudgetSince.Format(time.RFC1123), state, providerBudgetCause)
		}
		dashSrv.AddSystemAlert(providerBudgetAlertID, "error", msg)
		providerBudgetCause = msg
	} else {
		if reason := quotaExhaustedAgentReason(quotaExhaustedProcessCount(agentMgr.AllStatuses())); reason != "" {
			dashSrv.AddSystemAlert(providerBudgetAlertID, "error", "provider quota exhausted — "+reason)
		} else {
			dashSrv.ClearSystemAlert(providerBudgetAlertID)
		}
	}
	// Notify ONCE per latch, not once per cycle. The deduped banner above
	// already carries the ongoing state; a high-priority notification repeated
	// every eval cycle for as long as the provider stays clipped is a day of
	// pages saying the same thing. Keyed on the latch time, which recordRebuff
	// deliberately does not move forward, so a genuinely new clip after a
	// recovery notifies again. Matches applyBudgetAlerts, which notifies on the
	// crossing rather than on the condition.
	notifyProviderBudget := providerBudgetLatched && providerBudgetNotify.shouldSend(providerBudgetSince)
	if !providerBudgetLatched {
		providerBudgetProbe.reset()
		// The RECOVERY crossing: the latch a notification went out for has
		// cleared (a probe's inference call succeeded), so tell the operator
		// once that the hive resumed — the counterpart of the entering page,
		// without which the only signal of recovery is a banner quietly
		// vanishing. Every later healthy cycle is silent.
		if providerBudgetNotify.reset() {
			logger.Info("provider spending limit lifted: agent kicks resumed")
			notifier.Send("Provider spending limit lifted",
				"the inference provider is serving again — agent kicks have resumed",
				notify.PriorityDefault)
		}
	}

	// ADDITIVE CEL routing: evaluate operator-defined CEL trigger rules
	// (cfg.Triggers, pkg/celtrigger) against the items enumerated this cycle and
	// UNION any matched, gated agents into agentsDue. This runs alongside — never
	// instead of — the label/governor selection above: unionAgents only ever adds
	// names, and celMatchedAgents enforces the same pause/budget/on-demand gates,
	// so a CEL match can neither remove a governor-selected agent nor bypass a
	// paused agent or exhausted budget. Fully guarded/cheap: with no `triggers:`
	// configured, celEngineFor returns nil and celMatchedAgents does zero work.
	if celEngine := celEngineFor(cfg, logger); celEngine != nil {
		celAgents := celMatchedAgents(celEngine, actionable, cfg, gov, agentMgr.IsPaused, logger)
		if len(celAgents) > 0 {
			before := len(agentsDue)
			agentsDue = unionAgents(agentsDue, celAgents)
			if len(agentsDue) > before {
				logger.Info("celtrigger: unioned CEL-matched agents into due set",
					"added", agentsDue[before:], "cel_matched", celAgents)
			}
		}
	}

	// #4247/#4263 (parent #3845): apply the shared convergence admission at the
	// internal-kick dispatch boundary — AFTER governor policy evaluated the raw
	// queue, BEFORE the scheduler caches and renders issues. Gated by the
	// runtime convergence rollout mode, captured once per pass: with mode "off"
	// (the DEFAULT) this is entirely inert and kickActionable IS actionable;
	// "shadow" logs and records what would be withheld but still dispatches the
	// raw population; only "enforce" gates the scheduled/cached issue payloads
	// below. The raw actionable population stays authoritative for governor
	// policy, dashboard status, PR/review dispatch, escalation, and every path
	// not explicitly enrolled.
	kickActionable := applyConvergenceKickAdmission(cfg, dashSrv, actionable, notifier, logger)

	sched.SetLastActionable(kickActionable)
	reviewPlan := planReviewDispatch(cfg, actionable, agentMgr, logger)
	messages := sched.BuildKickMessages(kickActionable, agentsDue)
	reviewKickByMessage := map[string]review.DispatchKick{}
	for _, k := range append(reviewPlan.ReviewKicks, reviewPlan.FixKicks...) {
		if !gov.AgentEligibleForCELKick(k.Agent) || agentMgr.IsPaused(k.Agent) {
			logger.Info("review swarm kick suppressed by governor gate", "agent", k.Agent, "pr", k.PRRef)
			continue
		}
		messages = append(messages, scheduler.KickMessage{Agent: k.Agent, Message: k.Message, IssueRefs: []string{k.PRRef}})
		reviewKickByMessage[k.Agent+"\x00"+k.Message] = k
	}
	// #4294: drop EVERY assembled kick while the provider is refusing on a
	// spending limit. Placed after all three sources have contributed —
	// governor-due agents, the CEL union, and the review swarm — because gating
	// only `agentsDue` earlier would still let a CEL match or a review kick fire
	// into the same clipped key.
	//
	if len(messages) > 0 {
		filtered := messages[:0]
		for _, msg := range messages {
			if remaining, class, line, ok := agentMgr.ProviderErrorBackoffRemaining(msg.Agent); ok {
				logger.Warn("provider inference error: withholding agent kick during backoff",
					"agent", msg.Agent,
					"class", class,
					"retry_in", remaining.Round(time.Second),
					"error", line)
				continue
			}
			filtered = append(filtered, msg)
		}
		messages = filtered
	}

	// Suppression is total rather than per-agent because the limit is on the
	// KEY: no agent can succeed while it is clipped. It self-heals — the first
	// inference call that succeeds after the provider's window resets clears the
	// signal — so there is no timer to tune and no operator action required.
	//
	// Deliberately does NOT force-pause agents. Operator pause state is a human
	// decision (#2573) and must not be forged by an automatic signal that will
	// clear itself; withholding kicks achieves the saving without leaving paused
	// agents behind for a human to un-pause by hand.
	//
	// Probe cycle: when the last rebuff has gone stale, ONE kick is
	// deliberately allowed through to find out whether the provider is
	// serving again. Its inference calls are what clear the latch (on a
	// 2xx) or re-freshen it (on another rebuff) — nothing else can. Only
	// one: the question is "is the window still clipped", and every kick
	// beyond the first spends a run to learn the same answer. Releasing it
	// re-arms suppression immediately, so the cycles while the probe's run
	// is still in flight withhold again rather than leaking more kicks.
	kickGate := gateKickMessagesForProviderBudget(messages, suppressKicks, providerBudgetLatched)
	releaseProviderBudgetProbe := kickGate.ReleaseProbe
	if suppressKicks && len(messages) > 0 {
		logger.Warn("provider spending limit: withholding agent kicks",
			"withheld", kickGate.Withheld, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince,
			"next_probe_in", (providerBudgetProbeInterval - time.Since(providerBudgetProbe.freshest(providerBudgetLastRebuff))).Truncate(time.Second))
	} else if releaseProviderBudgetProbe {
		if len(kickGate.Withheld) > 0 {
			logger.Warn("provider spending limit: withholding all but the probe kick",
				"withheld", kickGate.Withheld, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince)
		}
		logger.Info("provider spending limit: releasing a single probe kick",
			"probe_agent", kickGate.Kept[0].Agent, "rebuffs", providerBudgetRebuffs, "since", providerBudgetSince,
			"last_rebuff", providerBudgetLastRebuff, "probe_interval", providerBudgetProbeInterval)
	}
	messages = kickGate.Kept
	if notifyProviderBudget {
		notifier.Send("Provider spending limit reached", providerBudgetCause, notify.PriorityHigh)
	}

	var deliveredReviewKicks []review.DispatchKick
	if len(messages) > 0 {
		for _, msg := range messages {
			if remaining, class, line, ok := agentMgr.ProviderErrorBackoffRemaining(msg.Agent); ok {
				logger.Warn("provider inference error: withholding agent kick during backoff",
					"agent", msg.Agent,
					"class", class,
					"retry_in", remaining.Round(time.Second),
					"error", line)
				continue
			}
			agentCfg := cfg.Agents[msg.Agent]
			_, kickSpan := tracing.StartSpan(ctx, "agent.kick", tracing.AgentKickAttributes(
				msg.Agent,
				agentCfg.Backend,
				agentCfg.Model,
				agentCfg.Role,
				string(govState.Mode),
				inferACMMLevel(cfg),
			)...)
			logger.Info("audit: governor kicking agent", "agent", msg.Agent, "trigger", "governor-eval")
			if err := agentMgr.SendKick(msg.Agent, msg.Message); err != nil {
				kickSpan.RecordError(err)
				kickSpan.End()
				logger.Warn("failed to send kick", "agent", msg.Agent, "error", err)
				continue
			}
			if k, ok := reviewKickByMessage[msg.Agent+"\x00"+msg.Message]; ok {
				deliveredReviewKicks = append(deliveredReviewKicks, k)
				persistReviewDispatchState(reviewPlan, deliveredReviewKicks, logger)
			}
			kickSpan.End()
			if releaseProviderBudgetProbe {
				providerBudgetProbe.markReleased(time.Now())
				releaseProviderBudgetProbe = false
			}
			gov.RecordKick(msg.Agent)
			dashSrv.AuditLog("governor", "kick", "trigger=governor-eval", msg.Agent)

			// Record issue-scoped kicks into the lifecycle timeline. Cheap,
			// guarded, and nil-safe (Record no-ops on a nil dashboard/store).
			recordKick(ctx, dashSrv, msg.Agent, msg.IssueRefs...)

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
	persistReviewDispatchState(reviewPlan, deliveredReviewKicks, logger)

	if actionable.Issues.SLAViolations > 0 {
		toNotify, capped := selectSLABreachNotifications(actionable.Issues.Items)
		for _, issue := range toNotify {
			notifier.Send(
				"SLA 2x breach",
				fmt.Sprintf("%s age %dm: %s\n%s", actionableIssueRef(issue), issue.AgeMinutes, issue.Title, issue.URL),
				notify.PriorityHigh,
			)
		}
		if capped {
			logger.Info("SLA notification cap reached, skipping remaining", "remaining", actionable.Issues.SLAViolations-len(toNotify))
		}
	}

	// Scan agent panes for login-required patterns and pause + notify if detected
	scanForLoginRequired(ctx, cfg, agentMgr, notifier, dashSrv, logger, loginSightings)

	// Epoch captured before reading agent/governor state so a mutation that
	// lands mid-build (restart-count/budget reset) drops this snapshot instead
	// of letting it revert the mutation on the dashboard (#4348).
	buildEpoch := dashSrv.BeginStatusSnapshot()
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
			safeFindings := make([]advisory.Finding, 0, len(findings))
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
				blockFinding := false
				if cfg.Ioscan.IsEnabled() && cfg.Ioscan.Canaries {
					reportText := strings.Join([]string{f.Title, f.Detail, f.File, f.Type, f.Severity}, "\n")
					if leak, ok := ioscan.DefaultCanaries.Scan(f.Agent, reportText, "advisory-finding"); ok {
						detail := fmt.Sprintf("rule=%s, agent=%s, source=%s", ioscan.CanaryLeakRule, leak.Agent, leak.Source)
						dashSrv.AuditLog(leak.Agent, "ioscan_canary_leak", detail, leak.Agent)
						if store, ok := beadStores[leak.Agent]; ok && store != nil {
							if b, berr := store.Create("Canary token leaked via "+leak.Source, beads.TypeAdvisory, beads.PriorityCritical, leak.Agent, ""); berr == nil {
								_ = store.SetMetadata(b.ID, "rule", ioscan.CanaryLeakRule)
								_ = store.SetMetadata(b.ID, "source", leak.Source)
							}
						}
						blockFinding = cfg.Ioscan.FailClosed()
					}
				}
				if blockFinding {
					logger.Warn("ioscan fail-closed blocked advisory finding with canary leak", "agent", f.Agent)
					continue
				}
				safeFindings = append(safeFindings, f)
			}
			if persisted := advisory.PersistAsBeads(safeFindings, beadStores); persisted > 0 {
				logger.Info("advisory findings persisted as beads", "count", persisted)
			}
		}
	}

	// Reload bead stores from disk before building the digest. Agents write
	// beads via the bd CLI which persists directly to disk, so the in-memory
	// stores can become stale between eval cycles. Reload failures are deduped
	// (WARN once per distinct error, then DEBUG) — see beads_reload.go (#5505).
	reloadBeadStores(beadStores, logger)

	// Phase 4 Part B: `plan`/`epic` label trigger. An actionable issue carrying a
	// plan label auto-mints an epic and requests decomposition — the same flow as
	// the dashboard "Plan this issue" click, but triggered by the label. It runs
	// AFTER the store reload so FindByExternalRef sees current state (idempotent:
	// no duplicate epic if one already exists). Cheap, synchronous, adds NO
	// goroutine, and drives the architect only via SendKick (never the launch
	// path). Gated by config/ACMM so low-maturity hives stay advisory-only.
	if acmmLvl := inferACMMLevel(cfg); cfg.Planning.PlanFromLabelEnabled(acmmLvl) {
		planFromLabeledIssues(actionable, beadStores, agentMgr, gov, dashSrv, logger, acmmLvl)
	}

	// Advisory digest: build from beads (the source of truth) before status broadcast.
	primaryRepo := primaryAdvisoryRepo(cfg)
	issueNum, hasPinnedAdvisoryIssue := advisoryIssueNumber(advisoryIssues, primaryRepo)
	hasExistingPinnedIssueForEmptyDigest := hasPinnedAdvisoryIssue &&
		primaryRepo == primaryRepoAtCycleStart &&
		hadPinnedAdvisoryIssueAtCycleStart
	if shouldBuildAdvisoryDigest(beadStores, ghClient, hasExistingPinnedIssueForEmptyDigest) {
		// Retire findings no agent has re-reported inside the staleness window
		// BEFORE the digest is built, so a stale finding never appears in the
		// comment one last time after it has been proven gone. Agents re-file a
		// finding for as long as its condition holds (beads.Store.Upsert), so
		// silence is the evidence here.
		advCfg := cfg.Governor.Advisory
		if advCfg.StalenessDays > 0 {
			if pruned := advisory.PruneStaleAdvisoryBeads(beadStores, time.Duration(advCfg.StalenessDays)*24*time.Hour); len(pruned) > 0 {
				logger.Info("closed stale advisory findings not re-reported within the staleness window",
					"count", len(pruned), "staleness_days", advCfg.StalenessDays, "titles", strings.Join(pruned, "; "))
			}
		}
		// Repo entries may be org-qualified ("org/repo"); the digest linkifier
		// needs the bare repo name alongside the org.
		org, repoName := cfg.Project.Org, primaryRepo
		if parts := strings.SplitN(primaryRepo, "/", 2); len(parts) == 2 {
			org, repoName = parts[0], parts[1]
		}

		// #3704: pin the digest to ONE repo commit. Resolve the target repo's
		// latest commit ONCE here (invariants 1 & 3), cite it in the rendered
		// comment (invariant 2, via the footer FormatDigestMarkdown emits when
		// AnalyzedSnapshot is set), and verify each finding's file path against
		// that exact commit so a since-removed path (e.g. "docs/install.md") is
		// flagged as outdated rather than cited as live. Best-effort: if the SHA
		// cannot be resolved, fall back to the previous unpinned behavior rather
		// than skip the digest.
		//
		// This is resolved BEFORE the digest is built because the top-N cap
		// consumes it: ranking cannot prefer a live finding over a since-removed
		// one unless it knows which is which at ranking time (#2364).
		digestOpts := advisory.DigestOptions{
			MaxFindings: advCfg.MaxFindings,
			ShowAll:     advCfg.ShowAll,
		}
		if ghClient != nil && org != "" && repoName != "" {
			branch := cfg.Policies.Branch
			if branch == "" {
				if r, _, rerr := ghClient.GetRepo(ctx, org, repoName); rerr == nil {
					branch = r.GetDefaultBranch()
				} else {
					logger.Warn("advisory: could not resolve default branch for snapshot", "repo", primaryRepo, "error", rerr)
				}
			}
			if branch != "" {
				if sha, serr := ghClient.LatestCommitHash(ctx, org, repoName, branch); serr == nil && sha != "" {
					digestOpts.Snapshot = &advisory.Snapshot{
						Owner:  org,
						Repo:   repoName,
						Branch: branch,
						SHA:    sha,
					}
					digestOpts.VerifyPath = func(path string) bool {
						exists, verr := ghClient.PathExistsAtRef(ctx, org, repoName, path, sha)
						if verr != nil {
							// Inconclusive check (network/rate-limit, not a 404):
							// treat as existing so a transient error never
							// mislabels a real path as outdated — and never
							// costs a real finding its top-N slot.
							logger.Warn("advisory: path existence check failed", "path", path, "repo", primaryRepo, "sha", sha, "error", verr)
							return true
						}
						return exists
					}
					logger.Info("advisory digest pinned to commit", "repo", primaryRepo, "branch", branch, "sha", sha)
				} else if serr != nil {
					logger.Warn("advisory: could not resolve latest commit for snapshot", "repo", primaryRepo, "branch", branch, "error", serr)
				}
			}
		}
		digest := advisory.BuildDigestFromBeads(beadStores, string(govState.Mode), digestOpts)
		if advisoryStore != nil {
			advisoryStore.SetLatestDigest(digest)
		}
		dashSrv.SetAdvisoryDigest(digest)
		statusPayload.AdvisoryDigest = digest

		// Post whenever there is something CURRENT to say: open findings,
		// recently resolved ones, or an empty evaluation for a hive that already
		// has a pinned advisory issue. The resolved and empty cases matter for
		// freshness: otherwise the pinned comment and AdvisoryLastPostedAt
		// freeze after the last finding disappears, and the hub reports a stale
		// advisory loop even though the agents are running cleanly.
		//
		// advisoryPostDue additionally paces the GitHub write to the
		// operator's governor.advisory.update_interval_s (#4820); 0/unset
		// keeps this exact per-cycle cadence. cfg is read live each cycle —
		// the same pattern as the staleness/max-findings knobs above — so a
		// dashboard edit applies from the next cycle without a restart. The
		// digest itself and the dashboard state above still refresh every
		// cycle; only the comment write is throttled. Note the #4821
		// write-through counts consecutive unchanged post ATTEMPTS, so its
		// forced full rewrite stretches with this interval (60 attempts ×
		// interval) — acceptable, since it only heals out-of-band comment
		// edits, and documented in the settings tooltip.
		// governor.advisory.target routes the comment write: GitHub (default,
		// the unchanged path below) or a designated Linear issue. For the
		// Linear route the configured issue plays the pinned issue's role in
		// the empty-digest freshness rule, so a clean Linear-sourced hive
		// keeps refreshing its comment exactly as a GitHub one does.
		advisoryTarget, advisoryLinearIssue, advisoryRouteErr := resolveAdvisoryDigestRoute(cfg)
		hasDigestHome := hasExistingPinnedIssueForEmptyDigest ||
			(advisoryTarget == config.AdvisoryTargetLinear && advisoryRouteErr == nil)
		if shouldPostAdvisoryDigest(digest, ghClient, hasDigestHome) &&
			advisoryPostDue(advCfg, primaryRepo, time.Now(), logger) {
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
			if digest.TotalCount == 0 && len(digest.RecentlyResolved) == 0 {
				logger.Info("advisory digest empty — posting freshness marker",
					"repo", primaryRepo, "issue", issueNum)
			}

			md := advisory.FormatDigestMarkdown(digest, advisory.DigestOptions{
				MaxFindings: digestOpts.MaxFindings,
				ShowAll:     digestOpts.ShowAll,
				Org:         org,
				ShowEmpty:   digest.TotalCount == 0 && len(digest.RecentlyResolved) == 0,
				PrimaryRepo: repoName,
			})
			if md != "" {
				if advisoryTarget != config.AdvisoryTargetGitHub {
					// Non-GitHub route. A misconfiguration (Linear chosen with
					// no linear_issue, or an unknown target) is recorded as a
					// post FAILURE, never redirected to the GitHub issue: the
					// operator opted out of it, and the hub's staleness pill is
					// how they learn the digest has nowhere to go.
					if advisoryRouteErr != nil {
						dashSrv.RecordAdvisoryError(advisoryRouteErr.Error())
						logger.Error("advisory digest not posted: target misconfigured",
							"target", advisoryTarget, "error", advisoryRouteErr)
					} else if err := postAdvisoryDigestToLinear(ctx, cfg, advisoryLinearIssue, md); err != nil {
						dashSrv.RecordAdvisoryError(err.Error())
						logger.Warn("failed to post advisory digest to linear", "issue", advisoryLinearIssue, "error", err)
					} else {
						logger.Info("posted advisory digest", "linear_issue", advisoryLinearIssue, "findings", digest.TotalCount, "via", "linear")
						dashSrv.RecordAdvisoryPost(digest.TotalCount)
						recordAdvisoryPostSuccess(primaryRepo, time.Now())
						dashSrv.RecordAdvisoryOverflow(digest.OverflowCount)
					}
				} else if hasPinnedAdvisoryIssue {
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
						// The App is the sole advisory-digest writer. The former
						// user-token fallback was removed (issue #1927): it only
						// existed to post the digest under the logged-in user's
						// identity when the App failed, which is exactly the
						// owner-attributed write path we no longer want — and it
						// forced every dashboard login through the excessive "repo"
						// scope. Record the App error so the hub flags the digest
						// as stale with its specific cause. err.Error() is the same
						// string logged just below — log-safe, never key material.
						dashSrv.RecordAdvisoryError(err.Error())
						logger.Warn("failed to post advisory digest via app", "repo", primaryRepo, "issue", issueNum, "error", err)
						switch classifyAdvisoryPostError(err) {
						case advisoryPostWriteForbidden:
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
						case advisoryPostRateLimited:
							logger.Warn("GitHub API rate limit hit, skipping advisory digest post", "repo", primaryRepo)
						default:
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
						recordAdvisoryPostSuccess(primaryRepo, time.Now())
						dashSrv.RecordAdvisoryOverflow(digest.OverflowCount)
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
						// The same proof retires stale ACCESS findings (#2575):
						// an advisory bead like "Insufficient repo permissions"
						// created while the App genuinely could not write was
						// never re-validated, so it stayed in the digest forever
						// after the App was correctly installed. A successful
						// App-authenticated digest post is the strongest
						// possible evidence the condition has healed, so close
						// those beads now; the next cycle's digest moves them to
						// "Recently Resolved" and rewrites the pinned comment.
						if healed := advisory.CloseHealedAppAuthFindings(beadStores); len(healed) > 0 {
							logger.Info("closed healed GitHub App access findings after successful App digest post",
								"count", len(healed), "titles", strings.Join(healed, "; "))
						}
						// Repo-ACCESS findings ("no clone mechanism", "no
						// repository access mechanism in L2 advisory mode",
						// …) are the second #2575 family: true before #4291
						// gave advisory tiers Contents:read and a working
						// credential-helper fetch, but a digest post only
						// proves issues:WRITE, so they need their own proof.
						// Verify with a real advisor-scoped Contents read of
						// the repo each finding names (or the primary repo
						// when it names none), memoized per repo — a finding
						// about a repo the hive genuinely cannot read stays
						// open.
						readVerified := map[string]bool{}
						canRead := func(ownerRepo string) bool {
							target := ownerRepo
							if target == "" {
								target = primaryRepo
							}
							owner, name := cfg.Project.Org, target
							if i := strings.LastIndex(target, "/"); i > 0 {
								owner, name = target[:i], target[i+1:]
							}
							if owner == "" || name == "" {
								return false
							}
							key := owner + "/" + name
							if v, ok := readVerified[key]; ok {
								return v
							}
							appAuth := ghClient.AppAuth()
							if appAuth == nil {
								// Static-token client: no advisor-tier token
								// can be minted, so the read path cannot be
								// verified — leave the finding open.
								return false
							}
							err := appAuth.VerifyRepoRead(ctx, owner, name)
							if err != nil {
								logger.Info("repo-access finding left open: advisor read probe failed",
									"repo", key, "error", err)
							}
							readVerified[key] = err == nil
							return readVerified[key]
						}
						if healed := advisory.CloseHealedRepoAccessFindings(beadStores, canRead); len(healed) > 0 {
							logger.Info("closed healed repo-access findings after verified advisory read path",
								"count", len(healed), "titles", strings.Join(healed, "; "))
						}
					}
				} else {
					// No pinned advisory issue for this repo, yet there IS
					// something to publish. This used to be a completely silent
					// skip (#4167): the digest stopped updating, the spoke
					// reported neither a post time nor an error, and the hub's
					// staleness gate therefore read the hive as "not an advisory
					// participant" and never raised the pill — a wedged digest
					// that looked exactly like a healthy PR-only hive. Record it
					// as a post FAILURE so the hub flags the hive stale with the
					// real cause, and log it once per cycle for the operator.
					msg := advisoryIssueMissingError(primaryRepo, advisoryEnsureErr)
					dashSrv.RecordAdvisoryError(msg)
					logger.Warn("advisory digest not posted: no pinned advisory issue",
						"repo", primaryRepo, "findings", digest.TotalCount)
				}
			}
		}
	} else if d := dashSrv.GetAdvisoryDigest(); d != nil {
		statusPayload.AdvisoryDigest = d
	}

	dashSrv.UpdateStatusIfFresh(statusPayload, buildEpoch)

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

// loginScanAction is what the detector should do about one agent this cycle.
type loginScanAction int

const (
	// loginScanIgnore: nothing that looks like a login problem, or a startup
	// modal is on screen. Any sighting streak is cleared.
	loginScanIgnore loginScanAction = iota
	// loginScanDeferAuthenticated: the pane matched, but the backend credential
	// is demonstrably valid, so this is residue or a stuck CLI — the manager's
	// token-restart heal's case, not an operator's (kubestellar/hive#5291).
	loginScanDeferAuthenticated
	// loginScanDeferStreak: the pane matched and the credential is not provably
	// good, but this is the first consecutive cycle to see it.
	loginScanDeferStreak
	// loginScanPause: pause the agent and page the operator.
	loginScanPause
)

// loginPauseMinSightings is how many CONSECUTIVE governor cycles must see a
// login pattern before the detector pauses (kubestellar/hive#5291).
//
// The manager's own pane poller learned this at its ~3s cadence, where a single
// sighting restarted healthy agents; it now requires loginStreakRestartMin = 3.
// The detector had no equivalent, and a pause is far more expensive than a
// restart — it is sticky, it needs a human to undo, and it cancels the agent
// context that hosts the heal. Two is deliberate rather than three: a governor
// cycle is minutes, not seconds, so each extra cycle is real delay for a
// genuine logout, and the credential gate above already covers the case this
// backstops. It matters most for backends with no credential file this process
// can check, where it is the only new protection.
const loginPauseMinSightings = 2

// loginSightingTracker counts CONSECUTIVE cycles in which each agent's pane
// matched a login pattern. A clean cycle resets the count to zero, so a match
// has to persist to accumulate — a single flicker never reaches the threshold.
type loginSightingTracker struct {
	mu     sync.Mutex
	streak map[string]int
}

func newLoginSightingTracker() *loginSightingTracker {
	return &loginSightingTracker{streak: map[string]int{}}
}

// loginSightings is the detector's process-scoped state. The governor cycle is
// a function rather than an object, so the consecutive-sighting counts have to
// outlive a single call; tests build their own tracker and pass it explicitly.
var loginSightings = newLoginSightingTracker()

// observe records this cycle's reading for one agent and returns the resulting
// consecutive-sighting count (1 on the first sighting).
func (t *loginSightingTracker) observe(agent string, matched bool) int {
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
	}
	if t == nil {
		// No tracker wired: behave as if every sighting is its own streak, which
		// is exactly the pre-#5291 single-observation behaviour.
		if matched {
			return loginPauseMinSightings
		}
		return 0
	}
	if !matched {
		delete(t.streak, agent)
		return 0
	}
	t.streak[agent]++
	return t.streak[agent]
}

// forget drops an agent's streak — on pause (it stops being scanned) and for
// agents that are no longer present, so the map cannot grow without bound
// across a long-lived process.
func (t *loginSightingTracker) forget(agent string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.streak, agent)
}

// retain drops every agent not in the given set.
func (t *loginSightingTracker) retain(present map[string]bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for name := range t.streak {
		if !present[name] {
			delete(t.streak, name)
		}
	}
}

// loginScanDecision is the detector's whole judgement about one agent, as a
// pure function of what was observed. It exists apart from scanForLoginRequired
// so the decision can be tested against real pane text without a tmux session,
// a manager, or a governor cycle.
//
// sightings is the consecutive-cycle count INCLUDING this one.
//
// The credential gate is the fix for kubestellar/hive#5291: the detector used
// to pause on pane text alone, and the pane during and just after an
// interactive /login necessarily contains login-screen chrome — so it fired on
// the evidence the operator's own fix had just produced, seven minutes after
// the credential was already valid. Worse, Pause() cancels the agent context
// and tears down the poller that hosts the token-restart heal (#4606), which is
// the mechanism built for exactly "login prompt on screen, credential valid".
// Pausing first therefore disabled the machinery that would have fixed the pane
// it misread.
//
// Text matching cannot be narrowed out of this: two earlier fixes tried
// (tail-only matching, then a tighter copilot pattern) and this incident is the
// third false positive. The pane legitimately contains login text at the moment
// the credential is freshest, so the credential has to be consulted.
func loginScanDecision(
	backend, paneText string,
	compiled []*regexp.Regexp,
	credentialValid bool,
	sightings int,
) (loginScanAction, *regexp.Regexp) {
	matched := loginScanMatch(backend, paneText, compiled)
	return loginScanVerdict(matched != nil, credentialValid, sightings), matched
}

// loginScanMatch reports which login pattern this pane trips, or nil for none.
// Separate from the verdict so the scan loop can match ONCE and use the answer
// both to advance the sighting streak and to decide.
func loginScanMatch(backend, paneText string, compiled []*regexp.Regexp) *regexp.Regexp {
	// Stand down while a startup-blocking modal (folder trust, codex update, …)
	// is on screen: that is not a login problem, and pausing the agent for it
	// cancels the trust-prompt watcher that would answer it — the deadlock that
	// kept copilot agents "sitting at login prompt" through every operator
	// re-login (hivecommons/hive, 2026-08-22). The watcher answers the modal
	// within seconds; if a REAL login prompt follows, the next detector tick
	// sees it on a clean pane.
	if agent.PaneShowsBlockingPrompt(backend, paneText) {
		return nil
	}
	for _, re := range compiled {
		if re.MatchString(paneText) {
			return re
		}
	}
	return nil
}

// loginScanVerdict turns "what the pane showed" into "what to do". It returns
// loginScanIgnore whenever matched is false, which is what lets the scan loop
// rely on a non-Ignore verdict implying a non-nil pattern to log.
func loginScanVerdict(matched, credentialValid bool, sightings int) loginScanAction {
	if !matched {
		return loginScanIgnore
	}
	if credentialValid {
		return loginScanDeferAuthenticated
	}
	if sightings < loginPauseMinSightings {
		return loginScanDeferStreak
	}
	return loginScanPause
}

// scanForLoginRequired checks each running agent's tmux pane output for login-required
// patterns. When a match is found, the agent is paused and a notification is sent.
func scanForLoginRequired(
	ctx context.Context,
	cfg *config.Config,
	agentMgr *agent.Manager,
	notifier *notify.Notifier,
	dashSrv *dashboard.Server,
	logger *slog.Logger,
	sightings *loginSightingTracker,
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

	// Scan the pane TAIL only. A login prompt the CLI is genuinely stuck at
	// sits at the BOTTOM of the pane; the 50-line window this used to read
	// reached deep into scrollback, where agent WORK OUTPUT that merely
	// mentions a pattern phrase lives — quality's scan findings quoting
	// "gh auth login" from auth documentation got the agent paused mid-kick
	// (hivecommons/hive, 2026-08-22 08:27, on a fully-authenticated CLI).
	// Same discipline as the poller's tail-only match (#4577).
	const paneLines = 12
	statuses := agentMgr.AllStatuses()
	scanned := make(map[string]bool, len(statuses))
	for name, proc := range statuses {
		if proc.State != "running" {
			continue
		}
		scanned[name] = true

		output, err := agentMgr.GetOutput(name, paneLines)
		if err != nil || len(output) == 0 {
			continue
		}

		joined := strings.Join(output, "\n")
		backend := cfg.Agents[name].Backend

		// #5291: ask the CREDENTIAL, not just the pane. A valid credential plus
		// a login prompt is the token-restart heal's case; only an invalid one
		// needs a human.
		credentialValid := agentMgr.AgentHasValidCredential(name)

		// Match once. The streak has to reflect what the pane SHOWED, including
		// on the cycles where a gate below declines to act on it, so the
		// sighting is recorded before the verdict is taken.
		re := loginScanMatch(backend, joined, compiled)
		streak := sightings.observe(name, re != nil)

		switch loginScanVerdict(re != nil, credentialValid, streak) {
		case loginScanIgnore:
			continue
		case loginScanDeferAuthenticated:
			// Logged at Info, not Warn: this is the detector working correctly,
			// and it is the line that explains an agent staying up with login
			// text on its pane.
			logger.Info("login pattern matched but the backend credential is valid — leaving it to the token-restart heal",
				"agent", name, "backend", backend, "pattern", re.String())
			continue
		case loginScanDeferStreak:
			logger.Info("login pattern matched but not yet on enough consecutive cycles — deferring",
				"agent", name, "backend", backend, "pattern", re.String(),
				"sightings", streak, "required", loginPauseMinSightings)
			continue
		case loginScanPause:
			logger.Warn("login required detected",
				"agent", name,
				"pattern", re.String(),
				"sightings", streak,
			)
			sightings.forget(name)

			// Attempt a per-agent token re-cache BEFORE pausing. On an
			// App-authenticated hive the likeliest cause of a "gh auth
			// login" prompt is an expired scoped-token cache (#4072);
			// re-minting it now means the operator's Resume immediately
			// works instead of 401ing straight back into this pause.
			// Best-effort: hives without App auth (or agents without a
			// dedicated UID) simply skip it.
			if refreshErr := agentMgr.RefreshAgentTokenFor(ctx, name); refreshErr == nil {
				logger.Info("re-cached per-agent scoped token before login-detector pause", "agent", name)
			}

			// Pause the agent instead of restarting
			if pauseErr := agentMgr.Pause(name, "login-detector", "login required detected"); pauseErr != nil {
				logger.Warn("failed to pause agent after login detection",
					"agent", name, "error", pauseErr)
			} else {
				dashSrv.AuditLog("system", "pause", "trigger=login-detector", name)
			}

			// Determine the login instruction based on the agent's backend
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
		}
	}
	// Agents that vanished (removed from config, stopped) must not keep a
	// streak alive in the map for the life of the process.
	sightings.retain(scanned)
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

// curatorConfigFromHive maps the hive.yaml curator block onto the knowledge
// package's own config. Enabled is carried across as a pointer so "absent"
// stays distinguishable from "explicitly false" — the scheduled promotion loop
// treats absent as OFF, and flattening it to a bool here would quietly turn
// unreviewed promotion on fleet-wide (#5430).
func curatorConfigFromHive(c config.KnowledgeCurator) knowledge.CuratorConfig {
	return knowledge.CuratorConfig{
		Enabled:              c.Enabled,
		Schedule:             c.Schedule,
		ExtractFrom:          c.ExtractFrom,
		AutoPromoteThreshold: c.AutoPromoteThreshold,
		PromoteFrom:          c.PromoteFrom,
		PromoteTo:            c.PromoteTo,
	}
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
// upgradeFailureSummary renders what the hub shows an operator. An empty
// LastError must never render as a dangling "attempts: " - a colon promising a
// reason and delivering none is worse than saying the reason was not captured,
// because it reads as truncation and sends the reader looking for the rest.
func upgradeFailureSummary(attempts int, lastError string) string {
	if strings.TrimSpace(lastError) == "" {
		return fmt.Sprintf("self-upgrade failed after %d attempts (no error recorded; the image never changed - check that the deployment tracks a tag carrying the target SHA)", attempts)
	}
	return fmt.Sprintf("self-upgrade failed after %d attempts: %s", attempts, lastError)
}

func recordUpgradeError(path string, upgradeErr error, logger *slog.Logger) {
	if upgradeErr == nil {
		return
	}
	// A marker that cannot be read is not a reason to drop the cause. The
	// earlier version returned here, which left LastError empty and produced
	// the bare "self-upgrade failed after 5 attempts: " the hub relays to the
	// dashboard - an alert naming a failure and nothing about it. Losing the
	// attempt count is survivable; losing the reason is what makes the failure
	// undiagnosable, so rebuild the marker around the error instead.
	var m upgradeMarker
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Warn("upgrade marker unreadable; recording the error against a fresh marker",
			"path", path, "error", err)
	} else {
		m = parseUpgradeMarker(data)
	}
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

// watchdogAuthProbes builds the per-provider credential probes for the
// watchdog by adapting the rotation package's provider probers (#4608) —
// the same machinery the #4645 probe rewrite targets, so that rewrite reaches
// the watchdog automatically.
func watchdogAuthProbes(cfg *config.Config) map[string]watchdog.AuthProbe {
	threshold := cfg.Governor.Rotation.EffectiveThreshold()
	probers := []rotation.Prober{
		rotation.ClaudeProber{ThresholdPct: threshold},
		rotation.CodexProber{ThresholdPct: threshold},
		rotation.AgyProber{ThresholdPct: threshold},
		rotation.DeepSeekProber{},
	}
	out := make(map[string]watchdog.AuthProbe, len(probers))
	for _, p := range probers {
		out[p.Provider()] = watchdog.RotationAuthProbe{Prober: p}
	}
	return out
}

// turnLossToSnapshot converts the manager's in-memory turn-loss accumulation
// into its persisted form, or nil when nothing has been recorded.
//
// Nil rather than a zero struct on purpose: `turn_loss` is omitempty, so an
// agent that has never been interrupted adds nothing to /data/hive-state.json.
// The overwhelming majority of agents are in that state, and a measurement that
// bloated every hive's state file with empty records would be its own argument
// for removing it.
func turnLossToSnapshot(loss agent.TurnLoss) *snapshot.AgentTurnLoss {
	if loss.Interruptions == 0 && len(loss.Recent) == 0 {
		return nil
	}
	out := &snapshot.AgentTurnLoss{
		Interruptions: loss.Interruptions,
		Producing:     loss.Producing,
		UpperBoundS:   loss.UpperBound.Seconds(),
		Bytes:         loss.Bytes,
	}
	for _, r := range loss.Recent {
		rec := snapshot.AgentTurnInterruption{
			At:         r.At,
			Reason:     r.Reason,
			SinceKickS: r.SinceKick.Seconds(),
			Producing:  r.Producing,
			Bytes:      r.Bytes,
		}
		if r.SinceOutput != nil {
			s := r.SinceOutput.Seconds()
			rec.SinceOutputS = &s
		}
		out.Recent = append(out.Recent, rec)
	}
	return out
}

func persistState(agentMgr *agent.Manager, gov *governor.Governor, cfg *config.Config, path string, logger *slog.Logger, dashSrv *dashboard.Server, wd *watchdog.Reconciler) {
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
			PausedBy:        proc.PausedBy,
			TurnLoss:        turnLossToSnapshot(proc.TurnLoss),
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

	cadenceOverrides := make(map[string]map[string]config.Cadence)
	for modeName, mode := range cfg.Governor.Modes {
		if len(mode.Cadences) > 0 {
			cadenceOverrides[modeName] = make(map[string]config.Cadence, len(mode.Cadences))
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
		LastEval:             govState.LastEval,
		ACMMLevel:            cfg.ACMMLevel,
	}

	// Persist the fleet breaker so an engaged kill-switch survives a restart.
	// Only written when engaged — a never-thrown breaker adds nothing.
	if engaged, breakerPaused := agentMgr.BreakerState(); engaged {
		state.Breaker = &snapshot.BreakerState{Engaged: true, Paused: breakerPaused}
	}

	// Persist the watchdog's backoff/crash-loop/condition state (RFC #4665
	// open question 2: it rides the existing state file).
	if wd != nil {
		if wdState := wd.Snapshot(); len(wdState) > 0 {
			state.Watchdog = wdState
		}
	}

	if err := snapshot.SaveState(path, state, logger); err != nil {
		logger.Error("failed to persist state", "error", err)
	}

	// Component reach counters (#3993) ride the SAME save cadence as the main
	// state file but live in their own file (reachStatePath — resolved OQ-2 of
	// #3973), so a reach write failure never corrupts agent/governor state.
	if err := tracing.SaveReachState(reachStatePath); err != nil {
		logger.Error("failed to persist reach state", "error", err)
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

		// #4298: per-budget-window history. Written on the same cadence as the
		// other series so a pod roll cannot lose more of one than the others.
		budgetHist := dashSrv.BudgetWindowHistory()
		if len(budgetHist) > 0 {
			budgetData, err := json.Marshal(budgetHist)
			if err == nil {
				atomicWrite("/data/budget-window-history.json", budgetData)
			}
		}

		// #4263: convergence soak telemetry, written atomically on the same
		// cadence as the other series so a pod roll cannot lose more of one
		// than the others.
		soakHist := dashSrv.ConvergenceSoakHistory()
		if len(soakHist) > 0 {
			soakData, err := json.Marshal(soakHist)
			if err == nil {
				atomicWrite("/data/convergence-soak-history.json", soakData)
			}
		}
	}
}

var (
	escalationStoreOnce sync.Once
	escalationStore     *escalation.Store
)

const escalationLedgerPath = "/data/metrics/fix-streaks.json"

// getEscalationStore lazily loads the shared fix-loop ledger (staleness clock +
// distinct-SHA attempt counts + re-engagement caps). It is the SINGLE
// loop-safety/dedup authority shared by the re-engagement paths (#2 merge
// watcher, #3 claim release, #4 reaper) and the human-escalation sweep, so they
// all agree on which red PRs are stale, how many times each has been
// re-nudged, and which have crossed the human-escalation threshold.
func getEscalationStore() *escalation.Store {
	escalationStoreOnce.Do(func() {
		escalationStore = escalation.Load(escalationLedgerPath)
	})
	return escalationStore
}

// hivePRObservations projects the enumerated hive-authored PRs into escalation
// observations (repo fully-qualified, Red == a required check failed). Shared by
// recordRedStaleness and the reaper so both classify PRs identically. A PR is
// "red" here strictly per HasFailingRequiredCheck — GENERIC check state, never a
// specific linter or language.
func hivePRObservations(cfg *config.Config, actionable *github.ActionableResult) []escalation.Observation {
	if actionable == nil {
		return nil
	}
	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	isAgentAuthor := func(author string) bool {
		return author == cfg.Project.AIAuthor || strings.HasSuffix(author, "[bot]")
	}
	var obs []escalation.Observation
	for _, pr := range actionable.PRs.Items {
		if !isAgentAuthor(pr.Author) {
			continue
		}
		obs = append(obs, escalation.Observation{
			Repo:    fullRepo(pr.Repo),
			Number:  pr.Number,
			HeadSHA: pr.HeadSHA,
			Red:     pr.HasFailingRequiredCheck(),
			Excerpt: pr.CIFailureExcerpt,
		})
	}
	return obs
}

// recordRedStaleness updates the shared staleness clock (first-seen-red per red
// head SHA) for every hive-authored PR in this pass. It must run before the
// claim guard and the reaper so their StaleRed() reads reflect the current tick.
// A disabled escalation subsystem skips it (the whole fix-loop machinery is off).
func recordRedStaleness(cfg *config.Config, actionable *github.ActionableResult) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	obs := hivePRObservations(cfg, actionable)
	getEscalationStore().ObserveRed(obs)
	for _, ob := range obs {
		if !ob.Red {
			continue
		}
		hookDispatcher().Fire(context.Background(), hooks.Payload{
			Transition: hooks.TransitionEscalationRed,
			Repo:       ob.Repo,
			Reason:     "required CI check red",
			Attrs: map[string]string{
				hooks.AttrPR: strconv.Itoa(ob.Number),
				"head_sha":   ob.HeadSHA,
				"excerpt":    ob.Excerpt,
			},
		})
	}
}

// mergeReEngageHook builds the Fix #2 re-engagement callback for the merge
// watcher. It normalizes the repo, then records a re-engagement under the shared
// escalation store's per-red-SHA cap. Passing an empty head SHA tells the store
// to reuse the red head SHA it last observed for this PR (the eval cycle's
// ObserveRed keeps it current), so the merge watcher does not need to re-fetch
// the head. Returns false when the cap is exhausted, so the watcher can log that
// the escalation path now owns the PR. A disabled escalation subsystem yields a
// nil hook (watcher falls back to quarantine-only).
func mergeReEngageHook(cfg *config.Config) github.MergeReEngageFunc {
	if cfg.Escalation.Disabled {
		return nil
	}
	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	return func(repo string, number int) bool {
		// Empty head SHA: reuse the store's tracked current red SHA for this PR
		// (do not reset the cap counter). The eval cycle records it via
		// ObserveRed; if the store has never seen this PR red, TryReEngage still
		// allows the first MaxReEngagements nudges.
		return getEscalationStore().TryReEngage(fullRepo(repo), number, "")
	}
}

// reapStuckRedPRs is Fix #4: the governor's backstop sweep. For each
// hive-authored PR that is red on a required check AND stale (StaleRed) AND not
// already escalated to a human, it records a re-engagement (deduped + capped via
// the escalation store) and logs the fix dispatch. The PR is already present in
// ci-failing.json via writeMergeEligible, so recording the re-engagement is what
// guarantees a stale PR is treated as actionable rather than abandoned, while
// the cap prevents re-firing every tick on a permanently-red, never-moving head.
// Entirely generic: it keys only off check state + staleness, never a linter.
func reapStuckRedPRs(cfg *config.Config, actionable *github.ActionableResult, escalatedPRs map[string]bool, logger *slog.Logger) {
	if cfg.Escalation.Disabled || actionable == nil {
		return
	}
	store := getEscalationStore()
	for _, o := range hivePRObservations(cfg, actionable) {
		if !o.Red {
			continue
		}
		key := escalation.Key(o.Repo, o.Number)
		if escalatedPRs[key] {
			// Already handed to a human (needs-human label); kick builders skip
			// it and we must not re-dispatch automated fixes.
			continue
		}
		if !store.StaleRed(o.Repo, o.Number, o.HeadSHA) {
			continue // still churning (fresh red SHA) — leave it to the fix agent
		}
		if !store.TryReEngage(o.Repo, o.Number, o.HeadSHA) {
			// Re-engagement cap reached for this red SHA: stop nudging. The
			// distinct-SHA escalation path owns it from here.
			continue
		}
		logger.Info("reaper: re-dispatching fix for stuck red PR",
			"repo", o.Repo, "pr", o.Number, "head_sha", o.HeadSHA,
			"re_engagements", store.ReEngagements(o.Repo, o.Number))
	}
}

// runEscalationSweep folds this enumeration pass into the fix-loop breaker
// ledger and fires the one-time escalation actions (evidence comment +
// needs-human label + ntfy) for any agent-authored PR that just crossed the
// threshold of distinct failed fix attempts. Returns the full set of
// escalated PR keys so the work-list writers can flag them. Deterministic by
// design: no agent judgment is involved in counting, evidence, or the
// stop-order. Human-authored PRs are never escalated.
//
// The two forge writes go through forge.IssueWriter rather than *github.Client
// so the evidence lands on whichever forge the hive is actually configured for
// (see governorForge in forgewire.go). On a GitHub hive the writer IS the
// *github.Client this used to take, so nothing about that path changed.
//
// rec receives a KindBlocked lifecycle event for each newly-escalated PR
// (#5656); a nil rec is a no-op, matching the other timeline producers.
func runEscalationSweep(
	ctx context.Context,
	cfg *config.Config,
	writer forge.IssueWriter,
	actionable *github.ActionableResult,
	notifier *notify.Notifier,
	rec lifecycleRecorder,
	logger *slog.Logger,
) map[string]bool {
	escalated := map[string]bool{}
	if cfg.Escalation.Disabled || writer == nil || actionable == nil {
		return escalated
	}
	getEscalationStore()

	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	isAgentAuthor := func(author string) bool {
		return author == cfg.Project.AIAuthor || strings.HasSuffix(author, "[bot]")
	}

	var obs []escalation.Observation
	type prMeta struct{ checks []string }
	meta := map[string]prMeta{}
	for _, pr := range actionable.PRs.Items {
		if !isAgentAuthor(pr.Author) {
			continue
		}
		repo := fullRepo(pr.Repo)
		obs = append(obs, escalation.Observation{
			Repo:    repo,
			Number:  pr.Number,
			HeadSHA: pr.HeadSHA,
			Red:     pr.CIStatus == "failure",
			Excerpt: pr.CIFailureExcerpt,
		})
		meta[escalation.Key(repo, pr.Number)] = prMeta{checks: pr.FailingChecks}
	}
	results := escalationStore.Sweep(obs, cfg.Escalation.EffectiveThreshold())

	for _, o := range obs {
		key := escalation.Key(o.Repo, o.Number)
		r, ok := results[key]
		if !ok {
			continue
		}
		if r.Escalated {
			escalated[key] = true
		}
		if !r.NewlyEscala {
			continue
		}
		escalated[key] = true
		excerpt := o.Excerpt
		if excerpt == "" {
			excerpt = escalationStore.Excerpt(o.Repo, o.Number)
		}
		body := escalation.CommentBody(r.Attempts, meta[key].checks, excerpt)
		if err := writer.CreateIssueComment(ctx, o.Repo, o.Number, body); err != nil {
			// Retry next pass rather than marking escalated with no comment:
			// the whole point is that the evidence reaches a human.
			logger.Warn("escalation comment failed; will retry next pass",
				"repo", o.Repo, "pr", o.Number, "error", err)
			continue
		}
		if err := writer.AddLabels(ctx, o.Repo, o.Number, []string{escalation.NeedsHumanLabel}); err != nil {
			logger.Warn("escalation label failed", "repo", o.Repo, "pr", o.Number, "error", err)
		}
		escalationStore.MarkEscalated(o.Repo, o.Number)
		// The escalation IS the real "blocked" lifecycle signal (#5656): a PR
		// out of automated fix attempts, handed to a human. Record it on the
		// item's journey so the panel's Blocked counter reflects reality, not
		// just hook annotations.
		recordBlocked(ctx, rec, cfg.Project.Org, o.Repo, o.Number, r.Attempts, meta[key].checks)
		logger.Info("fix loop escalated to human",
			"repo", o.Repo, "pr", o.Number, "attempts", r.Attempts,
			"failing_checks", strings.Join(meta[key].checks, ","))
		if notifier != nil {
			notifier.Send("Fix loop escalated",
				fmt.Sprintf("%s#%d red on %d fix attempts — needs a human (see PR comment for the raw error)", o.Repo, o.Number, r.Attempts),
				notify.PriorityHigh)
		}
	}
	return escalated
}

// autoMergeSweepInterval is the minimum spacing between label-queued
// auto-merge sweeps. The sweep piggybacks on the governor eval tick, which can
// fire much more often than once a minute; this floor keeps the sweep from
// hammering the GitHub API on short eval intervals.
const autoMergeSweepInterval = time.Minute

// trustedMergerFunc resolves a GitHub login against the hive's authorized-users
// allowlist and reports whether it holds at least config.RoleMerger — the same
// bar requireMergerOrOwnerRole enforces on the dashboard queue endpoint (audit
// F3).
//
// Fails CLOSED: a nil config or a login absent from the allowlist is NOT
// trusted, so an unclassifiable actor can never merge. cfg is read on every
// call so a config reload that grants or revokes the merger tier takes effect
// without a restart.
func trustedMergerFunc(cfg *config.Config) github.MergerAuthorizer {
	return func(login string) bool {
		if cfg == nil || strings.TrimSpace(login) == "" {
			return false
		}
		role, ok := cfg.Dashboard.AuthorizedRole(login)
		if !ok {
			return false
		}
		return config.RoleAtLeast(role, config.RoleMerger)
	}
}

// runAutoMergeSweepIfDue drains the label-queued auto-merge queue (the human
// "Approved ... for Hive auto-merge" path) at most once per
// autoMergeSweepInterval. All merge-eligibility decisions — queue-approval
// trust, the trusted-merger tier gate (SetMergerAuthorizer), check
// verification — live inside SweepQueuedAutoMerges; this function is only the
// scheduler and the dashboard audit sink.
// rotationTrigger is the PausedTrigger stamped on strand-pauses so rotation's
// auto-resume never resumes a pause it did not create.
const rotationTrigger = "provider-rotation"

// runRotationCheck applies RFC #3958 provider rotation after an eval cycle:
// for each agent not mid-task whose provider was positively measured as
// exhausted, move it to a backend with headroom at the same tier; when
// nothing has headroom, pause it loudly (strand). Stranded agents are
// auto-resumed when their provider recovers headroom. Never runs mid-task:
// only idle agents are candidates.
func runRotationCheck(ctx context.Context, cfg *config.Config, rotMgr *rotation.Manager, gov *governor.Governor, agentMgr *agent.Manager, logger *slog.Logger) {
	if rotMgr == nil || !cfg.Governor.Rotation.Enabled {
		return
	}
	govState := gov.GetState()
	for name, proc := range agentMgr.AllStatuses() {
		backend := proc.Config.Backend
		if proc.BackendOverride != "" {
			backend = proc.BackendOverride
		}

		// Auto-resume: a stranded agent whose provider recovered.
		if proc.Paused && proc.PausedTrigger == rotationTrigger {
			if rotMgr.StrandRecovered(backend) {
				if err := agentMgr.Resume(ctx, name, rotationTrigger, "provider headroom recovered"); err != nil {
					logger.Warn("rotation: auto-resume failed", "agent", name, "error", err)
				} else {
					logger.Info("rotation: auto-resumed stranded agent", "agent", name, "backend", backend)
				}
			}
			continue
		}
		if proc.Paused {
			continue // never touch an operator pause
		}
		// Never rotate mid-task: only idle agents are candidates.
		if proc.State == agent.StateRunning {
			continue
		}

		cadenceS := 0
		if c, ok := govState.Cadences[name]; ok && c.Interval > 0 {
			cadenceS = int(c.Interval / time.Second)
		}

		if !rotMgr.Exhausted(backend) {
			continue
		}
		next := rotMgr.NextBackendForCadence(name, backend, cadenceS)
		if next == "" {
			// Strand loudly: pause so the agent burns nothing until a
			// provider recovers; the loop above auto-resumes it.
			if err := agentMgr.Pause(name, rotationTrigger, "no provider has headroom (RFC #3958)"); err != nil {
				logger.Warn("rotation: strand-pause failed", "agent", name, "error", err)
			} else {
				logger.Info("rotation: stranding agent, no headroom anywhere", "agent", name, "backend", backend)
			}
			continue
		}
		if err := agentMgr.SetBackendOverride(name, next); err != nil {
			logger.Warn("rotation: backend override failed", "agent", name, "to", next, "error", err)
			continue
		}
		logger.Info("rotation: moved agent to new backend", "agent", name, "from", backend, "to", next)
	}
}

func runAutoMergeSweepIfDue(ctx context.Context, ghClient *github.Client, dashSrv *dashboard.Server, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < autoMergeSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	result, err := ghClient.SweepQueuedAutoMerges(ctx, github.AutoMergeSweepOptions{
		MaxMerges: github.DefaultAutoMergeSweepMaxMerges,
		Audit: func(event github.AutoMergeSweepEvent) {
			if dashSrv == nil {
				return
			}
			detail := fmt.Sprintf("repo=%s, pr=%d, author=%s, queued_by=%s, label=%s, head_sha=%s, merge_sha=%s",
				event.Repo, event.Number, event.Author, event.QueuedBy, event.Label, event.HeadSHA, event.MergeSHA)
			dashSrv.AuditLog("system", "automerge-sweep-merged", detail, "")
		},
	})
	if err != nil {
		logger.Warn("automerge sweep failed", "error", err)
		return
	}
	if len(result.Merged) > 0 || result.Seen > 0 {
		logger.Info("automerge sweep complete", "seen", result.Seen, "merged", len(result.Merged), "skipped", result.Skipped)
	}
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionSweepCompleted,
		Reason:     "queued automerge sweep complete",
		Attrs: map[string]string{
			"seen":    strconv.Itoa(result.Seen),
			"merged":  strconv.Itoa(len(result.Merged)),
			"skipped": strconv.Itoa(result.Skipped),
		},
	})
}

// mergeEligiblePath is a var (not a const) only so tests can point
// mergeTargetEligible at a temp file; production never reassigns it.
var mergeEligiblePath = "/var/run/hive-metrics/merge-eligible.json"

var (
	ciFailingPath      = "/var/run/hive-metrics/ci-failing.json"
	intentVerdictsPath = "/var/run/hive-metrics/intent-verdicts.json"
)

// acmmHoldGatedMinLevel / acmmHoldGatedMaxLevel bracket the ACMM levels whose
// merge policy is "hold-gated" — every agent-opened PR gets a "hold" label and
// no agent merges (see src/pkg/config/packs/level-{3,4,5}.yaml). L1/L2 are
// "manual" (agents open no PRs) and L6 is "auto-merge on green CI, no hold
// label", so both fall outside this range. Used by the F6 hold-label decider.
const (
	acmmHoldGatedMinLevel = 3
	acmmHoldGatedMaxLevel = 5
)

// shouldHoldAgentPR keeps public outreach claims human-reviewed even at L6,
// where ordinary agent PRs may auto-merge. The general ACMM hold gate remains
// unchanged for all roles at L3-L5.
func shouldHoldAgentPR(agentName string, level int) bool {
	if strings.EqualFold(strings.TrimSpace(agentName), "outreach") {
		return true
	}
	return level >= acmmHoldGatedMinLevel && level <= acmmHoldGatedMaxLevel
}

// mergeableJSONUnknown is the explicit wire value for "mergeability was never
// determined". It is spelled out rather than left as "" so a consumer reading
// merge-eligible.json cannot mistake an unpopulated field for a definitive
// "no" — the failure mode that made every PR read as unmergeable.
const mergeableJSONUnknown = "unknown"

// mergeTargetEligible reports whether (repo, number) currently appears in the
// governor's merge-eligible.json AT the expected head SHA. It reads the file
// FRESH on every call (never caches) because eligibility is recomputed each
// governor cycle — a stale cache could authorize a PR that has since fallen out
// of the list. On any read/parse error it returns false (FAIL CLOSED): if we
// cannot prove the target is eligible, we must not authorize the merge.
//
// M4 (CWE-367, TOCTOU): the governor records the head SHA it observed when it
// deemed the PR eligible (eligiblePR.HeadSHA). A branch can move between that
// review and the merge relay firing, so matching (repo, number) alone would let
// a moved head merge at a commit the governor never vetted. We therefore also
// require the entry's stored head_sha to equal expectSHA. A mismatch — or a
// stored SHA that is empty (governor could not observe it) — fails closed; the
// relay's SHA pin then fails the merge cleanly if a stale request slips through.
//
// merge-eligible.json stores repos as "owner/repo"; a MergeRequest.Repo may be
// bare ("repo") or fully qualified ("owner/repo"). We match on the bare repo
// name (the segment after the last "/") plus the number, so both request forms
// resolve to the same eligible entry without depending on the org prefix.
func mergeTargetEligible(repo string, number int, expectSHA string) bool {
	data, err := os.ReadFile(mergeEligiblePath)
	if err != nil {
		return false // fail closed: no list ⇒ nothing is eligible
	}
	var payload struct {
		Items []struct {
			Number  int    `json:"number"`
			Repo    string `json:"repo"`
			HeadSHA string `json:"head_sha"`
		} `json:"merge_eligible"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return false // fail closed: unparseable list ⇒ deny
	}
	want := bareRepoName(repo)
	wantSHA := strings.TrimSpace(expectSHA)
	for _, it := range payload.Items {
		if it.Number == number && bareRepoName(it.Repo) == want {
			// M4: bind authorization to the governor-observed head. An empty
			// stored SHA cannot be proven to match, so it fails closed rather
			// than authorizing an unpinned head.
			return strings.TrimSpace(it.HeadSHA) != "" && strings.TrimSpace(it.HeadSHA) == wantSHA
		}
	}
	return false
}

// bareRepoName returns the repo segment after the last "/", so "owner/repo" and
// "repo" compare equal. Used to match a MergeRequest.Repo against the
// "owner/repo" entries in merge-eligible.json regardless of prefix.
func bareRepoName(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// bindMergeAuthz wraps the manager's agent/UID/CanMerge authorizer with the
// F4 target-binding checks (CWE-863). The inner authz owns the "may this agent
// merge at all" decision; this wrapper owns "is THIS specific target one the
// governor deemed eligible, at a pinned SHA". Both must pass before MergePR is
// reached. Ordering: run the agent/UID/CanMerge check first (cheapest, and it
// gives the clearest denial reason), then the SHA + eligible-list binding.
func bindMergeAuthz(inner func(agent string, fileUID int) error) github.MergeRequestAuthorizer {
	return func(agent string, fileUID int, repo string, number int, expectSHA string) error {
		if err := inner(agent, fileUID); err != nil {
			return err
		}
		// (a) Require a pinned head SHA. An empty expectSHA means "merge whatever
		// HEAD is now", which is the TOCTOU hole: a PR that was eligible when the
		// governor last looked could have had a malicious commit pushed since.
		// MergePR passes expectSHA as the required head SHA, so a moved head fails
		// cleanly — but only if we insist it is set.
		if strings.TrimSpace(expectSHA) == "" {
			return fmt.Errorf("merge target %s#%d has no expected head SHA — refusing to merge an unpinned head (TOCTOU guard)", repo, number)
		}
		// (b) Require the target to be in the governor's current merge-eligible
		// list AT the expected head SHA. This binds authorization to a PR the
		// hive actually deemed landable this cycle, at the exact commit it
		// reviewed, so an injected agent cannot request landing an arbitrary
		// reachable PR (e.g. its own) whose required checks happen to pass, nor
		// land an eligible PR at a head that moved after review (M4, CWE-367).
		// Read fresh + fail closed (see mergeTargetEligible).
		if !mergeTargetEligible(repo, number, expectSHA) {
			return fmt.Errorf("merge target %s#%d is not in the current merge-eligible list at head %s — only governor-approved PRs may be landed via the merge relay, and only at the reviewed head SHA", repo, number, expectSHA)
		}
		return nil
	}
}

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
	claimLedgerOnce   sync.Once
	claimLedger       *github.ClaimLedger
	claimLedgerPath   = github.ClaimLedgerPath
	claimLedgerLoader = github.LoadClaimLedger
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
	ledger := getClaimLedger(logger)
	if ledger == nil {
		return
	}
	github.ApplyDuplicatePRGuard(ctx, ghClient, ledger, hiveIdentity(cfg), actionable, claimingPRRedStale(cfg, actionable), logger)
}

// getClaimLedger lazily loads the persisted claim ledger on first use (and
// keeps a usable empty ledger on a load failure), so a missing or corrupt
// /data ledger can never block the hive from booting. It is shared by the
// eval-cycle guard above and by the dashboard's IssueClaimed hook (#3768); the
// sync.Once publication makes the pointer safe to read from either goroutine,
// and the ledger itself is internally locked.
func getClaimLedger(logger *slog.Logger) *github.ClaimLedger {
	claimLedgerOnce.Do(func() {
		ledger, err := claimLedgerLoader(claimLedgerPath, logger)
		if err != nil {
			// LoadClaimLedger always returns a usable (possibly empty) ledger
			// alongside the error, so we keep it and just report the problem.
			logger.Warn("duplicate-PR guard: could not load persisted claim ledger, starting empty",
				"path", claimLedgerPath, "error", err)
		}
		claimLedger = ledger
	})
	return claimLedger
}

// claimingPRRedStale builds the Fix #3 release predicate: given a claiming PR
// (prRepo, prNumber), report whether it is red on a required check AND stale.
// It looks up the PR's live CI state + head SHA from this pass's enumeration and
// consults the shared staleness clock. A PR not found in the enumeration, or one
// that is green/pending, or one whose red head only just appeared, returns false
// — so a HEALTHY claiming PR still suppresses its issue. Returns a nil func when
// escalation is disabled, preserving the original unconditional-suppress
// behavior. GENERIC: keys only off check state + staleness.
func claimingPRRedStale(cfg *config.Config, actionable *github.ActionableResult) github.RedStaleFunc {
	if cfg.Escalation.Disabled || actionable == nil {
		return nil
	}
	fullRepo := func(repo string) string {
		if !strings.Contains(repo, "/") && cfg.Project.Org != "" {
			return cfg.Project.Org + "/" + repo
		}
		return repo
	}
	// Index this pass's PRs by bare-repo#number so a claim's PRRepo (which may
	// be bare or "owner/repo") resolves regardless of prefix.
	type prState struct {
		red     bool
		headSHA string
		repo    string
	}
	index := map[string]prState{}
	for _, pr := range actionable.PRs.Items {
		index[fmt.Sprintf("%s#%d", bareRepoName(pr.Repo), pr.Number)] = prState{
			red:     pr.HasFailingRequiredCheck(),
			headSHA: pr.HeadSHA,
			repo:    fullRepo(pr.Repo),
		}
	}
	store := getEscalationStore()
	return func(prRepo string, prNumber int) bool {
		st, ok := index[fmt.Sprintf("%s#%d", bareRepoName(prRepo), prNumber)]
		if !ok || !st.red {
			return false // not enumerated, or healthy → keep suppressing
		}
		return store.StaleRed(st.repo, prNumber, st.headSHA)
	}
}

func writeIntentVerdicts(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	actionable *github.ActionableResult,
	beadStores map[string]*beads.Store,
	logger *slog.Logger,
) map[string]intent.Verdict {
	verdicts := make(map[string]intent.Verdict)
	if cfg == nil || actionable == nil {
		return verdicts
	}
	_ = os.MkdirAll("/var/run/hive-metrics", 0o755)
	aiAuthor := strings.TrimSpace(cfg.EffectiveAIAuthor())
	intentCfg := intent.Config{
		TestPathPatterns:      cfg.Intent.TestPathPatterns,
		DocsPathPatterns:      cfg.Intent.DocsPathPatterns,
		GuardrailPathPatterns: cfg.Intent.GuardrailPathPatterns,
		FeatureSignals:        cfg.Intent.FeatureSignals,
	}
	var alignmentReviewer *intent.AlignmentReviewer
	if strings.TrimSpace(cfg.Intent.AlignmentModel) != "" {
		endpoint, apiKey, _ := cfg.Governor.ResolveReviewer()
		var err error
		alignmentReviewer, err = intent.NewAlignmentReviewer(intent.AlignmentReviewerConfig{
			Endpoint: endpoint,
			APIKey:   apiKey,
			Model:    cfg.Intent.AlignmentModel,
		})
		if err != nil {
			logger.Warn("intent alignment reviewer disabled", "error", err)
		}
	}
	type verdictRecord struct {
		Repo       string         `json:"repo"`
		Number     int            `json:"number"`
		Title      string         `json:"title"`
		Author     string         `json:"author"`
		Enforced   bool           `json:"enforced"`
		Verdict    intent.Verdict `json:"verdict"`
		Classify   string         `json:"classification_reason"`
		FetchError string         `json:"fetch_error,omitempty"`
	}
	records := make([]verdictRecord, 0, len(actionable.PRs.Items))
	for _, pr := range actionable.PRs.Items {
		fullRepo := fullRepoName(pr.Repo, cfg.Project.Org)
		key := fmt.Sprintf("%s/%d", fullRepo, pr.Number)
		agentPR := aiAuthor != "" && strings.EqualFold(pr.Author, aiAuthor)
		record := verdictRecord{
			Repo:     fullRepo,
			Number:   pr.Number,
			Title:    pr.Title,
			Author:   pr.Author,
			Enforced: cfg.Intent.Enforce,
		}
		if !agentPR {
			class := intent.Classify(intent.PR{Title: pr.Title, Labels: pr.Labels, Author: pr.Author, AgentAuthor: false}, intentCfg)
			verdict := intent.Evaluate(class, intent.Evidence{})
			verdicts[key] = verdict
			record.Verdict = verdict
			record.Classify = class.Reason
			records = append(records, record)
			continue
		}
		body, files, approved, err := fetchIntentPREvidence(ctx, ghClient, fullRepo, pr.Number)
		if err != nil {
			verdict := intent.Verdict{
				Tier:       intent.Tier1,
				Authorized: false,
				Reason:     "intent evidence unavailable: " + err.Error(),
				AgentPR:    true,
			}
			verdicts[key] = verdict
			record.Verdict = verdict
			record.FetchError = err.Error()
			records = append(records, record)
			logger.Warn("intent verification evidence fetch failed", "repo", fullRepo, "number", pr.Number, "error", err)
			continue
		}
		class := intent.Classify(intent.PR{
			Title:       pr.Title,
			Body:        body,
			Labels:      pr.Labels,
			Files:       files,
			Author:      pr.Author,
			AgentAuthor: true,
		}, intentCfg)
		evidence := intent.BuildEvidenceForRepo(body, fullRepo, beadStores, approved)
		verdict := intent.Evaluate(class, evidence)
		issueTexts, issueErr := fetchIntentIssueTexts(ctx, ghClient, fullRepo, body)
		if issueErr != nil {
			logger.Warn("intent alignment issue evidence fetch failed", "repo", fullRepo, "number", pr.Number, "error", issueErr)
		}
		refs := intent.LinkedIssueRefs(body, fullRepo)
		alignCtx := intent.BuildAlignmentContext(intent.PR{
			Title:       pr.Title,
			Body:        body,
			Labels:      pr.Labels,
			Files:       files,
			Author:      pr.Author,
			AgentAuthor: true,
		}, issueTexts, beadStores, refs)
		alignment := intent.EvaluateAlignment(alignCtx, class.Tier, intentCfg)
		if alignmentReviewer != nil {
			modelVerdict, err := alignmentReviewer.Review(ctx, alignCtx)
			if err != nil {
				logger.Warn("intent alignment model review failed open", "repo", fullRepo, "number", pr.Number, "error", err)
				alignment = intent.MergeAlignment(alignment, nil, err)
			} else {
				alignment = intent.MergeAlignment(alignment, &modelVerdict, nil)
			}
		}
		verdict.Alignment = &alignment
		verdicts[key] = verdict
		record.Verdict = verdict
		record.Classify = class.Reason
		records = append(records, record)
		if !verdict.Authorized {
			logger.Info("intent authorization denied", "repo", fullRepo, "number", pr.Number, "tier", verdict.Tier, "reason", verdict.Reason, "enforce", cfg.Intent.Enforce)
		}
		if alignment.Misaligned() {
			logger.Info("intent alignment denied", "repo", fullRepo, "number", pr.Number, "reason", alignment.Rationale, "enforce", cfg.Intent.Enforce)
			recordIntentAlignmentAdvisory(beadStores, fullRepo, pr.Number, alignment, logger)
		}
	}
	payload := map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"enforced":     cfg.Intent.Enforce,
		"verdicts":     records,
	}
	if data, err := json.Marshal(payload); err == nil {
		atomicWrite(intentVerdictsPath, data)
	} else {
		logger.Warn("failed to marshal intent verdicts", "error", err)
	}
	return verdicts
}

func fetchIntentPREvidence(ctx context.Context, ghClient *github.Client, repo string, number int) (string, []intent.ChangedFile, bool, error) {
	if ghClient == nil || ghClient.GoGitHub() == nil {
		return "", nil, false, github.ErrNoGitHubClient
	}
	owner, repoName, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || repoName == "" {
		return "", nil, false, fmt.Errorf("invalid repo %q", repo)
	}
	client := ghClient.GoGitHub()
	pr, _, err := client.PullRequests.Get(ctx, owner, repoName, number)
	if err != nil {
		return "", nil, false, fmt.Errorf("getting PR: %w", err)
	}
	var files []intent.ChangedFile
	fileOpts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.PullRequests.ListFiles(ctx, owner, repoName, number, fileOpts)
		if err != nil {
			return "", nil, false, fmt.Errorf("listing PR files: %w", err)
		}
		for _, f := range page {
			files = append(files, intent.ChangedFile{
				Filename:  f.GetFilename(),
				Status:    f.GetStatus(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		fileOpts.Page = resp.NextPage
	}
	if reported := pr.GetChangedFiles(); reported > len(files) {
		return "", nil, false, fmt.Errorf("incomplete PR file list: GitHub reported %d changed files but API returned %d; intent alignment requires the complete changed-file list", reported, len(files))
	}
	approved, err := hasMaintainerApproval(ctx, client, owner, repoName, number)
	if err != nil {
		return "", nil, false, err
	}
	return pr.GetBody(), files, approved, nil
}

func fetchIntentIssueTexts(ctx context.Context, ghClient *github.Client, defaultRepo, body string) ([]intent.TextEvidence, error) {
	if ghClient == nil || ghClient.GoGitHub() == nil {
		return nil, github.ErrNoGitHubClient
	}
	client := ghClient.GoGitHub()
	refs := intent.LinkedIssueRefs(body, defaultRepo)
	out := make([]intent.TextEvidence, 0, len(refs))
	for _, ref := range refs {
		repo := ref.Repo
		if repo == "" {
			repo = defaultRepo
		}
		owner, repoName, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || repoName == "" {
			continue
		}
		issue, _, err := client.Issues.Get(ctx, owner, repoName, ref.Number)
		if err != nil {
			return out, fmt.Errorf("getting linked issue %s#%d: %w", repo, ref.Number, err)
		}
		out = append(out, intent.TextEvidence{
			Source: fmt.Sprintf("issue %s#%d", repo, ref.Number),
			Title:  issue.GetTitle(),
			Body:   issue.GetBody(),
		})
	}
	return out, nil
}

func recordIntentAlignmentAdvisory(stores map[string]*beads.Store, repo string, number int, alignment intent.AlignmentVerdict, logger *slog.Logger) {
	store := stores["intent"]
	if store == nil {
		store = stores["quality"]
	}
	if store == nil {
		for _, candidate := range stores {
			if candidate != nil {
				store = candidate
				break
			}
		}
	}
	if store == nil {
		return
	}
	title := fmt.Sprintf("Intent alignment drift in %s#%d", repo, number)
	ref := fmt.Sprintf("gh-%s#%d", repo, number)
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type == beads.TypeAdvisory && b.Title == title && b.ExternalRef == ref && b.Status != beads.StatusClosed && b.Status != beads.StatusDone {
			return
		}
	}
	b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, "intent", ref)
	if err != nil {
		logger.Warn("failed to record intent alignment advisory", "repo", repo, "number", number, "error", err)
		return
	}
	_ = store.Update(b.ID, func(bead *beads.Bead) {
		bead.Notes = alignmentSummary(alignment)
	})
}

func alignmentSummary(alignment intent.AlignmentVerdict) string {
	var parts []string
	if alignment.Rationale != "" {
		parts = append(parts, alignment.Rationale)
	}
	for _, f := range alignment.DeterministicFindings {
		if f.Status == intent.AlignmentStatusMisaligned {
			parts = append(parts, f.Code+": "+f.Reason+" ("+strings.Join(f.Files, ", ")+")")
		}
	}
	if alignment.Model != nil && alignment.Model.Status == intent.AlignmentStatusMisaligned {
		parts = append(parts, "model: "+alignment.Model.Rationale)
	}
	if len(parts) == 0 {
		return "intent alignment check reported misalignment"
	}
	return strings.Join(parts, "\n")
}

func hasMaintainerApproval(ctx context.Context, client *gh.Client, owner, repo string, number int) (bool, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := make(map[string]string)
	maintainer := make(map[string]bool)
	for {
		reviews, resp, err := client.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing PR reviews: %w", err)
		}
		for _, review := range reviews {
			login := review.GetUser().GetLogin()
			if login == "" {
				continue
			}
			if maintainerAssociation(review.GetAuthorAssociation()) {
				switch review.GetState() {
				case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
					latest[login] = review.GetState()
				}
				maintainer[login] = true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	approved := false
	for login, state := range latest {
		if !maintainer[login] {
			continue
		}
		switch state {
		case "CHANGES_REQUESTED":
			return false, nil
		case "APPROVED":
			approved = true
		}
	}
	return approved, nil
}

func maintainerAssociation(association string) bool {
	switch strings.ToUpper(strings.TrimSpace(association)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}

func fullRepoName(repo, org string) string {
	if strings.Contains(repo, "/") || org == "" {
		return repo
	}
	return org + "/" + repo
}

// auditPRAttributionWindow bounds how far back the audit trail is scanned to
// map open PRs to the agent that opened them. Red PRs older than this fall
// back to scanner ownership in the kick builders — acceptable: 14d exceeds any
// PR the fleet should still be iterating on.
const auditPRAttributionWindow = 14 * 24 * time.Hour

// auditPRAgents maps "org/repo#number" → agent name from the audit trail's
// agent_pr_created entries (attribution.go records one per relay-opened PR,
// reuses included). Reading the on-disk log per eval tick keeps this
// stateless; OutputActionsSince touches no receiver state, so a zero-value
// AuditLog is safe here.
func auditPRAgents(org string, since time.Time, auditPath string) map[string]string {
	entries := (&dashboard.AuditLog{}).OutputActionsSince(since,
		map[string]bool{github.AuditActionAgentPRCreated: true}, auditPath)
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Agent == "" {
			continue
		}
		var repo, number string
		for _, part := range strings.Split(e.Detail, ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
				switch k {
				case "repo":
					repo = v
				case "number":
					number = v
				}
			}
		}
		if repo == "" || number == "" {
			continue
		}
		if !strings.Contains(repo, "/") && org != "" {
			repo = org + "/" + repo
		}
		out[repo+"#"+number] = e.Agent
	}
	return out
}

// anyRequiredCheckFailing reports whether any of a PR's failing check names
// is in the operator-declared required set.
func anyRequiredCheckFailing(failing []string, required map[string]bool) bool {
	for _, name := range failing {
		if required[name] {
			return true
		}
	}
	return false
}

func writeMergeEligible(actionable *github.ActionableResult, hold github.HoldResult, org string, escalatedPRs map[string]bool, enforceIntent bool, intentVerdicts map[string]intent.Verdict, requireReviewApproval bool, requiredChecks map[string]bool, logger *slog.Logger) {
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
		// HeadSHA is the governor-observed head commit at the moment eligibility
		// was decided. mergeTargetEligible compares the relay's expected SHA
		// against this value (M4, CWE-367): a branch that moved after review
		// no longer matches and fails closed.
		HeadSHA string `json:"head_sha,omitempty"`
	}

	type failingPR struct {
		Number  int    `json:"number"`
		Repo    string `json:"repo"`
		Title   string `json:"title"`
		Author  string `json:"author"`
		HeadSHA string `json:"head_sha,omitempty"`
		// FailingChecks + Excerpt carry the raw CI evidence into the kick
		// work list so fix agents see the actual error, not just "red".
		FailingChecks []string `json:"failing_checks,omitempty"`
		Excerpt       string   `json:"excerpt,omitempty"`
		// Escalated marks PRs past the fix-loop breaker threshold: kick
		// builders list them separately and agents must NOT dispatch more
		// fix work for them.
		Escalated bool `json:"escalated,omitempty"`
		// Agent is the hive agent whose relay request opened this PR (from the
		// audit trail's agent_pr_created entries). The scheduler's
		// fix-before-new section routes each red PR back to its author; empty
		// means unattributed (kick builders default it to scanner).
		Agent string `json:"agent,omitempty"`
	}

	prAgents := auditPRAgents(org, time.Now().Add(-auditPRAttributionWindow), "")

	var eligible []eligiblePR
	var failing []failingPR
	var reviewArtifact review.Artifact
	reviewLoaded := false
	if requireReviewApproval {
		var err error
		reviewArtifact, err = review.LoadArtifact("")
		if err != nil {
			logger.Warn("review approval required but review-verdicts.json is unavailable; merge eligibility will fail closed", "error", err)
		} else {
			reviewLoaded = true
		}
	}
	for _, pr := range actionable.PRs.Items {
		if pr.Draft {
			continue
		}
		key := fmt.Sprintf("%s/%d", pr.Repo, pr.Number)
		if holdSet[key] {
			continue
		}
		fullRepo := fullRepoName(pr.Repo, org)
		if enforceIntent {
			if verdict, ok := intentVerdicts[fmt.Sprintf("%s/%d", fullRepo, pr.Number)]; ok && verdict.AgentPR && !verdict.MergeAllowed() {
				reason := verdict.Reason
				if verdict.Authorized && verdict.Alignment != nil && verdict.Alignment.Misaligned() {
					reason = intent.ReasonAlignmentMisaligned + ": " + verdict.Alignment.Rationale
				}
				logger.Info("excluding PR from merge-eligible due to intent verification", "repo", fullRepo, "number", pr.Number, "tier", verdict.Tier, "reason", reason)
				continue
			}
		}

		if pr.CIStatus == "failure" {
			// A PR red ONLY on non-required checks (perma-red Playwright
			// shards, coverage) that GitHub itself reports mergeable is NOT a
			// failing PR — it is merge-eligible, mirroring the
			// pending-but-mergeable rule below. Without this, every dependabot
			// PR on a repo with permanently-red optional checks classified as
			// "failure", landed in ci-failing.json where no sweep or agent
			// would ever merge it, and accumulated indefinitely (observed on
			// kubestellar/console 2026-08-28: 16 dependabot PRs, oldest 11
			// days). Gated on an operator-declared required-check set: with no
			// set configured we cannot distinguish required from optional and
			// keep the old fail-closed behavior. The merge step re-enforces
			// branch protection, so this cannot merge anything GitHub blocks.
			onlyOptionalRed := len(requiredChecks) > 0 &&
				!anyRequiredCheckFailing(pr.FailingChecks, requiredChecks) &&
				pr.Mergeable == github.MergeableYes
			if !onlyOptionalRed {
				failing = append(failing, failingPR{
					Number:        pr.Number,
					Repo:          fullRepo,
					Title:         pr.Title,
					Author:        pr.Author,
					HeadSHA:       pr.HeadSHA,
					FailingChecks: pr.FailingChecks,
					Excerpt:       pr.CIFailureExcerpt,
					Escalated:     escalatedPRs[escalation.Key(fullRepo, pr.Number)],
					Agent:         prAgents[fmt.Sprintf("%s#%d", fullRepo, pr.Number)],
				})
				continue
			}
		}

		// A PR whose CI is still "pending" is nonetheless merge-eligible when
		// GitHub itself reports it as mergeable (mergeStateStatus=unstable):
		// that state means every REQUIRED check has passed and only
		// non-required checks remain outstanding. Those non-required checks —
		// a cancelled Mobile Browser Tests, a still-running coverage-report,
		// perpetually-pending tide — can never complete on their own, so
		// waiting for CIStatus=="success" (all checks done) leaves cleanly
		// mergeable PRs frozen out of the sweep indefinitely (observed
		// 2026-08-04: three green console PRs stuck for hours). The merge step
		// re-enforces branch protection, so trusting the mergeable verdict here
		// cannot merge anything GitHub would actually block.
		if pr.CIStatus == "pending" && pr.Mergeable != github.MergeableYes {
			// Genuinely not ready: a required check is still running (or
			// mergeability is unknown/no). Leave it out of both buckets, as
			// before — it neither merges nor gets a fix dispatched.
			continue
		}

		if pr.Mergeable == github.MergeableNo {
			// A conflicting PR cannot merge no matter how green its checks
			// are. Listing it as merge-eligible left the eligible count stuck
			// at N forever while nothing could actually merge (console
			// #23002/#23003, 2026-08-31: the only two build-gate-green PRs
			// were DIRTY go.mod dependabot bumps). Conflicts are the
			// rebase/needs-human path's job, not the sweep's — keep them out
			// of the eligible bucket.
			continue
		}

		dco := "unknown"
		for _, l := range pr.Labels {
			switch l {
			case "dco-signoff: yes":
				dco = "yes"
			case "dco-signoff: no":
				dco = "no"
			}
		}
		if requireReviewApproval && (!reviewLoaded || !reviewArtifact.HasAggregateApproval(fullRepo, pr.Number, pr.HeadSHA)) {
			continue
		}
		eligible = append(eligible, eligiblePR{
			Number:    pr.Number,
			Repo:      fullRepo,
			Title:     pr.Title,
			Author:    pr.Author,
			Labels:    pr.Labels,
			Mergeable: mergeableJSON(pr.Mergeable),
			DCO:       dco,
			HeadSHA:   pr.HeadSHA,
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

func planReviewDispatch(cfg *config.Config, actionable *github.ActionableResult, agentMgr *agent.Manager, logger *slog.Logger) review.DispatchPlan {
	if cfg == nil || actionable == nil || !cfg.Review.RequireApproval || !cfg.Review.FanOut {
		return review.DispatchPlan{}
	}
	state, err := review.LoadDispatchState("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review dispatch state unavailable; starting fresh", "error", err)
	}
	artifact, err := review.LoadArtifact("")
	if err != nil && !os.IsNotExist(err) {
		logger.Warn("review verdict artifact unavailable for dispatch planning", "error", err)
	}
	prs := make([]review.PullRequest, 0, len(actionable.PRs.Items))
	for _, pr := range actionable.PRs.Items {
		lane := classify.Classify(github.Issue{Title: pr.Title, Labels: pr.Labels}).Lane
		prs = append(prs, review.PullRequest{
			Repo:    pr.Repo,
			Number:  pr.Number,
			Title:   pr.Title,
			Author:  pr.Author,
			HeadSHA: pr.HeadSHA,
			URL:     pr.URL,
			Lane:    string(lane),
		})
	}
	agents := make([]review.AgentCapability, 0, len(cfg.Agents))
	for name, ac := range cfg.EnabledAgents() {
		agents = append(agents, review.AgentCapability{
			Name:           name,
			Enabled:        true,
			Paused:         ac.Paused || (agentMgr != nil && agentMgr.IsPaused(name)),
			OnDemand:       ac.OnDemand,
			UsesKick:       ac.UsesGovernorKick(),
			Role:           ac.Role,
			LaneKeywords:   ac.LaneKeywords,
			DetectKeywords: ac.DetectKeywords,
			Aliases:        ac.Aliases,
		})
	}
	plan := review.PlanDispatch(prs, artifact, state, review.DispatchOptions{
		RequireApproval:    cfg.Review.RequireApproval,
		FanOut:             cfg.Review.FanOut,
		MaxParallelReviews: cfg.Review.EffectiveMaxParallelReviews(),
		ReviewerAgents:     cfg.Review.ReviewerAgents,
		FixerAgent:         cfg.Review.FixerAgent,
		ProjectOrg:         cfg.Project.Org,
		AIAuthor:           cfg.EffectiveAIAuthor(),
		Agents:             agents,
	})
	if len(plan.ReviewKicks)+len(plan.FixKicks) > 0 {
		logger.Info("review swarm dispatch planned", "review_kicks", len(plan.ReviewKicks), "fix_kicks", len(plan.FixKicks))
	}
	return plan
}

func refreshReviewVerdicts(cfg *config.Config, logger *slog.Logger) {
	if cfg == nil || !cfg.Review.RequireApproval {
		return
	}
	artifact, err := review.CollectAndWrite("", "", review.AggregateOptions{})
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("failed to refresh review verdicts", "error", err)
		}
		return
	}
	logger.Info("review verdict artifact refreshed", "aggregates", len(artifact.Items))
}

func persistReviewDispatchState(plan review.DispatchPlan, delivered []review.DispatchKick, logger *slog.Logger) {
	planned := append(append([]review.DispatchKick(nil), plan.ReviewKicks...), plan.FixKicks...)
	if plan.State.GeneratedAt.IsZero() && len(planned) == 0 {
		return
	}
	state := review.ConfirmDelivered(plan.State, planned, delivered)
	if err := review.WriteDispatchState("", state); err != nil {
		logger.Warn("failed to persist review dispatch state", "error", err)
	}
}

// normalizedAutoMergeLabel resolves the configured queue label, falling back
// to the shared default when the value is blank. Client.SetAutoMergeLabel
// ignores blank input (keeping whatever was set before) and
// Client.AutoMergeLabel falls back on read, but the cmd layer normalizes
// eagerly too so a partially-populated config can never propagate an unnamed
// label to a fresh client.
func normalizedAutoMergeLabel(label string) string {
	if label = strings.TrimSpace(label); label != "" {
		return label
	}
	return github.AutoMergeQueuedLabel
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
	// #4041: a persisted sensing_login that is byte-identical to the
	// pre-#3959 default set carries no operator intent — it is the old
	// defaults materialized by an earlier save. Replaying it here would
	// re-pin the false-positive-prone generic patterns over the corrected
	// code defaults the config layer just applied. Skip it; a genuinely
	// customized list still replays verbatim.
	if len(o.SensingLogin) > 0 && !config.IsLegacyDefaultLoginPatterns(o.SensingLogin) {
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
	return loadNousStateFromPaths(logger, nousGovernorDir, nousSnapshotDir)
}

func loadNousStateFromPaths(logger *slog.Logger, governorDir, snapshotDir string) *dashboard.NousState {
	state := &dashboard.NousState{
		Mode:   "observe",
		Scope:  "governor",
		Phase:  "collecting",
		Status: make(map[string]interface{}),
		Config: make(map[string]interface{}),
	}

	if ledgerData, err := os.ReadFile(filepath.Join(governorDir, "ledger.json")); err == nil {
		var ledger struct {
			Iterations []map[string]interface{} `json:"iterations"`
		}
		if err := json.Unmarshal(ledgerData, &ledger); err == nil {
			state.Ledger = ledger.Iterations
			logger.Info("nous ledger loaded", "iterations", len(state.Ledger))
		}
	}

	if principlesData, err := os.ReadFile(filepath.Join(governorDir, "principles.json")); err == nil {
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
	if entries, err := os.ReadDir(snapshotDir); err == nil {
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

	// DETERMINISTIC LANE ORDER (#5856). classifyLane is first-match-wins over
	// this slice, and the loop above built it by ranging over cfg.Agents — a Go
	// MAP, whose iteration order is randomized per range. So an issue matching
	// two lanes went to whichever of them happened to come out of the map
	// first, and the winner could differ between two runs of the same binary on
	// the same config. The reported example matched sec-check on its title
	// ("security") and scanner on its label; which one it landed in was a coin
	// flip that nothing recorded.
	//
	// Sorting by name is a STABLE order, not a meaningful precedence — it does
	// not claim architect deserves an issue more than scanner does. What it buys
	// is reproducibility: the same issue and the same config now classify the
	// same way every time, so a misroute is a bug someone can chase instead of
	// an intermittency. Choosing a deliberate precedence between colliding lanes
	// is a separate call for whoever owns the lane table.
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Name < lanes[j].Name })
	if len(lanes) > 0 {
		classify.SetLanes(lanes)
	}
	// Tier-classification keywords (config-driven, mirroring SetLanes). Empty
	// lists leave the built-in defaults in force, so an absent classifier block
	// keeps behavior unchanged. Always call so a reload that CLEARS the block
	// restores defaults.
	classify.SetTierKeywords(cfg.Classifier.SimpleKeywords, cfg.Classifier.ComplexSignals)
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
	if _, err := fmt.Sscanf(color, "%x", &result); err != nil {
		return 0x95a5a6 // malformed hex: fall back to the same default as an empty string
	}
	return result
}

// logAgentSandboxPosture emits the sandbox gate diagnostics from
// config.AgentSandboxGateWarnings at WARN.
//
// Split out so boot and the config-watcher reload report identically — an
// operator who flips the Security tab's sandbox toggle never restarts, so a
// boot-only check would never reach the person who most needs it.
func logAgentSandboxPosture(logger *slog.Logger, cfg *config.Config) {
	for _, warning := range config.AgentSandboxGateWarnings(cfg) {
		logger.Warn("agent sandbox posture", "warning", warning)
	}
}

func runHub(logger *slog.Logger, configPath string) {
	port := 3001
	if p := os.Getenv("HIVE_HUB_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	logger.Info("starting in HUB mode", "port", port)

	hubSrv := hub.NewHubServer(port, logger, gitShort, gitBranch)
	if cfg, err := config.LoadWithDashboardOverlay(configPath); err == nil {
		notifier := notify.New(cfg.Notifications, logger)
		notifier.SetHiveID(cfg.HiveID)
		buildHookDispatcher(cfg, hookSinks{Notifier: notifier}, logger)
	} else if !errors.Is(err, os.ErrNotExist) {
		logger.Warn("hub hooks disabled: failed to load config", "path", configPath, "error", err)
	}
	installUpgradePauseEmitter(hubSrv)

	// /api/reach (#3994) needs merged-PR metadata (merge SHA, changed
	// files). The hub mode has no ambient GitHub client, so reuse the
	// standard token client when credentials exist; without a token the
	// endpoint reports 503 rather than serving fabricated data. The base
	// branch is the hub's own running branch — the lineage its fleet runs.
	if ghToken := os.Getenv("HIVE_GITHUB_TOKEN"); ghToken != "" {
		reachGH := github.NewClient(ghToken, "hivecommons", []string{"hive"}, logger, "")
		hubSrv.SetReachPRSource(hub.NewGitHubPRSource(reachGH, gitBranch))
	}
	// Wire 2a's heartbeat-fed registry store into the /api/reach endpoint
	// (#3973 epic: producer #3993 → consumer #3994). Unconditional — the
	// registry-backed reporter has no external dependencies, and without it
	// the endpoint would keep answering from the empty stub forever.
	hubSrv.SetReachReporter(hubSrv.RegistryReachReporter())

	// Long-lived SaaS pollers (provision watcher, SHA poller, auth audit,
	// advisory diagnostics) are started here — at the composition root — not
	// inside route registration, so constructing a HubServer stays free of
	// background goroutines.
	hubSrv.StartBackgroundPollers(context.Background())

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

// resolveLiteLLMInferenceRoute resolves the endpoint and model an agent's
// inference route should use for the built-in "litellm" backend. It is the
// whole route-install decision tree for that backend, lifted out of main() so
// it can be unit-tested (#5460); main() calls it and keeps ownership of key,
// CA bundle and logging.
//
// requestedModel is the model the agent asked for ("" when it named none). The
// returned model is that request when non-empty, otherwise the default
// inherited from whichever source supplied the endpoint.
//
// Resolution order — each step matches the behavior shipped in 231ca4b:
//
//  1. local_proxy: the Go translator forwards to the bundled litellm proxy on
//     loopback, overriding any configured remote endpoint.
//  2. the legacy governor.litellm block (HIVE_LITELLM_ENDPOINT or yaml), whose
//     default_model supplies the model.
//  3. the EXPLICIT gateway named by this backend. A hive configured only
//     through the Model Gateways tab leaves the legacy block empty; the key
//     and CA bundle already resolve from that gateway, so the endpoint must
//     too, or NO route is installed and every agent call dies "502 no
//     inference route" while the Gateways tab Test button happily passes
//     (ains-validation/pocketmini, 2026-08-31 — #5393).
//
// ok is false when no source yields an endpoint: the caller must warn and
// install NO route. It never invents an endpoint, and never returns a route
// with an empty endpoint — a silently empty endpoint is the 502 this whole
// path exists to prevent.
func resolveLiteLLMInferenceRoute(cfg *config.Config, backend, requestedModel string) (endpoint, model string, ok bool) {
	lc := cfg.Governor.LiteLLM
	model = requestedModel
	endpoint = lc.ResolveEndpoint()
	if lc.LocalProxy {
		endpoint = litellmLocalProxyURL()
	}
	if endpoint == "" {
		if gw := cfg.Governor.ResolveGateway(backend); gw != nil && gw.Endpoint != "" {
			endpoint = gw.Endpoint
			if model == "" {
				model = gw.DefaultModel
			}
		}
	}
	if endpoint == "" {
		return "", requestedModel, false
	}
	if model == "" {
		model = lc.DefaultModel
	}
	return endpoint, model, true
}

// resolveWatsonxGateway finds the gateway backing the built-in "watsonx" agent
// backend. It prefers a gateway explicitly NAMED watsonx, then falls back to
// the first gateway of KIND watsonx — so `backend: watsonx` works whether the
// operator named their gateway "watsonx" or something descriptive like
// "ibm-granite-prod". Returns nil when no watsonx gateway is configured.
func resolveWatsonxGateway(cfg *config.Config) *config.GatewayConfig {
	gws := cfg.Governor.ResolvedGateways()
	for i := range gws {
		if strings.EqualFold(gws[i].Name, config.GatewayKindWatsonx) &&
			strings.EqualFold(gws[i].Kind, config.GatewayKindWatsonx) {
			gw := gws[i]
			return &gw
		}
	}
	for i := range gws {
		if strings.EqualFold(gws[i].Kind, config.GatewayKindWatsonx) {
			gw := gws[i]
			return &gw
		}
	}
	return nil
}

// resolveGatewayAuth resolves the bearer token and non-secret extra headers an
// agent's inference route should present for a gateway.
//
// For every kind except watsonx this is the resolved API key verbatim and no
// extra headers. watsonx authenticates its OpenAI-compatible model gateway with
// a SHORT-LIVED IAM bearer minted from the IBM Cloud API key (not the raw key)
// and scopes billing/limits by a project id sent as X-IBM-Project-ID, so both
// are set here via the shared process-wide minter (pkg/watsonx.DefaultMinter),
// whose cache means one token is reused across inference, probes and discovery.
//
// Shared by the named-gateway branch and the built-in "watsonx" backend branch
// so the two cannot authenticate differently. Never logs the key or the token.
func resolveGatewayAuth(gw *config.GatewayConfig, agentName, backend string, logger *slog.Logger) (string, map[string]string) {
	apiKey := gw.ResolveAPIKey()
	if !strings.EqualFold(gw.Kind, config.GatewayKindWatsonx) {
		return apiKey, nil
	}
	if token, err := watsonx.DefaultMinter.Token(context.Background(), apiKey); err != nil {
		logger.Warn("watsonx IAM token mint failed; agent inference will fail until the key/project are valid",
			"agent", agentName, "gateway", backend, "error", err.Error())
		// Leave apiKey as-is (the raw key). watsonx will reject it, surfacing a
		// clear upstream 401 rather than a silent success — better than dropping
		// the route entirely.
	} else {
		apiKey = token
	}
	var extraHeaders map[string]string
	if gw.ProjectID != "" {
		extraHeaders = map[string]string{watsonx.ProjectIDHeader: gw.ProjectID}
	}
	return apiKey, extraHeaders
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
