package hub

// needsPush is the shape every hub->spoke reconcile rule shares: the hub has a
// value it wants (desired) and the spoke reports something else (observed).
//
// The observedKnown argument is the part that is NOT boilerplate. An empty
// report means one of two things and conflating them breaks the fleet in
// opposite directions:
//
//	observedKnown=true  — the spoke reported, and "" is a real value.
//	observedKnown=false — the spoke is too old to report this field at all.
//	                      That is UNKNOWN, not a mismatch. Pushing on it
//	                      re-sends every beat with no read-back that could
//	                      ever stop it, because the spoke cannot report what
//	                      it does not know how to report.
//
// Only the api_url rule needs observedKnown today, but naming the distinction
// keeps the next rule from quietly dropping it: the push flags look like copies
// of one expression and are not, and each difference was learned from a live
// outage.
func needsPush(desired, observed string, observedKnown bool) bool {
	return desired != "" && observedKnown && observed != desired
}
