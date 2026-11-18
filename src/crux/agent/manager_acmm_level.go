package agent

// SetACMMLevel updates the cached ACMM level used by agentMode() when
// launching agents. Call this whenever the ACMM level changes.
func (m *Manager) SetACMMLevel(level int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.project.ACMMLevel = level
	m.removeAgentsBelowACMMGateLocked(level)
}

func (m *Manager) GetACMMLevel() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.project.ACMMLevel
}
