package config

import "strings"

// BackendsConfig restricts which backends agents on this spoke may be placed
// on (hivecommons/hive#11310). Deny wins over Allow; an empty Allow means
// every backend is allowed. Names compare case-insensitively, like gateway
// names in ValidateBackend. The zero value allows everything.
type BackendsConfig struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty" json:"deny,omitempty"`
}

// Allowed reports whether name may be used as an agent backend. An empty name
// means "the hive default" and is always allowed.
func (b BackendsConfig) Allowed(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	if backendListContains(b.Deny, name) {
		return false
	}
	return len(b.Allow) == 0 || backendListContains(b.Allow, name)
}

// Fallback is the backend a placement onto a disallowed backend is replaced
// with: the first allowed entry of Allow, or "" (the hive default) when Allow
// is empty or names nothing usable.
func (b BackendsConfig) Fallback() string {
	for _, name := range b.Allow {
		name = strings.TrimSpace(name)
		if name != "" && b.Allowed(name) {
			return name
		}
	}
	return ""
}

// BackendAllowed reports whether this spoke's backends allow/deny list
// permits placing an agent on backend.
func (c *Config) BackendAllowed(backend string) bool {
	return c.Backends.Allowed(backend)
}

func backendListContains(list []string, name string) bool {
	for _, entry := range list {
		if strings.EqualFold(strings.TrimSpace(entry), name) {
			return true
		}
	}
	return false
}
