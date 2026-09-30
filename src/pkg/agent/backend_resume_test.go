package agent

import (
	"crypto/sha256"
	"encoding/hex"
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
	for _, backend := range []string{"agy", "bob", "", "vllm"} {
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

// writeGeminiTranscript writes a gemini session file whose first line is the
// metadata record carrying sessionID.
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
	if err := os.WriteFile(filepath.Join(projectDir, geminiProjectRootMarker), []byte(root+"\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func TestCaptureResumeHandle_GeminiScopesToAgentProject(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	tmp := filepath.Join(home, ".gemini", "tmp")
	const ownID = "aaaaaaaa-1111-2222-3333-444444444444"
	const otherID = "bbbbbbbb-5555-6666-7777-888888888888"
	writeGeminiProjectMarker(t, filepath.Join(tmp, "scanner"), "/data/agents/scanner")
	writeGeminiTranscript(t, filepath.Join(tmp, "scanner", "chats", "session-2026-09-30T08-00-aaaaaaaa.jsonl"), ownID, now.Add(-time.Hour))
	// ~/.gemini is fleet-shared: a newer session of another agent's project
	// must never be offered as this agent's conversation.
	writeGeminiProjectMarker(t, filepath.Join(tmp, "reviewer"), "/data/agents/reviewer")
	writeGeminiTranscript(t, filepath.Join(tmp, "reviewer", "chats", "session-2026-09-30T09-00-bbbbbbbb.jsonl"), otherID, now)

	h, ok := CaptureResumeHandleIn("scanner", 0, "gemini", "/data/agents/scanner", now)
	if !ok {
		t.Fatal("no handle captured for gemini")
	}
	if h.SessionID != ownID {
		t.Errorf("session id = %q, want the full id from the agent's own project", h.SessionID)
	}
	if h.Command != "gemini --resume "+ownID {
		t.Errorf("command = %q", h.Command)
	}
	if !h.TranscriptExists() || !strings.Contains(h.Transcript, filepath.Join("scanner", "chats")) {
		t.Errorf("transcript = %q", h.Transcript)
	}
	if !BackendSupportsResumeHandle("gemini") {
		t.Error("gemini does not claim a capturable resume handle")
	}
}

func TestCaptureResumeHandle_GeminiLegacyHashedProjectDir(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	sum := sha256.Sum256([]byte("/data/agents/scanner"))
	const id = "cccccccc-1111-2222-3333-444444444444"
	writeGeminiTranscript(t, filepath.Join(home, ".gemini", "tmp", hex.EncodeToString(sum[:]), "chats", "session-2026-09-30T08-00-cccccccc.jsonl"), id, now)

	h, ok := CaptureResumeHandleIn("scanner", 0, "gemini", "/data/agents/scanner/", now)
	if !ok || h.SessionID != id {
		t.Fatalf("handle = %+v, ok = %v; want %s from the hashed project dir", h, ok, id)
	}
}

func TestCaptureResumeHandle_GeminiNeedsWorkDirAndValidMetadata(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	project := filepath.Join(home, ".gemini", "tmp", "scanner")
	writeGeminiProjectMarker(t, project, "/data/agents/scanner")
	writeGeminiTranscript(t, filepath.Join(project, "chats", "session-2026-09-30T08-00-dddddddd.jsonl"), "dddddddd-1111-2222-3333-444444444444", now)

	if _, ok := CaptureResumeHandle("scanner", 0, "gemini", now); ok {
		t.Error("gemini captured a handle without a working directory to scope it")
	}
	if _, ok := CaptureResumeHandleIn("scanner", 0, "gemini", "/data/agents/other", now); ok {
		t.Error("gemini captured a handle from a different project")
	}

	// The newest transcript's id does not match its file name, or is unsafe
	// to render into a command: no handle rather than a wrong one.
	for _, bad := range []string{"eeeeeeee-1111-2222-3333-444444444444", "dddddddd; rm -rf /", ""} {
		writeGeminiTranscript(t, filepath.Join(project, "chats", "session-2026-09-30T09-00-dddddddd.jsonl"), bad, now.Add(time.Minute))
		if h, ok := CaptureResumeHandleIn("scanner", 0, "gemini", "/data/agents/scanner", now); ok {
			t.Errorf("sessionId %q produced handle %+v", bad, h)
		}
	}
}

func TestGeminiSessionID_RejectsUnreadableAndMalformed(t *testing.T) {
	dir := t.TempDir()
	if id := geminiSessionID(filepath.Join(dir, "missing.jsonl")); id != "" {
		t.Errorf("missing file yielded id %q", id)
	}
	path := filepath.Join(dir, "session-2026-09-30T08-00-ffffffff.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if id := geminiSessionID(path); id != "" {
		t.Errorf("malformed metadata yielded id %q", id)
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

func TestCaptureResumeHandle_PiScopesToAgentBucket(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	sessions := filepath.Join(home, ".pi", "agent", "sessions")
	const ownID = "aaaaaaaa-1111-2222-3333-444444444444"
	const otherID = "bbbbbbbb-5555-6666-7777-888888888888"
	writeResumeTranscript(t, filepath.Join(sessions, "--data-agents-scanner--", "2026-09-30T08-05-47-123Z_"+ownID+".jsonl"), now.Add(-time.Hour))
	// A newer session of another working directory must not be offered.
	writeResumeTranscript(t, filepath.Join(sessions, "--data-agents-reviewer--", "2026-09-30T09-05-47-123Z_"+otherID+".jsonl"), now)
	// Not a session file: no timestamp prefix.
	writeResumeTranscript(t, filepath.Join(sessions, "--data-agents-scanner--", "notes_"+otherID+".jsonl"), now)

	h, ok := CaptureResumeHandleIn("scanner", 0, "pi", "/data/agents/scanner", now)
	if !ok {
		t.Fatal("no handle captured for pi")
	}
	if h.SessionID != ownID {
		t.Errorf("session id = %q, want the agent's own bucket's session", h.SessionID)
	}
	if h.Command != "pi --session "+ownID {
		t.Errorf("command = %q", h.Command)
	}
	if !h.TranscriptExists() || !BackendSupportsResumeHandle("pi") {
		t.Errorf("transcript = %q, supported = %v", h.Transcript, BackendSupportsResumeHandle("pi"))
	}
	if _, ok := CaptureResumeHandle("scanner", 0, "pi", now); ok {
		t.Error("pi captured a handle without a working directory to scope it")
	}
}

func TestCaptureResumeHandle_OmpHomeRelativeAndAbsoluteBuckets(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	sessions := filepath.Join(home, ".omp", "agent", "sessions")
	const homeID = "cccccccc-1111-2222-3333-444444444444"
	const absID = "dddddddd-1111-2222-3333-444444444444"
	const otherID = "eeeeeeee-1111-2222-3333-444444444444"

	workInHome := filepath.Join(home, "work", "scanner")
	if err := os.MkdirAll(workInHome, 0o755); err != nil {
		t.Fatal(err)
	}
	writeResumeTranscript(t, filepath.Join(sessions, "-work-scanner", "2026-09-30T08-00-00-000Z_"+homeID+".jsonl"), now.Add(-time.Hour))
	writeResumeTranscript(t, filepath.Join(sessions, "-work-reviewer", "2026-09-30T09-00-00-000Z_"+otherID+".jsonl"), now)
	// A per-subagent transcript inside the session directory is not a session.
	writeResumeTranscript(t, filepath.Join(sessions, "-work-scanner", "2026-09-30T08-00-00-000Z_"+homeID, "Explorer.jsonl"), now)

	h, ok := CaptureResumeHandleIn("scanner", 0, "omp", workInHome, now)
	if !ok {
		t.Fatal("no handle captured for omp")
	}
	if h.SessionID != homeID || h.Command != "omp --resume "+homeID {
		t.Errorf("handle = %+v, want %s from the home-relative bucket", h, homeID)
	}

	writeResumeTranscript(t, filepath.Join(sessions, "--data-agents-scanner--", "2026-09-30T08-00-00-000Z_"+absID+".jsonl"), now)
	h, ok = CaptureResumeHandleIn("scanner", 0, "omp", "/data/agents/scanner", now)
	if !ok || h.SessionID != absID {
		t.Fatalf("handle = %+v, ok = %v; want %s from the absolute bucket", h, ok, absID)
	}
	if !BackendSupportsResumeHandle("omp") {
		t.Error("omp does not claim a capturable resume handle")
	}
}

func TestTimestampedSessionID_RejectsNonSessionNames(t *testing.T) {
	for _, name := range []string{
		"Explorer.jsonl",
		"notes_aaaaaaaa-1111.jsonl",
		"2026-09-30T08-00-00-000Z.jsonl",
		"2026-09-30T08-00-00-000Z_short.jsonl",
		"2026-09-30T08-00-00-000Z_bad;id-12345.jsonl",
		"abcd-09-30T08-00-00-000Z_aaaaaaaa-1111.jsonl",
	} {
		if id := timestampedSessionID(name); id != "" {
			t.Errorf("timestampedSessionID(%q) = %q, want no id", name, id)
		}
	}
	if id := timestampedSessionID("2026-09-30T08-00-00-000Z_aaaaaaaa-1111.jsonl"); id != "aaaaaaaa-1111" {
		t.Errorf("id = %q", id)
	}
}

// writeGooseLog writes a goose per-launch CLI log whose tracing spans carry
// sessionIDs in order, mirroring goose 1.52.0's
// ~/.local/state/goose/logs/cli/<date>/<launch-ts>.log layout.
func writeGooseLog(t *testing.T, path string, modTime time.Time, sessionIDs ...string) {
	t.Helper()
	body := `{"timestamp":"2026-09-30T13:55:26Z","level":"INFO","fields":{"message":"CLI command executed","command":"run"},"target":"goose_cli::cli"}` + "\n"
	for _, id := range sessionIDs {
		body += `{"spans":[{"gen_ai.agent.name":"goose","session.host":"hive","session.id":"` + id + `","name":"reply"}]}` + "\n"
	}
	writeResumeTranscript(t, path, modTime)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestCaptureResumeHandle_GooseReadsSessionIDFromNewestCLILog(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	logs := filepath.Join(home, ".local", "state", "goose", "logs", "cli")
	writeGooseLog(t, filepath.Join(logs, "2026-09-29", "20260929_080547.log"), now.Add(-25*time.Hour), "20260929_1")
	writeGooseLog(t, filepath.Join(logs, "2026-09-30", "20260930_095526.log"), now, "20260930_1", "20260930_2")
	// Not a launch log: must never be mistaken for a session.
	writeGooseLog(t, filepath.Join(logs, "2026-09-30", "install.log"), now.Add(time.Minute), "20260930_9")

	h, ok := CaptureResumeHandle("scanner", 0, "goose", now)
	if !ok {
		t.Fatal("no handle captured for goose")
	}
	if h.SessionID != "20260930_2" {
		t.Errorf("session id = %q, want the newest log's last session id", h.SessionID)
	}
	if h.Command != "goose session --resume --session-id 20260930_2" {
		t.Errorf("command = %q", h.Command)
	}
	if !h.TranscriptExists() || filepath.Base(h.Transcript) != "20260930_095526.log" {
		t.Errorf("transcript = %q, want the newest launch log", h.Transcript)
	}
	if !BackendSupportsResumeHandle("goose") {
		t.Error("goose does not claim a capturable resume handle")
	}
}

func TestCaptureResumeHandle_GooseNeedsACompletedTurnAndASafeID(t *testing.T) {
	home := homeForCapture(t)
	now := time.Now()
	logs := filepath.Join(home, ".local", "state", "goose", "logs", "cli", "2026-09-30")

	// A launch whose first turn has not finished has no session id in its
	// log yet: no handle rather than a stale or foreign one.
	writeGooseLog(t, filepath.Join(logs, "20260930_100000.log"), now)
	if h, ok := CaptureResumeHandle("scanner", 0, "goose", now); ok {
		t.Errorf("id-less log produced handle %+v", h)
	}

	// An id that is unsafe to render into a shell command is never offered.
	writeGooseLog(t, filepath.Join(logs, "20260930_100100.log"), now.Add(time.Minute), "20260930_1; rm -rf /")
	if h, ok := CaptureResumeHandle("scanner", 0, "goose", now); ok {
		t.Errorf("unsafe id produced handle %+v", h)
	}
}

func TestGooseLaunchLogID_SelectsOnlyLaunchLogs(t *testing.T) {
	for _, name := range []string{"install.log", "20260930.log", "2026-09-30_095526.log", "20260930_0955.log", "20260930_09552a.log", "20260930_095526.txt"} {
		if id := gooseLaunchLogID(name); id != "" {
			t.Errorf("gooseLaunchLogID(%q) = %q, want no id", name, id)
		}
	}
	if id := gooseLaunchLogID("20260930_095526.log"); id != "20260930_095526" {
		t.Errorf("id = %q", id)
	}
}

func TestGooseSessionID_ReadsTailOfLargeLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20260930_095526.log")
	filler := strings.Repeat(`{"timestamp":"2026-09-30T13:55:26Z","level":"DEBUG","fields":{"message":"x"}}`+"\n", 8192)
	body := filler + `{"spans":[{"session.id":"20260930_7","name":"reply"}]}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if id := gooseSessionID(path); id != "20260930_7" {
		t.Errorf("id = %q, want the id from the log's tail", id)
	}
	if id := gooseSessionID(filepath.Join(dir, "missing.log")); id != "" {
		t.Errorf("missing file yielded id %q", id)
	}
}
