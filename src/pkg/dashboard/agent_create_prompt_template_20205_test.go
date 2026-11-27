package dashboard

// hivecommons/hive#20205: the Create Agent dialog sent the edited prompt TEXT
// as kick_template, a bare file name everywhere else. The agent ran the
// built-in prompt and its overlay failed the next config load. The text now
// rides in prompt_template, is written to <name>.md, and kick_template names
// that file; a kick_template that is not a bare name is refused with a 400.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestHandleAgentCreate_PromptTemplateWrittenToFile(t *testing.T) {
	const agentName = "sunshine-reviewer"
	const prompt = "# Review one pull request\n\nReview the pull request (PR) in this assignment at a/b.\n"

	saveDir := t.TempDir()
	pinPromptTemplateDirs(t, saveDir, t.TempDir())

	s, deps := apiServer(t)
	agentsDir := t.TempDir()
	deps.Config.Data.AgentsDir = agentsDir

	rec := doPost(s, "/api/agents", map[string]any{
		"name": agentName,
		"agent": map[string]any{
			"backend":         "copilot",
			"enabled":         false,
			"prompt_template": prompt,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	got, err := os.ReadFile(filepath.Join(saveDir, agentName+".md"))
	if err != nil {
		t.Fatalf("prompt template file not written: %v", err)
	}
	if string(got) != prompt {
		t.Errorf("prompt template file = %q, want %q", got, prompt)
	}
	if kt := deps.Config.Agents[agentName].KickTemplate; kt != agentName+".md" {
		t.Errorf("live kick_template = %q, want %q", kt, agentName+".md")
	}
	overlays, err := config.LoadAgentOverrides(agentsDir)
	if err != nil {
		t.Fatalf("saved overlay no longer loads: %v", err)
	}
	if kt := overlays[agentName].KickTemplate; kt != agentName+".md" {
		t.Errorf("saved kick_template = %q, want %q", kt, agentName+".md")
	}
}

func TestHandleAgentCreate_RejectsPromptTextInKickTemplate(t *testing.T) {
	s, deps := apiServer(t)
	agentsDir := t.TempDir()
	deps.Config.Data.AgentsDir = agentsDir

	rec := doPost(s, "/api/agents", map[string]any{
		"name": "bad-template",
		"agent": map[string]any{
			"backend":       "copilot",
			"enabled":       false,
			"kick_template": "# Review one pull request\n\nSee a/b for details.\n",
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "prompt_template") {
		t.Errorf("error should point at prompt_template: %s", rec.Body.String())
	}
	if _, ok := deps.Config.Agents["bad-template"]; ok {
		t.Error("agent was created despite the invalid kick_template")
	}
	if entries, _ := os.ReadDir(agentsDir); len(entries) != 0 {
		t.Errorf("agents dir has %d entries; no agent file may be written for an invalid kick_template", len(entries))
	}
}
