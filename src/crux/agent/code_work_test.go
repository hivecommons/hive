package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

func TestCodeWorkSuccessfulEditRequiresActualWorktreeChange(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		command := exec.Command("git", append([]string{"-C", dir}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet", "-b", "fixture")
	git("config", "user.name", "Activity Test")
	git("config", "user.email", "activity@example.invalid")
	git("remote", "add", "origin", "https://github.com/org/repo.git")
	file := filepath.Join(dir, "source.go")
	if err := os.WriteFile(file, []byte("package before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.go")
	git("commit", "--quiet", "-s", "-m", "fixture")
	head := git("rev-parse", "HEAD")
	now := time.Now().UTC()
	since := now.Add(-time.Minute)
	start := now.Add(-time.Second)
	if err := os.WriteFile(file, []byte("package after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, start, start); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": file})
	transcript := func(name, success string, arguments []byte) string {
		return `{"type":"tool.execution_start","timestamp":"` + start.Format(time.RFC3339Nano) +
			`","data":{"toolCallId":"edit-1","toolName":"` + name + `","arguments":` + string(arguments) + `}}` + "\n" +
			`{"type":"tool.execution_complete","timestamp":"` + now.Format(time.RFC3339Nano) +
			`","data":{"toolCallId":"edit-1","success":` + success + `}}` + "\n"
	}
	got := ObserveCodeWorkTranscript(strings.NewReader(transcript("edit", "true", args)), dir, since, now)
	if got == nil || got.Repo != "org/repo" || got.Head != head || got.Path != "source.go" || !got.KickAt.Equal(since) {
		t.Fatalf("verified edit not observed: %+v", got)
	}
	for _, name := range []string{"bash", "read", "claim", "review_request"} {
		if got := ObserveCodeWorkTranscript(strings.NewReader(transcript(name, "true", args)), dir, since, now); got != nil {
			t.Fatalf("%s invented source work: %+v", name, got)
		}
	}
	for _, success := range []string{"false", "null"} {
		if got := ObserveCodeWorkTranscript(strings.NewReader(transcript("edit", success, args)), dir, since, now); got != nil {
			t.Fatalf("unsuccessful/unknown result invented source work: %+v", got)
		}
	}
	relative, _ := json.Marshal(map[string]string{"path": "source.go"})
	if got := ObserveCodeWorkTranscript(strings.NewReader(transcript("edit", "true", relative)), dir, since, now); got != nil {
		t.Fatal("a guessed current directory attributed an edit")
	}
	git("checkout", "--", "source.go")
	if got := ObserveCodeWorkTranscript(strings.NewReader(transcript("edit", "true", args)), dir, since, now); got != nil {
		t.Fatal("a successful receipt without actual source changes invented work")
	}
}

func TestCodeWorkSnapshotsAndCurrentKickIsolation(t *testing.T) {
	now := time.Now().UTC()
	m := &Manager{agents: map[string]*AgentProcess{"fixer": {Name: "fixer", LastKick: &now}}}
	agent := m.agents["fixer"]
	observation := &crux.CodeWork{Repo: "org/repo", Head: "head", KickAt: now, At: now}
	m.applyTranscriptActivityResultsLocked([]transcriptActivityResult{{name: "fixer", since: now.Add(-time.Second), codeWork: observation}}, now)
	if agent.CodeWork != nil {
		t.Fatal("a previous kick's source observation was accepted")
	}
	m.applyTranscriptActivityResultsLocked([]transcriptActivityResult{{name: "fixer", since: now, codeWork: observation}}, now)
	snapshot := agent.snapshot()
	snapshot.CodeWork.Head = "changed-copy"
	if agent.CodeWork.Head != "head" {
		t.Fatal("snapshot code-work evidence aliased live runtime state")
	}
}
