package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
)

func TestCodeWorkSuccessfulEditRequiresActualWorktreeChange(t *testing.T) {
	dir, file, head, start, now := codeWorkTestWorktree(t)
	since := now.Add(-time.Minute)
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
	codeWorkTestGit(t, dir, "checkout", "--", "source.go")
	if err := os.Chtimes(file, start, start); err != nil {
		t.Fatal(err)
	}
	if got := ObserveCodeWorkTranscript(strings.NewReader(transcript("edit", "true", args)), dir, since, now); got != nil {
		t.Fatal("a successful receipt without actual source changes invented work")
	}
}

func TestCodeWorkToolPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "source.go")
	added := filepath.Join(dir, "added.go")
	patch := "*** Begin Patch\n*** Update File: " + file + "\n*** Add File: " + added +
		"\n*** Update File: relative.go\n*** Delete File: " + file + "\n*** End Patch"
	args := func(value any) json.RawMessage {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, tc := range []struct {
		name string
		tool string
		args json.RawMessage
		want []string
	}{
		{"edit path", "edit", args(map[string]string{"path": file}), []string{file}},
		{"edit file_path", "Edit", args(map[string]string{"file_path": file}), []string{file}},
		{"create path", "create", args(map[string]string{"path": file}), []string{file}},
		{"write file_path", "Write", args(map[string]string{"file_path": file}), []string{file}},
		{"malformed edit", "edit", json.RawMessage("{"), nil},
		{"relative edit", "edit", args(map[string]string{"path": "source.go"}), nil},
		{"patch string", "apply_patch", args(patch), []string{file, added}},
		{"patch input", "apply_patch", args(map[string]string{"input": patch}), []string{file, added}},
		{"patch field", "apply_patch", args(map[string]string{"patch": patch}), []string{file, added}},
		{"malformed patch", "apply_patch", json.RawMessage("{"), nil},
		{"shell is not edit proof", "bash", args(map[string]string{"path": file}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceEditPaths(tc.tool, tc.args); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("source paths=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestCodeWorkTranscriptRejectsInvalidReceipts(t *testing.T) {
	dir, file, head, start, now := codeWorkTestWorktree(t)
	since := now.Add(-2 * codeWorkFreshWindow)
	args := map[string]string{"path": file}
	request := func(at time.Time, id string) string {
		return codeWorkTestEvent(t, "tool.execution_start", at, map[string]any{
			"toolCallId": id, "toolName": "edit", "arguments": args,
		})
	}
	result := func(at time.Time, receipt map[string]any) string {
		return codeWorkTestEvent(t, "tool.execution_complete", at, receipt)
	}
	receipt := map[string]any{"toolCallId": "edit-1", "success": true}
	complete := result(now, receipt)
	assistant := codeWorkTestEvent(t, "assistant.message", start, map[string]any{
		"toolRequests": []map[string]any{
			{"toolCallId": "", "name": "edit", "arguments": args},
			{"toolCallId": "edit-1", "name": "edit", "arguments": args},
		},
	})
	stale := now.Add(-codeWorkFreshWindow - time.Second)
	for _, tc := range []struct {
		name       string
		transcript string
		since      time.Time
		want       bool
	}{
		{"assistant request", assistant + complete, since, true},
		{"unknown kick", assistant + complete, time.Time{}, false},
		{"missing request id", request(start, "") + complete, since, false},
		{"missing result id", request(start, "edit-1") + result(now, map[string]any{"success": true}), since, false},
		{"error despite success", request(start, "edit-1") + result(now, map[string]any{"toolCallId": "edit-1", "success": true, "error": "edit denied"}), since, false},
		{"missing timestamp", request(start, "edit-1") + result(time.Time{}, receipt), since, false},
		{"before kick", request(since.Add(-time.Second), "edit-1") + result(since.Add(-time.Second), receipt), since, false},
		{"future result", request(start, "edit-1") + result(now.Add(time.Second), receipt), since, false},
		{"stale receipt", request(stale.Add(-time.Second), "edit-1") + result(stale, receipt), since, false},
		{"corrupt record", "{\n" + assistant + complete, since, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ObserveCodeWorkTranscript(strings.NewReader(tc.transcript), dir, tc.since, now)
			if (got != nil) != tc.want {
				t.Fatalf("observed=%+v want source work=%v", got, tc.want)
			}
			if got != nil && (got.Repo != "org/repo" || got.Head != head || got.Path != "source.go" ||
				got.Source != crux.CodeWorkSource || !got.KickAt.Equal(since) || !got.At.Equal(now) || !got.ObservedAt.Equal(now)) {
				t.Fatalf("receipt provenance lost: %+v", got)
			}
		})
	}
}

func TestScanCodeWorkUsesOnlyUnambiguousCurrentSession(t *testing.T) {
	dir, file, head, start, now := codeWorkTestWorktree(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	since := now.Add(-2 * codeWorkFreshWindow)
	target := kickTranscriptTarget{agentName: "fixer", backend: "copilot", workDir: dir, since: since}
	path := filepath.Join(home, ".copilot", "session-state", "current", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	transcript := codeWorkTestEvent(t, "tool.execution_start", start, map[string]any{
		"toolCallId": "edit-1", "toolName": "edit", "arguments": map[string]string{"path": file},
	}) + codeWorkTestEvent(t, "tool.execution_complete", now, map[string]any{"toolCallId": "edit-1", "success": true})
	if err := os.WriteFile(path, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	cache := &transcriptSessionCache{sessions: map[string]time.Time{
		"current": now,
		"older":   start,
		"before":  since.Add(-time.Second),
		"stale":   now.Add(-codeWorkFreshWindow - time.Second),
		"future":  now.Add(time.Second),
	}}
	got := scanCodeWork(target, cache, now)
	if got == nil || got.Repo != "org/repo" || got.Head != head || !got.KickAt.Equal(since) ||
		got.Path != "source.go" || got.Source != crux.CodeWorkSource || !got.At.Equal(now) || !got.ObservedAt.Equal(now) {
		t.Fatalf("current cached session lost verified source work: %+v", got)
	}
	cache.sessions["tied"] = now
	if got := scanCodeWork(target, cache, now); got != nil {
		t.Fatalf("tied sessions invented an owner: %+v", got)
	}
	cache.sessions = map[string]time.Time{"missing": now}
	if got := scanCodeWork(target, cache, now); got != nil {
		t.Fatalf("missing transcript invented source work: %+v", got)
	}
	cache.sessions = nil
	if got := scanCodeWork(target, cache, now); got != nil {
		t.Fatalf("empty session cache invented source work: %+v", got)
	}
	cache.sessions = map[string]time.Time{"current": now}
	if err := os.Truncate(path, codeWorkTranscriptLimit+1); err != nil {
		t.Fatal(err)
	}
	if got := scanCodeWork(target, cache, now); got != nil {
		t.Fatalf("oversized transcript was accepted: %+v", got)
	}
	if got := scanCodeWork(target, nil, now); got != nil {
		t.Fatalf("missing cache invented source work: %+v", got)
	}
	target.backend = "claude"
	if got := scanCodeWork(target, cache, now); got != nil {
		t.Fatalf("unsupported backend invented source work: %+v", got)
	}
	target.backend, target.since = "copilot", time.Time{}
	if got := scanCodeWork(target, cache, now); got != nil {
		t.Fatalf("unknown kick invented source work: %+v", got)
	}
}

func codeWorkTestWorktree(t *testing.T) (string, string, string, time.Time, time.Time) {
	t.Helper()
	dir := t.TempDir()
	codeWorkTestGit(t, dir, "init", "--quiet", "-b", "fixture")
	codeWorkTestGit(t, dir, "config", "user.name", "Activity Test")
	codeWorkTestGit(t, dir, "config", "user.email", "activity@example.invalid")
	codeWorkTestGit(t, dir, "remote", "add", "origin", "https://github.com/org/repo.git")
	file := filepath.Join(dir, "source.go")
	if err := os.WriteFile(file, []byte("package before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	codeWorkTestGit(t, dir, "add", "source.go")
	codeWorkTestGit(t, dir, "commit", "--quiet", "-s", "-m", "fixture")
	head := codeWorkTestGit(t, dir, "rev-parse", "HEAD")
	now := time.Now().UTC()
	start := now.Add(-time.Second)
	if err := os.WriteFile(file, []byte("package after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, start, start); err != nil {
		t.Fatal(err)
	}
	return dir, file, head, start, now
}

func codeWorkTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func codeWorkTestEvent(t *testing.T, kind string, at time.Time, data map[string]any) string {
	t.Helper()
	event, err := json.Marshal(map[string]any{"type": kind, "timestamp": at.Format(time.RFC3339Nano), "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return string(event) + "\n"
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
