package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// The capture logic and its fixtures live in agent/resumehandle; these tests
// cover only the Manager wiring, which needs a real HOME and session tree.

func homeForCapture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeResumeTranscript(t *testing.T, path string, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func writeGeminiTranscript(t *testing.T, path, sessionID string, modTime time.Time) {
	t.Helper()
	writeResumeTranscript(t, path, modTime)
	body := `{"sessionId":"` + sessionID + `","projectHash":"x"}` + "\n" + `{"type":"user"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func writeGeminiProjectMarker(t *testing.T, projectDir, root string) {
	t.Helper()
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", projectDir, err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(root+"\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func TestManagerResumeHandle_NilAndUnknownAgent(t *testing.T) {
	var m *Manager
	if _, ok := m.ResumeHandle("scanner"); ok {
		t.Error("nil manager returned a resume handle")
	}
	mgr := &Manager{agents: map[string]*AgentProcess{}}
	if _, ok := mgr.ResumeHandle("scanner"); ok {
		t.Error("unknown agent returned a resume handle")
	}
	mgr.agents["scanner"] = &AgentProcess{Name: "scanner", State: StateStopped}
	if _, ok := mgr.ResumeHandle("scanner"); ok {
		t.Error("stopped agent returned a resume handle")
	}
}

func TestManagerResumeHandle_GeminiUsesAgentWorkDir(t *testing.T) {
	home := homeForCapture(t)
	workRoot := t.TempDir()
	const id = "12345678-1111-2222-3333-444444444444"
	project := filepath.Join(home, ".gemini", "tmp", "scanner")
	writeGeminiProjectMarker(t, project, filepath.Join(workRoot, "scanner"))
	writeGeminiTranscript(t, filepath.Join(project, "chats", "session-2026-09-30T08-00-12345678.jsonl"), id, time.Now())
	mgr := &Manager{workDir: workRoot, agents: map[string]*AgentProcess{
		"scanner": {Name: "scanner", State: StateRunning, Config: config.AgentConfig{Backend: "gemini"}},
	}}

	h, ok := mgr.ResumeHandle("scanner")
	if !ok || h.SessionID != id || h.Backend != "gemini" {
		t.Fatalf("handle = %+v, ok = %v", h, ok)
	}
}

func TestManagerResumeHandle_RunningAgentCapturesBackendSession(t *testing.T) {
	home := homeForCapture(t)
	writeResumeTranscript(t, filepath.Join(home, ".copilot", "session-state", "sess-live-0003", "events.jsonl"), time.Now())
	mgr := &Manager{agents: map[string]*AgentProcess{
		"scanner": {Name: "scanner", State: StateRunning, Config: config.AgentConfig{Backend: "copilot"}},
	}}

	h, ok := mgr.ResumeHandle("scanner")
	if !ok {
		t.Fatal("running copilot agent produced no resume handle")
	}
	if h.SessionID != "sess-live-0003" || h.Backend != "copilot" {
		t.Errorf("handle = %+v", h)
	}
	if h.String() == "" {
		t.Error("String() is empty for a captured handle")
	}
}
