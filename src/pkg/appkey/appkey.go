// Package appkey owns the GitHub App signing-key files a spoke keeps on disk:
// where they live, which one is correct for the App the hive claims, and how a
// hub-delivered key is written down atomically.
//
// It was extracted from package main (hivecommons/hive#5898 phase 1), where it
// sat as ~200 lines of domain logic in a 9,600-line file that CI never ran:
// the test workflows run `go test ./pkg/...` only, so cmd/hive's tests — 475
// lines of them for this logic alone — were never executed. Moving the code
// here is what puts them under CI, with no workflow change.
//
// Deliberately NOT placed in pkg/github: this needs config.AppKeyFingerprint*,
// and pkg/github#5953 phase 1 just removed that package's dependency on
// pkg/config. Putting App-key file handling there would reintroduce exactly
// the upward dependency that change deleted.
package appkey

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Default paths on a running spoke.
const (
	DefaultDataDir            = "/data"
	DefaultDataKeyPath        = "/data/gh-app-key.pem"
	DefaultProvisionedDir     = "/secrets"
	DefaultProvisionedKeyPath = "/secrets/gh-app-key.pem"
	// DefaultFileMode is rw------- : signing material must never be readable by
	// anything else sharing the PVC or the pod.
	DefaultFileMode os.FileMode = 0o600
)

// perAppIDKeyFilePrefix / Suffix bracket the per-app-id key filename so a scan
// of the directory can recover the app_id from the name.
const (
	perAppIDKeyFilePrefix = "gh-app-key-"
	perAppIDKeyFileSuffix = ".pem"
)

// Resolver locates and writes App signing keys against a set of directories.
//
// The paths are fields rather than package-level vars — which is what they were
// in package main — so a test can point one Resolver at a temp dir without
// mutating state shared with every other test. Production uses Default().
type Resolver struct {
	// DataDir is the PVC directory where hub-delivered per-app-id keys land,
	// one file per App the fleet knows: gh-app-key-<appid>.pem.
	DataDir string
	// DataKeyPath is the generic PVC key path older builds wrote on every key
	// delivery. It does not name the App it holds; see Resolve.
	DataKeyPath string
	// ProvisionedDir is the read-only projected-Secret mount where provisioning
	// places per-app-id keys. Nothing ever writes here; it is a lookup source.
	ProvisionedDir string
	// ProvisionedKeyPath is the generic path the provisioning template hardcodes
	// for every App-using hive.
	ProvisionedKeyPath string
	// FileMode is applied to keys this Resolver writes.
	FileMode os.FileMode
}

// Default returns a Resolver using the production spoke layout.
func Default() *Resolver {
	return &Resolver{
		DataDir:            DefaultDataDir,
		DataKeyPath:        DefaultDataKeyPath,
		ProvisionedDir:     DefaultProvisionedDir,
		ProvisionedKeyPath: DefaultProvisionedKeyPath,
		FileMode:           DefaultFileMode,
	}
}

func (r *Resolver) PerAppIDKeyPath(appID int64) string {
	if appID <= 0 {
		return ""
	}
	return filepath.Join(r.DataDir, fmt.Sprintf("gh-app-key-%d.pem", appID))
}

// perAppIDProvisionedKeyPath is perAppIDKeyPath's read-only twin: the same
// per-app-id filename under the provisioning Secret mount. It is consulted only
// when the PVC has no usable key for the app_id, so a heartbeat-delivered key
// (which can be rotated) always wins over the one baked in at provision time.
func (r *Resolver) provisionedPerAppIDKeyPath(appID int64) string {
	if appID <= 0 {
		return ""
	}
	return filepath.Join(r.ProvisionedDir, fmt.Sprintf("gh-app-key-%d.pem", appID))
}

