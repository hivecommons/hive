package main

// GitHub App private-key file management: where per-App-ID and per-hive key
// files live on disk, how a key is written, resolved, fingerprinted and how a
// failure to find one is described.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

	"github.com/hivecommons/hive/pkg/apphealth"
	"github.com/hivecommons/hive/pkg/config"
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

// appKeyPaths snapshots the two App key path vars for a pkg/apphealth call.
// Read at call time on purpose: tests repoint these vars, and capturing them
// once would silently ignore that.
func appKeyPaths() apphealth.KeyPaths {
	return apphealth.KeyPaths{Spoke: spokeAppKeyPath, Provisioned: spokeProvisionedAppKeyPath}
}
