package github

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MemoryDrainRequestDir is where an agent drops a request to drain its own
// CLI memory into the knowledge store now (hivecommons/hive#17208). A Claude
// Code hook runs as the agent's UID and can neither write the hive-owned
// vault nor reach the in-process drainer, so it uses the same drop-box as the
// GitHub relays: the file's owning UID is the agent's identity, and the
// handler refuses a request that names another agent.
const MemoryDrainRequestDir = "/var/run/hive-metrics/memory-drain-requests"

// memoryDrainRequestDirForTest lets tests point the watcher at a temp dir.
var memoryDrainRequestDirForTest string

func memoryDrainRequestDir() string {
	if memoryDrainRequestDirForTest != "" {
		return memoryDrainRequestDirForTest
	}
	return MemoryDrainRequestDir
}

// memoryDrainRequestPollInterval is short because a hook waits on the result.
var memoryDrainRequestPollInterval = 2 * time.Second

// MemoryDrainRequest is the JSON an agent writes to MemoryDrainRequestDir.
// It carries no path: the hive resolves the stores from the agent's home.
// Cwd is the hook's working directory, used only to narrow the drain to that
// Claude project. Truncate empties the drained files that are fully stored.
// OnWrite marks a request from the PostToolUse hook, answered as a no-op
// unless agents.cli_memory.drain_on_write is on for the agent.
type MemoryDrainRequest struct {
	Agent    string `json:"agent"`
	Cwd      string `json:"cwd,omitempty"`
	Truncate bool   `json:"truncate,omitempty"`
	OnWrite  bool   `json:"on_write,omitempty"`
}

// MemoryDrainResponse is written next to the consumed request as
// <name>.result.json. OK with Ran false is the no-op answer (the drain is off
// for the agent, or it has nothing to drain).
type MemoryDrainResponse struct {
	OK        bool   `json:"ok"`
	Agent     string `json:"agent,omitempty"`
	Ran       bool   `json:"ran"`
	Store     string `json:"store,omitempty"`
	Drained   int    `json:"drained"`
	Dupes     int    `json:"dupes"`
	Skipped   int    `json:"skipped"`
	Spooled   int    `json:"spooled"`
	Truncated int    `json:"truncated"`
	Kept      int    `json:"kept"`
	Error     string `json:"error,omitempty"`
	At        string `json:"at"`
}

// MemoryDrainHandler runs one request. fileUID is the owner of the request
// file (-1 when unknown); the handler authorizes and audits.
type MemoryDrainHandler func(req MemoryDrainRequest, fileUID int) MemoryDrainResponse

// StartMemoryDrainRequestWatcher polls MemoryDrainRequestDir until ctx is
// cancelled. A nil handler starts nothing. The returned channel closes once
// the loop has exited.
func StartMemoryDrainRequestWatcher(ctx context.Context, logger *slog.Logger, handle MemoryDrainHandler) <-chan struct{} {
	done := make(chan struct{})
	if handle == nil || !ensureRequestDir(logger, "memory-drain", memoryDrainRequestDir()) {
		close(done)
		return done
	}
	interval := memoryDrainRequestPollInterval
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ProcessMemoryDrainRequestsOnce(logger, handle, time.Now)
			}
		}
	}()
	if logger != nil {
		logger.Info("memory-drain-request watcher started", slog.String("dir", memoryDrainRequestDir()))
	}
	return done
}

// ProcessMemoryDrainRequestsOnce runs one scan of the request directory.
func ProcessMemoryDrainRequestsOnce(logger *slog.Logger, handle MemoryDrainHandler, nowFn func() time.Time) {
	entries, err := os.ReadDir(memoryDrainRequestDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		handleOneMemoryDrainRequest(logger, filepath.Join(memoryDrainRequestDir(), name), handle, nowFn)
	}
}

func handleOneMemoryDrainRequest(logger *slog.Logger, path string, handle MemoryDrainHandler, nowFn func() time.Time) {
	data, fi, err := readUntrustedFile(path, requestMaxBytes)
	if err != nil {
		if errors.Is(err, errDropBoxFileRejected) {
			_ = os.Rename(path, path+".rejected")
			if logger != nil {
				logger.Warn("memory-drain-request watcher: REJECTED (unsafe file)",
					slog.String("path", path), slog.String("reason", err.Error()))
			}
		}
		return
	}
	var req MemoryDrainRequest
	if err := json.Unmarshal(data, &req); err != nil {
		if !quarantinable(path, nowFn()) {
			return
		}
		writeMemoryDrainResult(path, MemoryDrainResponse{Error: "invalid JSON: " + err.Error(), At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		return
	}
	resp := handle(req, fileOwnerUID(fi))
	resp.At = nowFn().UTC().Format(time.RFC3339)
	writeMemoryDrainResult(path, resp)
	if !resp.OK && !resp.Ran {
		_ = os.Rename(path, path+".denied")
		return
	}
	_ = os.Remove(path)
}

func writeMemoryDrainResult(reqPath string, resp MemoryDrainResponse) {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if b, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = writeRequestFile(out, b)
	}
}
