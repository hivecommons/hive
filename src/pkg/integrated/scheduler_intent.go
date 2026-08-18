package integrated

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	SchedulerStartIntentSchema = "hive.scheduler-start-intent.v1"
	schedulerStartIntentFile   = "scheduler-start-intent.json"
)

type SchedulerStartIntent struct {
	SchemaVersion   string    `json:"schema_version"`
	Repository      string    `json:"repository"`
	StateDir        string    `json:"state_dir"`
	IntervalSeconds int64     `json:"interval_seconds"`
	RequestedAt     time.Time `json:"requested_at"`
}

func (s *Store) SaveSchedulerStartIntent(intent SchedulerStartIntent) error {
	config, err := s.Load()
	if err != nil {
		return fmt.Errorf("load setup before recording scheduler start intent: %w", err)
	}
	intent.SchemaVersion = SchedulerStartIntentSchema
	intent.Repository = strings.TrimSpace(intent.Repository)
	stateDir, err := filepath.Abs(intent.StateDir)
	if err != nil {
		return fmt.Errorf("resolve scheduler start state directory: %w", err)
	}
	intent.StateDir = filepath.Clean(stateDir)
	if intent.RequestedAt.IsZero() {
		intent.RequestedAt = time.Now().UTC()
	}
	if err := validateSchedulerStartIntent(intent, config); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(s.dir, schedulerStartIntentFile), append(data, '\n'))
}

func (s *Store) LoadSchedulerStartIntent() (SchedulerStartIntent, bool, error) {
	path := filepath.Join(s.dir, schedulerStartIntentFile)
	data, err := readOrdinaryStateFile(path)
	if os.IsNotExist(err) {
		return SchedulerStartIntent{}, false, nil
	}
	if err != nil {
		return SchedulerStartIntent{}, false, err
	}
	var intent SchedulerStartIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return SchedulerStartIntent{}, false, fmt.Errorf("decode scheduler start intent: %w", err)
	}
	config, err := s.Load()
	if err != nil {
		return SchedulerStartIntent{}, false, err
	}
	if err := validateSchedulerStartIntent(intent, config); err != nil {
		return SchedulerStartIntent{}, false, err
	}
	return intent, true, nil
}

func (s *Store) DeleteSchedulerStartIntent() error {
	return durableRemoveStateFile(filepath.Join(s.dir, schedulerStartIntentFile))
}

func validateSchedulerStartIntent(intent SchedulerStartIntent, config Config) error {
	stateDir, err := filepath.Abs(intent.StateDir)
	if err != nil {
		return fmt.Errorf("scheduler start intent state directory is invalid: %w", err)
	}
	configuredStateDir, err := filepath.Abs(config.StateDir)
	if err != nil {
		return fmt.Errorf("configured state directory is invalid: %w", err)
	}
	sameStateDir := filepath.Clean(stateDir) == filepath.Clean(configuredStateDir)
	if runtime.GOOS == "windows" {
		sameStateDir = strings.EqualFold(filepath.Clean(stateDir), filepath.Clean(configuredStateDir))
	}
	if intent.SchemaVersion != SchedulerStartIntentSchema || intent.Repository == "" || !strings.EqualFold(intent.Repository, config.Repository) ||
		!sameStateDir || intent.IntervalSeconds < 60 || intent.IntervalSeconds > int64((24*time.Hour).Seconds()) || intent.RequestedAt.IsZero() {
		return fmt.Errorf("scheduler start intent is incomplete or does not match the installed repository")
	}
	return nil
}
