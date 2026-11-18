package cruxtest

import "github.com/hivecommons/hive/crux"

// SetKnowledgeAdviser records the mid-task knowledge advisor seam
// (hivecommons/hive#17052); it lives apart from recorder.go only because that
// file sits at its size-ratchet baseline.
func (r *Recorder) SetKnowledgeAdviser(adviser crux.KnowledgeAdviser) {
	r.record("SetKnowledgeAdviser", adviser != nil)
}

// SetSandboxGitHubReadRelay records the sandbox GitHub read relay seam
// (hivecommons/hive#18865): whether one was installed, and its extra NO_PROXY
// hosts. It lives here for the same size-ratchet reason as above.
func (r *Recorder) SetSandboxGitHubReadRelay(relay crux.SandboxGitHubReadRelay, noProxy []string) {
	r.record("SetSandboxGitHubReadRelay", relay != nil, noProxy)
}
