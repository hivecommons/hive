package agent

// Manager construction options and the persisted UID-map path they select.
// Kept out of manager.go so that file stays under the crux file-size ratchet.

type ManagerOption func(*managerOptions)

type managerOptions struct {
	uidMapPath    string
	uidMapPathSet bool
	sharedPool    SharedSessionPool
}

// WithUIDMapPath points a manager at a specific persisted UID map. Production
// callers keep the default UIDMapPath; tests can inject a hermetic path.
func WithUIDMapPath(path string) ManagerOption {
	return func(opts *managerOptions) {
		opts.uidMapPath = path
		opts.uidMapPathSet = true
	}
}

// WithSharedSessionPool opts a manager into a Hub-coordinated live CLI slot.
// Every Hive retains separate sessions and tenant data; the lease only limits
// how many CLI sessions across the participating Hives remain live.
func WithSharedSessionPool(pool SharedSessionPool) ManagerOption {
	return func(opts *managerOptions) { opts.sharedPool = pool }
}

func (m *Manager) effectiveUIDMapPath() string {
	if m.uidMapPath != "" {
		return m.uidMapPath
	}
	return UIDMapPath
}

func (m *Manager) saveUIDMap() {
	if m.uidMap == nil {
		return
	}
	path := m.effectiveUIDMapPath()
	_ = m.uidMap.Save(path)
	if path == PersistedUIDMapPath && UIDMapPath != "" && UIDMapPath != path {
		_ = m.uidMap.Save(UIDMapPath)
	}
}
