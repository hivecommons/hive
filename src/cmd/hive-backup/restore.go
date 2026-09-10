package main

import (
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/hubbackup"
	"github.com/hivecommons/hive/pkg/spokebackup"
)

// restoreOp is one planned file placement: content decrypted from the
// archive at src, to be written to dst with mode preserved from the archive.
// archivePath is kept for -dry-run and summary output, since src/dst are
// absolute filesystem paths that are less useful to print than the archive's
// own layout.
type restoreOp struct {
	archivePath string
	src         string
	dst         string
	mode        fs.FileMode
}

// cmdRestore decrypts a pkg/spokebackup archive (via the same hubbackup.Extract
// path cmdExtract uses) and places its files onto a spoke data directory:
// spoke/* -> dest/, beads/<agent>/* -> dest/beads/<agent>/. It refuses to
// overwrite a destination that already belongs to a different hive unless
// -force is given, and -dry-run prints the plan without writing anything.
func cmdRestore(args []string, logger *slog.Logger) {
	flags := flag.NewFlagSet("restore", flag.ExitOnError)
	file := flags.String("file", "", "archive to restore (required)")
	dest := flags.String("dest", "", "destination data directory (required)")
	force := flags.Bool("force", false, "restore even if -dest already belongs to a different hive-id")
	dryRun := flags.Bool("dry-run", false, "print the planned file operations and exit without writing")
	_ = flags.Parse(args)

	if *file == "" || *dest == "" {
		fmt.Fprintln(os.Stderr, "restore requires -file and -dest")
		os.Exit(exitCodeError)
	}

	key, data, err := loadKeyAndArchive(*file)
	if err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}

	if err := os.MkdirAll(*dest, 0o755); err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}

	// Extract into a scratch directory next to dest (not the system temp
	// dir) so the decrypt step stays on the same filesystem as the eventual
	// placement and is trivially cleaned up alongside it.
	scratch, err := os.MkdirTemp(*dest, ".hive-backup-restore-*")
	if err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	man, err := hubbackup.Extract(key, data, scratch)
	if err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}

	archiveHiveID, err := readArchiveHiveID(scratch, man)
	if err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}

	if err := checkExistingHiveID(*dest, archiveHiveID, *force); err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}

	ops, err := planRestoreOps(scratch, *dest)
	if err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}

	if *dryRun {
		fmt.Printf("dry run: %d file(s) would be restored to %s (hive-id: %s)\n",
			len(ops), *dest, archiveHiveID)
		for _, op := range ops {
			fmt.Printf("  %s -> %s\n", op.archivePath, op.dst)
		}
		return
	}

	for _, op := range ops {
		if err := copyPreservingMode(op.src, op.dst, op.mode); err != nil {
			logger.Error("restore failed", "err", err, "file", op.archivePath)
			os.Exit(exitCodeError)
		}
	}

	fmt.Printf("restored %d file(s) to %s (hive-id: %s)\n", len(ops), *dest, archiveHiveID)
}

// readArchiveHiveID returns the identity the archive belongs to, preferring
// the spoke/hive-id file it carries (the file that also lands on disk and is
// what an entrypoint reboot actually reads) and falling back to the
// manifest's SpokeIDs, which spokebackup.Build always stamps with the same
// value even on the rare backup that could not read hive-id off disk.
func readArchiveHiveID(scratch string, man *hubbackup.Manifest) (string, error) {
	idPath := filepath.Join(scratch, spokebackup.SpokePrefix, spokebackup.HiveIDFile)
	if content, err := os.ReadFile(idPath); err == nil {
		return strings.TrimSpace(string(content)), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if man != nil && len(man.SpokeIDs) > 0 {
		return man.SpokeIDs[0], nil
	}
	return "", nil
}

// checkExistingHiveID refuses the restore when dest already holds a
// different hive's identity, unless force is set. A dest with no hive-id yet
// (a genuinely fresh target) or one that already matches the archive is
// always allowed.
func checkExistingHiveID(dest, archiveHiveID string, force bool) error {
	existing, err := os.ReadFile(filepath.Join(dest, spokebackup.HiveIDFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	existingID := strings.TrimSpace(string(existing))
	if existingID == archiveHiveID {
		return nil
	}
	if force {
		return nil
	}
	return fmt.Errorf("dest %q already belongs to hive-id %q, archive is for hive-id %q; pass -force to overwrite",
		dest, existingID, archiveHiveID)
}

// planRestoreOps walks the extracted scratch directory and maps its
// spoke/* and beads/<agent>/* trees onto dest, per the archive layout
// documented in pkg/spokebackup: spoke/* -> dest/, beads/<agent>/* ->
// dest/beads/<agent>/. Symlinks are refused rather than followed, matching
// hubbackup.Extract, which never creates one in the first place.
func planRestoreOps(scratch, dest string) ([]restoreOp, error) {
	var ops []restoreOp

	walk := func(prefix, dstBase string) error {
		root := filepath.Join(scratch, prefix)
		info, err := os.Stat(root)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("archive member %q is not a directory", prefix)
		}
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("refusing to restore symlink %q", path)
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			ops = append(ops, restoreOp{
				archivePath: filepath.Join(prefix, rel),
				src:         path,
				dst:         filepath.Join(dstBase, rel),
				mode:        fi.Mode().Perm(),
			})
			return nil
		})
	}

	if err := walk(spokebackup.SpokePrefix, dest); err != nil {
		return nil, err
	}
	if err := walk(spokebackup.BeadsPrefix, filepath.Join(dest, spokebackup.BeadsPrefix)); err != nil {
		return nil, err
	}
	return ops, nil
}

// copyPreservingMode copies src to dst, creating parent directories as
// needed, and applies mode to the written file so the restored tree keeps
// the permissions hubbackup.Extract already assigned it.
func copyPreservingMode(src, dst string, mode fs.FileMode) error {
	content, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, content, mode)
}
