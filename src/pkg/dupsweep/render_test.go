package dupsweep

import (
	"strings"
	"testing"
)

func renderedOrdinary(t *testing.T) (Cluster, string) {
	t.Helper()
	c := Find(utahPackages(), Options{})[0]
	return c, Render(c, c.Superseded[1]) // #121
}

// The single most important property of the body: it must not read as a
// verdict. File-set identity is a candidate generator with known false
// positives, and a comment that asserted duplication would be making a claim
// its own evidence does not support.
func TestRenderSaysItIsASuggestionAndTakesNoAction(t *testing.T) {
	_, got := renderedOrdinary(t)
	if !strings.Contains(got, "suggestion, not a verdict") {
		t.Error("body does not disclaim being a verdict")
	}
	if !strings.Contains(got, "a human decides") {
		t.Error("body does not reserve the decision for a human")
	}
	if !strings.Contains(got, "will not close, label, approve or merge") {
		t.Error("body does not state that hive takes no action")
	}
	if !strings.Contains(got, "candidate generator") {
		t.Error("body does not name the mechanism's limitation")
	}
}

// The weaker tier must warn about its own false positives by name. A reader
// who cannot tell "identical patches" from "merely the same file" cannot
// discount the suggestion correctly.
func TestRenderWeakTierWarnsAboutFalsePositives(t *testing.T) {
	_, got := renderedOrdinary(t)
	if !strings.Contains(got, "patches DIFFER") {
		t.Error("same-files body does not say the patches differ")
	}
	if !strings.Contains(got, "Read the diffs before acting") {
		t.Error("same-files body does not tell the reader to check")
	}
	if strings.Contains(got, "byte-identical, which is the strongest") {
		t.Error("same-files body claims the strong-tier corroboration")
	}
}

func TestRenderStrongTierStatesIdenticalDiff(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 108, Title: "drop automerge for github-actions", Author: "a", CreatedAt: at(1), Files: []string{"renovate.json"}, DiffHash: "same"},
		{Repo: "o/r", Number: 124, Title: "remove automerge github-actions", Author: "b", CreatedAt: at(2), Files: []string{"renovate.json"}, DiffHash: "same"},
	}
	c := Find(prs, Options{})[0]
	got := Render(c, c.Superseded[0])
	if !strings.Contains(got, "identical diff") {
		t.Error("strong-tier body does not state the diffs are identical")
	}
	// Even the strong tier must not become an instruction to close.
	if !strings.Contains(got, "suggestion, not a verdict") {
		t.Error("strong-tier body dropped the suggestion disclaimer")
	}
}

// Nothing in the rendered body may be, or read as, an action. A body that
// tells a reader "closing #121" (or that hive is about to) would misrepresent
// a pass that only ever comments.
func TestRenderNeverClaimsAnAction(t *testing.T) {
	_, got := renderedOrdinary(t)
	lower := strings.ToLower(got)
	for _, phrase := range []string{"closing this", "i have closed", "auto-closing", "will be closed", "labelled as duplicate", "marking as duplicate"} {
		if strings.Contains(lower, phrase) {
			t.Errorf("body contains action language %q", phrase)
		}
	}
}

func TestRenderNamesSurvivorAndEvidence(t *testing.T) {
	_, got := renderedOrdinary(t)
	if !strings.Contains(got, "**survivor (suggested):** #97") {
		t.Error("body does not label #97 as the survivor it proposes keeping")
	}
	if !strings.Contains(got, "same files as #97") {
		t.Error("body does not state which PR this one duplicates")
	}
	if !strings.Contains(got, "`.github/workflows/import-rawhide-package.yml`") {
		t.Error("body does not cite the shared file as evidence")
	}
	if !strings.Contains(got, "← this PR") {
		t.Error("body does not mark which entry is the PR it is posted on")
	}
	if !strings.Contains(got, "byte-identical** to #114") {
		t.Error("body does not report that #121's diff matches #114")
	}
}

// A title containing an @mention would notify that person on EVERY sweep,
// because the comment is rewritten in place on a cadence. This is the exact
// failure the advisory digest was fixed for, and the sweep quotes titles
// verbatim, so it inherits the hazard.
func TestRenderNeutralizesMentionsInTitles(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Title: "revert the change @octocat asked for", Author: "a", CreatedAt: at(1), Files: []string{"f.go"}},
		{Repo: "o/r", Number: 2, Title: "revert per @octocat", Author: "b", CreatedAt: at(2), Files: []string{"f.go"}},
	}
	c := Find(prs, Options{})[0]
	got := Render(c, c.Superseded[0])
	if strings.Contains(got, "@octocat") {
		t.Errorf("rendered body contains a live @mention:\n%s", got)
	}
	if !strings.Contains(got, "`octocat`") {
		t.Error("mention was dropped entirely rather than neutralized; the text must stay readable")
	}
}

