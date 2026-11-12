package agent

import (
	"crypto/sha256"
	"os"
	"sync"
)

// strayGitIdentityWarned remembers, per (agent, config path), the content hash
// of the git config the stray-identity WARN was last emitted for. The launch
// check runs every cycle; without this the identical informational WARN would
// repeat and trip the repeated-error selfreport (hivecommons/hive#17703).
// State is in-memory, so a hive restart warns once more.
type strayGitIdentityWarned struct {
	mu   sync.Mutex
	seen map[string][sha256.Size]byte
}

// shouldWarn reports whether the finding at path is new for agentName: true on
// first sight and whenever the file content changed since the last warning.
func (s *strayGitIdentityWarned) shouldWarn(agentName, path string) bool {
	var sum [sha256.Size]byte
	if data, err := os.ReadFile(path); err == nil {
		sum = sha256.Sum256(data)
	}
	key := agentName + "\x00" + path
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.seen[key]; ok && prev == sum {
		return false
	}
	if s.seen == nil {
		s.seen = make(map[string][sha256.Size]byte)
	}
	s.seen[key] = sum
	return true
}
