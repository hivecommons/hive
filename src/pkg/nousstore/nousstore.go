// Package nousstore resolves and manages the durable on-disk layout of the
// Nous (Experiment/Strategy Lab) subsystem (RFC #11585 part 1, #11694).
//
// Before this package, Nous state was split across two roots: baseline
// snapshots lived on the PVC under /data/nous/snapshots, while the governor
// experiment ledger and learned principles were read from
// /var/run/nous/governor — a tmpfs that does not survive a container restart
// or an image upgrade. The dashboard's panel configuration (mode, scope and the
// goals/output/schedule/… sections) was in-memory only.
//
// Everything now lives under one root, /data/nous by default:
//
//	<root>/governor/ledger.json       experiment ledger
//	<root>/governor/principles.json   learned principles
//	<root>/snapshots/<epoch-ms>.json  baseline snapshots (bulk, regenerable)
//	<root>/panel-config.json          dashboard panel configuration
//	<root>/gates/pending.json         the gate the agent is waiting on (#11696)
//	<root>/gates/decision.json        the owner's answer to that gate
//	<root>/gates/history.jsonl        every gate decision ever recorded
//
// The root is configurable (data.nous_dir, overridden by HIVE_NOUS_DATA_DIR).
// On every boot MigrateLegacy copies any ledger/principles file from the
// previous /var/run/nous/governor location forward when the durable copy is
// missing or older, so a hive upgraded in place keeps its history and a
// governor loop that still writes to the old path is picked up on the next
// restart. Nothing is ever copied backwards or deleted.
package nousstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Layout constants. Exported so cmd/hive, pkg/dashboard and pkg/spokebackup
// agree on one spelling of every path.
const (
	// DefaultRoot is the durable Nous root on the spoke PVC.
	DefaultRoot = "/data/nous"

	// EnvRoot overrides the configured root (data.nous_dir). It wins over the
	// YAML value so an operator can relocate the tree without editing config.
	EnvRoot = "HIVE_NOUS_DATA_DIR"

	// LegacyGovernorDir is where the governor experiment loop wrote its ledger
	// and principles before #11694. It is tmpfs on a container and is read as
	// a migration source only.
	LegacyGovernorDir = "/var/run/nous/governor"

	// GovernorSubdir holds the governor-scope ledger and principles.
	GovernorSubdir = "governor"

	// SnapshotsSubdir holds baseline snapshots, one file per evaluation cycle.
	SnapshotsSubdir = "snapshots"

	// LedgerFile is the experiment ledger inside GovernorSubdir.
	LedgerFile = "ledger.json"

	// PrinciplesFile is the learned-principles file inside GovernorSubdir.
	PrinciplesFile = "principles.json"

	// PanelConfigFile persists the dashboard panel configuration at the root.
	PanelConfigFile = "panel-config.json"

	// GatesSubdir holds the gate protocol files; see gates.go.
	GatesSubdir = "gates"
)

// File modes for everything this package creates.
const (
	dirMode  fs.FileMode = 0o755
	fileMode fs.FileMode = 0o644
)

// tmpSuffix is appended to a file while a new version is being written; the
// rename onto the final name is atomic.
const tmpSuffix = ".tmp"

// migratedFiles are the governor-scope files MigrateLegacy carries forward.
var migratedFiles = []string{LedgerFile, PrinciplesFile}

// Layout is a resolved Nous root. The zero value is invalid; build one with
// Resolve or New.
type Layout struct {
	Root string
}

// New returns a Layout rooted at root (no env or default resolution).
func New(root string) Layout {
	return Layout{Root: filepath.Clean(root)}
}

// Resolve picks the Nous root with precedence env > configured > DefaultRoot.
// getenv may be nil, in which case the environment is not consulted; pass
// os.Getenv in production and a stub in tests.
func Resolve(configured string, getenv func(string) string) Layout {
	if getenv != nil {
		if v := strings.TrimSpace(getenv(EnvRoot)); v != "" {
			return New(v)
		}
	}
	if v := strings.TrimSpace(configured); v != "" {
		return New(v)
	}
	return New(DefaultRoot)
}

// GovernorDir is <root>/governor.
func (l Layout) GovernorDir() string { return filepath.Join(l.Root, GovernorSubdir) }

// SnapshotDir is <root>/snapshots.
func (l Layout) SnapshotDir() string { return filepath.Join(l.Root, SnapshotsSubdir) }

// LedgerPath is <root>/governor/ledger.json.
func (l Layout) LedgerPath() string { return filepath.Join(l.GovernorDir(), LedgerFile) }

// PrinciplesPath is <root>/governor/principles.json.
func (l Layout) PrinciplesPath() string { return filepath.Join(l.GovernorDir(), PrinciplesFile) }

