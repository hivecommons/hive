package agent

import (
	"github.com/hivecommons/hive/crux"
)

// SetConditionReporter installs the host's sink for the runtime's
// self-reported error conditions (restart storms, repeated kick failures,
// stalls, data-disk pressure, token-mint failures, CI-poll nudges). Hive
// wires pkg/fleetreport's runtime tracker; a standalone runtime may leave
// it unset, in which case observations are dropped. Safe to call
// concurrently with the runtime; the reporter is read on paths that hold
// m.mu, so it is stored atomically rather than under the lock.
func (m *Manager) SetConditionReporter(r crux.ConditionReporter) {
	if r == nil {
		m.conditionReporter.Store(nil)
		return
	}
	m.conditionReporter.Store(&r)
}

// observeCondition forwards an observation to the installed reporter with
// pane-derived text redacted first, so a host sink never sees a GitHub token
// or API key that scrolled through an agent pane.
func (m *Manager) observeCondition(class, key, message, diagnostics string) {
	r := m.conditionReporter.Load()
	if r == nil || *r == nil {
		return
	}
	(*r).Observe(class, key, RedactPaneTokens(message), RedactPaneTokens(diagnostics))
}

// clearCondition forwards a recovery to the installed reporter.
func (m *Manager) clearCondition(class, key string) {
	r := m.conditionReporter.Load()
	if r == nil || *r == nil {
		return
	}
	(*r).Clear(class, key)
}
