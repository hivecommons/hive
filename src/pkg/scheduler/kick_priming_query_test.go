package scheduler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// utah-packages#879 as a scanner task: before, the query was the hive's own
// routing labels plus the review backlog stem ("stage/010-actionable
// agent/reviewer kind/cleanup approved-direction reviewer review backlog
// style …"), so review notes from other repos outranked anything about
// utah-packages. The query keeps what the issue is about.
func TestExtractKeywords_DropsHiveRoutingAndNamesTheRepo(t *testing.T) {
	issue := github.Issue{
		Repo:   "projectbluefin/utah-packages",
		Number: 879,
		Title:  "[reviewer] Review backlog (style): spec file mixes tabs and spaces",
		Labels: []string{"stage/010-actionable", "agent/reviewer", "kind/cleanup", "approved-direction", "hive/claimed-by-scanner", "needs-triage"},
	}
	got := extractKeywords([]github.Issue{issue})
	want := []string{"kind/cleanup", "style", "spec", "file", "mixes", "tabs", "spaces"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keywords = %q\nwant       %q", got, want)
	}
}

func TestSearchableTitle(t *testing.T) {
	for title, want := range map[string]string{
		"[reviewer] Review backlog (security): token in log line": "security token in log line",
		"Review backlog (style): long line":                       "style long line",
		"[scanner] Sidebar anchors drift":                         "Sidebar anchors drift",
		"[ACMM L0] Add a CONTRIBUTING file":                       "[ACMM L0] Add a CONTRIBUTING file",
		"Review the backlog of stale PRs":                         "Review the backlog of stale PRs",
	} {
		if got := searchableTitle(title); got != want {
			t.Errorf("searchableTitle(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestIsNoiseLabel_HiveRoutingFamilies(t *testing.T) {
	for _, l := range []string{"stage/010-actionable", "agent/reviewer", "hive/claimed-by-scanner", "triage/needs-information",
		"size/l", "needs-human", "do-not-merge/hold", "dco-signoff: yes", "sentinel-alert", "approved-direction", "hive-review", "lgtm"} {
		if !isNoiseLabel(l) {
			t.Errorf("%q is a hive routing label and should not be a search term", l)
		}
	}
	for _, l := range []string{"kind/cleanup", "priority/high", "area/packaging", "rpm", "stagehand", "agentic"} {
		if isNoiseLabel(l) {
			t.Errorf("%q describes the work and should stay a search term", l)
		}
	}
}

// The task's repo ranks its own entries first: every entry carries a
// repo:<org>/<name> tag, so of two entries that match the issue equally, the
// one tagged with the task's repo comes first. The repo admits nothing: an
// entry that shares no word with the issue stays out however it is tagged.
func TestKnowledgeKick_TaskRepoRanksItsEntriesFirst(t *testing.T) {
	vault := t.TempDir()
	entry := func(name, title, repo string) {
		body := "---\ntitle: " + title + "\ntype: gotcha\nlayer: project\ntags: [repo:" + repo + ", packaging]\n---\n" +
			"Spec files use spaces for indentation, never tabs.\n"
		if err := os.WriteFile(filepath.Join(vault, name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry("other-repo", "Packaging conventions", "projectbluefin/bluefin-lts")
	entry("task-repo", "Packaging conventions", "projectbluefin/utah-packages")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := knowledge.NewFileStore(vault, "vault", logger)
	if err != nil {
		t.Fatal(err)
	}
	primer := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25}, logger)
	primer.AddFileStore("vault", store, knowledge.LayerProject)

	task := github.Issue{Repo: "projectbluefin/utah-packages", Number: 879, Title: "Spec file mixes tabs and spaces"}
	kc := kickContextFor(guidanceScheduler(t, "").cfg, "scanner", []github.Issue{task})
	if !reflect.DeepEqual(kc.Repos, []string{"projectbluefin/utah-packages"}) {
		t.Fatalf("kick context repos = %q", kc.Repos)
	}
	primed := primer.PrimeForKick(context.Background(), nil, kc, extractKeywords([]github.Issue{task}))
	if len(primed.Facts) != 2 {
		t.Fatalf("primed %d facts, want both entries", len(primed.Facts))
	}
	if primed.Facts[0].Slug != "task-repo" || primed.Facts[0].Confidence <= primed.Facts[1].Confidence {
		t.Fatalf("ranking = %s (%.3f), %s (%.3f); want the task repo's entry strictly first",
			primed.Facts[0].Slug, primed.Facts[0].Confidence, primed.Facts[1].Slug, primed.Facts[1].Confidence)
	}

	unrelated := primer.PrimeForKick(context.Background(), nil, kc, []string{"kernel", "panic"})
	if len(unrelated.Facts) != 0 {
		t.Fatalf("the repo tag alone admitted %d facts for an unrelated query", len(unrelated.Facts))
	}
}
