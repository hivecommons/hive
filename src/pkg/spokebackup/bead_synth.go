package spokebackup

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/hubbackup"
)

// The reserved automation vault contains flat markdown facts, not arbitrary
// imported vaults or attachments. Preserve it even when repo publishing is off.
//
// Only the default location (config.DefaultBeadSynthVaultPath) is captured; a
// custom knowledge.bead_synthesizer.vault_path is not read here and is
// documented as excluded in docs/backup-restore.md.
const beadSynthVault = "bead-synth-wiki"

// beadSynthFactMode matches the 0644 the synthesizer itself writes
// (knowledge.BeadSynthesizer). Facts are knowledge, not credentials, and the
// entrypoint re-applies no ownership under vaults/ at boot — restoring them
// owner-only would leave them unreadable to the synthesizer and reindex when
// the restore runs as a different uid than the dashboard.
const beadSynthFactMode = 0o644

func addBeadSynthVault(b *hubbackup.Builder, dataDir string, res *Result, logger *slog.Logger) error {
	relDir := filepath.Join("vaults", beadSynthVault)
	// Do not follow a symlink at either directory boundary into another vault
	// or an agent credential directory.
	for _, rel := range []string{"vaults", relDir} {
		info, err := os.Lstat(filepath.Join(dataDir, rel))
		if os.IsNotExist(err) {
			return nil // Older hives need not have synthesized any facts yet.
		}
		if err != nil || !info.IsDir() {
			res.Skipped[relDir] = "vault directory unavailable or not a real directory"
			logger.Warn("spoke backup: skipping bead synth vault", "dir", relDir)
			return nil
		}
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, relDir))
	if err != nil {
		res.Skipped[relDir] = err.Error()
		return nil
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		rel := filepath.Join(relDir, entry.Name())
		content, err := readCapped(filepath.Join(dataDir, rel))
		if err != nil {
			res.Skipped[rel] = err.Error()
			continue
		}
		if err := b.AddBytes(filepath.Join(beadSynthVault, entry.Name()), beadSynthFactMode, content); err != nil {
			return fmt.Errorf("archive bead synth fact: %w", err)
		}
	}
	return nil
}
