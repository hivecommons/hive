package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/pkg/config"
)

type fakeJobSweeper struct {
	opts  kubejob.SweepOptions
	calls int
	swept []kubejob.SweptJob
	err   error
}

func (f *fakeJobSweeper) SweepOrphans(_ context.Context, opts kubejob.SweepOptions) ([]kubejob.SweptJob, error) {
	f.calls++
	f.opts = opts
	return f.swept, f.err
}

func jobSweepConfig() *config.Config {
	on := true
	return &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		AgentSandbox: config.AgentSandboxConfig{Enabled: true, Image: "img"},
		Agents: map[string]config.AgentConfig{
			"reviewer": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &on, Runtime: "job", TimeoutS: 7200}},
			"scanner":  {Backend: "claude"},
		},
	}}
}

func TestSandboxJobBootSweepAuditsEachReap(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	sw := &fakeJobSweeper{swept: []kubejob.SweptJob{{Name: "hive-kick-reviewer-1", Agent: "hive-reviewer-sandbox", Age: 11 * 24 * time.Hour, Reason: kubejob.SweepReasonPendingPastDeadline}}}
	var audits []string
	audit := func(user, action, detail, agent string) {
		audits = append(audits, user+"|"+action+"|"+agent+"|"+detail)
	}
	cfg := jobSweepConfig()
	n := runSandboxJobBootSweep(context.Background(), sandboxJobSweepMaxAge(cfg), []string{"reviewer", "scanner"}, nil, sw, audit, logger)
	if n != 1 || sw.calls != 1 {
		t.Fatalf("reaped = %d, calls = %d", n, sw.calls)
	}
	if sw.opts.MaxAge != 2*time.Hour {
		t.Fatalf("MaxAge = %s, want the longest Job-runtime kick timeout", sw.opts.MaxAge)
	}
	if len(audits) != 1 || !strings.Contains(audits[0], "hive|sandbox_job_reaped|hive-reviewer-sandbox|job=hive-kick-reviewer-1") {
		t.Fatalf("audits = %v", audits)
	}
	if !strings.Contains(logBuf.String(), "reaped orphaned sandbox job") {
		t.Fatalf("reap not logged: %s", logBuf.String())
	}
	if sw.opts.Live == nil || sw.opts.Live("hive-reviewer-sandbox") {
		t.Fatal("with no runtime, no Job is live")
	}
}

func TestSandboxJobBootSweepStartsOnlyWithJobRuntime(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	cfg.Agents = map[string]config.AgentConfig{"scanner": {Backend: "claude"}}
	if startSandboxJobBootSweep(context.Background(), cfg, nil, nil, quiet) {
		t.Fatal("sweep started with no Job runtime configured")
	}
	if startSandboxJobBootSweep(context.Background(), nil, nil, nil, quiet) {
		t.Fatal("nil config must not sweep")
	}
	sw := &fakeJobSweeper{}
	done := make(chan struct{})
	prev := newSandboxJobSweeper
	newSandboxJobSweeper = func() sandboxJobSweeper { return sw }
	t.Cleanup(func() { newSandboxJobSweeper = prev })
	audit := func(string, string, string, string) {}
	if !startSandboxJobBootSweep(context.Background(), jobSweepConfig(), nil, audit, slog.New(slog.NewTextHandler(&signalWriter{done: done}, nil))) {
		t.Fatal("sweep did not start with a Job runtime configured")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("background sweep never finished")
	}
	if sw.calls != 1 || sw.opts.MaxAge != 2*time.Hour {
		t.Fatalf("sweep calls = %d, MaxAge = %s", sw.calls, sw.opts.MaxAge)
	}

	// A nil sweeper falls back to the factory.
	sw2 := &fakeJobSweeper{}
	newSandboxJobSweeper = func() sandboxJobSweeper { return sw2 }
	runSandboxJobBootSweep(context.Background(), time.Hour, nil, nil, nil, nil, quiet)
	if sw2.calls != 1 {
		t.Fatal("nil sweeper did not use the factory")
	}
}

// signalWriter closes done on the first write: the background sweep's
// completion log line.
type signalWriter struct {
	once sync.Once
	done chan struct{}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "sandbox job boot sweep") {
		w.once.Do(func() { close(w.done) })
	}
	return len(p), nil
}

func TestSandboxJobBootSweepLogsError(t *testing.T) {
	var logBuf bytes.Buffer
	sw := &fakeJobSweeper{err: errors.New("jobs.batch is forbidden")}
	cfg := &config.Config{}
	cfg.AgentSandbox = config.AgentSandboxConfig{Runtime: "job"}
	if !sandboxJobRuntimeConfigured(cfg) {
		t.Fatal("a global job runtime must count as configured")
	}
	runSandboxJobBootSweep(context.Background(), sandboxJobSweepMaxAge(cfg), nil, nil, sw, nil, slog.New(slog.NewTextHandler(&logBuf, nil)))
	if !strings.Contains(logBuf.String(), "sandbox job boot sweep incomplete") {
		t.Fatalf("error not logged: %s", logBuf.String())
	}
	if sw.opts.MaxAge != kubejob.DefaultKickTimeout {
		t.Fatalf("MaxAge = %s, want default", sw.opts.MaxAge)
	}
}

func TestSandboxJobLiveChecksRunningAgents(t *testing.T) {
	cfg := jobSweepConfig()
	rt := cruxtest.NewRuntime(cfg.Agents, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), crux.ProjectContext{})
	live := sandboxJobLive([]string{"reviewer", "scanner"}, rt)
	// Freshly built agents are stopped, so nothing is live.
	if live("hive-reviewer-sandbox") {
		t.Fatal("a stopped agent's Job must not count as live")
	}
	if live("crux-run-abc") {
		t.Fatal("a label matching no agent must not count as live")
	}
}
