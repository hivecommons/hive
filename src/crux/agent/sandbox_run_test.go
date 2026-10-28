package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/outputschema"
	"github.com/hivecommons/hive/crux/sandbox"
)

func TestParseSandboxUsage(t *testing.T) {
	copilot := strings.Join([]string{
		"doing work...",
		"Total usage est: 1 Premium request",
		"Breakdown by AI model:",
		" claude-sonnet-4.5      12.3k in, 456 out, 1.2k cached",
		" gpt-5                  1,204 in, 10 out",
	}, "\n")
	claude := `{"type":"system","subtype":"init"}
{"type":"result","subtype":"success","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":5},"modelUsage":{"claude-opus-4":{}}}
`
	cases := []struct {
		name   string
		stdout string
		want   crux.SandboxRunUsage
	}{
		{"copilot footer", copilot, crux.SandboxRunUsage{Model: "claude-sonnet-4.5", Input: 13504, Output: 466, CacheRead: 1200}},
		{"claude json result", claude, crux.SandboxRunUsage{Model: "claude-opus-4", Input: 100, Output: 20, CacheRead: 300, CacheCreate: 5}},
		{"plain transcript", "Opened PR #1402\nDone.\n", crux.SandboxRunUsage{}},
		{"empty", "", crux.SandboxRunUsage{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseSandboxUsage(tc.stdout); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseHumanCount(t *testing.T) {
	for in, want := range map[string]int64{"12.3k": 12300, "1,204": 1204, "2M": 2000000, "7": 7, "junk": 0} {
		if got := parseHumanCount(in); got != want {
			t.Errorf("parseHumanCount(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestBuildSandboxRun(t *testing.T) {
	started := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	ended := started.Add(29 * time.Minute)
	res := SandboxKickResult{
		Branch:      "hive/deps-1793130444",
		CommitCount: 2,
		PR:          &crux.PRResult{URL: "https://github.com/torch-spyre/spyre-inference/pull/1402"},
		Report:      &outputschema.AgentReport{Summary: "  bumped torch to 2.9  "},
		Sandbox:     sandbox.Result{Stdout: "line1\nline2\n"},
	}
	run := buildSandboxRun("job", started, ended, res, nil)
	if !run.Succeeded || run.Runtime != "job" || run.Duration() != 29*time.Minute || run.PRURL != res.PR.URL || run.CommitCount != 2 || run.Branch != res.Branch {
		t.Fatalf("run = %+v", run)
	}
	if run.Summary != "bumped torch to 2.9" {
		t.Errorf("summary must prefer the agent report, got %q", run.Summary)
	}

	// No report: the transcript tail is the summary, stderr only as fallback.
	res.Report = nil
	res.Sandbox.Stdout = strings.Repeat("x\n", 30) + "last line\n"
	run = buildSandboxRun("podman", started, ended, res, nil)
	if !strings.HasSuffix(run.Summary, "last line") || strings.Count(run.Summary, "\n") >= sandboxRunSummaryTailLines {
		t.Errorf("summary must be the bounded stdout tail, got %q", run.Summary)
	}
	res.Sandbox.Stdout = ""
	res.Sandbox.Stderr = "boom"
	run = buildSandboxRun("podman", started, ended, res, errors.New("sandbox exited 1"))
	if run.Succeeded || run.Error != "sandbox exited 1" || run.Summary != "boom" {
		t.Errorf("failed run = %+v", run)
	}

	// Long summaries are bounded.
	res.Report = &outputschema.AgentReport{Summary: strings.Repeat("s", sandboxRunSummaryMax*2)}
	run = buildSandboxRun("job", started, ended, res, nil)
	if len([]rune(run.Summary)) > sandboxRunSummaryMax {
		t.Errorf("summary not truncated: %d runes", len([]rune(run.Summary)))
	}
}

func TestPersistAndLoadSandboxRun(t *testing.T) {
	workDir := t.TempDir()
	if got, err := loadSandboxRun(workDir, "deps"); err != nil || got != nil {
		t.Fatalf("missing record must be (nil, nil), got %+v, %v", got, err)
	}
	started := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	first := SandboxRunHistoryEntry{Agent: "deps", Backend: "claude", Model: "opus", Run: crux.SandboxRun{Runtime: "job", StartedAt: started, EndedAt: started.Add(time.Minute), Error: "exit 1"}}
	second := first
	second.Run = crux.SandboxRun{Runtime: "job", StartedAt: started.Add(6 * time.Hour), EndedAt: started.Add(6*time.Hour + 29*time.Minute), Succeeded: true, Summary: "ok", Usage: crux.SandboxRunUsage{Model: "opus", Input: 10, Output: 2}}
	for _, e := range []SandboxRunHistoryEntry{first, second} {
		if err := persistSandboxRun(workDir, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := loadSandboxRun(workDir, "deps")
	if err != nil || got == nil {
		t.Fatalf("load: %+v, %v", got, err)
	}
	if !got.Succeeded || !got.StartedAt.Equal(second.Run.StartedAt) || got.Usage != second.Run.Usage || got.Summary != "ok" {
		t.Errorf("last run must be the latest persisted, got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(workDir, "sandbox-runs", "deps.json.tmp")); !os.IsNotExist(err) {
		t.Errorf("temp file must be renamed away, stat err = %v", err)
	}
	hist, err := os.ReadFile(crux.SandboxRunHistoryPath(workDir))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(hist)), "\n"); len(lines) != 2 {
		t.Errorf("history must hold one line per run, got %d: %q", len(lines), hist)
	}
	if got, err := loadSandboxRun(workDir, "quality"); err != nil || got != nil {
		t.Errorf("other agent must have no record, got %+v, %v", got, err)
	}
}
