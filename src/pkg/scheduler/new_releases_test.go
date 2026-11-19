package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// ${NEW_RELEASES} (#18494) renders the installed source's block for the
// kicked agent, renders empty with no source, and clears when the source is
// removed.
func TestSubstituteTemplate_NewReleases(t *testing.T) {
	s := New(&config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widgets"}}}, testLogger())
	tmpl := "before\n${NEW_RELEASES}after"

	got, _ := s.substituteTemplateWithPolicy(tmpl, &github.ActionableResult{}, "deps", nil)
	if got != "before\nafter" {
		t.Fatalf("no source: %q", got)
	}

	var asked string
	s.SetNewReleasesSource(func(agent string) string {
		asked = agent
		return "NEW UPSTREAM RELEASES:\n- requests: 2.32.0 -> 2.33.0\n"
	})
	got, _ = s.substituteTemplateWithPolicy(tmpl, &github.ActionableResult{}, "deps", nil)
	if asked != "deps" || !strings.Contains(got, "- requests: 2.32.0 -> 2.33.0") {
		t.Fatalf("with source: asked=%q got=%q", asked, got)
	}

	s.SetNewReleasesSource(nil)
	if got := s.newReleasesBlock("deps"); got != "" {
		t.Fatalf("removed source still renders %q", got)
	}
	var nilSched *Scheduler
	if got := nilSched.newReleasesBlock("deps"); got != "" {
		t.Fatalf("nil scheduler renders %q", got)
	}
}
