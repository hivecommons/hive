package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/proclock"
)

func main() {
	// Startup order is intentionally linear and dependency-ordered:
	//  1. version/config/logging/process singleton
	//  2. config overlays, identity, GitHub auth/client, and dashboard server
	//  3. agent manager, persisted runtime state, governor, scheduler, and queues
	//  4. hub heartbeat/self-upgrade wiring, notification sinks, and background lanes
	//  5. steady-state tick loop: watchdog, eval cycle, rotation, automerge, and persistence
	//
	// Keep new subsystem constructors on the existing *wire.go pattern and insert
	// them at the matching point above; do not move side effects across steps.
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

	wireSpokeSubsystems(*configPath, logger, startTime)
}
