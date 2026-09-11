package spokebackup

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/hubbackup"
)

// Restoring a spoke archive back into a data directory (#6529).
//
// Decrypting an archive was already possible — `hive-backup extract` calls
// hubbackup.Extract, which is format-agnostic and accepts a spoke archive
// unchanged. What was missing was the step AFTER extraction: nothing mapped the
// archive's two prefixes onto the container's /data layout, and nothing checked
// that the destination was not already a DIFFERENT hive. Doing it by hand meant
// a `cp` per path plus remembering that beads/ nests one level deeper, with no
// guard at all against splicing one hive's identity, config and GitHub App keys
// onto another's.
//
// The mapping is a flat rename of the two archive prefixes:
//
//	spoke/<name>         -> <dataDir>/<name>
//	beads/<agent>/<rel>  -> <dataDir>/beads/<agent>/<rel>
//	MANIFEST.json        -> not restored; it is archive metadata, not spoke state
//
// Everything else in the archive is ignored and reported, so a hub
// disaster-recovery archive pointed at this path fails loudly rather than
// half-restoring.
//
// Ownership is deliberately NOT set here. The entrypoint re-applies it on every
// boot — 0600 on the runtime config (hive_harden_runtime_config) and a per-agent
// chown of beads/<agent> — so a restore that guessed at container-internal UIDs
// would be both redundant and wrong when run from a host shell.

// restoreSecretMode is the mode forced onto every restored credential and
// config file: owner-only, no group or other access.
//
// hubbackup.Extract already masks group/other WRITE off an archive-supplied
// mode, but not group/other READ — so an archive claiming 0644 on a
// gh-app-key*.pem would restore a world-readable GitHub App private key. These
// files have exactly one correct mode and the archive does not get a say in it.
// The entrypoint hardens the config files again at boot; this closes the window
// before that first boot, and covers restores that never boot a container at
// all (a host-side migration staging directory, for example).
const restoreSecretMode fs.FileMode = 0o600

// restoreDirMode is the mode for directories this restore creates. Matches the
// 0755 hubbackup.Extract uses for extracted parents; beads dirs are re-chowned
// per agent by the entrypoint at boot.
const restoreDirMode fs.FileMode = 0o755

// restoreStagingPrefix names the temporary directory a restore extracts into
// before moving files to their final paths. It is created INSIDE the target
// data dir so every move is a same-filesystem rename (atomic, no partial file
// ever visible at a real path) and so the decrypted GitHub App keys never touch
// a world-traversable /tmp on the way through.
const restoreStagingPrefix = ".hive-restore-"

// RestoreOptions configures Restore.
type RestoreOptions struct {
	// DataDir is the target spoke data directory — the container's /data.
	// Required; Restore never falls back to the ambient DataDir(), because a
	// destructive default is a bad default.
	DataDir string

	// Force permits a restore whose identity check failed: the destination
	// already holds a hive-id that differs from the archive's, or the archive
	// carries no hive-id to compare against a destination that does. Without
	// it, both cases refuse.
	Force bool

	// DryRun reports what WOULD be written without writing anything. The
	// archive is still decrypted and verified, and the identity check still
	// runs, so a dry run is a real answer about a real archive.
	DryRun bool
}

// RestoreResult reports what a restore did, or (on a dry run) would do. It
// carries paths and counts only — never file content, and never the key.
type RestoreResult struct {
	// ArchiveHiveID is the hive-id carried by the archive, empty when it has
	// none (a backup taken before the hive registered).
	ArchiveHiveID string

	// ExistingHiveID is the hive-id already present at the destination, empty
	// when the destination is fresh.
	ExistingHiveID string

	// Forced is true when the identity check failed and Force overrode it, so
	// a caller can log the override rather than let it pass silently.
	Forced bool

	// Files lists the restored paths, relative to DataDir, sorted.
	Files []string

	// BeadDirs lists the per-agent bead directories restored, sorted.
	BeadDirs []string

	// Ignored lists archive members outside the spoke layout that were NOT
	// restored, sorted. MANIFEST.json is expected here; anything else means
	// the archive holds more than this restore path understands.
	Ignored []string

	// Manifest is the archive's verified manifest.
	Manifest *hubbackup.Manifest

	// DryRun echoes the option, so a caller rendering Files cannot mistake a
	// plan for an outcome.
	DryRun bool
}

