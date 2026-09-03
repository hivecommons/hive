package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Agent logs are tmux pane scrapes, so explanation (#3887) cannot go to a real
// second channel — it is tagged inline with config.ExplainLinePrefix and split
// at read time. These tests pin that split, and pin that it never becomes a
// redaction bypass.

const explainSampleLog = `> Bash(git status --porcelain)
EXPLAIN: Checking for uncommitted work before branching, so I do not strand edits.
On branch v4
> Bash(go test ./pkg/agent/)
EXPLAIN: Running the package suite because my change touches kick composition.
ok  	github.com/hivecommons/hive/v2/pkg/agent	0.4s
`

func TestFilterExplainLines_Only(t *testing.T) {
	got := filterExplainLines(explainSampleLog, "only")

	if strings.Contains(got, "Bash(git status") {
		t.Error("explain-only view leaked ordinary tool output")
	}
	if strings.Contains(got, "On branch v4") {
		t.Error("explain-only view leaked command output")
	}
	for _, want := range []string{"Checking for uncommitted work", "Running the package suite"} {
		if !strings.Contains(got, want) {
			t.Errorf("explain-only view dropped reasoning line %q", want)
		}
	}
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), config.ExplainLinePrefix) {
			t.Errorf("explain-only view kept a non-explain line: %q", line)
		}
	}
}

func TestFilterExplainLines_Hide(t *testing.T) {
	got := filterExplainLines(explainSampleLog, "hide")

	if strings.Contains(got, config.ExplainLinePrefix) {
		t.Error("hide view kept an explain line")
	}
	// Hiding explanation must reproduce exactly what an operator would see with
	// explain mode off — nothing of the real work may be lost with it.
	for _, want := range []string{"Bash(git status --porcelain)", "On branch v4", "Bash(go test ./pkg/agent/)", "ok  	github.com/hivecommons/hive/v2/pkg/agent"} {
		if !strings.Contains(got, want) {
			t.Errorf("hide view dropped real output %q", want)
		}
	}
}

// The two views must partition the log: every line goes to exactly one of them,
// so an operator reading both sees the whole log and neither view invents text.
func TestFilterExplainLines_ViewsPartitionTheLog(t *testing.T) {
	only := strings.Split(strings.TrimRight(filterExplainLines(explainSampleLog, "only"), "\n"), "\n")
	hide := strings.Split(strings.TrimRight(filterExplainLines(explainSampleLog, "hide"), "\n"), "\n")
	all := strings.Split(strings.TrimRight(explainSampleLog, "\n"), "\n")

	if len(only)+len(hide) != len(all) {
		t.Errorf("only(%d) + hide(%d) != full log(%d); the views overlap or drop lines",
			len(only), len(hide), len(all))
	}
}

// Negative control: the default response must be byte-identical to the log as
// it was served before this filter existed, for every mode that is not an
// explicit view. Otherwise adding the filter silently changed the main path.
func TestFilterExplainLines_UnknownModeIsPassthrough(t *testing.T) {
	for _, mode := range []string{"", "1", "true", "ONLY", "yes", "hidden"} {
		if got := filterExplainLines(explainSampleLog, mode); got != explainSampleLog {
			t.Errorf("mode %q altered the log; want byte-identical passthrough", mode)
		}
	}
}

// The CLI indents its own output, so an EXPLAIN line does not necessarily start
// at column 0. Anchoring the match there would leak indented explanation into
// the "hide" view and lose it from the "only" view.
func TestFilterExplainLines_MatchesIndentedMarker(t *testing.T) {
	log := "work line\n    EXPLAIN: indented reasoning\n"

	only := filterExplainLines(log, "only")
	if !strings.Contains(only, "indented reasoning") {
		t.Error("indented explain line missing from the only view")
	}
	hide := filterExplainLines(log, "hide")
	if strings.Contains(hide, "indented reasoning") {
		t.Error("indented explain line leaked into the hide view")
	}
}

func TestFilterExplainLines_PreservesTrailingNewline(t *testing.T) {
	withNL := "a\nEXPLAIN: b\n"
	if got := filterExplainLines(withNL, "hide"); !strings.HasSuffix(got, "\n") {
		t.Errorf("trailing newline lost: %q", got)
	}
	withoutNL := "a\nEXPLAIN: b"
	if got := filterExplainLines(withoutNL, "hide"); strings.HasSuffix(got, "\n") {
		t.Errorf("trailing newline invented: %q", got)
	}
}

func TestFilterExplainLines_EmptyResultIsEmpty(t *testing.T) {
	if got := filterExplainLines("no reasoning here\n", "only"); got != "" {
		t.Errorf("only view of a log with no explanation = %q, want empty", got)
	}
}

// SECURITY: this exercises prepareFullLog — the real code path
// handleAgentFullLog uses — not a reconstruction of it, so it fails if any view
// ever gains a path that skips redactTokens.
//
// It asserts redaction on EVERY mode, which is the property that actually
// matters: an agent in explain mode narrates what it was doing, so a command
// containing a token is more likely to be quoted into an EXPLAIN line than to
// appear on the raw pane. The explain filter selects lines; it sanitizes
// nothing, and must never be relied on to.
func TestPrepareFullLog_FilterDoesNotBypassRedaction(t *testing.T) {
	raw := "EXPLAIN: retrying because the call failed with ANTHROPIC_API_KEY='sk-hive-scanner'\nok\n"

	for _, mode := range []string{"", "only", "hide"} {
		got := prepareFullLog(raw, mode)
		if strings.Contains(got, "sk-hive-scanner") {
			t.Errorf("mode %q: secret survived redaction: %q", mode, got)
		}
	}

	// Redaction must not eat the explanation around the secret, or the view is
	// technically safe and practically useless.
	if !strings.Contains(prepareFullLog(raw, "only"), "retrying because the call failed") {
		t.Error("redaction destroyed the surrounding explanation")
	}
}

// The default (no explain param) response must still be exactly "the redacted
// log", so adding the filter changed nothing for existing callers.
func TestPrepareFullLog_DefaultIsRedactionOnly(t *testing.T) {
	if got, want := prepareFullLog(explainSampleLog, ""), redactTokens(explainSampleLog); got != want {
		t.Errorf("default response diverged from redact-only:\n got %q\nwant %q", got, want)
	}
}
