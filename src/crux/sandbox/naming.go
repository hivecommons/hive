package sandbox

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Shared naming between the sandbox kick executor (crux/agent) and the
// dashboard's job-runtime terminal pane (pkg/dashboard), which must find a
// kick's Job pod and persisted transcript knowing only the agent's name
// (hivecommons/hive#14798). The dashboard deliberately does not import
// crux/agent, so the conventions live here where both can reach them.

const (
	// TranscriptRelPath is where, relative to a kick's workspace, the
	// executor persists the scrubbed stdout/stderr of the run.
	TranscriptRelPath = ".hive/sandbox-transcript.log"
	// TranscriptExitPrefix opens the transcript's trailing "exit code: N"
	// line, written after stdout and stderr.
	TranscriptExitPrefix = "exit code: "

	// DefaultWorkDir is the agent manager's work directory when HIVE_WORK_DIR
	// is unset; the sandbox workspace root defaults to its "sandbox" child.
	DefaultWorkDir      = "/data/agents"
	workspaceRootSubdir = "sandbox"
	launchNamePrefix    = "hive-"
	launchNameSuffix    = "-sandbox"
	fallbackSlug        = "agent"
)

// AgentSlug is the lower-case [a-z0-9-] form of an agent name used in launch
// names, Job labels, branch names and workspace directory names.
func AgentSlug(agent string) string {
	s := strings.TrimSpace(strings.ToLower(agent))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return fallbackSlug
	}
	return out
}

// LaunchName is the LaunchSpec.Name the executor uses for an agent's kicks:
// hive-<slug>-sandbox. The kubejob launcher derives its Job name and agent
// label from it.
func LaunchName(agent string) string {
	return launchNamePrefix + AgentSlug(agent) + launchNameSuffix
}

// WorkspaceRoot resolves the directory kick workspaces are created under:
// the configured agent_sandbox.workspace_dir when set, else <workDir>/sandbox,
// with workDir itself defaulting to DefaultWorkDir. The agent manager and the
// dashboard both resolve it here so they always agree on where a kick's
// transcript lives.
func WorkspaceRoot(configured, workDir string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	if strings.TrimSpace(workDir) == "" {
		workDir = DefaultWorkDir
	}
	return filepath.Join(workDir, workspaceRootSubdir)
}

// WorkspaceDir is the per-kick workspace directory the executor creates
// under root: <root>/<slug>-<unixnano>.
func WorkspaceDir(root, agent string, unixNano int64) string {
	return filepath.Join(root, AgentSlug(agent)+"-"+strconv.FormatInt(unixNano, 10))
}

// LatestTranscript returns the newest persisted transcript for agent under
// the workspace root, by file modification time, or ok=false when the agent
// has no run with a transcript yet.
func LatestTranscript(root, agent string) (path string, ok bool) {
	slug := AgentSlug(agent)
	matches, err := filepath.Glob(filepath.Join(root, slug+"-*", filepath.FromSlash(TranscriptRelPath)))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	type cand struct {
		path string
		mod  int64
	}
	cands := make([]cand, 0, len(matches))
	for _, m := range matches {
		// "deps-*" also matches "deps-quality-<ts>": only a purely numeric
		// suffix is one of THIS agent's kick directories.
		dir := filepath.Base(filepath.Dir(filepath.Dir(m)))
		if _, err := strconv.ParseInt(strings.TrimPrefix(dir, slug+"-"), 10, 64); err != nil {
			continue
		}
		st, err := os.Stat(m)
		if err != nil || st.IsDir() {
			continue
		}
		cands = append(cands, cand{m, st.ModTime().UnixNano()})
	}
	if len(cands) == 0 {
		return "", false
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].mod != cands[j].mod {
			return cands[i].mod > cands[j].mod
		}
		return cands[i].path > cands[j].path
	})
	return cands[0].path, true
}

// TranscriptExitCode reads the trailing "exit code: N" line a transcript
// ends with. ok is false for transcripts written before the line existed or
// otherwise lacking it.
func TranscriptExitCode(transcript string) (code int, ok bool) {
	trimmed := strings.TrimRight(transcript, "\n")
	i := strings.LastIndexByte(trimmed, '\n')
	last := trimmed[i+1:]
	if !strings.HasPrefix(last, TranscriptExitPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(last, TranscriptExitPrefix)))
	if err != nil {
		return 0, false
	}
	return n, true
}