// Restore decrypts a spoke backup archive and writes it into opts.DataDir.
//
// key must come from hubbackup.LoadKey / ResolveKey. It is the key that SEALED
// the archive, which for a cross-deployment restore is the source hive's
// escrowed key — the destination's own key setting is irrelevant to opening a
// foreign archive.
//
// Nothing reaches a real destination path until the archive has been verified
// (SHA-256 manifest) AND the identity check has passed: the archive is decrypted
// into a staging directory first, and that staging directory is removed on every
// path out. A refused restore therefore leaves the destination byte-for-byte as
// it was, having written no config and no GitHub App key.
func Restore(key, sealed []byte, opts RestoreOptions, logger *slog.Logger) (*RestoreResult, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("no backup key supplied: a spoke archive cannot be opened without the key that sealed it")
	}
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		return nil, fmt.Errorf("no destination data directory supplied")
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Create the destination up front. A fresh restore onto a brand-new volume
	// is the primary case, so a missing directory is normal, not an error.
	if err := os.MkdirAll(dataDir, restoreDirMode); err != nil {
		return nil, fmt.Errorf("prepare destination %s: %w", dataDir, err)
	}

	staging, err := os.MkdirTemp(dataDir, restoreStagingPrefix)
	if err != nil {
		return nil, fmt.Errorf("create staging directory under %s: %w", dataDir, err)
	}
	// The staging tree holds decrypted GitHub App private keys. Remove it on
	// every path out, including the refusal paths below.
	defer func() {
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			logger.Warn("spoke restore: could not remove staging directory",
				"dir", staging, "err", rmErr)
		}
	}()

	// Verify + decrypt + untar. Extract enforces the traversal and size guards;
	// this function never parses the archive itself.
	man, err := hubbackup.Extract(key, sealed, staging)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}

	plan, err := planRestore(staging, dataDir)
	if err != nil {
		return nil, err
	}
	if len(plan.moves) == 0 {
		return nil, fmt.Errorf(
			"archive contains no %q entries, so it does not look like a spoke backup — "+
				"a hub disaster-recovery archive is restored with `hive-backup extract` "+
				"plus manual Secret/PVC reassembly, not this command", spokePrefix)
	}

	res := &RestoreResult{
		ArchiveHiveID: plan.archiveHiveID,
		Ignored:       plan.ignored,
		Manifest:      man,
		DryRun:        opts.DryRun,
		Files:         plan.relPaths(),
		BeadDirs:      plan.beadDirs(),
	}

	existing, err := readHiveID(filepath.Join(dataDir, hiveIDFile))
	if err != nil {
		return nil, fmt.Errorf("read destination %s: %w", hiveIDFile, err)
	}
	res.ExistingHiveID = existing

	if mismatch := identityRefusal(plan.archiveHiveID, existing); mismatch != "" {
		if !opts.Force {
			return nil, fmt.Errorf("%s — pass -force to restore anyway", mismatch)
		}
		res.Forced = true
		logger.Warn("spoke restore: identity check overridden by -force",
			"reason", mismatch, "archive_hive_id", plan.archiveHiveID, "existing_hive_id", existing)
	}

	if opts.DryRun {
		return res, nil
	}

	for _, mv := range plan.moves {
		if err := os.MkdirAll(filepath.Dir(mv.dst), restoreDirMode); err != nil {
			return nil, fmt.Errorf("create %s: %w", filepath.Dir(mv.dst), err)
		}
		if mv.secret {
			if err := os.Chmod(mv.src, restoreSecretMode); err != nil {
				return nil, fmt.Errorf("harden %s: %w", mv.rel, err)
			}
		}
		// Same filesystem by construction (staging lives inside dataDir), so
		// this is an atomic replace: a reader never sees a half-written
		// hive-id or a truncated App key.
		if err := os.Rename(mv.src, mv.dst); err != nil {
			return nil, fmt.Errorf("restore %s: %w", mv.rel, err)
		}
	}

	logger.Info("spoke restore complete",
		"data_dir", dataDir,
		"files", len(res.Files),
		"bead_dirs", len(res.BeadDirs),
		"hive_id", res.ArchiveHiveID,
		"forced", res.Forced)
	return res, nil
}

// restoreMove is one staged file and where it belongs under the data dir.
type restoreMove struct {
	// rel is the destination path relative to the data dir, used in messages
	// and in the result so neither leaks the staging directory's random name.
	rel string
	src string
	dst string
	// secret marks a file whose mode is forced to restoreSecretMode rather
	// than trusted from the archive.
	secret bool
}

