package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Backend resume-handle capture (hivecommons/hive#9606). Each test lays out
// the backend's real on-disk session tree under a temp HOME and checks the
// captured id, transcript and command.

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

// homeForCapture gives the capture path a temp HOME. AgentHome returns the
// process HOME for an agent with no allocated UID, which is the layout the
// hive uses when it runs as the hive user.
func homeForCapture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestCaptureResumeHandle_ClaudeNamesNewestTranscript(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	projects := filepath.Join(home, ".claude", "projects")
	writeResumeTranscript(t, filepath.Join(projects, "-data-work-hive", "11111111-2222-3333-4444-555555555555.jsonl"), now.Add(-2*time.Hour))
	writeResumeTranscript(t, filepath.Join(projects, "-data-work-other", "99999999-8888-7777-6666-555555555555.jsonl"), now.Add(-time.Minute))

	h, ok := CaptureResumeHandle("scanner", 0, "claude", now)
	if !ok {
		t.Fatal("no handle captured for claude")
	}
	if h.SessionID != "99999999-8888-7777-6666-555555555555" {
		t.Errorf("session id = %q, want the newest transcript's", h.SessionID)
	}
	if h.Command != "claude --resume "+h.SessionID {
		t.Errorf("command = %q", h.Command)
	}
	if !strings.HasSuffix(h.Transcript, ".jsonl") || !h.TranscriptExists() {
		t.Errorf("transcript = %q, want an existing .jsonl", h.Transcript)
	}
	if h.Backend != "claude" || !h.CapturedAt.Equal(now) {
		t.Errorf("handle = %+v, want backend claude captured at now", h)
	}
}

func TestCaptureResumeHandle_CopilotUsesSessionDirectories(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	root := filepath.Join(home, ".copilot", "session-state")
	writeResumeTranscript(t, filepath.Join(root, "sess-old-0001", "events.jsonl"), now.Add(-time.Hour))
	writeResumeTranscript(t, filepath.Join(root, "sess-new-0002", "events.jsonl"), now)

	h, ok := CaptureResumeHandle("scanner", 0, "copilot", now)
	if !ok {
		t.Fatal("no handle captured for copilot")
	}
	if h.SessionID != "sess-new-0002" {
		t.Errorf("session id = %q, want the newest session directory", h.SessionID)
	}
	if h.Command != "copilot --resume sess-new-0002" {
		t.Errorf("command = %q", h.Command)
	}
	if filepath.Base(h.Transcript) != "events.jsonl" {
		t.Errorf("transcript = %q, want the session's events.jsonl", h.Transcript)
	}
}

func TestCaptureResumeHandle_CodexReadsDateBucketedRollouts(t *testing.T) {
	homeForCapture(t)
	now := time.Now()
	codexHome := t.TempDir()
	prev := codexHomePrefix
	codexHomePrefix = filepath.Join(codexHome, "codex-")
	t.Cleanup(func() { codexHomePrefix = prev })
	sessions := filepath.Join(codexHomePath("scanner"), "sessions", "2026", "09", "30")
	writeResumeTranscript(t, filepath.Join(sessions, "rollout-2026-09-30T08-05-47-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl"), now)
	// Not a rollout file: must not be mistaken for a conversation.
	writeResumeTranscript(t, filepath.Join(sessions, "notes.jsonl"), now.Add(time.Hour))

	h, ok := CaptureResumeHandle("scanner", 0, "codex", now)
	if !ok {
		t.Fatal("no handle captured for codex")
	}
	if h.SessionID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("session id = %q, want the rollout uuid", h.SessionID)
	}
	if h.Command != "codex resume "+h.SessionID {
		t.Errorf("command = %q", h.Command)
	}
}

func TestCaptureResumeHandle_UnsupportedAndEmptyBackends(t *testing.T) {
	homeForCapture(t)
	now := time.Now()
	for _, backend := range []string{"agy", "gemini", "", "vllm"} {
		if BackendSupportsResumeHandle(backend) {
			t.Errorf("backend %q unexpectedly claims a capturable resume handle", backend)
		}
		if _, ok := CaptureResumeHandle("scanner", 0, backend, now); ok {
			t.Errorf("backend %q captured a handle", backend)
		}
	}
	// A supported backend that has simply never written a transcript.
	if _, ok := CaptureResumeHandle("scanner", 0, "claude", now); ok {
		t.Error("claude captured a handle with no session tree on disk")
	}
}

func TestCodexRolloutID_RejectsNonUUIDNames(t *testing.T) {
	for _, name := range []string{"rollout-2026-09-30T08-05-47-not-a-uuid.jsonl", "rollout.jsonl", "events.jsonl", "rollout-abc.jsonl"} {
		if id := codexRolloutID(name); id != "" {
			t.Errorf("codexRolloutID(%q) = %q, want no id", name, id)
		}
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