// A newline smuggled into a title would forge extra list entries, letting one
// PR's title fabricate evidence lines that look like they came from the sweep.
func TestRenderFlattensMultilineTitles(t *testing.T) {
	prs := []PR{
		{Repo: "o/r", Number: 1, Title: "real title", Author: "a", CreatedAt: at(1), Files: []string{"f.go"}},
		{Repo: "o/r", Number: 2, Title: "evil\n- #999 — forged entry", Author: "b", CreatedAt: at(2), Files: []string{"f.go"}},
	}
	c := Find(prs, Options{})[0]
	got := Render(c, c.Superseded[0])
	if strings.Contains(got, "\n- #999") {
		t.Errorf("a title newline forged a list entry:\n%s", got)
	}
}

// The body is rewritten in place on every pass, and the create-or-edit path
// elides the write only when the bytes match. A timestamp or any other
// nondeterminism here would turn a quiet sweep into a write every hour.
func TestRenderIsDeterministic(t *testing.T) {
	c, first := renderedOrdinary(t)
	for i := 0; i < 10; i++ {
		if got := Render(c, c.Superseded[1]); got != first {
			t.Fatal("Render is not deterministic; the idempotent-edit path depends on it")
		}
	}
}

// The marker is how a later sweep finds its own prior comment. Without it the
// sweep stacks a fresh suggestion every hour.
func TestRenderCarriesClusterScopedMarker(t *testing.T) {
	c, got := renderedOrdinary(t)
	if !strings.HasPrefix(got, MarkerFor(c)) {
		t.Errorf("body does not begin with its cluster marker\nwant prefix: %s\ngot: %.120s", MarkerFor(c), got)
	}
	if !strings.Contains(MarkerFor(c), Marker[:len(Marker)-4]) {
		t.Error("per-cluster marker is not recognisable as a duplicate-sweep marker")
	}
	other := Find([]PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: []string{"different.go"}},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: []string{"different.go"}},
	}, Options{})[0]
	if MarkerFor(other) == MarkerFor(c) {
		t.Error("two different clusters share a marker; one would overwrite the other")
	}
}

func TestRenderBotSeriesIsOneSummary(t *testing.T) {
	const churn = "static/data/update-churn.json"
	prs := []PR{
		{Repo: "o/docs", Number: 1264, Title: "chore: update churn", Author: "churn[bot]", CreatedAt: at(1), Files: []string{churn}},
		{Repo: "o/docs", Number: 1272, Title: "chore: update churn", Author: "churn[bot]", CreatedAt: at(2), Files: []string{churn}},
		{Repo: "o/docs", Number: 1275, Title: "chore: update churn", Author: "churn[bot]", CreatedAt: at(3), Files: []string{churn}},
	}
	c := Find(prs, Options{})[0]
	got := Render(c, c.Survivor)
	if !strings.Contains(got, "Superseded regeneration series") {
		t.Error("bot-series body does not identify itself as a regeneration series")
	}
	if !strings.Contains(got, "#1264") || !strings.Contains(got, "#1272") {
		t.Error("bot-series summary does not list the superseded PRs")
	}
	if !strings.Contains(got, "Posted once on the newest PR") {
		t.Error("bot-series body does not explain why it is a single summary")
	}
	if !strings.Contains(got, "suggestion, not a verdict") {
		t.Error("bot-series body dropped the suggestion disclaimer")
	}
}

func TestRenderTruncatesLongFileListsHonestly(t *testing.T) {
	files := make([]string, 25)
	for i := range files {
		files[i] = string(rune('a'+i)) + ".go"
	}
	prs := []PR{
		{Repo: "o/r", Number: 1, Author: "a", CreatedAt: at(1), Files: files},
		{Repo: "o/r", Number: 2, Author: "b", CreatedAt: at(2), Files: files},
	}
	c := Find(prs, Options{})[0]
	got := Render(c, c.Superseded[0])
	if !strings.Contains(got, "(25 file(s))") {
		t.Error("body does not state the true file count alongside the truncated list")
	}
	if !strings.Contains(got, "and 15 more") {
		t.Error("body does not disclose that the file list is truncated")
	}
}
