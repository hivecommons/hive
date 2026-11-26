package agent

// `hive claude-turn`: one headless Claude Code turn, run inside the pane of a
// translator-routed agent that opted into headless_turns (#19902).
//
// A translator-routed agent (an inference backend such as litellm, or a named
// model gateway) normally runs the interactive `claude` CLI resident in its
// tmux pane, pointed at Hive's Anthropic-Messages translator. Anthropic
// classifies requests by how the CLI was started: interactive Claude Code is
// not covered by the monthly API credits of Max/Ultra/Team plans, while
// `claude -p` run with a Console API key is billed as Agent SDK usage, which
// those credits do cover. headless_turns therefore keeps the translator path
// (key custody, metering, model discovery) exactly as it is and changes only
// the launch: the pane holds a shell, and every kick runs one `claude -p`.
//
// This runner mirrors `hive agy-turn` (agy_turn.go):
//
//   - Continuity. Each kick resumes the session the previous kick started, by
//     explicit id (`--resume <id>`), read from and recorded to a per-launch
//     file the pane shell owns. "Most recent" (`--continue`) is not safe: all
//     agents can share one HOME and therefore one session store.
//   - Prompt size. The prompt reaches claude on stdin as one stream-json user
//     message, never in argv, so large kicks cannot fail with E2BIG.
//   - Legibility. stream-json output is NDJSON; the runner renders it as pane
//     text and keeps ClaudeHeadlessRunningMarker on the pane's last line so
//     the kick and poll gates see the turn as running.
//
// The runner never edits the request the CLI sends: Hive must not rewrite or
// strip Claude Code's own framing to change how a request is classified.

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
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/agent/panesignal"
)

// ClaudeTurnSubcommand is the subcommand the manager types into a
// headless_turns pane: `hive claude-turn` in-process, `crux claude-turn`
// under `crux serve`.
const ClaudeTurnSubcommand = "claude-turn"

const (
	claudeHeadlessReadyMarker   = panesignal.ClaudeHeadlessReadyMarker
	claudeHeadlessRunningMarker = panesignal.ClaudeHeadlessRunningMarker
)

// claudeTurnStatusInterval is how often the runner refreshes the running
// status line while claude is silent. A var so tests can shorten it.
var claudeTurnStatusInterval = 10 * time.Second

// claudeTurnInterruptGrace is how long claude gets to exit on its own after a
// Ctrl+C before the runner kills it.
const claudeTurnInterruptGrace = 5 * time.Second

// claudeTurnHeadlessFlags are inserted right after the claude binary: one
// print-mode turn whose prompt arrives on stdin as stream-json and whose
// output is stream-json (which claude only emits with --verbose in -p mode).
var claudeTurnHeadlessFlags = []string{
	"-p", "--verbose",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
}

// claudeStreamInput is the stdin message for one user turn under
// `--input-format stream-json` (the shape the Claude Agent SDK sends).
type claudeStreamInput struct {
	Type            string                   `json:"type"`
	Message         claudeStreamInputMessage `json:"message"`
	ParentToolUseID *string                  `json:"parent_tool_use_id"`
	SessionID       string                   `json:"session_id"`
}

type claudeStreamInputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// claudeStreamEvent is the subset of claude's stream-json output the runner
// renders. Unknown events and fields are ignored.
type claudeStreamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Message   *struct {
		Content []claudeTurnBlock `json:"content"`
	} `json:"message"`
	// result event fields
	IsError      bool    `json:"is_error"`
	DurationMS   float64 `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type claudeTurnBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   map[string]any  `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// claudeTurnOptions are the parsed `hive claude-turn` flags.
type claudeTurnOptions struct {
	promptFile  string
	sessionFile string
	newSession  bool
	// command is the claude argv the turn runs: the binary first, then the
	// agent's launch flags (model, settings, deny rules). The runner adds the
	// headless flags and --resume.
	command []string
}

// RunClaudeTurn implements `hive claude-turn`. It returns the process exit
// code.
func RunClaudeTurn(args []string, stdout, stderr io.Writer) int {
	opts, err := parseClaudeTurnArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "hive %s: %v\n", ClaudeTurnSubcommand, err)
		return 2
	}
	return runClaudeTurn(opts, stdout, stderr)
}

