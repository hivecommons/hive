// Package spokebackup implements a self-service, owner-triggered backup of a
// single spoke, streamed to the owner's browser as an encrypted archive.
//
// # Relationship to the hub backup
//
// pkg/hubbackup runs unattended, nightly, across the whole fleet, and is
// deliberately config-only: including bulk agent state fleet-wide would be
// roughly 40GB and too slow to run nightly or restore quickly. That exclusion —
// most importantly of /data/beads, the agent work ledger — is documented in
// issue #2318.
//
// This package has the opposite cost profile. One owner backing up one hive on
// demand can afford the bead ledger, so beads ARE included here. That makes the
// two backups complementary rather than redundant: hubbackup answers "the hub
// cluster is gone", spokebackup answers "I want a copy of my hive that I
// control, including what my agents have learned and filed".
//
// The AES-256-GCM sealing, the SHA-256 manifest and the traversal-safe
// extraction are reused from pkg/hubbackup rather than reimplemented, so both
// archives share one audited crypto path and one archive format.
//
// # Encryption
//
// The archive is ALWAYS encrypted. It carries GitHub App private keys
// (gh-app-key*.pem), so an unencrypted copy would be a credential leak the
// moment it reached the owner's Downloads folder. The key comes from
// HIVE_BACKUP_KEY with no default; unset means the endpoint refuses to run
// rather than streaming plaintext secrets to a browser.
//
// The key is deliberately NOT embedded in, or derivable from, the archive —
// an artifact that carries its own decryption key is not encrypted at all.
package spokebackup

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hubbackup"
)

// Filesystem layout of spoke state.
const (
	// DefaultDataDir is the spoke PVC mount point.
	DefaultDataDir = "/data"

	// EnvDataDir overrides the spoke data directory (test seam).
	EnvDataDir = "HIVE_SPOKE_BACKUP_DATA_DIR"

	// beadsSubdir is the agent work ledger, relative to the data dir. This is
	// the directory hubbackup excludes; including it is this backup's reason
	// to exist. Its layout is one subdirectory per agent, so it grows with the
	// agent roster rather than being a fixed set of five.
	beadsSubdir = "beads"

	// configOverlayFile is the PVC overlay — the layer that wins the boot-time
	// merge over the ConfigMap seed, and therefore the file that actually
	// carries a hive's configuration. Sampled seeds are 4–11% of the running
	// config and omit whole top-level blocks (policies, data, knowledge,
	// notifications, hive_id).
	configOverlayFile = "hive.yaml.dashboard"

	// configRuntimeFile is the persisted runtime config, formerly
	// hive.yaml.bak. On Kubernetes it is the POST-MERGE SNAPSHOT written by
	// the entrypoint after the merge; on Docker/LXC, where there is no
	// ConfigMap and no overlay, it is the boot-time source of truth restored
	// over the config path on every boot. The old ".bak" name described only
	// the first role, which is why it was renamed.
	//
	// An earlier comment here stated that copy-config restores from it and
	// falls back to the seed only when it is absent. That is true on only 3
	// of 51 live K8s hives; the variant new hives get merely reports whether
	// it exists and never restores from it. So on Kubernetes it is not, in
	// general, "what comes back after a restart" — the overlay is.
	//
	// It is still captured, for two reasons: it is what the disaster fallback
	// reads when the ConfigMap is missing or empty, and it is the only copy
	// retaining env-derived secrets, since the overlay is written secret-free
	// on purpose (config.dashboardOverlayBytes).
	//
	// hive.yaml itself remains excluded: it is regenerated from seed + overlay
	// on every boot, and on a sampled production spoke was a stale root-owned
	// copy days older than the live config.
	configRuntimeFile = "hive.yaml.runtime"

	// configRuntimeFileLegacy is the pre-rename name. Still captured because
	// the rename is copy-forward: a hive that has not saved since upgrading
	// carries only this file, and omitting it would silently drop that hive's
	// config — and its env-derived secrets — from the archive.
	configRuntimeFileLegacy = "hive.yaml.bak"

	// hiveIDFile identifies this hive to the hub.
	hiveIDFile = "hive-id"

	// stateFile is the agent runtime state snapshot.
	stateFile = "hive-state.json"

	// appKeyGlob matches the GitHub App private keys. These are credentials:
	// they are why the archive must be encrypted, and they must never be
	// logged or written anywhere world-readable.
	appKeyGlob = "gh-app-key*.pem"
)

