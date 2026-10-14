package agent

// Tests for the kick log transcript source (#11762): the Copilot CLI and
// Claude Code readers against JSONL fixtures and golden text, the
// HIVE_KICK_LOG_SOURCE switch, the fallback to scrollback on a missing,
// corrupt or stale transcript, and the over-cap omission marker.

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	transcriptTestSessionID  = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	transcriptTestScrollback = "SCROLLBACK tail (3 lines hidden)\n"
)

func readTranscriptFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "kick_transcript", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(data)
}

func TestRenderKickTranscript_Golden(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture string
		golden  string
		render  func(io.Reader, time.Time) (string, error)
	}{
		{"copilot", "copilot_events.jsonl", "copilot_events.golden", renderCopilotTranscript},
		{"claude", "claude_session.jsonl", "claude_session.golden", renderClaudeTranscript},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.render(strings.NewReader(readTranscriptFixture(t, tc.fixture)), time.Time{})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			want := readTranscriptFixture(t, tc.golden)
			if strings.TrimRight(got, "\n") != strings.TrimRight(want, "\n") {
				t.Errorf("rendered transcript differs from %s:\n--- got ---\n%s\n--- want ---\n%s", tc.golden, got, want)
			}
			if strings.Contains(got, "\x1b") {
				t.Errorf("rendered transcript contains ANSI escapes:\n%q", got)
			}
			if strings.Contains(got, "lines hidden") {
				t.Errorf("rendered transcript used the truncated tool output:\n%s", got)
			}
		})
	}
}

// Entries before the kick started belong to earlier kicks of the same CLI
// session and must not leak into this kick's log.
func TestRenderKickTranscript_SkipsEntriesBeforeKick(t *testing.T) {
	since := time.Date(2026, 10, 14, 15, 0, 3, 500_000_000, time.UTC)
	for _, tc := range []struct {
		name    string
		fixture string
		render  func(io.Reader, time.Time) (string, error)
	}{
		{"copilot", "copilot_events.jsonl", renderCopilotTranscript},
		{"claude", "claude_session.jsonl", renderClaudeTranscript},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.render(strings.NewReader(readTranscriptFixture(t, tc.fixture)), since)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(got, "git status") || strings.Contains(got, "fix issue 42") {
				t.Errorf("entries before the kick were rendered:\n%s", got)
			}
			if !strings.Contains(got, "Done.") {
				t.Errorf("entries after the kick are missing:\n%s", got)
			}
		})
	}
}

func TestRenderKickTranscript_CorruptLineIsAnError(t *testing.T) {
	for name, render := range kickTranscriptReaders {
		t.Run(name, func(t *testing.T) {
			in := `{"type":"user","timestamp":"2026-10-14T15:00:01Z","message":{"content":"hi"}}` + "\nnot json\n"
			if _, err := render(strings.NewReader(in), time.Time{}); err == nil {
				t.Error("corrupt transcript rendered without error")
			}
		})
	}
}

// A final line without a newline is the CLI mid-write, not corruption.
func TestRenderKickTranscript_TornFinalLineIgnored(t *testing.T) {
	in := `{"type":"user.message","timestamp":"2026-10-14T15:00:01Z","data":{"content":"hi"}}` + "\n" + `{"type":"assistant.mes`
	got, err := renderCopilotTranscript(strings.NewReader(in), time.Time{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, "hi") {
		t.Errorf("complete line missing:\n%s", got)
	}
}

func TestKickLogSourceFromEnv(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", kickLogSourceScrollback},
		{"scrollback", kickLogSourceScrollback},
		{"transcript", kickLogSourceTranscript},
		{"auto", kickLogSourceAuto},
		{" AUTO ", kickLogSourceAuto},
		{"pipe-pane", kickLogSourceScrollback},
	} {
		t.Setenv(kickLogSourceEnv, tc.env)
		if got := kickLogSourceFromEnv(); got != tc.want {
			t.Errorf("%s=%q: got %q, want %q", kickLogSourceEnv, tc.env, got, tc.want)
		}
	}
}

