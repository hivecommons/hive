package agent

// `hive agy-turn`: one headless agy turn, run inside the agent's tmux pane.
//
// The headless agy shim (#8656, #8669) replaced the resident agy TUI — which
// held 15–20% of a core per agent for the whole turn, even while idle
// (google-antigravity/antigravity-cli#945) — with one `agy` process per kick.
// Running that process directly as `agy -p "$(cat prompt)"` had three gaps
// this runner closes:
//
//   - Continuity. The TUI kept one conversation across kicks; a bare `agy -p`
//     starts a new one every time. `-c/--continue` resumes the most recent
//     conversation, but every agent on a hive shares one HOME and therefore one
//     agy conversation store, so "most recent" can be another agent's. The
//     runner resumes by explicit id instead (`--conversation <id>`), reading
//     it from and recording it to a per-launch file the pane shell owns.
//   - Prompt size. A prompt passed in argv fails with E2BIG past Linux's
//     128 KiB per-argument limit, and large ${PR_LIST}/${ISSUE_LIST} kicks
//     reach that. `agy -p -` does not read stdin; the only stdin path agy
//     1.2.9 offers is `--input-format stream-json`, so the prompt goes there.
//   - Legibility. stream-json output is NDJSON. The runner renders it as
//     plain text in the pane (assistant text, one line per tool call) so the
//     dashboard terminal, pluk and kick logs stay readable, and it keeps the
//     running marker on the pane's last line for the kick and poll gates.

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// agyTurnStatusInterval is how often the runner refreshes the running status
// line while agy is silent (a long tool call). A var so tests can shorten it.
var agyTurnStatusInterval = 10 * time.Second

// agyTurnInterruptGrace is how long agy gets to exit on its own after a
// Ctrl+C before the runner kills it.
const agyTurnInterruptGrace = 5 * time.Second

const (
	agyTurnSubcommand = "agy-turn"

	// agyTurnToolOutputLines caps how much of each tool's output is echoed
	// into the pane. The agent sees the full output; the pane only needs
	// enough for an operator to follow along.
	agyTurnToolOutputLines = 3
	agyTurnMaxLineRunes    = 200
)

// agyStreamInput is the one stdin message agy's `--input-format stream-json`
// accepts for a user turn. The shape is not documented upstream; it was
// established against agy 1.2.9 from its own decode errors ("missing the
// \"event\" field", "cannot unmarshal string into ... streamInputUserMessage",
// "message has no content").
type agyStreamInput struct {
	Event   string                `json:"event"`
	Message agyStreamInputMessage `json:"message"`
}

type agyStreamInputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// agyStreamEvent is the subset of agy's `--output-format stream-json` events
// the runner renders. Unknown events and fields are ignored.
type agyStreamEvent struct {
	Event          string          `json:"event"`
	ConversationID string          `json:"conversation_id"`
	StepUpdate     *agyStepUpdate  `json:"step_update"`
	Result         *agyTurnResult  `json:"result"`
	Init           json.RawMessage `json:"init"`
}

type agyStepUpdate struct {
	ConversationID string       `json:"conversation_id"`
	StepIndex      int          `json:"step_index"`
	State          string       `json:"state"`
	StepType       string       `json:"step_type"`
	TextDelta      string       `json:"text_delta"`
	ToolName       string       `json:"tool_name"`
	ToolInfo       *agyToolInfo `json:"tool_info"`
}

type agyToolInfo struct {
	Name       string         `json:"name"`
	Parameters map[string]any `json:"parameters"`
	Output     string         `json:"output"`
}

type agyTurnResult struct {
	ConversationID  string  `json:"conversation_id"`
	Status          string  `json:"status"`
	Error           string  `json:"error"`
	DurationSeconds float64 `json:"duration_seconds"`
	NumTurns        int     `json:"num_turns"`
	Usage           struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// agyTurnOptions are the parsed `hive agy-turn` flags.
type agyTurnOptions struct {
	binary           string
	model            string
	effort           string
	promptFile       string
	conversationFile string
	newConversation  bool
}

// RunAgyTurn implements `hive agy-turn`. It returns the process exit code.
func RunAgyTurn(args []string, stdout, stderr io.Writer) int {
	opts, err := parseAgyTurnArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "hive %s: %v\n", agyTurnSubcommand, err)
		return 2
	}
	return runAgyTurn(opts, stdout, stderr)
}

