// Package runtrailer parses the git-style Hive run provenance trailers.
package runtrailer

import "strings"

const (
	KeyRun  = "Hive-Run"
	KeyPlan = "Hive-Plan"
	KeySpec = "Hive-Spec"
)

type DuplicatePolicy int

const (
	FirstWins DuplicatePolicy = iota
	LastWins
)

// Parse extracts the canonical Hive run-provenance trailers from message.
// Trailer keys are case-sensitive, line-anchored, and may be surrounded by
// whitespace before the colon. Values are trimmed.
func Parse(message string, duplicates DuplicatePolicy) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n") {
		key, value, ok := parseLine(line)
		if !ok {
			continue
		}
		if duplicates == FirstWins {
			if strings.TrimSpace(out[key]) != "" {
				continue
			}
		}
		out[key] = value
	}
	return out
}

func parseLine(line string) (string, string, bool) {
	key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	switch key {
	case KeyRun, KeyPlan, KeySpec:
		return key, strings.TrimSpace(value), true
	default:
		return "", "", false
	}
}
