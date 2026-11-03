package dashboard

import (
	"strings"
	"testing"
)

func TestRepoCardPillRowsUseSharedGrid(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".repo-issue-pill-wrap, .repo-pr-pill-wrap { display: grid;",
		"grid-template-columns: max-content minmax(0, 1fr) max-content;",
		".repo-pill-actions { display: flex; flex-wrap: wrap; min-width: 0; max-width: 100%; justify-content: end;",
		".repo-pill-action-slot { display: inline-flex; align-items: center; justify-content: center; min-width: 1.65rem; min-height: 1.45rem; overflow: visible; }",
		".repo-pill-action-slot.repo-pill-hold-slot { min-width: 4.8rem; }",
		"function repoPillActionCluster(slots)",
		`<span class="repo-issue-pill-wrap repo-pill-row${repoPillFilterHiddenClass(rowKinds, pillFilter)}"`,
		`<span class="repo-pr-pill-wrap repo-pill-row${repoPillFilterHiddenClass(rowKinds, pillFilter)}"`,
		"repoPillActionCluster([prBadge + cpActionHTML(i), planBtn, designBtn, holdBtn])",
		"repoPillActionCluster([reviewPill, cpActionHTML(p) + queueBtn + stateBadge, issueBadge, holdBtn])",
		"holdToggleChip(cardRepo, i, 'issue', isHeldIssue, canToggleHold, heldTip)",
		"repo-pill-action-slot .pill-icon { max-width: none; }",
		"const cls = index === 3 ? 'repo-pill-action-slot repo-pill-hold-slot' : 'repo-pill-action-slot';",
		".repo-pill-action-slot.empty { display: none; pointer-events: none; }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing shared repo pill layout snippet %q", want)
		}
	}
}

// A pill's signals and critical-path chips wrap under its title, which keeps
// a 10rem floor (pinned in TestRepoCardPillColumnsStaticWiring). Without it, a
// row whose chips outgrew a narrow column squeezed the title to one character
// per line.
func TestRepoCardPillChipsWrapUnderTitle(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".repo-pill-main { display: flex; flex-wrap: wrap; align-items: baseline;",
		".repo-pill-main .cp-chip { max-width: 100%; overflow: hidden; text-overflow: ellipsis; }",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing pill chip-wrap snippet %q", want)
		}
	}
}
