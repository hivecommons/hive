// Command hive-backup creates, verifies and restores encrypted disaster-recovery
// backups of the Hive hub and its spoke fleet.
//
// It is a standalone binary so the backup path can run as a CronJob without
// starting a hub server, and so a restore can be performed from any machine
// with kubectl access and the escrowed HIVE_BACKUP_KEY.
//
// Usage:
//
//	hive-backup run                 # back up to OCI Object Storage
//	hive-backup run -local out.enc  # back up to a local file
//	hive-backup verify              # verify the newest stored archive
//	hive-backup verify -file f.enc  # verify a local archive
//	hive-backup extract -file f.enc -dest ./restore
//	hive-backup restore -file f.enc -dest /data   # restore a spoke archive
//	hive-backup list                # list stored archives
//
// extract and restore differ in what they do with the decrypted tree. extract
// stops at a plain directory, preserving the archive's own path prefixes — the
// right verb for inspecting an archive or for a hub disaster-recovery archive,
// whose Secret/PVC reassembly is manual. restore goes on to place a per-spoke
// archive into a live data directory, mapping spoke/ onto the data-dir root and
// beads/<agent>/ one level down, and refusing to overwrite a destination that
// already belongs to a different hive.
//
// HIVE_BACKUP_KEY must be set to a 64-character hex AES-256 key. It has no
// default: an unset key aborts rather than writing plaintext.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/hivecommons/hive/pkg/hubbackup"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/spokebackup"
)

// exitCodeError is returned for any operational failure so a CronJob shows
// as failed and alerting fires.
const exitCodeError = 1

func main() {
	logger := slog.New(logscrub.NewHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if len(os.Args) < 2 {
		usage()
		os.Exit(exitCodeError)
	}

	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:], logger)
	case "verify":
		cmdVerify(os.Args[2:], logger)
	case "extract":
		cmdExtract(os.Args[2:], logger)
	case "restore":
		cmdRestore(os.Args[2:], logger)
	case "list":
		cmdList(logger)
	default:
		usage()
		os.Exit(exitCodeError)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `hive-backup — Hive hub disaster-recovery backup

Commands:
  run [-local FILE] [-skip-spokes]   create an encrypted backup
  verify [-file FILE]                verify newest stored, or a local archive
  extract -file FILE -dest DIR       decrypt an archive to a directory
  restore -file FILE -dest DIR       restore a SPOKE archive into a data dir
                                     [-force] [-dry-run]
  list                               list stored archives

Environment:
  %s   (required) 64-char hex AES-256 key; escrow this OUTSIDE the cluster
  %s        OCI Object Storage bucket name
  %s      hub data directory (default %s)
  %s     archives to retain (default %d)
`, hubbackup.EnvBackupKey, hubbackup.EnvBucket, hubbackup.EnvDataDir,
		hubbackup.DefaultDataDir, hubbackup.EnvRetention, hubbackup.DefaultRetention)
}

func cmdRun(args []string, logger *slog.Logger) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	local := fs.String("local", "", "write archive to this local path instead of object storage")
	skipSpokes := fs.String("skip-spokes", "", "set to 'true' for a hub-only backup")
	_ = fs.Parse(args)

	opts := hubbackup.Options{
		LocalOnly:  *local != "",
		LocalPath:  *local,
		SkipSpokes: *skipSpokes == "true",
	}
	man, dest, err := hubbackup.Run(opts, logger)
	if err != nil {
		logger.Error("backup failed", "err", err)
		os.Exit(exitCodeError)
	}
	fmt.Printf("backup complete: %s\n", dest)
	fmt.Printf("  files:        %d\n", len(man.Files))
	fmt.Printf("  spokes:       %d\n", len(man.SpokeIDs))
	fmt.Printf("  secrets:      %d\n", len(man.SecretNames))
	if len(man.SpokeErrors) > 0 {
		fmt.Printf("  SPOKE GAPS:   %d (see manifest)\n", len(man.SpokeErrors))
		for id, e := range man.SpokeErrors {
			fmt.Printf("    - %s: %s\n", id, e)
		}
	}
}

func cmdVerify(args []string, logger *slog.Logger) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	file := fs.String("file", "", "verify this local archive instead of the newest stored one")
	_ = fs.Parse(args)

	var man *hubbackup.Manifest
	var err error
	if *file != "" {
		key, kerr := hubbackup.LoadKey()
		if kerr != nil {
			logger.Error("verify failed", "err", kerr)
			os.Exit(exitCodeError)
		}
		data, rerr := os.ReadFile(*file)
		if rerr != nil {
			logger.Error("verify failed", "err", rerr)
			os.Exit(exitCodeError)
		}
		man, err = hubbackup.Verify(key, data)
	} else {
		man, err = hubbackup.VerifyLatest(logger)
	}
	if err != nil {
		logger.Error("verify failed", "err", err)
		os.Exit(exitCodeError)
	}
	fmt.Printf("archive OK: %d files, %d spokes, created %s\n",
		len(man.Files), len(man.SpokeIDs), man.CreatedAt.Format("2006-01-02T15:04:05Z"))
	if len(man.SpokeErrors) > 0 {
		fmt.Printf("WARNING: %d spokes were NOT captured:\n", len(man.SpokeErrors))
		for id, e := range man.SpokeErrors {
			fmt.Printf("  - %s: %s\n", id, e)
		}
	}
}

