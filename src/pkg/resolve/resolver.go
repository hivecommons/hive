// Package resolve provides a pluggable ${VAR} substitution engine shared by
// hive's two substitution sites: config loading (config.expandEnvVars, over the
// raw hive.yaml text) and per-kick prompt templating (scheduler/dashboard).
//
// Historically each site hardcoded its variables — config used a single
// os.LookupEnv regex, and the scheduler used a fixed strings.NewReplacer of ~29
// pairs. Neither could gain a new variable without a Go edit. This package makes
// the set of resolvable variables extensible via config (a `variables:` block)
// and via code (RegisterResolver), while preserving the exact legacy behavior
// when nothing custom is configured — an unset or unknown ${VAR} is always left
// verbatim, never blanked.
package resolve

import (
	"context"
	"errors"
	"regexp"
)

// VarPattern matches ${NAME}. It is the same pattern config used for
// expandEnvVars, exported so the config package can share the single source of
// truth for what a substitutable reference looks like.
var VarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// ErrUnresolved is returned by a Resolver when it does not handle the requested
// variable, or handles it but has no value (e.g. an unset env var with no
// default). It is a sentinel, NOT a failure: the registry falls through to the
// next resolver, and if every resolver returns it the original ${VAR} text is
// left in place. Resolvers return a real (non-ErrUnresolved) error only for a
// genuine fault worth logging (e.g. a script that timed out); the registry
// still leaves the literal in place but records the error.
var ErrUnresolved = errors.New("resolve: unresolved")

// Request is what a Resolver is asked to resolve.
type Request struct {
	// Name is the bare variable name inside the braces, e.g. "AGENT_NAME" for
	// "${AGENT_NAME}".
	Name string
	// Raw is the full literal match, e.g. "${AGENT_NAME}". Returned unchanged
	// when a variable is left unresolved.
	Raw string
	// Ctx carries timeout/cancellation for resolvers that do I/O (script, http).
	Ctx context.Context
	// Runtime is non-nil only at per-kick call sites (System B). It exposes the
	// live built-in producers (issue lists, knowledge, etc.). It is nil during
	// config load (System A), where no runtime context exists.
	Runtime *RuntimeContext
}

// Result carries a resolved value plus a non-secret provenance tag.
type Result struct {
	// Value is the substituted text.
	Value string
	// Source is a non-secret provenance tag, e.g. "env:HIVE_REGION", "static",
	// "builtin", "script:<hash>", "http:<host>". Safe to surface in the UI/logs;
	// never contains the resolved value for secret-bearing resolvers.
	Source string
}

// Resolver resolves a single variable reference. Implementations must return
// ErrUnresolved (not a hard error, not an empty Result) for variables they do
// not handle, so the registry can fall through and ultimately preserve the
// literal ${VAR}.
type Resolver interface {
	Resolve(req Request) (Result, error)
}

// Factory builds a Resolver from an operator-declared VarSpec and the active
// Policy. It is the registration seam: a new resolver type is one Factory
// registered via RegisterResolver, with no changes to the call sites. A Factory
// may return an error to refuse construction — e.g. a script/http factory when
// the Policy has not enabled execution/network access.
type Factory func(spec VarSpec, pol Policy) (Resolver, error)
