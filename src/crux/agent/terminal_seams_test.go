package agent

// termSeams returns the Manager's funcTerminal, installing one on first use.
// Tests set individual method funcs on the returned value, mirroring the old
// per-field seam assignments while using the v5 TerminalSession contract.
func termSeams(m *Manager) *funcTerminal {
	if ft, ok := m.terminal.(*funcTerminal); ok {
		return ft
	}
	if ft, ok := m.terminal.(funcTerminal); ok {
		m.terminal = &ft
		return &ft
	}
	ft := &funcTerminal{}
	m.terminal = ft
	return ft
}
