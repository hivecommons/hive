package scheduler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// utah-packages#879 as a scanner task: before, the query was the hive's own
// routing labels, its complexity tier and the review backlog stem
// ("stage/010-actionable agent/reviewer kind/cleanup approved-direction
// medium reviewer review backlog style …"), so review notes from other repos
// outranked anything about utah-packages. The query keeps what the issue is
// about.
func TestExtractKeywords_DropsHiveRouting(t *testing.T) {
	issue := github.Issue{
		Repo:           "projectbluefin/utah-packages",
		Number:         879,
		Title:          "[reviewer] Review backlog (style): spec file mixes tabs and spaces",
		ComplexityTier: "Medium",
		Labels: []string{"stage/010-actionable", "agent/reviewer", "kind/cleanup", "approved-direction",
			"hive/claimed-by-scanner", "needs-triage", "epic", "claimed", "author/human", "hive-pause/h1", "origin/governor"},
	}
	got := extractKeywords([]github.Issue{issue})
	want := []string{"kind/cleanup", "style", "spec", "file", "mixes", "tabs", "spaces"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keywords = %q\nwant       %q", got, want)
	}
}

// An issue whose every term is hive routing falls back to the query earlier
// releases built, so the filter never leaves a kick with no knowledge section
// it used to get.
func TestExtractKeywords_AllRoutingFallsBackToLegacyQuery(t *testing.T) {
	issue := github.Issue{
		Repo:   "projectbluefin/utah",
		Number: 12,
		Title:  "[ci-maintainer] Fix CI",
		Labels: []string{"agent/ci-maintainer", "stage/010-actionable", "kind/bug"},
	}
	got := extractKeywords([]github.Issue{issue})
	want := []string{"agent/ci-maintainer", "stage/010-actionable", "ci-maintainer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keywords = %q\nwant the legacy query %q", got, want)
	}
	if !reflect.DeepEqual(got, legacyKeywords([]github.Issue{issue})) {
		t.Fatal("the fallback is not the legacy query")
	}
}

func TestSearchableTitle(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = map[string]config.AgentConfig{"packager": {}}
	for title, want := range map[string]string{
		"[reviewer] Review backlog (security): token in log line": "security token in log line",
		"Review backlog (style): long line":                       "style long line",
		"[scanner] Sidebar anchors drift":                         "Sidebar anchors drift",
		"[selfreport] Agent pane froze":                           "Agent pane froze",
		"[packager] Bump the spec":                                "Bump the spec",
		"[nvidia] driver fails to build on F44":                   "[nvidia] driver fails to build on F44",
		"[ACMM L0] Add a CONTRIBUTING file":                       "[ACMM L0] Add a CONTRIBUTING file",
		"Review the backlog of stale PRs":                         "Review the backlog of stale PRs",
	} {
		if got := searchableTitle(cfg, title); got != want {
			t.Errorf("searchableTitle(%q) = %q, want %q", title, got, want)
		}
	}
	if got := searchableTitle(nil, "[packager] Bump the spec"); got != "[packager] Bump the spec" {
		t.Errorf("an unconfigured agent name was stripped as a filer: %q", got)
	}
}

func TestIsNoiseLabel_HiveRoutingFamilies(t *testing.T) {
	for _, l := range []string{"stage/010-actionable", "stage/stalled", "agent/reviewer", "hive/claimed-by-scanner", "hive: churn-triaged",
		"origin/governor", "triage/needs-information", "size/l", "needs-human", "do-not-merge/hold", "dco-signoff: yes",
		"sentinel-alert", "approved-direction", "hive-review", "lgtm", "claimed", "epic", "author/human", "hive-pause/h1"} {
		if !isNoiseLabel(l) {
			t.Errorf("%q is a hive routing label and should not be a search term", l)
		}
	}
	for _, l := range []string{"kind/cleanup", "priority/high", "area/packaging", "rpm", "stagehand", "agentic", "authoring", "epics-tracker"} {
		if isNoiseLabel(l) {
			t.Errorf("%q describes the work and should stay a search term", l)
		}
	}
}

// The task's repo ranks its own entries first within a type: of two entries
// that match the issue equally, the one tagged with the task's repo comes
// first, a code map tagged with the bare repo name counts, and the confidence
// the kick shows is not changed by the ranking. The repo admits nothing: an
// entry that shares no word with the issue stays out however it is tagged.
func TestKnowledgeKick_TaskRepoRanksItsEntriesFirst(t *testing.T) {
	for _, tc := range []struct{ name, taskTag string }{
		{"org/name tag", "projectbluefin/utah-packages"},
		{"bare code-map tag", "utah-packages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vault := t.TempDir()
			entry := func(name, repo string) {
				body := "---\ntitle: Packaging conventions\ntype: gotcha\nlayer: project\ntags: [repo:" + repo + ", packaging]\n---\n" +
					"Spec files use spaces for indentation, never tabs.\n"
				if err := os.WriteFile(filepath.Join(vault, name+".md"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			entry("other-repo", "projectbluefin/bluefin-lts")
			entry("task-repo", tc.taskTag)
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
			plain := primer.PrimeForKick(context.Background(), nil, knowledge.KickContext{}, extractKeywords([]github.Issue{task}))
			primed := primer.PrimeForKick(context.Background(), nil, kc, extractKeywords([]github.Issue{task}))
			if len(primed.Facts) != 2 || len(plain.Facts) != 2 {
				t.Fatalf("primed %d / %d facts, want both entries", len(primed.Facts), len(plain.Facts))
			}
			if primed.Facts[0].Slug != "task-repo" {
				t.Fatalf("ranking = %s, %s; want the task repo's entry first", primed.Facts[0].Slug, primed.Facts[1].Slug)
			}
			for _, f := range primed.Facts {
				for _, g := range plain.Facts {
					if f.Slug == g.Slug && f.Confidence != g.Confidence {
						t.Errorf("%s: confidence %.3f with the repo boost, %.3f without; the boost must not change it", f.Slug, f.Confidence, g.Confidence)
					}
				}
			}

			unrelated := primer.PrimeForKick(context.Background(), nil, kc, []string{"kernel", "panic"})
			if len(unrelated.Facts) != 0 {
				t.Fatalf("the repo tag alone admitted %d facts for an unrelated query", len(unrelated.Facts))
			}
		})
	}
}
