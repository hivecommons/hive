package worksource

import "testing"

// TestToGitHubIssuesPreservesSourceIdentity pins the compatibility-envelope half
// of #4245. ToGitHubIssues is the ONLY path a non-default work source takes into
// the actionable result, and it used to drop SourceType and ExternalID — which,
// for an item with no issue number, is the item's entire identity. Everything
// downstream then had nothing left to tell two Linear items apart.
func TestToGitHubIssuesPreservesSourceIdentity(t *testing.T) {
	in := []Issue{
		{SourceType: "linear", Repo: "acme/repo", ExternalID: "ENG-1", Title: "first", URL: "https://linear.app/1"},
		{SourceType: "linear", Repo: "acme/repo", ExternalID: "ENG-2", Title: "second", URL: "https://linear.app/2"},
	}

	out := ToGitHubIssues(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 projected issues, got %d", len(out))
	}
	for i, got := range out {
		if got.SourceType != in[i].SourceType {
			t.Errorf("item %d: SourceType dropped by the projection (%q)", i, got.SourceType)
		}
		if got.ExternalID != in[i].ExternalID {
			t.Errorf("item %d: ExternalID dropped by the projection (%q)", i, got.ExternalID)
		}
		if got.Number != 0 {
			t.Errorf("item %d: a string-keyed source must not gain an issue number (%d)", i, got.Number)
		}
	}

	// The two projected items must remain distinguishable — the property the
	// whole change exists to preserve.
	a := Ref{SourceType: out[0].SourceType, Repo: out[0].Repo, ExternalID: out[0].ExternalID, Number: out[0].Number}
	b := Ref{SourceType: out[1].SourceType, Repo: out[1].Repo, ExternalID: out[1].ExternalID, Number: out[1].Number}
	if a.Key() == b.Key() {
		t.Fatalf("projection collapsed two distinct items onto %q", a.Key())
	}
}

// TestToGitHubIssuesKeepsGitHubItemsUnchanged proves GitHub-backed items keep
// their number, and therefore their byte-identical "repo#number" key.
func TestToGitHubIssuesKeepsGitHubItemsUnchanged(t *testing.T) {
	out := ToGitHubIssues([]Issue{
		{SourceType: "github", Repo: "acme/repo", ExternalID: "42", Number: 42, Title: "issue"},
	})
	if len(out) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(out))
	}
	if out[0].Number != 42 {
		t.Fatalf("github issue number lost: %d", out[0].Number)
	}
	ref := Ref{SourceType: out[0].SourceType, Repo: out[0].Repo, ExternalID: out[0].ExternalID, Number: out[0].Number}
	if got := ref.Key(); got != "acme/repo#42" {
		t.Fatalf("github key = %q, want the unchanged acme/repo#42", got)
	}
}

// TestKeyRefusesFabricatedIdentity is the negative case that matters most: a
// Ref with no number and no external ID has NO identity, and Key() must say so
// rather than returning something that looks usable.
func TestKeyRefusesFabricatedIdentity(t *testing.T) {
	if got := (Ref{Repo: "acme/repo"}).Key(); got != "" {
		t.Errorf("Ref with no identity produced key %q, want empty", got)
	}
	if got := (Ref{ExternalID: "ENG-1"}).Key(); got != "" {
		t.Errorf("Ref with no repository produced key %q, want empty", got)
	}
	if (Ref{Repo: "acme/repo", ExternalID: "ENG-1"}).IsGitHubIssue() {
		t.Error("a string-keyed item must not be reported as GitHub-backed")
	}
}
