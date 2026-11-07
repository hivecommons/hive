package main

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestSelectKnowledgeAdvice(t *testing.T) {
	facts := []knowledge.Fact{
		{Slug: "primed", Title: "Already in the kick", Body: "x"},
		{Slug: "", Title: "No slug", Body: "x"},
		{Slug: "untitled", Title: "  ", Body: "x"},
		{Slug: "tree", Title: "Re-pin the tree", Body: "line one\n\n  line  two " + strings.Repeat("w", 400),
			Sources: []knowledge.Source{{PR: "1"}, {DocURL: "https://wiki/tree"}}},
		{Slug: "tree", Title: "Duplicate slug", Body: "x"},
		{Slug: "third", Title: "Third", Body: "short"},
		{Slug: "fourth", Title: "Fourth", Body: "never reached"},
	}
	got := selectKnowledgeAdvice(facts, []string{"primed"}, 2)
	if len(got) != 2 {
		t.Fatalf("got %d advice: %+v", len(got), got)
	}
	if got[0].Slug != "tree" || got[0].Title != "Re-pin the tree" || got[0].Ref != "https://wiki/tree" {
		t.Errorf("first = %+v", got[0])
	}
	if !strings.HasPrefix(got[0].Summary, "line one line two w") || !strings.HasSuffix(got[0].Summary, "…") {
		t.Errorf("summary = %q", got[0].Summary)
	}
	if n := len([]rune(got[0].Summary)); n > knowledgeAdviseSummaryRunes {
		t.Errorf("summary is %d runes, cap %d", n, knowledgeAdviseSummaryRunes)
	}
	if got[1].Slug != "third" || got[1].Ref != "knowledge entry third" || got[1].Summary != "short" {
		t.Errorf("second = %+v", got[1])
	}
	if selectKnowledgeAdvice(nil, nil, 2) != nil {
		t.Error("no facts should yield nil")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("héllo", 5); got != "héllo" {
		t.Errorf("under cap = %q", got)
	}
	if got := truncateRunes("héllo wörld", 6); got != "héllo…" {
		t.Errorf("over cap = %q", got)
	}
}

func TestKnowledgeAdviserPolicy(t *testing.T) {
	cfg := &config.Config{Knowledge: config.KnowledgeConfig{Enabled: true, Advisor: true, AdvisorMaxPerKick: 4}}
	a := &knowledgeAdviser{cfg: func() *config.Config { return cfg }, logger: slog.Default()}
	want := crux.KnowledgeAdvisorPolicy{Enabled: true, MaxPerKick: 4}
	if got := a.Policy("developer"); !reflect.DeepEqual(got, want) {
		t.Errorf("policy = %+v, want %+v", got, want)
	}
	cfg.Knowledge.Advisor = false
	if got := a.Policy("developer"); got.Enabled {
		t.Errorf("disabled policy = %+v", got)
	}
	a.cfg = func() *config.Config { return nil }
	if got := a.Policy("developer"); got.Enabled {
		t.Errorf("nil config policy = %+v", got)
	}
}

func TestKnowledgeAdviserAdvise_NoPrimerIsSilent(t *testing.T) {
	cfg := &config.Config{Knowledge: config.KnowledgeConfig{Enabled: true, Advisor: true}}
	a := &knowledgeAdviser{
		cfg:    func() *config.Config { return cfg },
		primer: func() *knowledge.Primer { return nil },
		primed: func(string) []string { return nil },
		logger: slog.Default(),
	}
	if got := a.Advise("developer", crux.KnowledgeScope{Key: "src/a", Path: "src/a/x.go"}); got != nil {
		t.Errorf("advice without a primer = %+v", got)
	}
}
