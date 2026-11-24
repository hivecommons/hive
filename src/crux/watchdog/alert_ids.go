package watchdog

// Dashboard alert IDs the reconciler raises and clears, one per agent.
func crashLoopAlertID(name string) string { return alertPrefix + "crashloop-" + name }
func wedgeAlertID(name string) string     { return alertPrefix + "wedged-restart-" + name }
func authAlertID(name string) string      { return alertPrefix + "auth-" + name }
func producingAlertID(name string) string { return alertPrefix + "producing-" + name }