func parseAgyTurnArgs(args []string, stderr io.Writer) (agyTurnOptions, error) {
	var opts agyTurnOptions
	fs := flag.NewFlagSet(agyTurnSubcommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.binary, "agy", "agy", "agy binary to run")
	fs.StringVar(&opts.model, "model", "", "agy --model (passed with --effort)")
	fs.StringVar(&opts.effort, "effort", "", "agy --effort; defaults to agy's own default when a model is set")
	fs.StringVar(&opts.promptFile, "prompt-file", "", "file holding the prompt for this turn (required)")
	fs.StringVar(&opts.conversationFile, "conversation-file", "", "file recording the conversation id to resume; empty disables continuity")
	fs.BoolVar(&opts.newConversation, "new-conversation", false, "ignore the recorded conversation and start a new one")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.promptFile == "" {
		return opts, errors.New("--prompt-file is required")
	}
	return opts, nil
}

// agyTurnArgs builds agy's argv for one stream-json turn.
func agyTurnArgs(model, effort, conversationID string) []string {
	args := []string{"--dangerously-skip-permissions"}
	if model != "" {
		// agy silently ignores --model without --effort (see agyInteractiveLaunchCmd).
		args = append(args, "--model", model, "--effort", agyLaunchEffort(model, effort))
	}
	if conversationID != "" {
		args = append(args, "--conversation", conversationID)
	}
	return append(args, "--input-format", "stream-json", "--output-format", "stream-json")
}

func runAgyTurn(opts agyTurnOptions, stdout, stderr io.Writer) int {
	prompt, err := os.ReadFile(opts.promptFile)
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: reading prompt: %v\n", agyTurnSubcommand, err)
		return 1
	}
	input, err := json.Marshal(agyStreamInput{
		Event:   "user",
		Message: agyStreamInputMessage{Role: "user", Content: string(prompt)},
	})
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: encoding prompt: %v\n", agyTurnSubcommand, err)
		return 1
	}
	input = append(input, '\n')

	resumeID := ""
	if !opts.newConversation {
		resumeID = readAgyConversationID(opts.conversationFile)
	}

	cmd := exec.Command(opts.binary, agyTurnArgs(opts.model, opts.effort, resumeID)...)
	cmd.Stdin = strings.NewReader(string(input))
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: %v\n", agyTurnSubcommand, err)
		return 1
	}

	r := newAgyTurnRenderer(stdout, time.Now())
	r.status()
	if err := cmd.Start(); err != nil {
		r.finish()
		fmt.Fprintf(stderr, "hive %s: starting %s: %v\n", agyTurnSubcommand, opts.binary, err)
		return 1
	}

	// A Ctrl+C in the pane reaches agy too (same foreground process group).
	// Outlive it rather than dying first, so the pane still gets the summary
	// and the conversation id agy reported; kill agy only if it ignores the
	// interrupt.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	stop := make(chan struct{})
	var ticker sync.WaitGroup
	ticker.Add(1)
	go func() {
		defer ticker.Done()
		t := time.NewTicker(agyTurnStatusInterval)
		defer t.Stop()
		var killAfter <-chan time.Time
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.status()
			case <-interrupts:
				if killAfter == nil {
					killAfter = time.After(agyTurnInterruptGrace)
				}
			case <-killAfter:
				_ = cmd.Process.Kill()
			}
		}
	}()

	recorded := resumeID
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		id := r.handleLine(sc.Bytes())
		if id == "" || id == recorded {
			continue
		}
		if resumeID != "" && recorded == resumeID {
			r.printf("HIVE agy: conversation %s could not be resumed; agy started %s\n", resumeID, id)
		}
		// Record the id the moment agy reports it, not at the end: a turn
		// interrupted by a restart or a kick's Ctrl+C must still be resumable.
		if err := writeAgyConversationID(opts.conversationFile, id); err != nil {
			r.printf("HIVE agy: recording conversation id: %v\n", err)
		}
		recorded = id
	}
	scanErr := sc.Err()
	if scanErr != nil {
		// Keep draining so agy never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, out)
	}
	waitErr := cmd.Wait()
	close(stop)
	ticker.Wait()
	r.finish()

	if scanErr != nil {
		fmt.Fprintf(stderr, "hive %s: reading agy output: %v\n", agyTurnSubcommand, scanErr)
	}
	code := 0
	var exitErr *exec.ExitError
	switch {
	case errors.As(waitErr, &exitErr):
		code = exitErr.ExitCode()
		if code < 0 {
			code = 1
		}
	case waitErr != nil:
		fmt.Fprintf(stderr, "hive %s: %v\n", agyTurnSubcommand, waitErr)
		code = 1
	}
	if code == 0 && (r.result == nil || !strings.EqualFold(r.result.Status, "SUCCESS")) {
		code = 1
	}
	return code
}

