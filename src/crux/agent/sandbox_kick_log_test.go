package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/outputschema"
	"github.com/hivecommons/hive/crux/sandbox"
)

func TestSandboxKickLogBody(t *testing.T) {
	res := SandboxKickResult{
		Branch:       "hive/deps/20261027195251",
		CommitCount:  1,
		Workspace:    "/data/agents/sandbox/deps-1",
		ReportPath:   "/data/agents/sandbox/deps-1/.hive/agent-report.json",
		ReportStatus: outputschema.StatusFailed,
		PRWithheld:   "agent report status=failed: Artifactory 401",
		Sandbox:      sandbox.Result{Stdout: "installing rpms", Stderr: "401 Unauthorized\n", ExitCode: 0},
	}
	body := sandboxKickLogBody("job", res, errors.New("sandbox run reported failure"))
	for _, want := range []string{
		"runtime: job\n", "exit_code: 0\n", "commits: 1\n",
		"branch: hive/deps/20261027195251\n",
		"pr_withheld: agent report status=failed: Artifactory 401\n",
		"agent_report_status: failed\n",
		"error: sandbox run reported failure\n",
		"---- stdout ----\ninstalling rpms\n",
		"---- stderr ----\n401 Unauthorized\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "pr:") && !strings.Contains(body, "pr_withheld") {
		t.Errorf("no PR URL must be rendered when none was opened:\n%s", body)
	}
	if strings.Contains(sandboxKickLogBody("podman", SandboxKickResult{}, nil), "error:") {
		t.Error("a clean run must not render an error line")
	}
}

// A job-runtime agent has no tmux pane, so "full log" must be the captured
// container output hive archived when the kick ended — the only durable copy
// once the Job is GC'd and the workspace swept.
func TestSandboxedKickArchivesLogAndServesFullLog(t *testing.T) {
	yes := true
	m, _, _ := jobRuntimeManager(t,
		&config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob},
		config.AgentSandboxConfig{Enabled: true, Image: "dev-image",
			Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data"}})
	logDir := t.TempDir()
	m.SetKickLogDir(logDir)
	m.mu.Lock()
	m.kickLogRetention = 3
	m.mu.Unlock()

	if _, err := m.CaptureFullLog("deps"); err == nil || !strings.Contains(err.Error(), "no archived run yet") {
		t.Fatalf("CaptureFullLog before any kick: err = %v, want the no-archived-run explanation", err)
	}
	if hdr := m.FullLogHeader("deps"); hdr != "" {
		t.Errorf("FullLogHeader for a sandboxed agent = %q, want empty (archive carries its own)", hdr)
	}

	if err := m.SendKick("deps", "bump torch"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitIdle(t, m, "deps")

	// The archive is written after m.mu is released, so poll (ticker, not a
	// fixed sleep: the sleep ratchet forbids new time.Sleep calls in tests).
	var run *crux.SandboxRun
	waitFor(t, "sandbox kick log archive", func() bool {
		run = m.AllStatuses()["deps"].SandboxLastRun
		if run == nil || run.LogID == "" {
			return false
		}
		_, err := os.Stat(filepath.Join(logDir, "deps", run.LogID))
		return err == nil
	})
	if !strings.HasSuffix(run.LogID, "-sandbox.log") {
		t.Errorf("LogID = %q, want the -sandbox.log suffix", run.LogID)
	}

	got, err := m.CaptureFullLog("deps")
	if err != nil {
		t.Fatalf("CaptureFullLog after kick: %v", err)
	}
	for _, want := range []string{"==== hive kick log ====", "agent: deps", "source: sandbox-job", "reason: sandbox", "---- stdout ----\nok\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("full log missing %q:\n%s", want, got)
		}
	}

	infos, err := m.ListKickLogs("deps")
	if err != nil || len(infos) != 1 || infos[0].ID != run.LogID {
		t.Fatalf("ListKickLogs = %+v, %v; want exactly the archived run %q", infos, err, run.LogID)
	}
	if content, err := m.ReadKickLog("deps", run.LogID); err != nil || content != got {
		t.Errorf("ReadKickLog must serve the same archive as the full log (err=%v, same=%v)", err, content == got)
	}
}

func TestSandboxedKickSkipsArchiveWhenRetentionOff(t *testing.T) {
	yes := true
	m, _, _ := jobRuntimeManager(t,
		&config.AgentSandboxOverride{Enabled: &yes, Runtime: config.SandboxRuntimeJob},
		config.AgentSandboxConfig{Enabled: true, Image: "dev-image",
			Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data"}})
	logDir := t.TempDir()
	m.SetKickLogDir(logDir)
	m.mu.Lock()
	m.kickLogRetention = 0
	m.mu.Unlock()
	if err := m.SendKick("deps", "bump"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitIdle(t, m, "deps")
	if run := m.AllStatuses()["deps"].SandboxLastRun; run == nil || run.LogID != "" {
		t.Fatalf("retention 0 must record the run without an archive id, got %+v", run)
	}
	if entries, _ := os.ReadDir(filepath.Join(logDir, "deps")); len(entries) != 0 {
		t.Errorf("retention 0 must write nothing, found %d files", len(entries))
	}
}

func TestBuildSandboxRunCarriesPRWithheld(t *testing.T) {
	started := time.Date(2026, 10, 27, 19, 52, 51, 0, time.UTC)
	res := SandboxKickResult{
		CommitCount: 1,
		Branch:      "hive/deps/20261027195251",
		PRWithheld:  "agent report status=failed: " + strings.Repeat("x", sandboxRunSummaryMax*2),
		Report:      &outputschema.AgentReport{Summary: "rpm install failed", Status: outputschema.StatusFailed},
	}
	run := buildSandboxRun("job", started, started.Add(time.Minute), res, errors.New("sandbox run reported failure"))
	if run.Succeeded || run.PRURL != "" {
		t.Fatalf("withheld run must not look successful: %+v", run)
	}
	if !strings.HasPrefix(run.PRWithheld, "agent report status=failed: ") || len([]rune(run.PRWithheld)) > sandboxRunSummaryMax {
		t.Errorf("PRWithheld = %q (len %d), want the bounded reason", run.PRWithheld, len([]rune(run.PRWithheld)))
	}
}
