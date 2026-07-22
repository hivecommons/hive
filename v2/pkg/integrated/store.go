package integrated

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Store struct {
	dir        string
	configPath string
	auditPath  string
}

type AuditEntry struct {
	Timestamp  time.Time `json:"timestamp"`
	Action     string    `json:"action"`
	Allowed    bool      `json:"allowed"`
	Repository string    `json:"repository,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("integrated Hive state directory is required")
	}
	if err := ensureOrdinaryDirectoryChain(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, configPath: filepath.Join(dir, "config.json"), auditPath: filepath.Join(dir, "audit.jsonl")}, nil
}

func (s *Store) Load() (Config, error) {
	data, err := readOrdinaryStateFile(s.configPath)
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, err
	}
	needsMigration := config.SchemaVersion != ConfigSchema
	if !supportedDurableConfigSchema(config.SchemaVersion) {
		return Config{}, fmt.Errorf("unsupported integrated config schema %q", config.SchemaVersion)
	}
	if config.MaxRepairAttempts == 0 {
		config.MaxRepairAttempts = 3
	}
	if config.ExecutionMode == "" {
		config.ExecutionMode = ExecutionLocal
	}
	if needsMigration {
		// Persist the current writer guard before returning older state to any
		// caller. Older Hive binaries reject this schema instead of silently
		// dropping managed-path or hosted lifecycle ownership fields on save.
		if err := s.save(&config); err != nil {
			return Config{}, fmt.Errorf("migrate integrated config schema: %w", err)
		}
	}
	return config, nil
}

func (s *Store) Save(config Config) error {
	return s.save(&config)
}

func (s *Store) save(config *Config) error {
	config.SchemaVersion = ConfigSchema
	config.UpdatedAt = time.Now().UTC()
	if config.InstalledAt.IsZero() {
		config.InstalledAt = config.UpdatedAt
	}
	if err := ensureStateOwnershipMarker(filepath.Dir(s.dir), *config); err != nil {
		return err
	}
	// Bypass Config.MarshalJSON only for the permission-bounded durable state;
	// normal command/status JSON intentionally redacts repository preimages.
	type persistedConfig Config
	data, err := json.MarshalIndent(persistedConfig(*config), "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(s.configPath, append(data, '\n'))
}

const stateOwnershipMarkerFile = ".hive-state-owner.json"

type stateOwnershipMarker struct {
	SchemaVersion string `json:"schema_version"`
	Repository    string `json:"repository"`
	RepositoryID  string `json:"repository_id"`
}

func ensureStateOwnershipMarker(stateDir string, config Config) error {
	if strings.TrimSpace(config.Repository) == "" || strings.TrimSpace(config.RepositoryID) == "" {
		return nil
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	info, err := os.Lstat(stateDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Hive state root is not an ordinary directory")
	}
	marker := stateOwnershipMarker{SchemaVersion: "hive.state-owner.v1", Repository: config.Repository, RepositoryID: config.RepositoryID}
	path := filepath.Join(stateDir, stateOwnershipMarkerFile)
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Hive state ownership marker is linked or non-regular")
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var existing stateOwnershipMarker
		if json.Unmarshal(data, &existing) != nil || existing != marker {
			return fmt.Errorf("Hive state ownership marker does not match repository %s/%s", config.Repository, config.RepositoryID)
		}
		return nil
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(path, append(data, '\n'))
}

func (s *Store) Audit(entry AuditEntry) {
	_ = s.AuditStrict(entry)
}

func (s *Store) AuditStrict(entry AuditEntry) error {
	entry.Timestamp = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	info, statErr := os.Lstat(s.auditPath)
	if statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("Hive audit path is linked or non-regular")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	created := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(s.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if created {
		return syncStateParentDirectory(s.auditPath)
	}
	return nil
}

func readOrdinaryStateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("Hive state file %s is linked or non-regular", filepath.Base(path))
	}
	return os.ReadFile(path)
}

func (s *Store) Dir() string { return s.dir }