func TestCapKickTranscript(t *testing.T) {
	if got := capKickTranscript("short", 100); got != "short" {
		t.Errorf("under-cap transcript changed: %q", got)
	}
	text := strings.Repeat("a", 500) + strings.Repeat("b", 500)
	got := capKickTranscript(text, 200)
	// The 29-byte marker leaves 171 bytes: 85 of head, 86 of tail, so 829 of
	// the 1000 are omitted.
	if !strings.Contains(got, "\n[… 829 bytes omitted …]\n") {
		t.Errorf("missing omission marker: %q", got)
	}
	if !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "bbbb") {
		t.Errorf("head/tail not kept: %q", got)
	}
	if len(got) != 200 {
		t.Errorf("capped length = %d, want 200", len(got))
	}
	if got := capKickTranscript(strings.Repeat("é", 400), 101); !utf8.ValidString(got) {
		t.Errorf("cap split a rune: %q", got)
	}
}

// transcriptTestManager is kickLogTestManager with HOME pointed at a fresh
// directory (the agent has no UID, so its CLI home is $HOME) and the given
// backend and source.
func transcriptTestManager(t *testing.T, backend, source string) (*Manager, *AgentProcess, string, string) {
	t.Helper()
	m, agent, dir := kickLogTestManager(t, transcriptTestScrollback)
	home := t.TempDir()
	t.Setenv("HOME", home)
	agent.Config.Backend = backend
	m.kickLogSource = source
	return m, agent, dir, home
}

