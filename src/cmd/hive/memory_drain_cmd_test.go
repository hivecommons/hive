package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge/climemory"
)

func withMemoryDrainRequestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prevDir, prevPoll := memoryDrainRequestDir, memoryDrainPollEvery
	memoryDrainRequestDir, memoryDrainPollEvery = dir, 10*time.Millisecond
	t.Cleanup(func() { memoryDrainRequestDir, memoryDrainPollEvery = prevDir, prevPoll })
	return dir
}

func hookJSON(t *testing.T, event, cwd, filePath string) string {
	t.Helper()
	in := map[string]any{"hook_event_name": event, "cwd": cwd}
	if filePath != "" {
		in["tool_input"] = map[string]any{"file_path": filePath, "content": "x"}
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMemoryDrainDispatched(t *testing.T) {
	withMemoryDrainRequestDir(t)
	var stdout, stderr bytes.Buffer
	handled, code := dispatchSubcommand([]string{memoryDrainSubcommand, "--cli", "codex"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d", handled, code)
	}
	if !strings.Contains(stderr.String(), "unsupported --cli") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestMemoryDrainPostToolUseOutsideMemoryExitsWithoutRequest(t *testing.T) {
	dir := withMemoryDrainRequestDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, p := range []string{
		"/data/agents/scanner/acme/main.go",
		filepath.Join(home, ".claude", "projects", "-x", "notes", "MEMORY.md"),
		filepath.Join(home, ".claude", "projects", "-x", "memory"),
		filepath.Join(home, ".claude", "settings.json"),
		"relative/memory/MEMORY.md",
		"",
	} {
		var stdout, stderr bytes.Buffer
		start := time.Now()
		code := runMemoryDrain([]string{"--cli", "claude", "--agent", "scanner", "--wait", "5s"},
			strings.NewReader(hookJSON(t, "PostToolUse", "/data/agents/scanner/acme", p)), &stdout, &stderr)
		if code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("%q: code=%d stdout=%q stderr=%q", p, code, stdout.String(), stderr.String())
		}
		if time.Since(start) > time.Second {
			t.Fatalf("%q: took %s, want an immediate exit", p, time.Since(start))
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("requests written for writes outside the memory dir: %v", entries)
	}
}

func TestMemoryDrainWritesRequestAndTimesOutQuietly(t *testing.T) {
	dir := withMemoryDrainRequestDir(t)
	t.Setenv("HIVE_AGENT", "scanner")
	var stdout, stderr bytes.Buffer
	code := runMemoryDrain([]string{"--cli", "claude", "--wait", "50ms"},
		strings.NewReader(hookJSON(t, "SessionEnd", "/data/agents/scanner/acme", "")), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stderr.String(), "no answer from the hive") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "scanner-*.json"))
	if len(matches) != 1 {
		t.Fatalf("requests = %v", matches)
	}
	if fi, err := os.Stat(matches[0]); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("request mode = %v, err=%v", fi, err)
	}
	var req github.MemoryDrainRequest
	b, _ := os.ReadFile(matches[0])
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	if req != (github.MemoryDrainRequest{Agent: "scanner", Cwd: "/data/agents/scanner/acme", Truncate: true}) {
		t.Fatalf("request = %+v", req)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".*")); len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestMemoryDrainMissingRequestDirWarns(t *testing.T) {
	withMemoryDrainRequestDir(t)
	memoryDrainRequestDir = filepath.Join(t.TempDir(), "absent")
	var stdout, stderr bytes.Buffer
	code := runMemoryDrain([]string{"--cli", "claude", "--agent", "scanner"}, strings.NewReader(`{"hook_event_name":"SessionEnd"}`), &stdout, &stderr)
	if code != 0 || !strings.Contains(stderr.String(), "could not queue") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(memoryDrainRequestDir); !os.IsNotExist(err) {
		t.Fatalf("the client must not create the hive's request dir: %v", err)
	}
}

// drainNowViaDrainer is a CLIMemoryDrainNowHost that resolves the agent's
// Claude memory dir the way the manager does and runs the installed drainer.
type drainNowViaDrainer struct {
	fn     crux.CLIMemoryDrainer
	memDir string
	got    []crux.CLIMemoryDrainNowRequest
}

func (h *drainNowViaDrainer) DrainCLIMemoryNow(req crux.CLIMemoryDrainNowRequest) (crux.CLIMemoryDrainResult, bool, error) {
	h.got = append(h.got, req)
	return h.fn(crux.CLIMemoryDrainRequest{
		Agent: req.Agent, Backend: "claude", Truncate: req.Truncate,
		Sources: []crux.CLIMemorySource{{CLI: climemory.CLIClaude, Path: h.memDir}},
	}), true, nil
}

// TestMemoryDrainHookEndToEnd: a PostToolUse hook on MEMORY.md queues a
// request, the hive side drains it into a draft fact in the bead-synth vault
// and empties MEMORY.md, and the hook reports the result.
func TestMemoryDrainHookEndToEnd(t *testing.T) {
	cfg := bootKnowledgeConfig(t)
	cfg.AgentsCLIMemory.DrainToKnowledge = true
	cfg.AgentsCLIMemory.DrainOnWrite = true
	prevSpool := cliMemorySpoolDir
	cliMemorySpoolDir = t.TempDir()
	t.Cleanup(func() { cliMemorySpoolDir = prevSpool })
	b := newBootKnowledgeBoot(t, cfg)
	rt := &drainCapturingRuntime{hostedRuntime: b.agentMgr}
	b.agentMgr = rt
	b.bootKnowledgeWith(newBootKnowledgeFake(t).deps)
	b.installCLIMemoryDrainer()
	if rt.fn == nil {
		t.Fatal("drainer not installed")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	memDir := filepath.Join(home, ".claude", "projects", "-data-agents-scanner-acme-widgets", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	memFile := filepath.Join(memDir, "MEMORY.md")
	if err := os.WriteFile(memFile, []byte("- The widgets build needs CGO_ENABLED=1 on CI runners\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := withMemoryDrainRequestDir(t)
	host := &drainNowViaDrainer{fn: rt.fn, memDir: memDir}
	handle := cliMemoryDrainRequestHandler(host, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			reqs, _ := filepath.Glob(filepath.Join(dir, "*.json"))
			for _, p := range reqs {
				if strings.HasSuffix(p, ".result.json") {
					continue
				}
				var req github.MemoryDrainRequest
				data, err := os.ReadFile(p)
				if err != nil || json.Unmarshal(data, &req) != nil {
					continue
				}
				out, _ := json.Marshal(handle(req, os.Getuid()))
				_ = os.WriteFile(strings.TrimSuffix(p, ".json")+".result.json", out, 0o644)
				_ = os.Remove(p)
			}
		}
	}()

	var stdout, stderr bytes.Buffer
	code := runMemoryDrain([]string{"--cli", "claude", "--agent", "scanner", "--wait", "10s"},
		strings.NewReader(hookJSON(t, "PostToolUse", "/data/agents/scanner/acme.widgets", memFile)), &stdout, &stderr)
	cancel()
	<-served
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "drained 1 facts") || !strings.Contains(stdout.String(), "truncated 1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(host.got) != 1 || !host.got[0].OnWrite || !host.got[0].Truncate ||
		host.got[0].Agent != "scanner" || host.got[0].Cwd != "/data/agents/scanner/acme.widgets" {
		t.Fatalf("drain-now requests = %+v", host.got)
	}

	entries, err := filepath.Glob(filepath.Join(b.beadSynthVaultPath(), "cli-memory-*.md"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("vault entries = %v, err=%v", entries, err)
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state: draft", "agent: scanner", "CGO_ENABLED=1"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("entry missing %q:\n%s", want, data)
		}
	}
	if fi, err := os.Stat(memFile); err != nil || fi.Size() != 0 {
		t.Fatalf("MEMORY.md not emptied: %v, err=%v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("request dir not cleaned up: %v", left)
	}
}