// Archive layout: top-level directories inside the tarball.
const (
	// spokePrefix holds files copied from the spoke PVC root.
	spokePrefix = "spoke"

	// beadsPrefix holds the agent work ledger.
	beadsPrefix = "beads"
)

// Size guards. A backup is streamed through the dashboard to a browser, so it
// must stay responsive; an unbounded read of a ~796MB PVC would not be.
const (
	// MaxArchiveBytes caps the assembled plaintext archive. Measured on a
	// production spoke the included set is roughly 3-5MB before compression
	// (beads 2.6MB, hive-state.json ~110KB, config ~14KB, keys ~3KB), so this
	// leaves ample headroom for a hive with a much larger bead ledger while
	// still refusing to stream something browser-hostile.
	MaxArchiveBytes = 256 << 20 // 256 MiB

	// MaxFileBytes skips any single file larger than this. It keeps one
	// runaway log or state blob from dominating an otherwise small archive.
	MaxFileBytes = 64 << 20 // 64 MiB
)

// archiveCapBytes is the cap Build applies, indirected through a variable so a
// test can shrink it and prove Build really enforces a cap. Production always
// uses MaxArchiveBytes; nothing outside tests assigns this.
var archiveCapBytes = MaxArchiveBytes

// BuildTimeout bounds a single backup so a wedged filesystem read cannot hold
// a dashboard request open indefinitely.
const BuildTimeout = 2 * time.Minute

// includedRootFiles are the individual files copied from the data-dir root.
//
// Everything not listed here is excluded deliberately:
//
//   - nous/    287MB of timestamped learned-context snapshots. Regenerable,
//     and the single largest directory on the PVC.
//   - home/    agent CLI state and credential caches. Bulk, regenerable, and
//     carries third-party tokens that should not be duplicated into an
//     archive the owner will store outside the cluster.
//   - logs/    32MB of rotating logs. Diagnostic, not state.
//   - graph/, snapshots/, vaults/  derived or bulk artifacts (~19MB combined).
//   - prompt-history.jsonl  3.1MB append-only transcript; diagnostic.
//   - audit.jsonl  860KB append-only audit trail. Excluded to keep the
//     archive small and fast; it is a record OF actions, not state needed to
//     reconstitute the hive.
//   - dashboard-sessions.json  live browser session tokens. Excluded on
//     purpose: these are credentials with no restore value, and restoring
//     them would resurrect sessions that should have expired.
//   - hive.yaml  regenerated from seed + overlay on every boot; see
//     configOverlayFile and configRuntimeFile.
//
// Both runtime-config names are listed: missing files are recorded in Skipped
// rather than failing, so a hive carries whichever it has.
var includedRootFiles = []string{
	configOverlayFile,
	configRuntimeFile,
	configRuntimeFileLegacy,
	hiveIDFile,
	stateFile,
}

// DataDir returns the spoke data directory, honouring the test-seam override.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv(EnvDataDir)); v != "" {
		return v
	}
	return DefaultDataDir
}

// ArchiveName returns a download filename for a backup of hiveID taken at t.
// The ".enc" suffix is load-bearing UX: it tells the owner at a glance that
// the file in their Downloads folder is not directly readable.
func ArchiveName(hiveID string, t time.Time) string {
	id := sanitizeComponent(hiveID)
	if id == "" {
		id = "hive"
	}
	return fmt.Sprintf("hive-spoke-backup-%s-%s.tar.gz.enc", id, t.UTC().Format("20060102T150405Z"))
}