func parseClaudeTurnArgs(args []string, stderr io.Writer) (claudeTurnOptions, error) {
	var opts claudeTurnOptions
	fs := flag.NewFlagSet(ClaudeTurnSubcommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.promptFile, "prompt-file", "", "file holding the prompt for this turn (required)")
	fs.StringVar(&opts.sessionFile, "session-file", "", "file recording the session id to resume; empty disables continuity")
	fs.BoolVar(&opts.newSession, "new-session", false, "ignore the recorded session and start a new one")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: hive %s --prompt-file FILE [--session-file FILE] [--new-session] -- claude [launch flags...]\n", ClaudeTurnSubcommand)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if opts.promptFile == "" {
		return opts, errors.New("--prompt-file is required")
	}
	opts.command = fs.Args()
	if len(opts.command) == 0 || strings.TrimSpace(opts.command[0]) == "" {
		return opts, errors.New("missing claude command after --")
	}
	for _, a := range opts.command[1:] {
		switch a {
		case "-p", "--print", "--resume", "-r", "--continue", "-c", "--input-format", "--output-format":
			return opts, fmt.Errorf("claude command must not set %s; the runner owns it", a)
		}
	}
	return opts, nil
}

// claudeTurnArgs builds claude's argv (without the binary) for one turn.
func claudeTurnArgs(launchArgs []string, sessionID string) []string {
	args := append([]string{}, claudeTurnHeadlessFlags...)
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return append(args, launchArgs...)
}

func runClaudeTurn(opts claudeTurnOptions, stdout, stderr io.Writer) int {
	prompt, err := os.ReadFile(opts.promptFile)
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: reading prompt: %v\n", ClaudeTurnSubcommand, err)
		return 1
	}
	input, err := json.Marshal(claudeStreamInput{
		Type:    "user",
		Message: claudeStreamInputMessage{Role: "user", Content: string(prompt)},
	})
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: encoding prompt: %v\n", ClaudeTurnSubcommand, err)
		return 1
	}
	input = append(input, '\n')

	resumeID := ""
	if !opts.newSession {
		resumeID = readAgyConversationID(opts.sessionFile)
	}

	r := newClaudeTurnRenderer(stdout, time.Now())
	code, sawSession := runClaudeTurnOnce(opts, input, resumeID, r, stderr)
	if resumeID != "" && !sawSession && code != 0 {
		// claude refuses an unknown --resume id before it starts a session
		// (an expired or pruned transcript). Losing continuity beats losing
		// the kick: start over once with a fresh session.
		r.printf("HIVE claude: session %s could not be resumed; starting a new session\n", resumeID)
		r.reset()
		code, _ = runClaudeTurnOnce(opts, input, "", r, stderr)
	}
	r.finish()
	return code
}

// runClaudeTurnOnce runs one claude process. It reports the exit code and
// whether claude reported a session id at all.
func runClaudeTurnOnce(opts claudeTurnOptions, input []byte, resumeID string, r *claudeTurnRenderer, stderr io.Writer) (int, bool) {
	cmd := exec.Command(opts.command[0], claudeTurnArgs(opts.command[1:], resumeID)...)
	cmd.Stdin = strings.NewReader(string(input))
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: %v\n", ClaudeTurnSubcommand, err)
		return 1, false
	}
	r.status()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "hive %s: starting %s: %v\n", ClaudeTurnSubcommand, opts.command[0], err)
		return 1, false
	}

	// A Ctrl+C in the pane reaches claude too (same foreground process
	// group). Outlive it so the pane still gets the summary and the session
	// id; kill claude only if it ignores the interrupt.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)

	stop := make(chan struct{})
	var ticker sync.WaitGroup
	ticker.Add(1)
	go func() {
		defer ticker.Done()
		t := time.NewTicker(claudeTurnStatusInterval)
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
					killAfter = time.After(claudeTurnInterruptGrace)
				}
			case <-killAfter:
				_ = cmd.Process.Kill()
			}
		}
	}()

	recorded := resumeID
	sawSession := false
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		id := r.handleLine(sc.Bytes())
		if id == "" {
			continue
		}
		sawSession = true
		if id == recorded {
			continue
		}
		// Record the id as soon as claude reports it, not at the end: a turn
		// interrupted by a restart or a kick's Ctrl+C must still be resumable.
		if err := writeAgyConversationID(opts.sessionFile, id); err != nil {
			r.printf("HIVE claude: recording session id: %v\n", err)
		}
		recorded = id
	}
	scanErr := sc.Err()
	if scanErr != nil {
		// Keep draining so claude never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, out)
	}
	waitErr := cmd.Wait()
	close(stop)
	ticker.Wait()

	if scanErr != nil {
		fmt.Fprintf(stderr, "hive %s: reading claude output: %v\n", ClaudeTurnSubcommand, scanErr)
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
		fmt.Fprintf(stderr, "hive %s: %v\n", ClaudeTurnSubcommand, waitErr)
		code = 1
	}
	if res := r.resultEvent(); code == 0 && (res == nil || res.IsError) {
		code = 1
	}
	return code, sawSession
}

// claudeTurnRenderer turns claude's stream-json output into pane text. Like
// agyTurnRenderer, every write ends by redrawing a status line carrying
// ClaudeHeadlessRunningMarker, so the marker stays on the pane's last line
// while the turn runs.
type claudeTurnRenderer struct {
	mu          sync.Mutex
	w           io.Writer
	started     time.Time
	now         func() time.Time
	statusShown bool
	lastTool    string
	result      *claudeStreamEvent
}

