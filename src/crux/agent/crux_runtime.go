package agent

import "github.com/hivecommons/hive/crux"

// *Manager is the runtime crux.Runtime describes. The assertion lives here,
// not in the crux root package, because crux/agent imports the root and the
// root must not import it back.
var _ crux.Runtime = (*Manager)(nil)

// *Manager is also the in-process crux.Host: the one implementation that
// can be handed Go closures and minters directly (hivecommons/hive#14138).
var _ crux.Host = (*Manager)(nil)

// AsRuntime adapts a manager to crux.Runtime, returning a nil interface for a
// nil manager so consumers' `runtime != nil` checks keep their meaning.
func AsRuntime(m *Manager) crux.Runtime {
	if m == nil {
		return nil
	}
	return m
}