// readAgyConversationID returns the recorded conversation id, or "" when there
// is none (no file configured, first turn since launch, or unreadable).
func readAgyConversationID(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !validAgyConversationID(id) {
		return ""
	}
	return id
}

// validAgyConversationID accepts the UUID-shaped ids agy reports and refuses
// anything that could be read as a flag or carry whitespace.
func validAgyConversationID(id string) bool {
	if id == "" || len(id) > 128 || strings.HasPrefix(id, "-") {
		return false
	}
	for _, c := range id {
		if !(c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

// writeAgyConversationID atomically replaces the conversation file.
func writeAgyConversationID(path, id string) error {
	if path == "" || !validAgyConversationID(id) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// agyTurnRenderer turns agy's stream-json output into pane text.
//
// Output is written whole lines at a time, and every write ends by redrawing
// a status line that carries agyHeadlessRunningMarker. The marker is therefore
// always on the pane's last line while the turn runs, however much output has
// scrolled the turn's header away: paneShowsAgentWorking keeps the kick path
// from typing into a live turn, and a partial assistant line can never be
// mistaken for the shell prompt.
type agyTurnRenderer struct {
	mu          sync.Mutex
	w           io.Writer
	started     time.Time
	now         func() time.Time
	statusShown bool
	pending     map[int]string // step index → assistant text not yet ended by a newline
	lastTool    string
	result      *agyTurnResult
}

func newAgyTurnRenderer(w io.Writer, started time.Time) *agyTurnRenderer {
	return &agyTurnRenderer{w: w, started: started, now: time.Now, pending: map[int]string{}}
}

// statusLine is the line the pane shows while the turn runs.
func (r *agyTurnRenderer) statusLine() string {
	elapsed := r.now().Sub(r.started).Round(time.Second)
	line := fmt.Sprintf("%s %s", agyHeadlessRunningMarker, elapsed)
	if r.lastTool != "" {
		line += " · " + r.lastTool
	}
	return line
}

// emit writes complete lines (may be "") and then redraws the status line.
func (r *agyTurnRenderer) emit(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	if r.statusShown {
		b.WriteString("\r\x1b[K")
	}
	b.WriteString(text)
	b.WriteString(r.statusLine())
	r.statusShown = true
	_, _ = io.WriteString(r.w, b.String())
}

func (r *agyTurnRenderer) status() { r.emit("") }

func (r *agyTurnRenderer) printf(format string, args ...any) {
	r.emit(fmt.Sprintf(format, args...))
}

// finish flushes any unterminated assistant text, replaces the status line
// with the turn summary, and leaves the cursor at the start of a line for the
// shell's ready marker.
func (r *agyTurnRenderer) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	if r.statusShown {
		b.WriteString("\r\x1b[K")
		r.statusShown = false
	}
	b.WriteString(r.flushPendingLocked())
	if res := r.result; res != nil {
		fmt.Fprintf(&b, "HIVE agy turn: status=%s turns=%d duration=%.1fs tokens=%d conversation=%s\n",
			res.Status, res.NumTurns, res.DurationSeconds, res.Usage.TotalTokens, res.ConversationID)
		if res.Error != "" {
			fmt.Fprintf(&b, "HIVE agy turn error: %s\n", res.Error)
		}
	} else {
		b.WriteString("HIVE agy turn: agy exited without a result\n")
	}
	_, _ = io.WriteString(r.w, b.String())
}

func (r *agyTurnRenderer) flushPendingLocked() string {
	if len(r.pending) == 0 {
		return ""
	}
	steps := make([]int, 0, len(r.pending))
	for s := range r.pending {
		steps = append(steps, s)
	}
	sort.Ints(steps)
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(r.pending[s])
		b.WriteString("\n")
	}
	r.pending = map[int]string{}
	return b.String()
}

// handleLine renders one line of agy output and returns the conversation id
// it carries, if any.
func (r *agyTurnRenderer) handleLine(line []byte) string {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return ""
	}
	var ev agyStreamEvent
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &ev) != nil || ev.Event == "" {
		r.emit(trimmed + "\n")
		return ""
	}
	switch ev.Event {
	case "init":
		return ev.ConversationID
	case "step_update":
		if ev.StepUpdate == nil {
			return ""
		}
		r.handleStep(ev.StepUpdate)
		return ev.StepUpdate.ConversationID
	case "result":
		if ev.Result == nil {
			return ""
		}
		r.mu.Lock()
		r.result = ev.Result
		r.mu.Unlock()
		return ev.Result.ConversationID
	}
	return ev.ConversationID
}

