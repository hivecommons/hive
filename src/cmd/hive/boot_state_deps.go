package main

import (
	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/snapshot"
)

// bootStateDeps are bootState's disk effects (#7571, step 2): reading the
// persisted snapshot from the /data PVC, and — only on the one-time
// config_overrides migration — writing hive.yaml and re-saving the stripped
// snapshot. Every restore step in between mutates the in-memory governor,
// agent manager, and config and runs for real.
type bootStateDeps struct {
	loadState  func(logger *slog.Logger) (*snapshot.PersistedState, error)
	saveState  func(state *snapshot.PersistedState, logger *slog.Logger) error
	saveConfig func(cfg *config.Config) error
}

func defaultBootStateDeps() bootStateDeps {
	return bootStateDeps{
		loadState: func(logger *slog.Logger) (*snapshot.PersistedState, error) {
			return snapshot.LoadState(hiveStatePath, logger)
		},
		saveState: func(state *snapshot.PersistedState, logger *slog.Logger) error {
			return snapshot.SaveState(hiveStatePath, state, logger)
		},
		saveConfig: func(cfg *config.Config) error { return cfg.Save() },
	}
}
