package agent

import "fmt"

// GetBufferOutput returns output from the ring buffer directly, bypassing
// the tmux pane capture. The ring buffer accumulates all output over time
// (up to 500 lines) while the pane capture only has visible lines.
func (m *Manager) GetBufferOutput(name string, lines int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}

	if agent.OutputBuffer != nil && agent.OutputBuffer.Count() > 0 {
		return agent.OutputBuffer.Last(lines), nil
	}

	if pane := agent.FilteredPaneLines(lines); len(pane) > 0 {
		return pane, nil
	}

	return nil, nil
}

// GetBufferSince returns ring-buffer lines at positions seq and later plus the
// next position, so followers can stream without inferring overlap by content.
func (m *Manager) GetBufferSince(name string, seq uint64) ([]string, uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, seq, fmt.Errorf("agent %s not found", name)
	}
	if agent.OutputBuffer == nil {
		return nil, seq, nil
	}
	lines, next := agent.OutputBuffer.Since(seq)
	return lines, next, nil
}

func (m *Manager) GetOutput(name string, lines int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}

	if pane := agent.FilteredPaneLines(lines); len(pane) > 0 {
		return pane, nil
	}

	if agent.OutputBuffer != nil {
		return agent.OutputBuffer.Last(lines), nil
	}

	return nil, nil
}