// writeTestTranscript stores content where backend_resume.go's layout for
// backend looks for the newest session, returning the file path.
func writeTestTranscript(t *testing.T, home, backend, content string) string {
	t.Helper()
	var path string
	switch backend {
	case "copilot":
		path = filepath.Join(home, ".copilot", "session-state", transcriptTestSessionID, "events.jsonl")
	case "claude":
		path = filepath.Join(home, ".claude", "projects", "-repo-scanner", transcriptTestSessionID+".jsonl")
	default:
		t.Fatalf("no transcript layout for %s", backend)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newestArchiveBody(t *testing.T, m *Manager, dir string) string {
	t.Helper()
	infos, err := m.ListKickLogs("scanner")
	if err != nil || len(infos) == 0 {
		t.Fatalf("ListKickLogs = %v, %v", infos, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "scanner", infos[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestArchiveKickLog_TranscriptSource(t *testing.T) {
	for _, tc := range []struct{ backend, fixture string }{
		{"copilot", "copilot_events.jsonl"},
		{"claude", "claude_session.jsonl"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			m, agent, dir, home := transcriptTestManager(t, tc.backend, kickLogSourceTranscript)
			writeTestTranscript(t, home, tc.backend, readTranscriptFixture(t, tc.fixture))
			if !m.archiveKickLogLocked(agent, "kick") {
				t.Fatal("archive not written")
			}
			body := newestArchiveBody(t, m, dir)
			for _, want := range []string{"source: transcript\n", "$ git status\n", "fix issue 42", "Done."} {
				if !strings.Contains(body, want) {
					t.Errorf("archive missing %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, "lines hidden") || strings.Contains(body, "SCROLLBACK") {
				t.Errorf("transcript archive carries scrollback content:\n%s", body)
			}
		})
	}
}

// The default source never looks at the transcript: the archive is the
// scrollback exactly as before, labelled as such.
func TestArchiveKickLog_DefaultSourceIsScrollback(t *testing.T) {
	m, agent, dir, home := transcriptTestManager(t, "copilot", "")
	writeTestTranscript(t, home, "copilot", readTranscriptFixture(t, "copilot_events.jsonl"))
	if !m.archiveKickLogLocked(agent, "kick") {
		t.Fatal("archive not written")
	}
	body := newestArchiveBody(t, m, dir)
	if !strings.Contains(body, "source: scrollback\n") {
		t.Errorf("header missing scrollback source:\n%s", body)
	}
	if !strings.HasSuffix(body, "=======================\n"+transcriptTestScrollback) {
		t.Errorf("default archive body is not the scrollback verbatim:\n%s", body)
	}
}

func TestArchiveKickLog_AutoFallsBackToScrollback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, agent *AgentProcess, home string)
	}{
		{"missing", func(*testing.T, *AgentProcess, string) {}},
		{"corrupt", func(t *testing.T, _ *AgentProcess, home string) {
			writeTestTranscript(t, home, "copilot", "not json\n{}\n")
		}},
		{"predates kick", func(t *testing.T, agent *AgentProcess, home string) {
			path := writeTestTranscript(t, home, "copilot", readTranscriptFixture(t, "copilot_events.jsonl"))
			old := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			kick := time.Now().Add(-time.Hour)
			agent.LastKick = &kick
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, agent, dir, home := transcriptTestManager(t, "copilot", kickLogSourceAuto)
			var logs bytes.Buffer
			m.logger = slog.New(slog.NewTextHandler(&logs, nil))
			tc.setup(t, agent, home)
			if !m.archiveKickLogLocked(agent, "kick") {
				t.Fatal("archive not written")
			}
			body := newestArchiveBody(t, m, dir)
			if !strings.Contains(body, "source: scrollback\n") || !strings.HasSuffix(body, transcriptTestScrollback) {
				t.Errorf("fallback archive is not the scrollback:\n%s", body)
			}
			if n := strings.Count(logs.String(), "transcript unavailable"); n != 1 {
				t.Errorf("fallback warnings = %d, want 1:\n%s", n, logs.String())
			}
		})
	}
}

// A backend with no reader is simply not a transcript backend under auto: no
// warning, scrollback archive.
func TestArchiveKickLog_AutoUnsupportedBackendIsSilent(t *testing.T) {
	m, agent, dir, _ := transcriptTestManager(t, "codex", kickLogSourceAuto)
	var logs bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if !m.archiveKickLogLocked(agent, "kick") {
		t.Fatal("archive not written")
	}
	if body := newestArchiveBody(t, m, dir); !strings.Contains(body, "source: scrollback\n") {
		t.Errorf("header missing scrollback source:\n%s", body)
	}
	if strings.Contains(logs.String(), "transcript unavailable") {
		t.Errorf("auto warned for a backend without a reader:\n%s", logs.String())
	}
}

func TestArchiveKickLog_TranscriptOverCapKeepsHeadAndTail(t *testing.T) {
	m, agent, dir, home := transcriptTestManager(t, "copilot", kickLogSourceTranscript)
	m.kickLogMaxBytes = 400
	big := strings.Repeat("x", 2000)
	writeTestTranscript(t, home, "copilot",
		`{"type":"user.message","timestamp":"2026-10-14T15:00:01Z","data":{"content":"first turn"}}`+"\n"+
			`{"type":"tool.execution_complete","timestamp":"2026-10-14T15:00:02Z","data":{"toolCallId":"c1","success":true,"result":{"content":"`+big+`"}}}`+"\n"+
			`{"type":"assistant.message","timestamp":"2026-10-14T15:00:03Z","data":{"content":"last turn"}}`+"\n")
	if !m.archiveKickLogLocked(agent, "kick") {
		t.Fatal("archive not written")
	}
	body := newestArchiveBody(t, m, dir)
	for _, want := range []string{"source: transcript\n", "first turn", "last turn", " bytes omitted …]\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("capped archive missing %q:\n%s", want, body)
		}
	}
}

// The live full log and its header use the same source as the archive.
func TestCaptureFullLog_TranscriptSource(t *testing.T) {
	m, _, _, home := transcriptTestManager(t, "claude", kickLogSourceAuto)
	origExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = origExists })

	if h := m.FullLogHeader("scanner"); !strings.Contains(h, "source: scrollback\n") {
		t.Errorf("header before any transcript:\n%s", h)
	}
	writeTestTranscript(t, home, "claude", readTranscriptFixture(t, "claude_session.jsonl"))
	log, err := m.CaptureFullLog("scanner")
	if err != nil {
		t.Fatalf("CaptureFullLog: %v", err)
	}
	if !strings.Contains(log, "$ git status\n") || strings.Contains(log, "SCROLLBACK") {
		t.Errorf("live log is not the transcript:\n%s", log)
	}
	if h := m.FullLogHeader("scanner"); !strings.Contains(h, "source: transcript\n") {
		t.Errorf("header with a transcript:\n%s", h)
	}
}
