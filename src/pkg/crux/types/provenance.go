package types

// ProofProvenance is the bounded replay handle a convergence proof record
// and a stage receipt carry: enough to re-issue the exact observation and
// audit which runs were counted — deliberately NOT a log archive. It lives
// here because the agent-report schema the runtime validates
// (pkg/crux/outputschema) embeds it; pkg/convergence/proof aliases it as
// proof.Provenance.
type ProofProvenance struct {
	// CheckRunIDs are the check-run IDs counted toward the result, sorted.
	CheckRunIDs []int64 `json:"check_run_ids,omitempty"`
	// Query names the observation call shape, e.g.
	// "ListCheckRunsForRef@<headSHA>".
	Query string `json:"query"`
}
