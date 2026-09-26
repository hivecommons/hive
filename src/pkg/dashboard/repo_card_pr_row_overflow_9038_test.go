package dashboard

import (
	"strings"
	"testing"
)

func TestRepoCardPRRowOverflow9038StaticWiring(t *testing.T) {
	html := indexHTML(t)
	want := []string{
		".repo-issue-pill-wrap, .repo-pr-pill-wrap { display: grid; grid-template-columns: minmax(4.5rem, max-content) minmax(0, 1fr) max-content; align-items: center; gap: var(--sp-2); min-width: 0; max-width: 100%; }",
		".repo-pill-status { display: inline-block; text-align: center; min-width: 4.5rem; max-width: 5.25rem;",
		".repo-pill-status.repo-issue-pill, .repo-pill-status.repo-pr-pill {",
		"display: inline-block; text-align: center; min-width: 4.5rem; max-width: 5.25rem;",
		"overflow: hidden; text-overflow: ellipsis;",
		".repo-issue-pill-wrap .repo-issue-pill:not(.repo-pill-status) { min-width: 0; overflow: hidden; }",
		".repo-pr-pill-wrap .repo-pr-pill:not(.pill-icon):not(.repo-pill-status) { min-width: 0; overflow: hidden; }",
	}
	for _, snippet := range want {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html missing #9038 PR row overflow guard %q", snippet)
		}
	}

	gone := []string{
		"grid-template-columns: minmax(3.65rem, max-content) minmax(10.5rem, 1fr) max-content;",
		".repo-pr-pill-wrap .repo-pr-pill:not(.pill-icon) { min-width: 0; overflow: hidden; }",
		".repo-issue-pill-wrap .repo-issue-pill { min-width: 0; overflow: hidden; }",
	}
	for _, snippet := range gone {
		if strings.Contains(html, snippet) {
			t.Errorf("index.html still has rigid #9038 overflow snippet %q", snippet)
		}
	}
}