// sanitizeComponent reduces s to characters safe in a filename, so a hive ID
// can never inject path separators or quotes into a Content-Disposition header.
func sanitizeComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// Result reports what a backup captured, for logging and for the manifest the
// owner sees. It intentionally carries no file CONTENT and no secret values.
type Result struct {
	// Sealed is the encrypted archive, ready to stream.
	Sealed []byte

	// Manifest describes the archive contents and digests.
	Manifest *hubbackup.Manifest

	// BeadDirs lists the per-agent bead directories captured.
	BeadDirs []string

	// Skipped records files that could not be read or were too large, so a
	// gap is visible in the result rather than silently absent.
	Skipped map[string]string
}

// Build assembles and seals a backup of this spoke.
//
// key must come from hubbackup.LoadKey (HIVE_BACKUP_KEY); Build does not read
// the environment itself so a caller can never accidentally pass a default.
func Build(key []byte, hiveID string, logger *slog.Logger) (*Result, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("refusing to build an unencrypted spoke backup: no key supplied")
	}
	dataDir := DataDir()
	res := &Result{Skipped: map[string]string{}}

	b := hubbackup.NewBuilder()

	// 1. Authoritative config, hive identity and agent state.
	for _, name := range includedRootFiles {
		path := filepath.Join(dataDir, name)
		content, err := readCapped(path)
		if err != nil {
			// A missing hive-state.json or hive-id is survivable; a missing
			// config is worth surfacing, but neither should abort the backup
			// the owner asked for. Record the gap instead.
			res.Skipped[name] = err.Error()
			logger.Warn("spoke backup: skipping file", "file", name, "err", err)
			continue
		}
		if err := b.AddBytes(filepath.Join(spokePrefix, name), 0o600, content); err != nil {
			return nil, fmt.Errorf("archive %s: %w", name, err)
		}
	}

	// 2. GitHub App private keys. Credentials — never logged, and the reason
	//    this archive is unconditionally encrypted.
	keyPaths, _ := filepath.Glob(filepath.Join(dataDir, appKeyGlob))
	sort.Strings(keyPaths)
	for _, path := range keyPaths {
		name := filepath.Base(path)
		content, err := readCapped(path)
		if err != nil {
			res.Skipped[name] = err.Error()
			logger.Warn("spoke backup: skipping app key", "file", name, "err", err)
			continue
		}
		if err := b.AddBytes(filepath.Join(spokePrefix, name), 0o600, content); err != nil {
			return nil, fmt.Errorf("archive app key: %w", err)
		}
	}

	// 3. The bead ledger — the whole point of this backup. One subdirectory
	//    per agent, discovered rather than hardcoded, so a hive running agents
	//    beyond the original five still gets a complete ledger.
	beadsDir := filepath.Join(dataDir, beadsSubdir)
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		res.Skipped[beadsSubdir] = err.Error()
		logger.Warn("spoke backup: beads dir unreadable", "dir", beadsDir, "err", err)
	} else {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			agent := e.Name()
			src := filepath.Join(beadsDir, agent)
			if err := b.AddTree(src, filepath.Join(beadsPrefix, agent), nil, logger); err != nil {
				res.Skipped[filepath.Join(beadsSubdir, agent)] = err.Error()
				logger.Warn("spoke backup: bead dir failed", "agent", agent, "err", err)
				continue
			}
			res.BeadDirs = append(res.BeadDirs, agent)
		}
	}

	man := &hubbackup.Manifest{
		FormatVersion: hubbackup.FormatVersion(),
		CreatedAt:     time.Now().UTC(),
		HubDataDir:    dataDir,
		SpokeIDs:      []string{hiveID},
		SpokeErrors:   map[string]string{},
	}
	for k, v := range res.Skipped {
		man.SpokeErrors[k] = v
	}

	sealed, err := b.Finish(key, man, archiveCapBytes)
	if err != nil {
		return nil, err
	}
	res.Sealed = sealed
	res.Manifest = man
	return res, nil
}

// readCapped reads path, refusing files above MaxFileBytes so a single runaway
// file cannot blow up an archive that is streamed to a browser.
func readCapped(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if info.Size() > MaxFileBytes {
		return nil, fmt.Errorf("file is %d bytes, above the %d-byte per-file cap", info.Size(), MaxFileBytes)
	}
	return os.ReadFile(path)
}
