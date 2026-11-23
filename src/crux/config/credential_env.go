package config

import (
	"slices"
	"strings"
	"sync"
)

var (
	extraCredentialEnvMu   sync.RWMutex
	extraCredentialEnvVars []string
)

// SetExtraCredentialEnvVars registers environment variable names that carry a
// credential only the operator's config names. Each call replaces the previous set; blank names are dropped.
func SetExtraCredentialEnvVars(names ...string) {
	var clean []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			clean = append(clean, n)
		}
	}
	extraCredentialEnvMu.Lock()
	defer extraCredentialEnvMu.Unlock()
	extraCredentialEnvVars = clean
}

// ExtraCredentialEnvVars returns a copy of the registered names.
func ExtraCredentialEnvVars() []string {
	extraCredentialEnvMu.RLock()
	defer extraCredentialEnvMu.RUnlock()
	return slices.Clone(extraCredentialEnvVars)
}
