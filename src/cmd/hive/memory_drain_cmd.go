package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// memoryDrainSubcommand is the `hive memory-drain` verb the Claude Code
// SessionEnd and PostToolUse hooks run (hivecommons/hive#17209):
//
//	hive memory-drain --cli claude --agent "$HIVE_AGENT"
//
// It runs as the agent's UID inside the agent's session, so it cannot drain
// anything itself. It drops a request into github.MemoryDrainRequestDir and
// waits a bounded time for the hive's <name>.result.json. A hook must never
// fail or stall the session it runs in, so every outcome exits 0 and every
// problem is a warning on stderr.
const memoryDrainSubcommand = "memory-drain"

const memoryDrainUsage = `usage: hive memory-drain --cli claude [--agent <name>] [--wait <duration>]

Run by Claude Code's SessionEnd and PostToolUse hooks, which Hive seeds when
agents.cli_memory.drain_to_knowledge is on. Reads the hook JSON on stdin and
asks the hive to drain this agent's Claude memory into draft knowledge, then
empty the files whose every fact is stored. Always exits 0.
`

// Seams for tests.
var (
	memoryDrainRequestDir  = github.MemoryDrainRequestDir
	memoryDrainPollEvery   = 200 * time.Millisecond
	memoryDrainDefaultWait = 10 * time.Second
)

// claudeHookInput is the part of Claude Code's hook stdin the drain reads.
type claudeHookInput struct {
	Cwd           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	ToolInput     struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

const claudeHookPostToolUse = "PostToolUse"

// memoryDrainMaxStdin caps the hook JSON read; a Write tool_input carries
// the whole file content, which the drain never needs.
const memoryDrainMaxStdin = 4 << 20

var memoryDrainUnsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func runMemoryDrain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(memoryDrainSubcommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, memoryDrainUsage) }
	cli := fs.String("cli", "", "the CLI whose memory to drain (claude)")
	agent := fs.String("agent", os.Getenv("HIVE_AGENT"), "the agent whose memory to drain (default $HIVE_AGENT)")
	wait := fs.Duration("wait", memoryDrainDefaultWait, "how long to wait for the hive's answer")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	warn := func(format string, a ...any) {
		fmt.Fprintf(stderr, "hive %s: warning: "+format+"\n", append([]any{memoryDrainSubcommand}, a...)...)
	}
	if *cli != "claude" {
		warn("unsupported --cli %q; only claude has a memory hook", *cli)
		return 0
	}

	var in claudeHookInput
	data, err := io.ReadAll(io.LimitReader(stdin, memoryDrainMaxStdin))
	if err != nil {
		warn("reading hook input: %v", err)
	}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &in); err != nil {
			warn("hook input is not JSON: %v", err)
		}
	}
	onWrite := in.HookEventName == claudeHookPostToolUse
	if onWrite && !isClaudeMemoryFile(in.ToolInput.FilePath) {
		return 0
	}

	name := strings.TrimSpace(*agent)
	if name == "" {
		warn("no agent: pass --agent or set HIVE_AGENT")
		return 0
	}
	req := github.MemoryDrainRequest{Agent: name, Cwd: in.Cwd, Truncate: true, OnWrite: onWrite}
	reqPath, err := writeMemoryDrainRequest(memoryDrainRequestDir, req)
	if err != nil {
		warn("could not queue the drain request: %v", err)
		return 0
	}
	resp, err := awaitMemoryDrainResult(reqPath, *wait)
	if err != nil {
		warn("%v; the hive drains this memory at the next kick", err)
		return 0
	}
	switch {
	case !resp.OK || resp.Error != "":
		warn("drain refused or incomplete: %s", resp.Error)
	case resp.Ran:
		fmt.Fprintf(stdout, "hive %s: drained %d facts into %s (dupes %d, truncated %d, kept %d)\n",
			memoryDrainSubcommand, resp.Drained, resp.Store, resp.Dupes, resp.Truncated, resp.Kept)
	}
	return 0
}

// isClaudeMemoryFile reports whether path is under
// <claude config dir>/projects/<slug>/memory/, the only writes the
// PostToolUse hook acts on.
func isClaudeMemoryFile(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return false
		}
		base = filepath.Join(home, ".claude")
	}
	rel, err := filepath.Rel(filepath.Join(base, "projects"), filepath.Clean(path))
	if err != nil {
		return false
	}
	parts := strings.SplitN(filepath.ToSlash(rel), "/", 3)
	return len(parts) == 3 && parts[0] != "" && parts[0] != ".." && parts[1] == "memory" && parts[2] != ""
}

// writeMemoryDrainRequest drops req into dir atomically: the temp name does
// not end in .json, so the watcher never reads a half-written request. The
// directory is the hive's to create; a missing one means the drain is off.
func writeMemoryDrainRequest(dir string, req github.MemoryDrainRequest) (string, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("request dir %s is unavailable (is agents.cli_memory.drain_to_knowledge on?)", dir)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d.json", memoryDrainUnsafeName.ReplaceAllString(req.Agent, "_"), time.Now().UnixNano())
	path := filepath.Join(dir, name)
	f, err := os.CreateTemp(dir, "."+name+".tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once renamed
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// awaitMemoryDrainResult polls for the hive's answer to the request at
// reqPath for at most wait.
func awaitMemoryDrainResult(reqPath string, wait time.Duration) (github.MemoryDrainResponse, error) {
	resultPath := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	deadline := time.Now().Add(wait)
	for {
		data, err := os.ReadFile(resultPath)
		if err == nil {
			var resp github.MemoryDrainResponse
			if jerr := json.Unmarshal(data, &resp); jerr == nil {
				_ = os.Remove(resultPath) // best effort; the hive owns the file
				return resp, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return github.MemoryDrainResponse{}, fmt.Errorf("reading the drain result: %w", err)
		}
		if !time.Now().Before(deadline) {
			return github.MemoryDrainResponse{}, fmt.Errorf("no answer from the hive within %s (request %s stays queued)", wait, filepath.Base(reqPath))
		}
		time.Sleep(memoryDrainPollEvery)
	}
}