func cmdExtract(args []string, logger *slog.Logger) {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	file := fs.String("file", "", "archive to extract (required)")
	dest := fs.String("dest", "", "destination directory (required)")
	_ = fs.Parse(args)

	if *file == "" || *dest == "" {
		fmt.Fprintln(os.Stderr, "extract requires -file and -dest")
		os.Exit(exitCodeError)
	}
	key, err := hubbackup.LoadKey()
	if err != nil {
		logger.Error("extract failed", "err", err)
		os.Exit(exitCodeError)
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		logger.Error("extract failed", "err", err)
		os.Exit(exitCodeError)
	}
	man, err := hubbackup.Extract(key, data, *dest)
	if err != nil {
		logger.Error("extract failed", "err", err)
		os.Exit(exitCodeError)
	}
	fmt.Printf("extracted %d files to %s\n", len(man.Files), *dest)
}

// cmdRestore places a per-spoke archive into a data directory (#6529).
//
// This is the step `extract` deliberately stops short of. extract leaves a tree
// still carrying the archive's own prefixes, which an operator then had to
// hand-copy — a `cp` per path, remembering that beads/ nests one level deeper,
// with nothing checking that the destination was not already a different hive.
//
// The key comes from hubbackup.LoadKey (HIVE_BACKUP_KEY), exactly as for
// extract. For a cross-deployment restore that is the SOURCE hive's escrowed
// key: the archive never carries its own key, and the destination's own key
// setting has no bearing on opening a foreign archive.
func cmdRestore(args []string, logger *slog.Logger) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	file := fs.String("file", "", "spoke archive to restore (required)")
	dest := fs.String("dest", "", "target spoke data directory, e.g. /data (required)")
	force := fs.Bool("force", false, "restore even though the destination belongs to a different hive")
	dryRun := fs.Bool("dry-run", false, "report what would be restored without writing anything")
	_ = fs.Parse(args)

	if *file == "" || *dest == "" {
		// -dest has no default on purpose: this command writes over a hive's
		// identity, config and GitHub App keys, and a destructive default is a
		// bad default.
		fmt.Fprintln(os.Stderr, "restore requires -file and -dest")
		os.Exit(exitCodeError)
	}
	key, err := hubbackup.LoadKey()
	if err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}
	sealed, err := os.ReadFile(*file)
	if err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}

	res, err := spokebackup.Restore(key, sealed, spokebackup.RestoreOptions{
		DataDir: *dest,
		Force:   *force,
		DryRun:  *dryRun,
	}, logger)
	if err != nil {
		logger.Error("restore failed", "err", err)
		os.Exit(exitCodeError)
	}

	verb := "restored"
	if res.DryRun {
		verb = "would restore"
	}
	fmt.Printf("%s %d files and %d bead directories to %s\n",
		verb, len(res.Files), len(res.BeadDirs), *dest)
	if res.ArchiveHiveID != "" {
		fmt.Printf("  archive hive-id: %s\n", res.ArchiveHiveID)
	}
	if res.ExistingHiveID != "" {
		fmt.Printf("  destination hive-id (before): %s\n", res.ExistingHiveID)
	}
	if res.Forced {
		fmt.Println("  WARNING: identity check overridden by -force")
	}
	for _, f := range res.Files {
		fmt.Printf("  %s\n", f)
	}
	if len(res.Ignored) > 0 {
		// MANIFEST.json is expected here. Anything else means the archive held
		// members this restore path does not place, which an operator should
		// see rather than have silently dropped.
		fmt.Printf("  not restored (archive metadata or outside the spoke layout): %d\n", len(res.Ignored))
		for _, name := range res.Ignored {
			fmt.Printf("    - %s\n", name)
		}
	}
	if !res.DryRun {
		// The entrypoint owns ownership and the config hardening, so the next
		// boot is part of the procedure, not an optional extra.
		fmt.Println("  (re)start the hive container to apply ownership and load the restored config")
	}
}

func cmdList(logger *slog.Logger) {
	store, err := hubbackup.NewObjectStore(os.Getenv(hubbackup.EnvBucket))
	if err != nil {
		logger.Error("list failed", "err", err)
		os.Exit(exitCodeError)
	}
	names, err := store.List()
	if err != nil {
		logger.Error("list failed", "err", err)
		os.Exit(exitCodeError)
	}
	for _, n := range names {
		fmt.Println(n)
	}
}
