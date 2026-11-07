package cruxtest

import "github.com/hivecommons/hive/crux"

// SetKnowledgeAdviser records the mid-task knowledge advisor seam
// (hivecommons/hive#17052); it lives apart from recorder.go only because that
// file sits at its size-ratchet baseline.
func (r *Recorder) SetKnowledgeAdviser(adviser crux.KnowledgeAdviser) {
	r.record("SetKnowledgeAdviser", adviser != nil)
}
