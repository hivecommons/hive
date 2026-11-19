package config

// APIKeyFileWarnings returns one operator-facing message for every litellm or
// gateway api_key_file outside the allowed secret dirs (hive#18533). Such a
// path is never read (audit N8), and key resolution silently falls back to the
// Secret mount, the PVC copy or an env var, so the operator gets a key other
// than the one they pointed at.
//
// Boot WARNS rather than failing: an existing hive.yaml with such a path has
// been running on its fallback key, and refusing to boot it would take a
// working spoke down over a setting that was already being ignored. The
// dashboard and API save paths REJECT a new out-of-root path with the same
// message (see pkg/dashboard).
func (g GovernorConfig) APIKeyFileWarnings() []string {
	var out []string
	if msg := g.LiteLLM.APIKeyFileProblem(); msg != "" {
		out = append(out, msg)
	}
	for _, gw := range g.Gateways {
		if msg := gw.APIKeyFileProblem(); msg != "" {
			out = append(out, msg)
		}
	}
	return out
}
