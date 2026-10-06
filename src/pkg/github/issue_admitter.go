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

// ReporterAdmitter is the optional reporter-trust half of admission
// (hivecommons/hive#9665). A filter that implements it is asked, BEFORE
// Admits, whether the issue's reporter may have it worked without triage.
// config.IssueFilterConfig implements it; the zero-value filter admits
// everyone, so existing hives change nothing.
type ReporterAdmitter interface {
	// AdmitsReporter reports whether an issue with these labels, filed by
	// login with this GitHub author_association, may be worked.
	AdmitsReporter(labels []string, login, association string) bool
	// ReporterTrustEnabled reports whether the gate is switched on at all, so
	// a refusal can be counted as "awaiting reporter triage" rather than as
	// an ordinary filter refusal.
	ReporterTrustEnabled() bool
}

// ReporterTrustNoticeConfig is the optional user-facing side of
// ReporterAdmitter. Config implementations provide the labels and toggles used
// when an untrusted reporter's issue is held out of the queue.
type ReporterTrustNoticeConfig interface {
	ReporterTrustTrustedAssociationsForNotice() []string
	ReporterTrustRequiredLabelsForNotice() []string
	ReporterTrustAwaitingLabel() string
	ReporterTrustCommentEnabled() bool
}

// ReporterTrustClankerConfig is the optional clanker-requested side of
// ReporterAdmitter (hivecommons/hive#10766): the switch, label and addendum
// for steering untrusted reporters and PR authors to the contributor relay.
// A filter that does not implement it leaves the policy off.
type ReporterTrustClankerConfig interface {
	ReporterTrustClankerRequestedOn() bool
	ReporterTrustClankerRequestedLabel() string
	ReporterTrustClankerRequestedAddendum() string
	ReporterTrustTrusts(login, association string) bool
}

// HardSuppressClassifier is the optional configurable form of the issue
// escalation labels that park an issue outside the actionable queue. It returns
// the canonical bucket label (needs-human, needs-direction, needs-decision, or
// needs-spec) for counting, even when the operator configured different GitHub
// label names.
type HardSuppressClassifier interface {
	HardSuppressIssueBucket(labels []string) string
}

// admitAllIssues is the filter used when none has been installed. It preserves
// the pre-existing zero-value behaviour of config.IssueFilterConfig, where an
// unset filter admits every issue.
type admitAllIssues struct{}

func (admitAllIssues) Admits([]string) bool { return true }
