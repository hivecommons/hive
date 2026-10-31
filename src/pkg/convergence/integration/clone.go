package integration

import "slices"

func cloneIntent(i Intent) Intent {
	i.Components = slices.Clone(i.Components)
	i.Contracts = slices.Clone(i.Contracts)
	i.Paths = slices.Clone(i.Paths)
	i.Generated = slices.Clone(i.Generated)
	i.Prerequisites = slices.Clone(i.Prerequisites)
	i.Relations = slices.Clone(i.Relations)
	return i
}

func cloneEvidence(e Evidence) Evidence {
	e.Subject.Members = slices.Clone(e.Subject.Members)
	e.CheckIDs = slices.Clone(e.CheckIDs)
	return e
}

func cloneJob(j Job) Job {
	j.Candidates = slices.Clone(j.Candidates)
	return j
}
