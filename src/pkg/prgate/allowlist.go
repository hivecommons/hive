package prgate

import (
	"errors"
	"fmt"
)

// Context names the gate may publish. The list is closed: statuses on a PR
// head are matched by name against branch protection, so a poster able to
// write an arbitrary context could satisfy or fail any required check
// (DCO included). PostableContext is the only door and every write goes
// through it.
const (
	ContextVerify       = "verify PR contents"
	ContextPRType       = NamePRType
	ContextPRIssue      = NamePRIssue
	ContextVerifyShadow = "verify PR contents (hive)"
)

var allowedContexts = map[string]struct{}{
	ContextVerify:       {},
	ContextPRType:       {},
	ContextPRIssue:      {},
	ContextVerifyShadow: {},
}

// ErrContextNotAllowed is returned for any context outside the allowlist.
var ErrContextNotAllowed = errors.New("pr gate: commit status context is not on the allowlist")

// AllowedContexts returns the allowlisted contexts (a copy, in a stable order).
func AllowedContexts() []string {
	return []string{ContextVerify, ContextPRType, ContextPRIssue, ContextVerifyShadow}
}

// PostableContext reports whether name is exactly one of the allowlisted
// contexts. The match is exact and case-sensitive: `dco`, `DCO`, a padded
// name or a lookalike are all refused.
func PostableContext(name string) error {
	if _, ok := allowedContexts[name]; ok {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrContextNotAllowed, name)
}
