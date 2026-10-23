package config

import (
	"sort"
)

func sortedRuntimeNames() []string {
	names := make([]string, 0, len(ValidSandboxRuntimes))
	for n := range ValidSandboxRuntimes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
