package signals

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/hivecommons/hive/crux/api"
)

const dirMode = 0o700

// agentNamePattern is the runtime's definition-name gate; the name
// becomes a file name here, so nothing else is accepted.
var agentNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ErrBadAgentName is an agent name that cannot name a file safely.
var ErrBadAgentName = errors.New("signals: bad agent name")

// OutcomeStore persists the last api.KickOutcome of each agent as
// <dir>/<agent>.json, replaced whole on every settle. It is the RFC S5
// "persist the last outcome per agent": after a restart the saved record,
// not a remembered event, says whether a turn ended.
type OutcomeStore struct {
	dir string
	mu  sync.Mutex
}

// DefaultDir is the signals directory under the runtime's work dir.
func DefaultDir(workDir string) string { return filepath.Join(workDir, DirName) }

// OpenOutcomes returns the store under dir/agents, creating it.
func OpenOutcomes(dir string) (*OutcomeStore, error) {
	path := filepath.Join(dir, AgentsDirName)
	if err := os.MkdirAll(path, dirMode); err != nil {
		return nil, fmt.Errorf("signals: %w", err)
	}
	return &OutcomeStore{dir: path}, nil
}

// Put saves outcome as agent's last. The write is atomic (temp file and
// rename) so a reader never sees a half-written record.
func (s *OutcomeStore) Put(agent string, outcome api.KickOutcome) error {
	if !agentNamePattern.MatchString(agent) {
		return fmt.Errorf("%w: %q", ErrBadAgentName, agent)
	}
	data, err := json.Marshal(outcome)
	if err != nil {
		return fmt.Errorf("signals: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	final := filepath.Join(s.dir, agent+".json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, fileMode); err != nil {
		return fmt.Errorf("signals: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("signals: %w", err)
	}
	return nil
}

// Get returns agent's saved outcome; ok is false when none was saved.
func (s *OutcomeStore) Get(agent string) (api.KickOutcome, bool, error) {
	if !agentNamePattern.MatchString(agent) {
		return api.KickOutcome{}, false, fmt.Errorf("%w: %q", ErrBadAgentName, agent)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.dir, agent+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return api.KickOutcome{}, false, nil
		}
		return api.KickOutcome{}, false, fmt.Errorf("signals: %w", err)
	}
	var out api.KickOutcome
	if err := json.Unmarshal(data, &out); err != nil {
		return api.KickOutcome{}, false, fmt.Errorf("%w: %s: %v", ErrCorrupt, agent, err)
	}
	return out, true, nil
}
