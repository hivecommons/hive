package dashboard

// NousPanelSnapshot is the operator-set Experiment Lab panel state that must
// survive a restart (#11694): Mode, Scope and the section Config map. The
// dashboard owns the shape; cmd/hive wires the on-disk format via
// NousState.PersistPanel so this package stays free of storage concerns.
type NousPanelSnapshot struct {
	Mode   string
	Scope  string
	Config map[string]interface{}
}

// PersistPanelConfig hands a copy of Mode, Scope and Config to PersistPanel
// so the panel configuration survives a process restart. It is a no-op when
// PersistPanel is unset. Callers must NOT hold Mu.
func (ns *NousState) PersistPanelConfig() error {
	if ns == nil || ns.PersistPanel == nil {
		return nil
	}
	ns.Mu.Lock()
	snap := NousPanelSnapshot{
		Mode:   ns.Mode,
		Scope:  ns.Scope,
		Config: copyPanelConfigMap(ns.Config),
	}
	ns.Mu.Unlock()
	return ns.PersistPanel(snap)
}

// ApplyPanelConfig overlays a persisted panel snapshot onto the state:
// non-empty Mode/Scope replace the defaults and a non-nil Config replaces
// the in-memory map. Status mirrors of mode/scope are refreshed when present.
func (ns *NousState) ApplyPanelConfig(snap NousPanelSnapshot) {
	ns.Mu.Lock()
	defer ns.Mu.Unlock()
	if snap.Mode != "" {
		ns.Mode = snap.Mode
	}
	if snap.Scope != "" {
		ns.Scope = snap.Scope
	}
	if snap.Config != nil {
		ns.Config = copyPanelConfigMap(snap.Config)
	}
	if ns.Status != nil {
		ns.mirrorModeScopeLocked()
	}
}

// SetMode records an operator-chosen mode and mirrors it into Status, which
// is what GET /api/nous/status serves. Before #12039 the handler only wrote
// ns.Mode, so the PUT returned 200 and was audited while the status pill
// (and the segmented buttons rendered from it) stayed on the old value until
// the next restart re-applied the persisted panel. Callers must NOT hold Mu.
func (ns *NousState) SetMode(mode string) {
	ns.Mu.Lock()
	defer ns.Mu.Unlock()
	ns.Mode = mode
	ns.mirrorModeScopeLocked()
}

// SetScope is SetMode for the scope selector.
func (ns *NousState) SetScope(scope string) {
	ns.Mu.Lock()
	defer ns.Mu.Unlock()
	ns.Scope = scope
	ns.mirrorModeScopeLocked()
}

// mirrorModeScopeLocked copies Mode/Scope into the served Status map. Mu must
// be held. A nil Status (bare NousState in tests) is created so the mirror is
// never silently dropped.
func (ns *NousState) mirrorModeScopeLocked() {
	if ns.Status == nil {
		ns.Status = make(map[string]interface{})
	}
	ns.Status["mode"] = ns.Mode
	ns.Status["scope"] = ns.Scope
}

// copyPanelConfigMap shallow-copies the section map so a persisted snapshot
// cannot be mutated by a concurrent PUT while it is being marshalled.
func copyPanelConfigMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// persistNousPanel persists the Nous panel configuration after a mutating
// /api/nous call, logging rather than failing the request when the write
// fails — the in-memory mutation already succeeded.
func (s *Server) persistNousPanel() {
	if s.deps == nil || s.deps.Nous == nil {
		return
	}
	if err := s.deps.Nous.PersistPanelConfig(); err != nil && s.logger != nil {
		s.logger.Warn("nous: failed to persist panel config", "error", err)
	}
}
