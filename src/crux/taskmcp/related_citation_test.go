package taskmcp

import (
	"testing"
	"time"
)

// related_work and history count an item as related when its title or body
// cites the task's issue. The match looked only at what follows "#N", so
// issue 42 of any other repository counted as a citation of this
// repository's issue 42, and a full link to the issue did not count at all.
func TestCitesIssue(t *testing.T) {
	scope := Scope{Repo: "owner/repo", Number: 42}
	for text, want := range map[string]bool{
		// references to this issue
		"#42":                               true,
		"Split out of #42":                  true,
		"Follow-up to #42.":                 true,
		"(#42)":                             true,
		"see [#42](https://example.test/x)": true,
		"owner/repo#42":                     true,
		"Blocked by Owner/Repo#42, sadly":   true,
		"https://github.com/owner/repo/issues/42":              true,
		"see https://github.com/owner/repo/pull/42 for more":   true,
		"https://github.com/Owner/Repo/issues/42#issuecomment": true,
		"https://git.example.test/owner/repo/-/issues/42":      true,
		"line one\n#42\nline three":                            true,

		// another number
		"#420":      false,
		"#4":        false,
		"#42a":      false,
		"#42-x":     false,
		"# 42":      false,
		"number 42": false,
		"https://github.com/owner/repo/issues/420": false,
		"https://github.com/owner/repo/issues/4":   false,

		// another repository's issue 42
		"Blocked on kubernetes/kubernetes#42 upstream": false,
		"other/repo#42":     false,
		"xowner/repo#42":    false,
		"owner/repo-two#42": false,
		"repo#42":           false,
		"https://github.com/other/repo/issues/42":                 false,
		"https://github.com/owner/repo-two/issues/42":             false,
		"https://github.com/mirror/owner/repo/issues/42":          false,
		"https://git.example.test/owner/repo/-/merge_requests/42": false,

		// not a reference
		"abc#42": false,
		"PR#42":  false,
		"a_#42":  false,
		"":       false,
	} {
		if got := citesIssue(text, scope); got != want {
			t.Errorf("citesIssue(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestCitesIssueWithoutARepository(t *testing.T) {
	scope := Scope{Number: 7}
	for text, want := range map[string]bool{
		"see #7":                                 true,
		"owner/repo#7":                           false,
		"https://github.com/owner/repo/issues/7": false,
	} {
		if got := citesIssue(text, scope); got != want {
			t.Errorf("citesIssue(%q) = %v, want %v", text, got, want)
		}
	}
	if citesIssue("#0", Scope{Repo: "owner/repo"}) {
		t.Error("a task with no issue number cites nothing")
	}
}

// The filter end to end: an item that only cites another repository's issue
// is not served, and one that links this repository's issue is.
func TestFilterRelatedWorkCitationsAreForThisRepository(t *testing.T) {
	scope := Scope{TaskID: "t", Repo: "owner/repo", Number: 42}
	current := RelatedItem{Kind: "issue", Repo: "owner/repo", Number: 42}
	candidates := []RelatedItem{
		{Kind: "issue", Repo: "owner/repo", Number: 50, Data: ServedText{Body: "Blocked on kubernetes/kubernetes#42 upstream"}},
		{Kind: "issue", Repo: "owner/repo", Number: 51, Data: ServedText{Body: "Continues https://github.com/owner/repo/issues/42"}},
		{Kind: "pull_request", Repo: "owner/repo", Number: 52, Data: ServedText{Title: "Part of #42"}},
	}
	data, _ := FilterRelatedWork(scope, current, candidates, 0, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), PageRequest{})
	got := map[int]bool{}
	for _, item := range data.Items {
		got[item.Number] = true
	}
	if got[50] || !got[51] || !got[52] || len(got) != 2 {
		t.Fatalf("related items = %v, want #51 and #52 only", got)
	}
}
