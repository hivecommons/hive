package dashboard

import (
	"strings"
	"testing"
)

func TestRepoCardPillColumnsStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".repo-pills { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);",
		".repo-pills.repo-pills-issues-only, .repo-pills.repo-pills-prs-only { grid-template-columns: minmax(0, 1fr); }",
		".repo-pills-issues-only .repo-pill-col-prs, .repo-pills-prs-only .repo-pill-col-issues { display: none; }",
		".repo-pill-col { display: flex; flex-direction: column; gap: var(--sp-2); min-width: 0; overflow: hidden; }",
		".repo-issue-pill-wrap { display: flex; flex-wrap: wrap;",
		".repo-pr-pill-wrap { display: flex; flex-wrap: wrap;",
		".repo-issue-pill-wrap .repo-issue-pill { min-width: 0; flex: 1 1 auto; overflow: hidden; }",
		".repo-pr-pill-wrap .repo-pr-pill:not(.pill-icon) { min-width: 0; flex: 1 1 auto; overflow: hidden; }",
		".repo-issue-pill .pill-num { flex: 0 0 auto;",
		".repo-pr-pill .pill-num { flex: 0 0 auto;",
		".repo-issue-pill .pill-title { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }",
		".repo-pr-pill .pill-title { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }",
		"const pillColClass = issueCol && prCol ? '' : (issueCol ? ' repo-pills-issues-only' : ' repo-pills-prs-only');",
		"<div class=\"repo-pills${pillColClass}\">",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing repo-card pill column wiring %q", want)
		}
	}
}
