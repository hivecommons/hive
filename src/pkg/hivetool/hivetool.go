// Package hivetool is the read side of hive-tool/v1 (RFC #18464): the tool
// surface Hive hosts for credential-less backends (stateless free-tier LLMs
// driven by turn.Runner, hosted sessions' custom tools, chat bridges). The
// remote model holds no credential; it emits a tool call, toolapprove
// classifies it (crux/toolapprove hive-tool classes), and this Executor runs
// it against a repository pinned at construction.
//
// Only read tools and the terminal markers are implemented here. Write tools
// (issue.comment, repo.patch_files, pr.create, ...) belong behind pushbroker
// and AuthorizePROpen and are deliberately absent: calling one returns an
// error naming the tools that are available.
//
// Design rules measured in live runs (RFC #18464 comment 2026-11-16):
//   - every output is bounded (MaxOutputChars) with an explicit "narrow the
//     request" hint, because context re-send is the binding cost;
//   - search must be a Hive-owned index (CodeSearcher), not GitHub code
//     search, which returned nothing for exact strings and made models loop;
//   - an identical repeated call is answered with a pointer to the earlier
//     result instead of re-spending context;
//   - a session ends only on task.complete / task.abort (Outcome), never on
//     a prose answer.
package hivetool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/turn"
)

// Protocol is the wire protocol identifier.
const Protocol = "hive-tool/v1"

// Tool names. They match crux/toolapprove's hive-tool classes.
const (
	ToolIssueRead    = "issue.read"
	ToolRepoList     = "repo.list_files"
	ToolRepoRead     = "repo.read_file"
	ToolRepoSearch   = "repo.search_code"
	ToolChecksRead   = "checks.read"
	ToolTaskComplete = "task.complete"
	ToolTaskAbort    = "task.abort"
)

// Limits.
const (
	MaxOutputChars   = 12000
	MaxReadLines     = 200
	MaxSearchResults = 30
	MaxListEntries   = 300
)

// Issue is the view issue.read returns.
type Issue struct {
	Number int
	Title  string
	Body   string
	Labels []string
	State  string
}

// Entry is one directory entry.
type Entry struct {
	Path string
	Type string // "file" | "dir"
}

// CheckRun is one check run on a ref.
type CheckRun struct {
	Name       string
	Status     string
	Conclusion string
	Summary    string
}

// Reader is the read-only repository surface. ref is the pinned base ref.
type Reader interface {
	Issue(ctx context.Context, repo string, number int) (Issue, error)
	List(ctx context.Context, repo, ref, dir string) ([]Entry, error)
	File(ctx context.Context, repo, ref, file string) ([]byte, error)
	Checks(ctx context.Context, repo, ref string) ([]CheckRun, error)
}

// Hit is one search result.
type Hit struct {
	Path string
	Line int
	Text string
}

// CodeSearcher answers repo.search_code. Back it with a Hive-owned index
// (CodeIndexSearcher) rather than GitHub code search.
type CodeSearcher interface {
	Search(ctx context.Context, repo, ref, query, dir string, limit int) ([]Hit, error)
}