// heldPerAppIDKeyFingerprints scans the PVC for per-app-id key files
// (gh-app-key-<appid>.pem) and returns app_id (decimal string) → fingerprint for
// every one that holds a usable key. It is what the spoke reports so the hub
// delivers the fleet's additional keys idempotently: a key already present with
// the right fingerprint is not re-sent.
//
// It never returns key material — only fingerprints. A missing directory,
// unreadable file, or unparseable key is silently skipped: the worst case is the
// hub re-delivers a key the spoke already writes idempotently, never a crash.
func (r *Resolver) HeldFingerprints() map[string]string {
	entries, err := os.ReadDir(r.DataDir)
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
		fp, fpErr := config.AppKeyFingerprintFromFile(filepath.Join(r.DataDir, name))
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
func (r *Resolver) WritePerAppIDKey(appID int64, pemData string) (string, error) {
	path := r.PerAppIDKeyPath(appID)
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
	if err := os.MkdirAll(r.DataDir, 0o700); err != nil {
		return "", fmt.Errorf("create app key dir: %w", err)
	}
	tmp, err := os.CreateTemp(r.DataDir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return "", fmt.Errorf("create temp app key file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename below succeeds
	if err := tmp.Chmod(r.FileMode); err != nil {
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

// resolveAppKeyFile picks which App private key this process will actually sign
// with, given the configured key_file and the GH_APP_KEY_FILE env override.
//
// WHY THE /data PREFERENCE MATTERS
//
// A hub-delivered key lands at r.DataKeyPath (/data, on the PVC) and the
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
func (r *Resolver) Resolve(configured, envOverride string, appID int64) string {
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
		if v == r.DataKeyPath || v == r.ProvisionedKeyPath {
			if p := r.PerAppIDKeyPath(appID); p != "" {
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
	if p := r.PerAppIDKeyPath(appID); p != "" {
		if fp, err := config.AppKeyFingerprintFromFile(p); err == nil && fp != "" {
			return p
		}
	}
	// Same idea, but from the read-only provisioning mount: a hive provisioned
	// with the fleet's full key set can sign as its configured App on its very
	// first boot, before any heartbeat has delivered anything to the PVC. Ranked
	// BELOW the PVC copy so a rotated key delivered by heartbeat always wins over
	// the one frozen into the Secret at provision time.
	if p := r.provisionedPerAppIDKeyPath(appID); p != "" {
		if fp, err := config.AppKeyFingerprintFromFile(p); err == nil && fp != "" {
			return p
		}
	}
	// Prefer a usable hub-delivered key on the PVC; fall back to the provisioning
	// mount only when /data has no parseable key.
	if fp, err := config.AppKeyFingerprintFromFile(r.DataKeyPath); err == nil && fp != "" {
		return r.DataKeyPath
	}
	return r.ProvisionedKeyPath
}

// PerAppIDKeyPathFor is PerAppIDKeyPath for callers that already hold the
// app_id in string form — notably the dashboard's forge-app inventory, which
// builds its rows from the keys of HeldFingerprints. It exists so the
// filename scheme stays inside this package instead of being reassembled by
// every caller from exported prefix/suffix constants.
func (r *Resolver) PerAppIDKeyPathFor(appIDStr string) string {
	if appIDStr == "" {
		return ""
	}
	return filepath.Join(r.DataDir, perAppIDKeyFilePrefix+appIDStr+perAppIDKeyFileSuffix)
}

// ReportedFingerprint is the fingerprint of the key this Resolver would
// actually sign with, for reporting upward (the hub heartbeat). Moved here
// from package main with the rest of the App-key file logic
// (hivecommons/hive#5898 phase 1); it is a pure read over the same candidate
// ladder Resolve walks, so it belongs beside it rather than in a binary.
// reportedAppKeyFingerprint returns the non-secret fingerprint of the App key
// this spoke is ACTUALLY using, for the heartbeat payload. It fingerprints the
// resolved key file rather than a hard-coded path so the hub compares against
// the key that would really sign a JWT.
//
// Returns "" whenever there is no usable key — no file, empty file, or
// unparseable contents. All three mean the same thing to the hub ("this spoke
// cannot authenticate") and are repaired identically. The private key itself is
// never returned, and never enters the payload.
func (r *Resolver) ReportedFingerprint(keyFile string, appID int64) string {
	// Lead with the same path resolveAppKeyFile would sign with, so the hub is
	// told about the key actually in effect and never about a shadowed one.
	candidates := []string{
		r.Resolve(keyFile, os.Getenv("GH_APP_KEY_FILE"), appID),
		r.PerAppIDKeyPath(appID),
		keyFile, r.DataKeyPath, r.ProvisionedKeyPath,
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
func (r *Resolver) HasPerHiveKey(keyFile string, appID int64) bool {
	fp, err := config.AppKeyFingerprintFromFile(r.ProvisionedKeyPath)
	if err != nil || fp == "" {
		return false
	}
	// The provisioned key exists. It is the effective credential only if the
	// resolved key file still points at it — resolveAppKeyFile is the single
	// authority on that, so an unconfigured hive that has already taken delivery
	// of a /data key (or a per-app-id key) correctly stops claiming a per-hive
	// override.
	return r.Resolve(keyFile, os.Getenv("GH_APP_KEY_FILE"), appID) == r.ProvisionedKeyPath
}
