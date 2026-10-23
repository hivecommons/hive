// Package reviewtext provides dependency-free review prose classification shared
// by review aggregation and stage derivation.
package reviewtext

import "strings"

func ConcreteRemediationText(text string) bool {
	q := strings.ToLower(strings.TrimSpace(text))
	if q == "" {
		return false
	}
	terms := []string{
		"add ", "set ", "populate", "wire", "plumb", "compute", "copy", "reword",
		"rename", "remove", "update", "route", "fix ", "use ", "producer",
		"changelog", "fragment", "trim", "duplicate", "instead", "either add", "either adding", "file:", ".go:", ".ts:", ".tsx:",
	}
	for _, term := range terms {
		if strings.Contains(q, term) {
			return true
		}
	}
	return false
}

func RiskEscalationTerms() []string {
	return []string{
		"security", "vulnerab", "secret", "credential", "authentication", "authorization", "permission", "privilege",
		"data loss", "delete", "destructive", "irreversible", "migration", "public-facing",
		"breaking", "policy", "governance", "license", "licensing", "copyright", "legal",
		"spend", "cost", "billing", "privacy", "compliance", "unreadable", "could not read",
		"cannot read", "can't read", "could not judge", "cannot judge", "missing context",
		"unfamiliar subsystem", "ambiguous", "cannot test", "can't test", "high-risk",
	}
}

// RiskEscalationText reports whether prose must remain a human decision.
func RiskEscalationText(text string) bool {
	q := strings.ToLower(strings.TrimSpace(text))
	if q == "" {
		return false
	}
	for _, term := range RiskEscalationTerms() {
		if strings.Contains(q, term) {
			return true
		}
	}
	return false
}
