package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/redmain"
)

// The repo card carries "main red since <time> on <check>; fix: #N waiting
// on <who>" while the red-main breaker is tripped, and nothing once green.
func TestRepoCardRedMainLine(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widget", "green"}}}
	fact := &redmain.Fact{Repo: "acme/widget", Check: "unit", Since: time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC),
		FixPRs: []redmain.FixPR{{Number: 77, WaitingOn: "@alice"}}}
	a := &github.ActionableResult{RedMain: map[string]*redmain.Fact{"acme/widget": fact}}

	repos := buildRepos(cfg, a, governor.State{})
	if len(repos) != 2 {
		t.Fatalf("got %d cards", len(repos))
	}
	if want := "main red since 2026-10-22 08:00 UTC on unit; fix: #77 waiting on @alice"; repos[0].RedMainLine != want || repos[0].RedMain != fact {
		t.Errorf("widget card = %q, want %q", repos[0].RedMainLine, want)
	}
	if repos[1].RedMainLine != "" || repos[1].RedMain != nil {
		t.Errorf("green repo card carries red-main: %+v", repos[1].RedMain)
	}
	if repos := buildRepos(cfg, &github.ActionableResult{}, governor.State{}); repos[0].RedMainLine != "" {
		t.Errorf("green main still on the card: %q", repos[0].RedMainLine)
	}
}

func TestRepoCardRedMainMarkup(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{"r.redMainLine", `class="repo-red-main"`, "${redMainHtml}"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
