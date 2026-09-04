package github

// IssueAdmitter decides whether an issue carrying the given labels is eligible
// for agent work.
//
// This exists so the client can honour the operator's project.issue_filter
// without importing pkg/config. The filter's semantics — exact,
// case-insensitive matching, empty means admit everything, and "exclude wins"
// being the caller's job — belong to the configuration layer that owns the
// policy; the client only needs the verdict. config.IssueFilterConfig
// satisfies this interface as-is, so construction sites keep passing it
// unchanged (kubestellar/hive#5953, phase 1).
//
// Keeping the decision behind an interface rather than copying the matching
// logic here is deliberate: this is an approval gate, and a second
// implementation of it that drifted from the first would fail open.
type IssueAdmitter interface {
	// Admits reports whether an issue with these labels may be worked.
	Admits(labels []string) bool
}

// admitAllIssues is the filter used when none has been installed. It preserves
// the pre-existing zero-value behaviour of config.IssueFilterConfig, where an
// unset filter admits every issue.
type admitAllIssues struct{}

func (admitAllIssues) Admits([]string) bool { return true }
