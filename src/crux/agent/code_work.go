package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

const (
	codeWorkTranscriptLimit = 8 << 20
	codeWorkFreshWindow     = 2 * time.Minute
	codeWorkGitTimeout      = 2 * time.Second
)

type sourceEdit struct {
	path    string
	at      time.Time
	started time.Time
}

type sourceEditCall struct {
	paths []string
	at    time.Time
}

// ObserveCodeWorkTranscript reads explicit successful edit results, never
// prose or generic shell intentions, then verifies a real source change.
// Unsupported/partial transcripts and unverifiable worktrees stay unknown.
func ObserveCodeWorkTranscript(r io.Reader, workDir string, since, now time.Time) *crux.CodeWork {
	if since.IsZero() {
		return nil
	}
	calls := map[string]sourceEditCall{}
	var edits []sourceEdit
	err := forEachTranscriptLine(io.LimitReader(r, codeWorkTranscriptLimit), func(line []byte) error {
		var event copilotTranscriptEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return err
		}
		at := parseTranscriptTime(event.Timestamp)
		if at.IsZero() || at.Before(since) || at.After(now) {
			return nil
		}
		switch event.Type {
		case "assistant.message":
			for _, request := range event.Data.ToolRequests {
				if request.ToolCallID == "" {
					continue
				}
				calls[request.ToolCallID] = sourceEditCall{paths: sourceEditPaths(request.Name, request.Arguments), at: at}
			}
		case "tool.execution_start":
			if event.Data.ToolCallID == "" {
				return nil
			}
			calls[event.Data.ToolCallID] = sourceEditCall{paths: sourceEditPaths(event.Data.ToolName, event.Data.Arguments), at: at}
		case "tool.execution_complete":
			if event.Data.ToolCallID == "" || event.Data.Success == nil || !*event.Data.Success ||
				transcriptText(event.Data.Error) != "" || now.Sub(at) > codeWorkFreshWindow {
				return nil
			}
			call := calls[event.Data.ToolCallID]
			for _, path := range call.paths {
				edits = append(edits, sourceEdit{path: path, at: at, started: call.at})
			}
			delete(calls, event.Data.ToolCallID)
		}
		return nil
	})
	if err != nil {
		return nil
	}
	for i := len(edits) - 1; i >= 0; i-- {
		if observation := verifySourceEdit(edits[i], now); observation != nil {
			observation.KickAt = since
			return observation
		}
	}
	return nil
}

func sourceEditPaths(name string, arguments json.RawMessage) []string {
	var paths []string
	switch name {
	case "edit", "create", "Edit", "Write":
		var args struct {
			Path     string `json:"path"`
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(arguments, &args) != nil {
			return nil
		}
		path := args.Path
		if path == "" {
			path = args.FilePath
		}
		if filepath.IsAbs(path) {
			paths = append(paths, filepath.Clean(path))
		}
	case "apply_patch":
		var patch string
		if json.Unmarshal(arguments, &patch) != nil {
			var args struct {
				Input string `json:"input"`
				Patch string `json:"patch"`
			}
			if json.Unmarshal(arguments, &args) != nil {
				return nil
			}
			patch = args.Input
			if patch == "" {
				patch = args.Patch
			}
		}
		for _, line := range strings.Split(patch, "\n") {
			for _, prefix := range []string{"*** Update File: ", "*** Add File: "} {
				if strings.HasPrefix(line, prefix) {
					path := strings.TrimPrefix(line, prefix)
					// A tool may have changed CWD since launch. Relative paths
					// are intentionally not attributed using a guessed directory.
					if filepath.IsAbs(path) {
						paths = append(paths, filepath.Clean(path))
					}
				}
			}
		}
	}
	return paths
}

func verifySourceEdit(edit sourceEdit, now time.Time) *crux.CodeWork {
	info, err := os.Lstat(edit.path)
	if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(edit.started) || info.ModTime().After(edit.at) || info.ModTime().After(now) {
		return nil
	}

	git := func(args ...string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), codeWorkGitTimeout)
		defer cancel()
		command := exec.CommandContext(ctx, "git", append([]string{"-C", filepath.Dir(edit.path)}, args...)...)
		command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
		bytes, err := command.Output()
		return strings.TrimSpace(string(bytes)), err == nil
	}
	root, ok := git("rev-parse", "--show-toplevel")
	if !ok {
		return nil
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	canonicalFile, err := filepath.EvalSymlinks(edit.path)
	if err != nil {
		return nil
	}
	relative, err := filepath.Rel(root, canonicalFile)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	head, ok := git("rev-parse", "HEAD")
	if !ok || head == "" {
		return nil
	}
	origin, ok := git("remote", "get-url", "origin")
	if !ok {
		return nil
	}
	if strings.HasPrefix(origin, "git@") {
		parts := strings.SplitN(origin, ":", 2)
		if len(parts) != 2 {
			return nil
		}
		origin = "ssh://" + strings.TrimPrefix(parts[0], "git@") + "/" + parts[1]
	}
	remote, err := url.Parse(origin)
	if err != nil || remote.Hostname() == "" {
		return nil
	}
	repo := strings.TrimSuffix(strings.Trim(remote.Path, "/"), ".git")
	if len(strings.Split(repo, "/")) != 2 {
		return nil
	}
	// The successful tool receipt alone is insufficient: the file must
	// actually differ from HEAD or be a nonignored new source file.
	changed, ok := git("diff", "--name-only", "HEAD", "--", canonicalFile)
	if !ok {
		return nil
	}
	if changed == "" {
		changed, ok = git("ls-files", "--others", "--exclude-standard", "--", canonicalFile)
	}
	if !ok || changed == "" {
		return nil
	}
	return &crux.CodeWork{Repo: repo, Head: head, Path: relative, Source: crux.CodeWorkSource, At: edit.at, ObservedAt: now}
}

func cloneCodeWork(observation *crux.CodeWork) *crux.CodeWork {
	if observation == nil {
		return nil
	}
	copy := *observation
	return &copy
}

func scanCodeWork(target kickTranscriptTarget, cache *transcriptSessionCache, now time.Time) *crux.CodeWork {
	if target.backend != "copilot" || target.since.IsZero() || cache == nil {
		return nil
	}
	// Reuse the existing liveness scan's session index; never repeat the
	// potentially large resume-handle walk or scan on dashboard reads.
	name := ""
	var newest time.Time
	ambiguous := false
	for session, at := range cache.sessions {
		if at.Before(target.since) || at.After(now) || now.Sub(at) > codeWorkFreshWindow {
			continue
		}
		if at.After(newest) {
			name, newest, ambiguous = session, at, false
		} else if at.Equal(newest) {
			ambiguous = true
		}
	}
	if name == "" || ambiguous {
		return nil
	}
	path := filepath.Join(AgentHome(target.agentName, target.uid, "copilot"), ".copilot", "session-state", name, "events.jsonl")
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > codeWorkTranscriptLimit {
		return nil
	}
	return ObserveCodeWorkTranscript(file, target.workDir, target.since, now)
}