// restorePlan is the full set of moves derived from a staged archive, plus what
// the plan noticed about the archive along the way.
type restorePlan struct {
	moves         []restoreMove
	ignored       []string
	archiveHiveID string
	beads         map[string]struct{}
}

func (p *restorePlan) relPaths() []string {
	out := make([]string, 0, len(p.moves))
	for _, mv := range p.moves {
		out = append(out, mv.rel)
	}
	sort.Strings(out)
	return out
}

func (p *restorePlan) beadDirs() []string {
	out := make([]string, 0, len(p.beads))
	for agent := range p.beads {
		out = append(out, agent)
	}
	sort.Strings(out)
	return out
}

// planRestore walks the staged extraction and maps each member onto its data-dir
// destination. It reads the archive's hive-id while it is there, so the identity
// check runs off the same tree the moves come from rather than a second parse.
//
// Walking the extracted TREE rather than the manifest is deliberate: the tree is
// what Extract actually wrote, after its traversal and size guards, so a plan
// built from it cannot name a path Extract refused to create.
func planRestore(staging, dataDir string) (*restorePlan, error) {
	plan := &restorePlan{beads: map[string]struct{}{}}
	ignored := map[string]struct{}{}

	err := filepath.WalkDir(staging, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(staging, path)
		if relErr != nil || rel == "." {
			return relErr
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			// Extract only ever writes regular files; anything else in the
			// staging tree is not ours to move.
			ignored[filepath.ToSlash(rel)] = struct{}{}
			return nil
		}

		parts := strings.Split(filepath.ToSlash(rel), "/")
		switch {
		case parts[0] == spokePrefix && len(parts) == 2:
			name := parts[1]
			if name == hiveIDFile {
				id, readErr := readHiveID(path)
				if readErr != nil {
					return fmt.Errorf("read archive %s: %w", hiveIDFile, readErr)
				}
				plan.archiveHiveID = id
			}
			plan.moves = append(plan.moves, restoreMove{
				rel:    name,
				src:    path,
				dst:    filepath.Join(dataDir, name),
				secret: true, // config, identity and App keys — all owner-only
			})
		case parts[0] == beadsPrefix && len(parts) >= 3:
			agent := parts[1]
			plan.beads[agent] = struct{}{}
			relDst := filepath.Join(append([]string{beadsSubdir}, parts[1:]...)...)
			plan.moves = append(plan.moves, restoreMove{
				rel: relDst,
				src: path,
				dst: filepath.Join(dataDir, relDst),
			})
		default:
			// MANIFEST.json lands here, as do a hub archive's hub/, secrets/
			// and spokes/ trees. Reported, never restored.
			ignored[filepath.ToSlash(rel)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan extracted archive: %w", err)
	}

	plan.ignored = make([]string, 0, len(ignored))
	for name := range ignored {
		plan.ignored = append(plan.ignored, name)
	}
	sort.Strings(plan.ignored)
	// Deterministic order so a dry run and the restore that follows it agree,
	// and so a failure part-way through is reproducible.
	sort.Slice(plan.moves, func(i, j int) bool { return plan.moves[i].rel < plan.moves[j].rel })
	return plan, nil
}

// identityRefusal returns a human-readable reason to refuse, or "" when the
// restore is identity-safe.
//
// Safe cases: the destination is fresh (no hive-id), or it already IS the
// archive's hive — the ordinary "restore my own hive" recovery.
//
// Refused: the destination holds a DIFFERENT identity (restoring would splice
// one hive's config, GitHub App keys and beads onto another's identity), or the
// archive has no hive-id to prove it belongs to a destination that does. The
// second case is refused rather than allowed because "cannot tell" is not the
// same as "verified safe" when the failure mode is a credential mix-up.
func identityRefusal(archiveID, existingID string) string {
	switch {
	case existingID == "":
		return ""
	case archiveID == existingID:
		return ""
	case archiveID == "":
		return fmt.Sprintf(
			"destination already belongs to hive %q but the archive carries no %s, "+
				"so this restore cannot be shown to be for that hive", existingID, hiveIDFile)
	default:
		return fmt.Sprintf(
			"destination already belongs to hive %q but the archive is for hive %q; "+
				"restoring would splice one hive's config and GitHub App keys onto another's identity",
			existingID, archiveID)
	}
}

// readHiveID reads a hive-id file, trimming surrounding whitespace so a trailing
// newline never reads as a different identity. A missing file is not an error:
// it is what a fresh destination looks like.
func readHiveID(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(content)), nil
}
