package resolve

// RuntimeContext carries the live, per-kick built-in variable producers for
// System B (prompt templating). It lives in this package — rather than in
// scheduler — so resolve/ has no dependency on github/scheduler/dashboard,
// avoiding an import cycle. Callers fill Vars with thunks; a thunk runs only if
// the template actually references its variable, so expensive producers (e.g.
// knowledge priming) are not computed for templates that don't use them.
type RuntimeContext struct {
	// Vars maps a built-in variable name (without braces) to a producer. The
	// producer is called at most once per Expand call for a given name; see
	// Registry.Expand for memoization.
	Vars map[string]func() string
}

// lookup returns the producer for name and whether it exists.
func (rc *RuntimeContext) lookup(name string) (func() string, bool) {
	if rc == nil || rc.Vars == nil {
		return nil, false
	}
	fn, ok := rc.Vars[name]
	return fn, ok
}
