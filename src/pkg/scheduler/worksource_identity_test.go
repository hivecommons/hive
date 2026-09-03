package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// ── Source-aware identity in internal-agent kicks (#4245) ────────────────────

// TestIssueRefsCarryExternalWork proves non-GitHub work reaches an
// internal-agent kick at all. issueRefsForAgent used to `continue` on
// `Number <= 0`, so every Linear and Jira item was dropped before an agent ever
// saw it — and the two items here would have produced zero refs, not two.
func TestIssueRefsCarryExternalWork(t *testing.T) {
	issues := []github.Issue{
		{Repo: "acme/repo", SourceType: "linear", ExternalID: "ENG-1", Title: "first"},
		{Repo: "acme/repo", SourceType: "linear", ExternalID: "ENG-2", Title: "second"},
	}

	refs := issueRefsForAgent("scanner", issues)
	if len(refs) != 2 {
		t.Fatalf("both external items must be referenced in the kick, got %d: %v", len(refs), refs)
	}
	if refs[0] == refs[1] {
		t.Fatalf("distinct external items collapsed onto one ref: %v", refs)
	}
	for _, r := range refs {
		if strings.HasSuffix(r, "#0") {
			t.Errorf("kick referenced the fabricated #0 identity: %q", r)
		}
	}
	if refs[0] != "acme/repo!ENG-1" || refs[1] != "acme/repo!ENG-2" {
		t.Errorf("refs = %v, want the native keys", refs)
	}
}

// TestIssueRefsKeepGitHubSpelling is the compatibility half: GitHub-backed refs
// must stay byte-identical, because recordKick and everything downstream of it
// has always received this exact string.
func TestIssueRefsKeepGitHubSpelling(t *testing.T) {
	refs := issueRefsForAgent("scanner", []github.Issue{
		{Repo: "acme/repo", Number: 42, Title: "a github issue"},
	})
	if len(refs) != 1 || refs[0] != "acme/repo#42" {
		t.Fatalf("github ref = %v, want [acme/repo#42]", refs)
	}
}

// TestIssueRefsDropUnidentifiableItems pins the one case that must still be
// dropped: no number AND no external key. Referencing it would mean inventing
// an identity.
func TestIssueRefsDropUnidentifiableItems(t *testing.T) {
	refs := issueRefsForAgent("scanner", []github.Issue{
		{Repo: "acme/repo", Title: "no identity"},
		{Repo: "", Number: 7, Title: "no repository"},
		{Repo: "acme/repo", Number: 3, Title: "fine"},
	})
	if len(refs) != 1 || refs[0] != "acme/repo#3" {
		t.Fatalf("only the identifiable item may be referenced, got %v", refs)
	}
}

// TestIssueRefsDedupeOnCanonicalIdentity proves the de-duplication key moved
// with the identity: the same external item enumerated twice is one ref.
func TestIssueRefsDedupeOnCanonicalIdentity(t *testing.T) {
	refs := issueRefsForAgent("scanner", []github.Issue{
		{Repo: "acme/repo", SourceType: "jira", ExternalID: "OPS-1"},
		{Repo: "acme/repo", SourceType: "jira", ExternalID: "OPS-1"},
	})
	if len(refs) != 1 {
		t.Fatalf("a repeated enumeration of one item must yield one ref, got %v", refs)
	}
}

// TestSchedulerHasNoSecondKeyImplementation is the bypass guard: the scheduler
// must produce exactly what pkg/worksource produces. A parallel implementation
// here would pass every helper test in pkg/worksource and still send agents
// "#0" refs.
func TestSchedulerHasNoSecondKeyImplementation(t *testing.T) {
	cases := []github.Issue{
		{Repo: "acme/repo", Number: 42, ExternalID: "42"},
		{Repo: "acme/repo", SourceType: "linear", ExternalID: "ENG-1"},
		{Repo: "acme/repo", SourceType: "jira", ExternalID: "OPS-9"},
		{Repo: "acme/repo", SourceType: "github_projects", Number: 7, ExternalID: "7"},
	}
	for _, issue := range cases {
		want := worksource.Ref{
			SourceType: issue.SourceType,
			Repo:       issue.Repo,
			ExternalID: issue.ExternalID,
			Number:     issue.Number,
			URL:        issue.URL,
		}.Key()
		if got := issueKey(issue); got != want {
			t.Errorf("scheduler keyed %+v as %q, pkg/worksource says %q — second implementation", issue, got, want)
		}
	}
}

// TestIssueDisplayRefNeverRendersZero pins the message BODY, not just the ref
// list. The kick text formatted "%s#%d" directly, so an external item was
// described to the agent as "acme/repo#0".
func TestIssueDisplayRefNeverRendersZero(t *testing.T) {
	external := github.Issue{Repo: "acme/repo", SourceType: "linear", ExternalID: "ENG-1"}
	if got := issueDisplayRef(external); got != "acme/repo!ENG-1" {
		t.Errorf("display ref = %q, want the native key", got)
	}
	if strings.Contains(issueDisplayRef(external), "#0") {
		t.Error("display ref rendered the fabricated #0 identity")
	}

	gh := github.Issue{Repo: "acme/repo", Number: 42}
	if got := issueDisplayRef(gh); got != "acme/repo#42" {
		t.Errorf("github display ref = %q, want acme/repo#42 unchanged", got)
	}

	// An item with no identity at all falls back to the bare repo rather than
	// rendering a key nothing can match.
	if got := issueDisplayRef(github.Issue{Repo: "acme/repo"}); got != "acme/repo" {
		t.Errorf("unidentifiable item display = %q, want the bare repo", got)
	}
}
