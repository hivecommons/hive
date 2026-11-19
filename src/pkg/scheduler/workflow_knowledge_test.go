package scheduler

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func TestPrimeWorkflowDispatchReviewerRules(t *testing.T) {
	dir := t.TempDir()
	// Exercise the shipped criteria, not a second copy of their frontmatter.
	for _, name := range []string{"techdocs-assessment-evidence.md", "techdocs-assessment-ownership.md"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", "knowledge", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, state, lane, body string) {
		t.Helper()
		text := "---\ntitle: cncf-techdocs-assessment " + name + "\ntype: gotcha\nlayer: project\nstate: " + state + "\nsource: https://github.com/cncf/techdocs/pull/424#discussion_r4617524979\nscope:\n  lanes: [" + lane + "]\n---\n" + body
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A subsequently captured rule uses the same scope as the seeded criteria.
	write("captured", "approved", "cncf-techdocs-assessment", "Captured reviewer rule sentinel")
	write("draft", "draft", "cncf-techdocs-assessment", "Unapproved reviewer rule sentinel")
	write("elsewhere", "approved", "other-workflow", "Other workflow rule sentinel")
	write("base", "approved", "quality", "Base quality rule sentinel")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := knowledge.NewFileStore(dir, "reviewer-rules", logger)
	if err != nil {
		t.Fatal(err)
	}
	newSched := func(maxBytes int) *Scheduler {
		s := newScheduler()
		p := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25, MaxBytes: maxBytes}, logger)
		p.AddFileStore("reviewer-rules", store, knowledge.LayerProject)
		s.SetPrimer(p)
		return s
	}
	d := github.WorkflowDispatch{Repo: "cncf/techdocs", Number: 395, Title: "Review Acme", Model: "cncf-techdocs-assessment@curated-2026.11.2"}
	msg := KickMessage{Agent: "quality", Message: "Original workflow instructions", IssueRefs: []string{d.Ref()}}
	s := newSched(16384)
	got := s.PrimeWorkflowDispatch(msg, d)
	for _, want := range []string{"# Relevant Knowledge", "requested documentation domain", "authoritative roster", "Captured reviewer rule sentinel", "Base quality rule sentinel"} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("workflow kick missing %q: %s", want, got.Message)
		}
	}
	for _, unwanted := range []string{"Unapproved reviewer rule sentinel", "Other workflow rule sentinel"} {
		if strings.Contains(got.Message, unwanted) {
			t.Errorf("workflow kick leaked %q", unwanted)
		}
	}
	if !strings.HasSuffix(got.Message, msg.Message) || got.Agent != msg.Agent || strings.Join(got.IssueRefs, ",") != d.Ref() {
		t.Fatalf("priming changed dispatch instructions or identity: %+v", got)
	}
	// Criteria follow the workflow even when a different agent fills its profile.
	otherAgent := msg
	otherAgent.Agent = "scanner"
	if got := s.PrimeWorkflowDispatch(otherAgent, d); !strings.Contains(got.Message, "Captured reviewer rule sentinel") {
		t.Fatal("changing the profile's agent lost reviewer rules")
	}
	// A different version of the same model uses the same criteria.
	d.Model = "cncf-techdocs-assessment@future"
	if got := s.PrimeWorkflowDispatch(msg, d); !strings.Contains(got.Message, "Captured reviewer rule sentinel") {
		t.Fatal("model version change lost reviewer rules")
	}
	d.Model = "other-workflow@1"
	if got := s.PrimeWorkflowDispatch(msg, d); strings.Contains(got.Message, "Captured reviewer rule sentinel") {
		t.Fatal("TechDocs rule leaked to another workflow on the same quality agent")
	}
	// Generic quality work must not inherit assessment-only criteria either.
	if got := s.primeKnowledge("quality", []github.Issue{{Title: "cncf-techdocs-assessment"}}); strings.Contains(got, "Captured reviewer rule sentinel") {
		t.Fatal("TechDocs rule leaked to a generic quality kick")
	}
	d.Model = "cncf-techdocs-assessment@curated-2026.11.2"
	// Agent-level repository filters and the rendering cap still apply.
	s.cfg.Knowledge.AgentScopes = map[string]config.KnowledgeAgentScope{"quality": {Repos: []string{"unrelated/repo"}}}
	if got := s.PrimeWorkflowDispatch(msg, d); strings.Contains(got.Message, "authoritative roster") {
		t.Fatal("workflow priming bypassed agent repository scope")
	}
	capped := newSched(512).PrimeWorkflowDispatch(msg, d)
	if len(capped.Message)-len(msg.Message) > 514 {
		t.Fatal("workflow knowledge exceeded its byte cap")
	}
	if got := newScheduler().PrimeWorkflowDispatch(msg, d); got.Message != msg.Message {
		t.Fatal("disabled primer changed the workflow kick")
	}
}
