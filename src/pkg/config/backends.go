package config

// BackendAllowed reports whether this spoke's backends allow/deny list
// permits placing an agent on backend.
func (c *Config) BackendAllowed(backend string) bool {
	return c.Backends.Allowed(backend)
}
