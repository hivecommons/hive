package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/scheduler"
)

func TestKnowledgeAdviserLLM_RecordsDecisionPerArm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "knowledge-advisor-decisions.jsonl")
	store := retro.NewAdvisorDecisionStore(path)
	const reason = "SECRET REASON TEXT"
	for _, tc := range []struct {
		name  string
		a     config.AdvisorLLMConfig
		issue string
	}{
		{"rank", config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank}, "o/r#1"},
		{"shadow", config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeShadow}, "o/r#2"},
		{"holdout", config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, HoldoutPercent: 100}, "o/r#3"},
	} {
		f := newLLMFixture(t, tc.a)
		f.d.decisions = store
		f.setKick("developer", scheduler.KickSummary{ID: "developer#" + tc.name, Text: tc.issue + " title", Refs: []string{tc.issue}})
		f.setReply(`{"slug": "second", "reason": "` + reason + `"}`)
		f.d.Advise("developer", llmTestScope)
		if n := len(f.decisions(t)); n != 1 {
			t.Fatalf("%s: logged %d decisions, want 1", tc.name, n)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		issue, mode, outcome string
	}{
		{"o/r#1", "rank", "delivered"},
		{"o/r#2", "shadow", "delivered"},
		{"o/r#3", "rank", "held-out"},
	} {
		got := store.AdvisorDecisions(tc.issue)
		if len(got) != 1 || got[0].Mode != tc.mode || got[0].Outcome != tc.outcome {
			t.Errorf("%s decisions = %+v, want one %s/%s", tc.issue, got, tc.mode, tc.outcome)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stored := string(data)
	if n := strings.Count(strings.TrimSpace(stored), "\n") + 1; n != 3 {
		t.Fatalf("stored %d records, want 3:\n%s", n, stored)
	}
	for _, want := range []string{`"arm":"deliver"`, `"arm":"shadow"`, `"arm":"holdout"`, `"slug":"second"`, `"slug":"first"`} {
		if !strings.Contains(stored, want) {
			t.Errorf("store missing %s:\n%s", want, stored)
		}
	}
	for _, leak := range []string{reason, `"reason"`, llmTestScope.PaneTail, "Edit(", "REDACTED"} {
		if strings.Contains(stored, leak) {
			t.Errorf("store contains %q:\n%s", leak, stored)
		}
	}
}