// PanelConfigPath is <root>/panel-config.json.
func (l Layout) PanelConfigPath() string { return filepath.Join(l.Root, PanelConfigFile) }

// EnsureDirs creates the governor and snapshot directories (and the root).
func (l Layout) EnsureDirs() error {
	for _, dir := range []string{l.GovernorDir(), l.SnapshotDir(), l.GatesDir()} {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return fmt.Errorf("create nous dir %s: %w", dir, err)
		}
	}
	return nil
}

// MigrateResult reports what MigrateLegacy did, for logging.
type MigrateResult struct {
	// Copied lists destination paths written from the legacy location.
	Copied []string
	// Skipped lists destination paths left alone because the durable copy is
	// at least as new as the legacy file, or no legacy file exists.
	Skipped []string
}

// MigrateLegacy copies each governor-scope file from legacyDir into
// l.GovernorDir() when the durable copy is absent or strictly older than the
// legacy one. It never deletes or overwrites a newer durable copy, and a
// missing legacyDir is not an error — a fresh hive simply has nothing to
// migrate. An empty legacyDir, or one equal to the durable dir, disables
// migration.
func (l Layout) MigrateLegacy(legacyDir string, logger *slog.Logger) (MigrateResult, error) {
	var res MigrateResult
	if strings.TrimSpace(legacyDir) == "" || filepath.Clean(legacyDir) == l.GovernorDir() {
		return res, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	var errs []error
	for _, name := range migratedFiles {
		src := filepath.Join(legacyDir, name)
		dst := filepath.Join(l.GovernorDir(), name)
		srcInfo, err := os.Stat(src)
		if err != nil || !srcInfo.Mode().IsRegular() {
			res.Skipped = append(res.Skipped, dst)
			continue
		}
		if dstInfo, err := os.Stat(dst); err == nil && !dstInfo.ModTime().Before(srcInfo.ModTime()) {
			res.Skipped = append(res.Skipped, dst)
			continue
		}
		if err := copyFile(src, dst); err != nil {
			errs = append(errs, err)
			continue
		}
		res.Copied = append(res.Copied, dst)
		logger.Info("nous: migrated legacy state to durable root", "from", src, "to", dst)
	}
	return res, errors.Join(errs...)
}

// ResolveGovernorDir returns the directory the ledger and principles should be
// read from: the durable governor dir when it holds a ledger or principles
// file, otherwise legacyDir as a read-only fallback (when it has either file).
// The boolean reports whether the fallback was chosen.
func (l Layout) ResolveGovernorDir(legacyDir string) (string, bool) {
	durable := l.GovernorDir()
	if hasAnyFile(durable, migratedFiles) {
		return durable, false
	}
	if strings.TrimSpace(legacyDir) != "" && hasAnyFile(legacyDir, migratedFiles) {
		return legacyDir, true
	}
	return durable, false
}

func hasAnyFile(dir string, names []string) bool {
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), dirMode); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	return writeAtomic(dst, data)
}

// PanelConfig is the operator-set dashboard state that must survive a
// restart: the trust-ladder mode, the scope, and the per-section config map
// written through PUT /api/nous/config/*.
type PanelConfig struct {
	Mode    string                 `json:"mode,omitempty"`
	Scope   string                 `json:"scope,omitempty"`
	Config  map[string]interface{} `json:"config,omitempty"`
	SavedAt time.Time              `json:"saved_at"`
}

// SavePanelConfig writes pc to l.PanelConfigPath() atomically, stamping
// SavedAt with now.
func (l Layout) SavePanelConfig(pc PanelConfig, now time.Time) error {
	pc.SavedAt = now.UTC()
	data, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal nous panel config: %w", err)
	}
	if err := os.MkdirAll(l.Root, dirMode); err != nil {
		return fmt.Errorf("create nous root %s: %w", l.Root, err)
	}
	return writeAtomic(l.PanelConfigPath(), data)
}

// LoadPanelConfig reads the persisted panel configuration. The boolean is
// false when no file exists; a malformed file is an error so a corrupt
// config is reported rather than silently replaced with defaults.
func (l Layout) LoadPanelConfig() (PanelConfig, bool, error) {
	var pc PanelConfig
	data, err := os.ReadFile(l.PanelConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return pc, false, nil
	}
	if err != nil {
		return pc, false, fmt.Errorf("read nous panel config: %w", err)
	}
	if err := json.Unmarshal(data, &pc); err != nil {
		return pc, false, fmt.Errorf("parse nous panel config %s: %w", l.PanelConfigPath(), err)
	}
	return pc, true, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + tmpSuffix
	if err := os.WriteFile(tmp, data, fileMode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
