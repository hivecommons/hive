package github

// SetRequiredChecks installs the config-declared required-status-check set
// (config.AutoMergeConfig.RequiredCheckSet) consulted by the merge-request
// watcher's pre-merge CI gate (merge_ci_gate.go, #6173) before it ever calls
// GitHub's branch-protection API. It mirrors automerge.Engine.SetRequiredChecks
// so both merge lanes gate on the same declared set. nil/empty clears it,
// meaning "not config-declared": the gate then falls back to the API and, if
// that also fails, to the isMetaCheck/isIgnorableCICheck allowlist. Safe to
// call repeatedly (e.g. on every config reload); the watcher goroutine reads
// the installed value through requiredChecksMu.
func (c *Client) SetRequiredChecks(set map[string]bool) {
	if c == nil {
		return
	}
	c.requiredChecksMu.Lock()
	defer c.requiredChecksMu.Unlock()
	c.requiredChecks = set
}

// configRequiredChecks returns the currently installed config-declared
// required-check set and whether one is installed.
func (c *Client) configRequiredChecks() (map[string]bool, bool) {
	if c == nil {
		return nil, false
	}
	c.requiredChecksMu.RLock()
	defer c.requiredChecksMu.RUnlock()
	if len(c.requiredChecks) == 0 {
		return nil, false
	}
	return c.requiredChecks, true
}
