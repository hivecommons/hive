package dashboard

import (
	"strings"
	"testing"
)

func TestRepoCardPillColumns9053StaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"position: relative; min-width: 0; container-type: inline-size;",
		"@container (max-width: 520px) { .repo-pills { grid-template-columns: minmax(0, 1fr); } }",
		".repo-pills.repo-pills-issues-only, .repo-pills.repo-pills-prs-only { grid-template-columns: minmax(0, 1fr); }",
		".repo-issue-pill-wrap, .repo-pr-pill-wrap { display: grid; grid-template-columns: minmax(4.5rem, max-content) minmax(6rem, 1fr) max-content; align-items: center; gap: var(--sp-2); min-width: 0; max-width: 100%; }",
		".repo-pill-actions { display: grid; grid-auto-flow: column; grid-auto-columns: minmax(1.65rem, max-content); min-width: 0; overflow: hidden; justify-content: end; align-items: center; gap: var(--sp-1); }",
		".repo-pill-action-slot.repo-pill-hold-slot { min-width: 4.8rem; }",
		".repo-pill-action-slot.empty { display: none; pointer-events: none; }",
		"const cls = index === 3 ? 'repo-pill-action-slot repo-pill-hold-slot' : 'repo-pill-action-slot';",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing #9053 repo pill column guard %q", want)
		}
	}

	for _, gone := range []string{
		"grid-template-columns: minmax(1.65rem, max-content) minmax(1.65rem, max-content) minmax(1.65rem, max-content) minmax(4.8rem, max-content);",
		".repo-pill-action-slot.empty { visibility: hidden; pointer-events: none; }",
		"grid-template-columns: minmax(4.5rem, max-content) minmax(0, 1fr) max-content;",
	} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html still has pre-#9053 repo pill column snippet %q", gone)
		}
	}
}
