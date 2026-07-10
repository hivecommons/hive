package integrated

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, configPath: filepath.Join(dir, "config.json"), auditPath: filepath.Join(dir, "audit.jsonl")}, nil
}

func (s *Store) Load() (Config, error) {
	data, err := os.ReadFile(s.configPath)
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, err
	}
	if config.SchemaVersion != ConfigSchema {
		return Config{}, fmt.Errorf("unsupported integrated config schema %q", config.SchemaVersion)
	}
	return config, nil
}

func (s *Store) Save(config Config) error {
	config.SchemaVersion = ConfigSchema
	config.UpdatedAt = time.Now().UTC()
	if config.InstalledAt.IsZero() {
		config.InstalledAt = config.UpdatedAt
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	temporary := s.configPath + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.configPath)
}

func (s *Store) Audit(entry AuditEntry) {
	entry.Timestamp = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	file, err := os.OpenFile(s.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(data, '\n'))
}

func (s *Store) Dir() string { return s.dir }
