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
//	hive-backup list                # list stored archives
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