// Outcome is the terminal result a session declared.
type Outcome struct {
	Tool     string `json:"tool"` // task.complete | task.abort
	Summary  string `json:"summary"`
	Reason   string `json:"reason,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
	// Citations are the summary's path:line references, each marked
	// verified when the session served that location.
	Citations []Citation `json:"citations,omitempty"`
}

// Executor implements turn.ToolExecutor for the read tools.
type Executor struct {
	Repo     string // owner/name, pinned: the model cannot choose another repo
	Ref      string // base ref reads resolve against
	Reader   Reader
	Searcher CodeSearcher // optional; repo.search_code errors when nil
	// RequireEvidence makes task.complete refuse a summary that cites no
	// path:line this session actually served. Measured live: a weak model
	// "completed" a triage in two turns by paraphrasing the issue without
	// reading any code.
	RequireEvidence bool

	mu       sync.Mutex
	seen     map[string]string // call fingerprint -> first call id
	evidence map[string][]span // path -> served line spans
	outcome  *Outcome
}

var _ turn.ToolExecutor = (*Executor)(nil)

// Outcome reports the terminal declaration, if any.
func (x *Executor) Outcome() (Outcome, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.outcome == nil {
		return Outcome{}, false
	}
	return *x.outcome, true
}

// ErrUnknownTool is returned for a name outside the implemented set.
var ErrUnknownTool = errors.New("hivetool: unknown or unavailable tool")

// Execute runs one approved tool call.
func (x *Executor) Execute(ctx context.Context, call turn.ToolCall) (turn.ToolResult, error) {
	res := turn.ToolResult{ToolCallID: call.ID, Name: call.Name}
	args := map[string]any{}
	if strings.TrimSpace(call.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return res, fmt.Errorf("arguments are not a JSON object: %v", err)
		}
	}
	if first, dup := x.remember(call); dup {
		return res, fmt.Errorf("duplicate call: %s with identical arguments already ran (call %s); its result is earlier in the conversation. Use it, try something different, or call %s", call.Name, first, ToolTaskComplete)
	}
	var out string
	var err error
	switch call.Name {
	case ToolIssueRead:
		out, err = x.issueRead(ctx, args)
	case ToolRepoList:
		out, err = x.repoList(ctx, args)
	case ToolRepoRead:
		out, err = x.repoRead(ctx, args)
	case ToolRepoSearch:
		out, err = x.repoSearch(ctx, args)
	case ToolChecksRead:
		out, err = x.checksRead(ctx, args)
	case ToolTaskComplete, ToolTaskAbort:
		out, err = x.terminal(call.Name, args)
	default:
		err = fmt.Errorf("%w %q; available: %s", ErrUnknownTool, call.Name, strings.Join(ReadToolNames(), ", "))
	}
	if err != nil {
		x.forget(call)
		return res, err
	}
	res.Output = Bound(out)
	return res, nil
}

func fingerprint(call turn.ToolCall) string {
	var v any
	if json.Unmarshal([]byte(call.Arguments), &v) == nil {
		if b, err := json.Marshal(v); err == nil { // canonical key order
			return call.Name + "\x00" + string(b)
		}
	}
	return call.Name + "\x00" + call.Arguments
}

func (x *Executor) remember(call turn.ToolCall) (string, bool) {
	if call.Name == ToolTaskComplete || call.Name == ToolTaskAbort {
		return "", false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.seen == nil {
		x.seen = map[string]string{}
	}
	fp := fingerprint(call)
	if first, ok := x.seen[fp]; ok {
		return first, true
	}
	x.seen[fp] = call.ID
	return "", false
}

// forget lets a failed call be retried (e.g. after a transient error).
func (x *Executor) forget(call turn.ToolCall) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.seen, fingerprint(call))
}

// Bound truncates s to MaxOutputChars with a hint to narrow the request.
func Bound(s string) string {
	if len(s) <= MaxOutputChars {
		return s
	}
	cut := MaxOutputChars
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n…[truncated %d chars: narrow the request (line range, path)]", len(s)-cut)
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func argString(a map[string]any, k string) string {
	switch v := a[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return fmt.Sprint(int(v))
	}
	return ""
}

// argInt accepts numbers and numeric strings (models send both).
func argInt(a map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := a[k].(type) {
		case float64:
			return int(v)
		case string:
			var n int
			if _, err := fmt.Sscan(strings.TrimSpace(v), &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// cleanPath confines a model-supplied path to the repository.
func cleanPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	if strings.HasPrefix(p, "/") {
		p = strings.TrimLeft(p, "/")
	}
	if p == "" || p == "." {
		return "", nil
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q escapes the repository", p)
	}
	return c, nil
}

func (x *Executor) issueRead(ctx context.Context, a map[string]any) (string, error) {
	n := argInt(a, "number", "issue", "issue_number")
	if n <= 0 {
		return "", errors.New("number is required")
	}
	is, err := x.Reader.Issue(ctx, x.Repo, n)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("#%d [%s] %s\nlabels: %s\n\n%s", is.Number, is.State, is.Title, strings.Join(is.Labels, ", "), is.Body), nil
}

func (x *Executor) repoList(ctx context.Context, a map[string]any) (string, error) {
	dir, err := cleanPath(argString(a, "path"))
	if err != nil {
		return "", err
	}
	entries, err := x.Reader.List(ctx, x.Repo, x.Ref, dir)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	var b strings.Builder
	for i, e := range entries {
		if i == MaxListEntries {
			fmt.Fprintf(&b, "…%d more entries; list a subdirectory\n", len(entries)-i)
			break
		}
		fmt.Fprintf(&b, "%s %s\n", e.Type, e.Path)
	}
	if b.Len() == 0 {
		return "(empty directory)", nil
	}
	return b.String(), nil
}

func (x *Executor) repoRead(ctx context.Context, a map[string]any) (string, error) {
	file, err := cleanPath(argString(a, "path"))
	if err != nil {
		return "", err
	}
	if file == "" {
		return "", errors.New("path is required")
	}
	raw, err := x.Reader.File(ctx, x.Repo, x.Ref, file)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	start := argInt(a, "start_line", "line_start", "from")
	end := argInt(a, "end_line", "line_end", "to")
	if start < 1 {
		start = 1
	}
	if start > len(lines) {
		return "", fmt.Errorf("%s has %d lines; start_line %d is past the end", file, len(lines), start)
	}
	if end < start || end > len(lines) {
		end = len(lines)
	}
	if end-start+1 > MaxReadLines {
		end = start + MaxReadLines - 1
	}
	x.mu.Lock()
	x.recordEvidence(file, start, end)
	x.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "%s lines %d-%d of %d (ref %s)\n", file, start, end, len(lines), x.Ref)
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i, lines[i-1])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "…continue with start_line=%d\n", end+1)
	}
	return b.String(), nil
}

func (x *Executor) repoSearch(ctx context.Context, a map[string]any) (string, error) {
	if x.Searcher == nil {
		return "", errors.New("code search is not available on this hive")
	}
	q := argString(a, "query")
	if q == "" {
		return "", errors.New("query is required")
	}
	dir, err := cleanPath(argString(a, "path"))
	if err != nil {
		return "", err
	}
	hits, err := x.Searcher.Search(ctx, x.Repo, x.Ref, q, dir, MaxSearchResults+1)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return fmt.Sprintf("no matches for %q%s; try a shorter identifier or another term", q, inDir(dir)), nil
	}
	var b strings.Builder
	x.mu.Lock()
	for i, h := range hits {
		if i == MaxSearchResults {
			break
		}
		x.recordEvidence(h.Path, h.Line, h.Line)
	}
	x.mu.Unlock()
	for i, h := range hits {
		if i == MaxSearchResults {
			b.WriteString("…more matches; narrow with path\n")
			break
		}
		fmt.Fprintf(&b, "%s:%d: %s\n", h.Path, h.Line, strings.TrimSpace(h.Text))
	}
	return b.String(), nil
}

func inDir(dir string) string {
	if dir == "" {
		return ""
	}
	return " under " + dir
}

func (x *Executor) checksRead(ctx context.Context, a map[string]any) (string, error) {
	ref := argString(a, "ref")
	if ref == "" {
		ref = x.Ref
	}
	runs, err := x.Reader.Checks(ctx, x.Repo, ref)
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "no check runs on " + ref, nil
	}
	var b strings.Builder
	for _, r := range runs {
		fmt.Fprintf(&b, "%s: %s/%s", r.Name, r.Status, r.Conclusion)
		if s := strings.TrimSpace(r.Summary); s != "" {
			fmt.Fprintf(&b, " — %s", s)
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func (x *Executor) terminal(tool string, a map[string]any) (string, error) {
	o := Outcome{Tool: tool, Summary: argString(a, "summary"), Reason: argString(a, "reason"), PRNumber: argInt(a, "pr_number")}
	if tool == ToolTaskComplete && o.Summary == "" {
		return "", errors.New("summary is required")
	}
	if tool == ToolTaskAbort && o.Reason == "" && o.Summary == "" {
		return "", errors.New("reason is required")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.outcome != nil {
		return "", fmt.Errorf("session already ended with %s", x.outcome.Tool)
	}
	o.Citations = x.citationsLocked(o.Summary)
	verified := 0
	for _, c := range o.Citations {
		if c.Verified {
			verified++
		}
	}
	if tool == ToolTaskComplete && x.RequireEvidence && verified == 0 {
		return "", fmt.Errorf("task.complete refused: the summary cites no path:line this session read or found by search (read so far: %s). Read the responsible code, then cite it as path:line", x.evidenceSummaryLocked())
	}
	x.outcome = &o
	return fmt.Sprintf("recorded (%d of %d citations verified); the session is over, do not call further tools", verified, len(o.Citations)), nil
}

// ---- tool definitions ------------------------------------------------------

type def struct {
	name, desc string
	props      map[string]map[string]any
	required   []string
}

var defs = []def{
	{ToolIssueRead, "Read a GitHub issue of this repository: title, state, labels and body.",
		map[string]map[string]any{"number": {"type": "integer", "description": "issue number"}}, []string{"number"}},
	{ToolRepoList, "List one directory of the repository at the pinned ref. Use \"\" for the root.",
		map[string]map[string]any{"path": {"type": "string", "description": "directory, e.g. src/pkg"}}, []string{"path"}},
	{ToolRepoRead, fmt.Sprintf("Read a file by line range (at most %d lines per call) at the pinned ref.", MaxReadLines),
		map[string]map[string]any{"path": {"type": "string"}, "start_line": {"type": "integer"}, "end_line": {"type": "integer"}}, []string{"path"}},
	{ToolRepoSearch, "Exact search of the repository for an identifier or string. Returns path:line: text. Optional path narrows to a directory.",
		map[string]map[string]any{"query": {"type": "string"}, "path": {"type": "string"}}, []string{"query"}},
	{ToolChecksRead, "Read CI check runs (name, status, conclusion) for a ref; defaults to the pinned ref.",
		map[string]map[string]any{"ref": {"type": "string"}}, nil},
	{ToolTaskComplete, "Finish the task. summary must hold the result: findings with file:line evidence, the proposed change, and what you could not verify.",
		map[string]map[string]any{"summary": {"type": "string"}, "pr_number": {"type": "integer"}}, []string{"summary"}},
	{ToolTaskAbort, "Stop: the task is blocked or cannot be done. Explain why in reason.",
		map[string]map[string]any{"reason": {"type": "string"}}, []string{"reason"}},
}

// ReadToolNames lists the tools this package implements.
func ReadToolNames() []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.name)
	}
	return out
}

// Definitions returns the tool definitions for turn.WithAvailableTools, in
// the router's convention (Tool=name, Content=description,
// Arguments=JSON-Schema parameters). Only tools the agent mode may use are
// offered: fewer tools mean fewer tokens per turn and fewer wrong calls.
// All read tools are available from ADVISORY up.
func Definitions(mode agentmode.AgentMode) []toolapprove.ToolRequest {
	_ = mode // write tools (mode-gated) are not implemented in this package yet
	out := make([]toolapprove.ToolRequest, 0, len(defs))
	for _, d := range defs {
		props := map[string]any{}
		for k, v := range d.props {
			props[k] = v
		}
		schema := map[string]any{"type": "object", "properties": props}
		if len(d.required) > 0 {
			schema["required"] = d.required
		}
		out = append(out, toolapprove.ToolRequest{Tool: d.name, Content: d.desc, Arguments: schema})
	}
	return out
}

// FinishOnly returns only the terminal tools, for a forced-finish phase: in
// live runs a model that kept exploring produced its triage once offered
// nothing but task.complete.
func FinishOnly() []toolapprove.ToolRequest {
	var out []toolapprove.ToolRequest
	for _, d := range Definitions(agentmode.ModeAdvisory) {
		if d.Tool == ToolTaskComplete || d.Tool == ToolTaskAbort {
			out = append(out, d)
		}
	}
	return out
}
