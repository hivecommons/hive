package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

const (
	// sandboxRunSummaryMax bounds SandboxRun.Summary so a run record, which
	// is persisted and shipped in every status snapshot, never carries a
	// whole transcript.
	sandboxRunSummaryMax = 2000
	// sandboxRunSummaryTailLines is how much of the transcript tail stands in
	// for the summary when the agent left no report.
	sandboxRunSummaryTailLines = 20
)

func sandboxRunPath(workDir, agent string) string {
	return filepath.Join(crux.SandboxRunsDir(workDir), sanitizeBranchPart(agent)+".json")
}

func cloneSandboxRun(r *crux.SandboxRun) *crux.SandboxRun {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

// buildSandboxRun turns an executor result into the run record the dashboard
// and the token collector read.
func buildSandboxRun(runtime string, started, ended time.Time, res SandboxKickResult, runErr error) crux.SandboxRun {
	run := crux.SandboxRun{
		Runtime:     runtime,
		StartedAt:   started,
		EndedAt:     ended,
		Succeeded:   runErr == nil,
		TimedOut:    res.TimedOut,
		CommitCount: res.CommitCount,
		Branch:      res.Branch,
		Usage:       parseSandboxUsage(res.Sandbox.Stdout),
	}
	if runErr != nil {
		run.Error = truncateRunes(runErr.Error(), sandboxRunSummaryMax)
	}
	if res.PR != nil {
		run.PRURL = res.PR.URL
	}
	if res.Report != nil && strings.TrimSpace(res.Report.Summary) != "" {
		run.Summary = truncateRunes(strings.TrimSpace(res.Report.Summary), sandboxRunSummaryMax)
	} else {
		run.Summary = sandboxTranscriptTail(res.Sandbox.Stdout, res.Sandbox.Stderr)
	}
	return run
}

// sandboxTranscriptTail is the last non-empty lines of the transcript, stdout
// first and stderr only when stdout had nothing to say.
func sandboxTranscriptTail(stdout, stderr string) string {
	src := stdout
	if strings.TrimSpace(src) == "" {
		src = stderr
	}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	kept := make([]string, 0, sandboxRunSummaryTailLines)
	for i := len(lines) - 1; i >= 0 && len(kept) < sandboxRunSummaryTailLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		kept = append([]string{strings.TrimRight(lines[i], " \t")}, kept...)
	}
	return truncateRunes(strings.Join(kept, "\n"), sandboxRunSummaryMax)
}

var (
	// copilotUsageLineRe matches one row of the Copilot CLI's non-interactive
	// footer, e.g. " claude-sonnet-4.5   12.3k in, 456 out, 1.2k cached".
	copilotUsageLineRe = regexp.MustCompile(`^\s*(\S+)\s+([\d.,]+[kKmM]?)\s+in,\s*([\d.,]+[kKmM]?)\s+out(?:,\s*([\d.,]+[kKmM]?)\s+cached)?`)
	copilotUsageHeader = "Breakdown by AI model"
)

// parseSandboxUsage recovers token usage from a sandbox transcript. It
// understands the Copilot CLI's "Breakdown by AI model" footer and the Claude
// Code JSON / stream-json result record ({"type":"result","usage":{...}}).
// A transcript with neither yields a zero usage, never an estimate.
func parseSandboxUsage(stdout string) crux.SandboxRunUsage {
	if u, ok := parseClaudeJSONUsage(stdout); ok {
		return u
	}
	if u, ok := parseCopilotFooterUsage(stdout); ok {
		return u
	}
	return crux.SandboxRunUsage{}
}

func parseClaudeJSONUsage(stdout string) (crux.SandboxRunUsage, bool) {
	type usage struct {
		Input       int64 `json:"input_tokens"`
		Output      int64 `json:"output_tokens"`
		CacheRead   int64 `json:"cache_read_input_tokens"`
		CacheCreate int64 `json:"cache_creation_input_tokens"`
	}
	type result struct {
		Type       string                     `json:"type"`
		Usage      *usage                     `json:"usage"`
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	}
	var found crux.SandboxRunUsage
	ok := false
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.Contains(line, `"usage"`) {
			continue
		}
		var r result
		if json.Unmarshal([]byte(line), &r) != nil || r.Type != "result" || r.Usage == nil {
			continue
		}
		found = crux.SandboxRunUsage{Input: r.Usage.Input, Output: r.Usage.Output, CacheRead: r.Usage.CacheRead, CacheCreate: r.Usage.CacheCreate}
		for model := range r.ModelUsage {
			found.Model = model
			break
		}
		ok = found.Total() > 0
	}
	return found, ok
}

func parseCopilotFooterUsage(stdout string) (crux.SandboxRunUsage, bool) {
	idx := strings.LastIndex(stdout, copilotUsageHeader)
	if idx < 0 {
		return crux.SandboxRunUsage{}, false
	}
	var total crux.SandboxRunUsage
	for _, line := range strings.Split(stdout[idx+len(copilotUsageHeader):], "\n") {
		m := copilotUsageLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if total.Model == "" {
			total.Model = m[1]
		}
		total.Input += parseHumanCount(m[2])
		total.Output += parseHumanCount(m[3])
		total.CacheRead += parseHumanCount(m[4])
	}
	return total, total.Total() > 0
}

// parseHumanCount reads "12.3k", "1,204" or "2M" as an integer count.
func parseHumanCount(s string) int64 {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" {
		return 0
	}
	mult := 1.0
	switch s[len(s)-1] {
	case 'k', 'K':
		mult, s = 1000, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1000000, s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f*mult + 0.5)
}

// SandboxRunHistoryEntry is one line of runs.jsonl: the run plus the agent
// it belongs to and the backend/model it ran on, which the token collector
// needs and the run record itself does not carry.
type SandboxRunHistoryEntry struct {
	Agent   string          `json:"agent"`
	Backend string          `json:"backend,omitempty"`
	Model   string          `json:"model,omitempty"`
	Run     crux.SandboxRun `json:"run"`
}

// persistSandboxRun writes the agent's last-run file and appends the run to
// the history. Failures are returned for logging only; the in-memory record
// is already set.
func persistSandboxRun(workDir string, entry SandboxRunHistoryEntry) error {
	dir := crux.SandboxRunsDir(workDir)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return err
	}
	last, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := sandboxRunPath(workDir, entry.Agent)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, last, 0o660); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	f, err := os.OpenFile(crux.SandboxRunHistoryPath(workDir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o660)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(last, '\n'))
	return err
}

// loadSandboxRun reads the agent's persisted last run; (nil, nil) when none.
func loadSandboxRun(workDir, agent string) (*crux.SandboxRun, error) {
	data, err := os.ReadFile(sandboxRunPath(workDir, agent))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entry SandboxRunHistoryEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, err
	}
	if entry.Run.StartedAt.IsZero() {
		return nil, nil
	}
	return &entry.Run, nil
}
