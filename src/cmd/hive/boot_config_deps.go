package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/proclock"
	"github.com/hivecommons/hive/pkg/tracing"
)

// bootConfigDeps carries the process-level effects of bootConfig (#7571,
// step 2): everything in that phase that reads argv/env, exits the process,
// takes the singleton flock, touches /data, starts the hub, or installs a
// signal handler. Production wires the real thing through
// defaultBootConfigDeps; tests substitute fakes so the phase's decisions —
// fast paths, marker verdicts, fatal exits, cleanup registration — can be
// asserted without a /data volume or a second process.
//
// Package-global setters (dashboard.Set*, slog.SetDefault, gitShort) are
// deliberately NOT injected: they are pure assignments, and faking them
// would only hide the fact that the phase mutates them.
type bootConfigDeps struct {
	// args is os.Args: args[0] is the binary, args[1] the fast-path verb.
	args []string
	// stdout receives the --version line and the config-check report.
	stdout, stderr io.Writer
	getenv         func(key string) string
	// exit terminates the process. In production it never returns; a fake
	// must stop the phase some other way (panic with a sentinel), because the
	// code after each exit call assumes it did not come back.
	exit func(code int)
	// parseFlags registers and parses the process flag set, returning the
	// -config value. Isolated because flag.CommandLine can only register
	// "config" once per process.
	parseFlags func(defaultConfigPath string) string
	// releaseChannel and selfImage read the Deployment image (in-cluster
	// only; "" elsewhere) for the version badge and trace attributes.
	releaseChannel func() string
	selfImage      func() string
	// reconcileUpgradeOutcome records a landed upgrade before anything reads
	// the marker.
	reconcileUpgradeOutcome func(runningSHA string, logger *slog.Logger)
	// acquireLock takes the process singleton flock and returns its release.
	acquireLock func(path string) (release func(), err error)
	// readUpgradeMarker / clearUpgradeMarker back the stale-marker check.
	readUpgradeMarker  func() ([]byte, error)
	clearUpgradeMarker func() error
	// runHub is the HIVE_MODE=hub branch; it blocks for the hub's lifetime.
	runHub func(logger *slog.Logger, configPath string)
	// loadConfig is config.LoadWithDashboardOverlay.
	loadConfig func(path string) (*config.Config, error)
	// fileLogger builds the rolling-file logger from the loaded config.
	fileLogger func(cfg *config.Config) *slog.Logger
	// hiveID loads or generates the persistent hive identity.
	hiveID func(logger *slog.Logger) string
	// stat backs the persisted-runtime-config provenance check.
	stat func(path string) (os.FileInfo, error)
	// initTracing is tracing.Init; loadReachState is tracing.LoadReachState
	// against the fixed reach-state path.
	initTracing    func(ctx context.Context, cfg tracing.Config) (shutdown func(context.Context) error, err error)
	loadReachState func(commit string, logger *slog.Logger) error
	// notifySignals subscribes ch to the termination signals.
	notifySignals func(ch chan<- os.Signal)
}

func defaultBootConfigDeps() bootConfigDeps {
	return bootConfigDeps{
		args:   os.Args,
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
		exit:   os.Exit,
		parseFlags: func(defaultConfigPath string) string {
			configPath := flag.String("config", defaultConfigPath, "path to hive.yaml config file")
			flag.Parse()
			return *configPath
		},
		releaseChannel: hub.SelfImageReleaseChannel,
		selfImage:      hub.SelfDeploymentImage,
		reconcileUpgradeOutcome: func(runningSHA string, logger *slog.Logger) {
			reconcileUpgradeOutcomeAtBoot(upgradeMarkerPath, lastUpgradeOutcomePath, runningSHA, logger)
		},
		acquireLock: func(path string) (func(), error) {
			lock, err := proclock.Acquire(path)
			if err != nil {
				return nil, err
			}
			return lock.Release, nil
		},
		readUpgradeMarker:  func() ([]byte, error) { return os.ReadFile(upgradeMarkerPath) },
		clearUpgradeMarker: func() error { return os.Remove(upgradeMarkerPath) },
		runHub:             runHub,
		loadConfig:         config.LoadWithDashboardOverlay,
		fileLogger: func(cfg *config.Config) *slog.Logger {
			l := cfg.Governor.Logging
			return setupLogger(l.Dir, l.MaxSizeMB, l.MaxAgeDays, l.MaxBackups, l.Compress, l.Level)
		},
		hiveID: loadOrGenerateHiveID,
		stat:   os.Stat,
		initTracing: func(ctx context.Context, cfg tracing.Config) (func(context.Context) error, error) {
			return tracing.Init(ctx, cfg)
		},
		loadReachState: func(commit string, logger *slog.Logger) error {
			return tracing.LoadReachState(reachStatePath, commit, logger)
		},
		notifySignals: func(ch chan<- os.Signal) { signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM) },
	}
}