func newClaudeTurnRenderer(w io.Writer, started time.Time) *claudeTurnRenderer {
	return &claudeTurnRenderer{w: w, started: started, now: time.Now}
}

func (r *claudeTurnRenderer) statusLine() string {
	elapsed := r.now().Sub(r.started).Round(time.Second)
	line := fmt.Sprintf("%s %s", claudeHeadlessRunningMarker, elapsed)
	if r.lastTool != "" {
		line += " · " + r.lastTool
	}
	return line
}

// emit writes complete lines (may be "") and then redraws the status line.
func (r *claudeTurnRenderer) emit(text string) {
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

func (r *claudeTurnRenderer) status() { r.emit("") }

func (r *claudeTurnRenderer) printf(format string, args ...any) {
	r.emit(fmt.Sprintf(format, args...))
}

// reset forgets the previous attempt's result before a retry.
func (r *claudeTurnRenderer) reset() {
	r.mu.Lock()
	r.result = nil
	r.lastTool = ""
	r.mu.Unlock()
}

func (r *claudeTurnRenderer) resultEvent() *claudeStreamEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result
}

// finish replaces the status line with the turn summary and leaves the cursor
// at the start of a line for the shell's ready marker.
func (r *claudeTurnRenderer) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	if r.statusShown {
		b.WriteString("\r\x1b[K")
		r.statusShown = false
	}
	if res := r.result; res != nil {
		status := res.Subtype
		if status == "" {
			status = "unknown"
		}
		in, out := 0, 0
		if res.Usage != nil {
			in, out = res.Usage.InputTokens, res.Usage.OutputTokens
		}
		fmt.Fprintf(&b, "HIVE claude turn: status=%s turns=%d duration=%.1fs input_tokens=%d output_tokens=%d session=%s\n",
			status, res.NumTurns, res.DurationMS/1000, in, out, res.SessionID)
		if res.IsError && strings.TrimSpace(res.Result) != "" {
			fmt.Fprintf(&b, "HIVE claude turn error: %s\n", truncateStr(oneLine(res.Result), agyTurnMaxLineRunes))
		}
	} else {
		b.WriteString("HIVE claude turn: claude exited without a result\n")
	}
	_, _ = io.WriteString(r.w, b.String())
}

// handleLine renders one line of claude output and returns the session id it
// carries, if any.
func (r *claudeTurnRenderer) handleLine(line []byte) string {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return ""
	}
	var ev claudeStreamEvent
	if !strings.HasPrefix(trimmed, "{") || json.Unmarshal([]byte(trimmed), &ev) != nil || ev.Type == "" {
		r.emit(trimmed + "\n")
		return ""
	}
	switch ev.Type {
	case "assistant":
		if ev.Message != nil {
			r.handleAssistant(ev.Message.Content)
		}
	case "user":
		if ev.Message != nil {
			r.handleToolResults(ev.Message.Content)
		}
	case "result":
		r.mu.Lock()
		r.result = &ev
		r.lastTool = ""
		r.mu.Unlock()
	}
	return ev.SessionID
}

func (r *claudeTurnRenderer) handleAssistant(blocks []claudeTurnBlock) {
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			text := strings.TrimRight(blk.Text, "\n")
			if strings.TrimSpace(text) != "" {
				r.emit(text + "\n")
			}
		case "tool_use":
			r.mu.Lock()
			r.lastTool = blk.Name
			r.mu.Unlock()
			r.emit("▸ " + truncateStr(blk.Name+" "+summarizeClaudeToolInput(blk.Input), agyTurnMaxLineRunes) + "\n")
		}
	}
}

func (r *claudeTurnRenderer) handleToolResults(blocks []claudeTurnBlock) {
	for _, blk := range blocks {
		if blk.Type != "tool_result" {
			continue
		}
		r.mu.Lock()
		r.lastTool = ""
		r.mu.Unlock()
		text := claudeTurnToolResultText(blk.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if blk.IsError {
			text = "error: " + text
		}
		r.emit(indentAgyToolOutput(text))
	}
}

// claudeTurnToolResultText flattens a tool_result's content, which claude sends
// either as a string or as an array of content blocks.
func claudeTurnToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []claudeTurnBlock
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// summarizeClaudeToolInput renders a tool call's input as one short line,
// with the field operators read most (a Bash command, a file path) first.
func summarizeClaudeToolInput(input map[string]any) string {
	for _, key := range []string{"command", "file_path", "pattern", "url"} {
		if v, ok := input[key].(string); ok && v != "" {
			return oneLine(v)
		}
	}
	return summarizeAgyToolParams(&agyToolInfo{Parameters: input})
}
