package config

import (
	"strings"
)

// IsRepoScoped reports whether this agent declares a repository scope. False —
// the zero value, and every agent in every config written before #6204 — means
// the agent serves the whole hive.
func (a AgentConfig) IsRepoScoped() bool {
	for _, r := range a.Repos {
		if strings.TrimSpace(r) != "" {
			return true
		}
	}
	return false
}

// ReposIsOperatorOwned reports whether an operator explicitly set this agent's
// repo scope, which makes it immune to pack reconciliation — the same contract
// ModelIsOperatorOwned provides for models and PauseIsOperatorOwned for run
// state.
//
// This marker is the specific failure mode #6204 asks to design against. ACMM
// packs reconcile agents on EVERY restart, and the ownership family
// (#5632/#5706) exists because a pack that does not know a field was chosen by
// a human silently reverts it on the next pod roll. No pack ships a repo scope
// today, so nothing reverts it today — the marker is here so that a pack which
// one day does cannot widen a specialist back to the whole hive without anyone
// noticing.
func (a AgentConfig) ReposIsOperatorOwned() bool {
	return a.ReposOwner == FieldOwnerOperator
}