func (r *agyTurnRenderer) handleStep(s *agyStepUpdate) {
	switch s.StepType {
	case "agent_response":
		r.mu.Lock()
		text := r.pending[s.StepIndex] + s.TextDelta
		var complete string
		if i := strings.LastIndex(text, "\n"); i >= 0 {
			complete, text = text[:i+1], text[i+1:]
		}
		if s.State != "ACTIVE" && text != "" {
			complete, text = complete+text+"\n", ""
		}
		if text == "" {
			delete(r.pending, s.StepIndex)
		} else {
			r.pending[s.StepIndex] = text
		}
		r.mu.Unlock()
		if complete != "" {
			r.emit(complete)
		}
	case "tool":
		name := s.ToolName
		if name == "" && s.ToolInfo != nil {
			name = s.ToolInfo.Name
		}
		switch s.State {
		case "ACTIVE":
			r.mu.Lock()
			r.lastTool = name
			r.mu.Unlock()
			r.emit("▸ " + truncateStr(name+" "+summarizeAgyToolParams(s.ToolInfo), agyTurnMaxLineRunes) + "\n")
		case "DONE":
			r.mu.Lock()
			r.lastTool = ""
			r.mu.Unlock()
			if s.ToolInfo != nil && strings.TrimSpace(s.ToolInfo.Output) != "" {
				r.emit(indentAgyToolOutput(s.ToolInfo.Output))
			}
		default:
			r.mu.Lock()
			r.lastTool = ""
			r.mu.Unlock()
			r.emit(fmt.Sprintf("▸ %s %s\n", name, strings.ToLower(s.State)))
		}
	}
}

// summarizeAgyToolParams renders tool parameters as one short line, with
// CommandLine (run_command) first since it is the one operators read most.
func summarizeAgyToolParams(info *agyToolInfo) string {
	if info == nil || len(info.Parameters) == 0 {
		return ""
	}
	if cmd, ok := info.Parameters["CommandLine"].(string); ok && cmd != "" {
		return oneLine(cmd)
	}
	keys := make([]string, 0, len(info.Parameters))
	for k := range info.Parameters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, err := json.Marshal(info.Parameters[k])
		if err != nil {
			continue
		}
		parts = append(parts, k+"="+oneLine(string(v)))
	}
	return strings.Join(parts, " ")
}

func indentAgyToolOutput(output string) string {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(output, "\r\n", "\n"), "\n"), "\n")
	var b strings.Builder
	for i, l := range lines {
		if i == agyTurnToolOutputLines {
			fmt.Fprintf(&b, "  … %d more lines\n", len(lines)-i)
			break
		}
		b.WriteString("  ")
		b.WriteString(truncateStr(strings.TrimRight(l, "\r"), agyTurnMaxLineRunes))
		b.WriteString("\n")
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
