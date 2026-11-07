package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// TestPrimeKnowledge_DraftsReachTheirAuthor proves the kick-path draft rule
// (#17076): a draft is primed only when the agent's agent_scopes entry lists
// draft in include_states and the draft's author is the agent, its base agent
// or an owner login; it is rendered with a "(draft)" marker; approved-only
// stays the default for every other agent.
func TestPrimeKnowledge_DraftsReachTheirAuthor(t *testing.T) {
	dir := t.TempDir()
	write := func(name, title, state, author string) {
		t.Helper()
		content := "---\ntitle: " + title + "\ntype: gotcha\nlayer: project\nstate: " + state + "\n"
		if author != "" {
			content += "author: " + author + "\n"
		}
		content += "---\nRelevant to the webhook retry path.\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("approved.md", "Webhook retry approved fact", "approved", "")
	write("dev-draft.md", "Webhook retry developer draft", "draft", "developer")
	write("owner-draft.md", "Webhook retry owner draft", "draft", "alice")
	write("other-draft.md", "Webhook retry scanner draft", "draft", "scanner")
	write("anon-draft.md", "Webhook retry anonymous draft", "draft", "")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := knowledge.NewFileStore(dir, "vault", logger)
	if err != nil {
		t.Fatal(err)
	}
	primer := knowledge.NewPrimer(nil, knowledge.PrimerConfig{MaxFacts: 25}, logger)
	primer.AddFileStore("vault", store, knowledge.LayerProject)

	s := newScheduler()
	s.cfg.Dashboard.AuthorizedUsers = []string{"github:alice", "bob:viewer"}
	s.cfg.Agents = map[string]config.AgentConfig{"developer-2": {ReplicaOf: "developer"}}
	s.cfg.Knowledge.AgentScopes = map[string]config.KnowledgeAgentScope{
		"developer": {IncludeStates: []string{"draft"}},
		"architect": {IncludeStates: []string{"all"}},
		"quality":   {Layers: []string{"project"}},
		"reviewer":  {IncludeStates: []string{"deprecated"}},
	}
	s.SetPrimer(primer)
	issues := []github.Issue{makeIssue("org/repo", 1, "Webhook retry fails", "", 5, []string{"webhook", "retry"}, false)}

	const (
		approved   = "Webhook retry approved fact"
		devDraft   = "Webhook retry developer draft"
		ownerDraft = "Webhook retry owner draft"
		otherDraft = "Webhook retry scanner draft"
		anonDraft  = "Webhook retry anonymous draft"
	)
	tests := []struct {
		name  string
		agent string
		want  []string
		deny  []string
	}{
		{"unscoped agent: approved only", "outreach", []string{approved}, []string{devDraft, ownerDraft, otherDraft, anonDraft}},
		{"scope without include_states: approved only", "quality", []string{approved}, []string{devDraft, ownerDraft, otherDraft, anonDraft}},
		{"include_states without draft: approved only", "reviewer", []string{approved}, []string{devDraft, ownerDraft, otherDraft, anonDraft}},
		{"draft opt-in: own and owner drafts, not others", "developer", []string{approved, devDraft, ownerDraft}, []string{otherDraft, anonDraft}},
		{"replica inherits base agent identity", "developer-2", []string{approved, devDraft, ownerDraft}, []string{otherDraft, anonDraft}},
		{"all opt-in without a matching author: owner drafts only", "architect", []string{approved, ownerDraft}, []string{devDraft, otherDraft, anonDraft}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.primeKnowledge(tc.agent, issues)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("%s: missing %q in:\n%s", tc.agent, w, got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("%s: unexpected %q in:\n%s", tc.agent, d, got)
				}
			}
			drafts := strings.Count(got, "(draft)")
			wantDrafts := 0
			for _, w := range tc.want {
				if w != approved {
					wantDrafts++
				}
			}
			if drafts != wantDrafts {
				t.Errorf("%s: %d \"(draft)\" markers, want %d in:\n%s", tc.agent, drafts, wantDrafts, got)
			}
			if strings.Contains(got, "**"+approved+"** (draft)") {
				t.Errorf("%s: approved fact marked as draft", tc.agent)
			}
		})
	}
}

func TestKickDraftAuthors(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = map[string]config.AgentConfig{"scanner-3": {ReplicaOf: "scanner"}}
	cfg.Dashboard.AuthorizedUsers = []string{"github:alice", "carol:owner", "bob:viewer"}

	tests := []struct {
		agent string
		want  []string
	}{
		{"scanner", []string{"scanner", "alice", "carol"}},
		{"scanner-3", []string{"scanner-3", "scanner", "alice", "carol"}},
		{"", []string{"alice", "carol"}},
	}
	for _, tc := range tests {
		got := kickDraftAuthors(cfg, tc.agent)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("kickDraftAuthors(%q) = %v, want %v", tc.agent, got, tc.want)
		}
	}
	if got := kickDraftAuthors(nil, "dev"); len(got) != 1 || got[0] != "dev" {
		t.Errorf("nil config: got %v, want [dev]", got)
	}
}
