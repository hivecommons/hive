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

// AddExtraCredentialEnvVars registers environment variable names that carry a
// credential only the operator's config names. Names accumulate for the life
// of the process and are never removed. Blank and duplicate names are ignored.
func AddExtraCredentialEnvVars(names ...string) {
	extraCredentialEnvMu.Lock()
	defer extraCredentialEnvMu.Unlock()
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(extraCredentialEnvVars, n) {
			extraCredentialEnvVars = append(extraCredentialEnvVars, n)
		}
	}
}

// ExtraCredentialEnvVars returns a copy of the registered names in the order
// they were first added.
func ExtraCredentialEnvVars() []string {
	extraCredentialEnvMu.RLock()
	defer extraCredentialEnvMu.RUnlock()
	return slices.Clone(extraCredentialEnvVars)
}
