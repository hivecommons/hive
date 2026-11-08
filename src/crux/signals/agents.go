package signals

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

const (
	// dirMode keeps the outcome directory owner-only.
	dirMode = 0o700
	// maxAgentNameLen matches the CLI's definition-name gate.
	maxAgentNameLen = 64
)

// agentName is the definition-name gate (cmd/crux validateDefinitionName):
// the name becomes a file name, so it must be a plain identifier.
var agentName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// AgentsDir is where AgentOutcomes keeps its records under a work dir.
func AgentsDir(workDir string) string { return filepath.Join(workDir, DirName, AgentsDirName) }

// SavedOutcome is one agent's record on disk: the outcome of its most
// recent kicked turn, as the runtime's kick-outcome observer reported it.
type SavedOutcome struct {
	Agent   string          `json:"agent"`
	Outcome api.KickOutcome `json:"outcome"`
	SavedAt time.Time       `json:"saved_at"`
}

// AgentOutcomes is the last KickOutcome of each agent, one JSON file per
// agent, replaced atomically on every settle. A live event is a
// notification; this record is what a driver that missed it reads.
type AgentOutcomes struct {
	dir   string
	scrub func(string) string
	now   func() time.Time
	mu    sync.Mutex
}

// OpenAgentOutcomes creates dir (0700) when missing and returns the store
// over it, scrubbing and stamping with pol.
func OpenAgentOutcomes(dir string, pol Policy) (*AgentOutcomes, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("signals: %w", err)
	}
	pol = pol.defaults()
	return &AgentOutcomes{dir: dir, scrub: pol.Scrub, now: pol.Now}, nil
}

func (s *AgentOutcomes) path(agent string) (string, error) {
	if len(agent) > maxAgentNameLen || !agentName.MatchString(agent) {
		return "", fmt.Errorf("%w: agent name %q", ErrBadRequest, agent)
	}
	return filepath.Join(s.dir, agent+".json"), nil
}

// Save replaces agent's record with o. The reason, a line of the agent's
// pane, is scrubbed before it is written.
func (s *AgentOutcomes) Save(agent string, o api.KickOutcome) error {
	path, err := s.path(agent)
	if err != nil {
		return err
	}
	o.Reason = s.scrub(o.Reason)
	data, err := json.Marshal(SavedOutcome{Agent: agent, Outcome: o, SavedAt: s.now().UTC()})
	if err != nil {
		return fmt.Errorf("signals: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp, err := os.CreateTemp(s.dir, "."+agent+"-*.tmp")
	if err != nil {
		return fmt.Errorf("signals: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("signals: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("signals: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("signals: %w", err)
	}
	return nil
}

// Load returns agent's saved record; none saved is ErrNotFound.
func (s *AgentOutcomes) Load(agent string) (SavedOutcome, error) {
	path, err := s.path(agent)
	if err != nil {
		return SavedOutcome{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SavedOutcome{}, ErrNotFound
		}
		return SavedOutcome{}, fmt.Errorf("signals: %w", err)
	}
	var rec SavedOutcome
	if err := json.Unmarshal(data, &rec); err != nil {
		return SavedOutcome{}, fmt.Errorf("%w: %s: %v", ErrCorrupt, filepath.Base(path), err)
	}
	return rec, nil
}
