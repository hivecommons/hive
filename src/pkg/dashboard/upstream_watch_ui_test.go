package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// Guard for the renderAll()/hiveToast() class of bug (the
// convergence_ui_test.go pattern) applied to the upstream-watch divergence
// panel (hivecommons/hive#9969): the shell, its refresh action and the tab
// hook must all be present, every callee must be defined, and the panel must
// read exactly the JSON field names handleUpstreamWatch emits.
func TestUpstreamWatchUIHasNoUndefinedCallees(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	for _, snippet := range []string{
		`id="upstream-watch-panel"`,
		`data-action="loadUpstreamWatch"`,
		`case 'loadUpstreamWatch': loadUpstreamWatch(); break;`,
		`if (tabId === 'Features') loadUpstreamWatch();`,
		`/api/upstream-watch`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}

	for _, fn := range []string{"loadUpstreamWatch", "esc"} {
		defined := regexp.MustCompile(`(?:function\s+` + regexp.QuoteMeta(fn) + `\s*\(|(?:const|let|var)\s+` + regexp.QuoteMeta(fn) + `\s*=)`)
		if !defined.MatchString(html) {
			t.Errorf("index.html calls %s() from the upstream-watch UI but never defines it", fn)
		}
	}

	// A silent rename on either side blanks the panel, so the field names are
	// pinned against the handler's JSON tags.
	for _, field := range []string{
		"data.configured", "data.enabled", "data.state_error", "data.repos",
		"repo.upstream", "repo.watermark", "repo.last_run_at", "repo.surfaced",
		"repo.ported", "repo.dismissed", "repo.skipped", "repo.recent",
		"it.upstream_ref", "it.upstream_url", "it.diff_url", "it.issue_number",
		"it.issue_url", "it.state", "it.recorded_at",
	} {
		if !strings.Contains(html, field) {
			t.Errorf("upstream-watch panel never reads %s", field)
		}
	}

	// The dashboard's no-native-dialogs rule: the panel renders into its own
	// container and must not reach for window.alert/prompt/confirm.
	start := strings.Index(html, "async function loadUpstreamWatch")
	end := strings.Index(html, "async function loadLinearAgentStatus")
	if start < 0 || end < start {
		t.Fatal("could not locate the loadUpstreamWatch body in index.html")
	}
	body := html[start:end]
	for _, banned := range []string{"alert(", "prompt(", "confirm("} {
		if strings.Contains(body, banned) {
			t.Errorf("loadUpstreamWatch uses %s — use the in-app modal/toast helpers", banned)
		}
	}
}
