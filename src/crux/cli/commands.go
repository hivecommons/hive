package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hivecommons/hive/crux"
)

// subFlags builds a subcommand flag set that reports to stderr and never
// exits the process.
func (e *Env) subFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("crux "+name, flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	return fs
}

// parseSub parses args and maps the flag package's outcomes onto exit codes;
// ok is false when the caller should return code.
func parseSub(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, false
		}
		return ExitUsage, false
	}
	return ExitOK, true
}

// cmdRun is `crux run`: Definitions.AddAgent (for a definition file) and
// Lifecycle.Start, then stream Observer.GetBufferOutput until ctx ends and
// Lifecycle.Stop. --detach returns after Start and leaves the agent running.
func (e *Env) cmdRun(ctx context.Context, opts globalOptions, args []string) int {
	fs := e.subFlags("run")
	detach := fs.Bool("detach", false, "return after Start; do not stream or stop the agent")
	interval := fs.Duration("interval", defaultStreamInterval, "output poll interval while streaming")
	noSocket := fs.Bool("no-socket", false, "do not serve the control socket while streaming")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux run [-detach] [-interval D] [-no-socket] <definition.yaml | agent>")
		return ExitUsage
	}
	target := fs.Arg(0)

	defs, project, err := e.Loader.LoadConfig(opts.config)
	if err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	rt := e.newRuntime(defs, project)

	name := target
	if isDefinitionPath(target) {
		defName, cfg, err := e.Loader.LoadDefinitionFile(target)
		if err != nil {
			fmt.Fprintln(e.Stderr, "crux:", err)
			return ExitError
		}
		name = defName
		rt.AddAgent(name, cfg)
	} else if _, known := defs[name]; !known {
		fmt.Fprintf(e.Stderr, "crux: agent %q is not in --config; pass a definition .yaml to run one from a file\n", name)
		return ExitError
	}

	if err := rt.Start(ctx, name); err != nil {
		fmt.Fprintf(e.Stderr, "crux: start %s: %v\n", name, err)
		return ExitError
	}
	fmt.Fprintf(e.Stdout, "started %s\n", name)
	if *detach {
		return ExitOK
	}

	if !*noSocket {
		stopServing, err := e.serveControl(ctx, rt, opts.socket)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crux: control socket %s: %v (continuing without it)\n", opts.socket, err)
		} else {
			defer stopServing()
		}
	}

	e.stream(ctx, rt, name, *interval)

	if err := rt.Stop(name); err != nil {
		fmt.Fprintf(e.Stderr, "crux: stop %s: %v\n", name, err)
		return ExitError
	}
	fmt.Fprintf(e.Stdout, "stopped %s\n", name)
	return ExitOK
}

// stream prints new ring-buffer lines for name on every tick until ctx ends.
func (e *Env) stream(ctx context.Context, rt crux.Runtime, name string, interval time.Duration) {
	ticks, stop := e.ticker(interval)
	defer stop()
	var prev []string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			cur, err := rt.GetBufferOutput(name, streamWindow)
			if err != nil {
				fmt.Fprintf(e.Stderr, "crux: output %s: %v\n", name, err)
				return
			}
			for _, line := range newTail(prev, cur) {
				fmt.Fprintln(e.Stdout, line)
			}
			prev = cur
		}
	}
}

// newTail returns the lines of cur that were not already printed from prev,
// treating both as windows onto the same append-only buffer: the longest
// suffix of prev that is a prefix of cur is the overlap.
func newTail(prev, cur []string) []string {
	max := len(prev)
	if len(cur) < max {
		max = len(cur)
	}
	for k := max; k > 0; k-- {
		if equalLines(prev[len(prev)-k:], cur[:k]) {
			return cur[k:]
		}
	}
	return cur
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// processRow is the ps projection of a crux.Process: the columns an operator
// reads at a glance, and the JSON shape --json emits.
type processRow struct {
	Name         string     `json:"name"`
	ID           string     `json:"id"`
	State        string     `json:"state"`
	Backend      string     `json:"backend"`
	Model        string     `json:"model,omitempty"`
	Paused       bool       `json:"paused"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	LastKick     *time.Time `json:"last_kick,omitempty"`
	RestartCount int        `json:"restart_count"`
}

func rowFor(p *crux.Process) processRow {
	backend := p.Config.Backend
	if p.BackendOverride != "" {
		backend = p.BackendOverride
	}
	model := p.Config.Model
	if p.ModelOverride != "" {
		model = p.ModelOverride
	}
	return processRow{
		Name:         p.Name,
		ID:           p.ID,
		State:        string(p.State),
		Backend:      backend,
		Model:        model,
		Paused:       p.Paused,
		StartedAt:    p.StartedAt,
		LastKick:     p.LastKick,
		RestartCount: p.RestartCount,
	}
}

// cmdPS is `crux ps`: Observer.AllStatuses.
func (e *Env) cmdPS(rt crux.Runtime, args []string) int {
	fs := e.subFlags("ps")
	asJSON := fs.Bool("json", false, "emit a JSON array instead of a table")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	statuses := rt.AllStatuses()
	rows := make([]processRow, 0, len(statuses))
	for _, p := range statuses {
		if p == nil {
			continue
		}
		rows = append(rows, rowFor(p))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	if *asJSON {
		return e.writeJSON(rows)
	}
	return e.printPS(rows)
}

// printPS writes the ps table; it is shared with remote mode.
func (e *Env) printPS(rows []processRow) int {
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tSTATE\tBACKEND\tMODEL\tUPTIME\tLAST KICK\tRESTARTS")
	now := time.Now()
	for _, r := range rows {
		state := r.State
		if r.Paused {
			state += " (paused)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			r.Name, r.ID, state, r.Backend, orDash(r.Model), since(now, r.StartedAt), since(now, r.LastKick), r.RestartCount)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	return ExitOK
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// since renders how long ago t was, or a dash for never.
func since(now time.Time, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return now.Sub(*t).Truncate(time.Second).String()
}

// logsOutput is the --json shape of `crux logs`.
type logsOutput struct {
	Agent   string   `json:"agent"`
	Source  string   `json:"source"`
	Header  string   `json:"header,omitempty"`
	Lines   []string `json:"lines,omitempty"`
	Content string   `json:"content,omitempty"`
}

// cmdLogs is `crux logs`: Observer.GetOutput by default, GetBufferOutput
// with --buffer, CaptureFullLog (with FullLogHeader) with --full.
func (e *Env) cmdLogs(rt crux.Runtime, args []string) int {
	fs := e.subFlags("logs")
	lines := fs.Int("n", defaultLogLines, "lines to print from the pane or buffer")
	buffer := fs.Bool("buffer", false, "read the ring buffer (GetBufferOutput) instead of the live pane")
	full := fs.Bool("full", false, "capture the whole scrollback (CaptureFullLog)")
	asJSON := fs.Bool("json", false, "emit JSON instead of raw lines")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux logs [-n N] [-buffer] [-full] [-json] <agent>")
		return ExitUsage
	}
	if *lines < 0 {
		fmt.Fprintln(e.Stderr, "crux: -n must not be negative")
		return ExitUsage
	}
	if *buffer && *full {
		fmt.Fprintln(e.Stderr, "crux: -buffer and -full are exclusive")
		return ExitUsage
	}
	name := rt.ResolveAgent(fs.Arg(0))
	out := logsOutput{Agent: name}
	var err error
	switch {
	case *full:
		out.Source = "full"
		out.Header = rt.FullLogHeader(name)
		out.Content, err = rt.CaptureFullLog(name)
	case *buffer:
		out.Source = "buffer"
		out.Lines, err = rt.GetBufferOutput(name, *lines)
	default:
		out.Source = "pane"
		out.Lines, err = rt.GetOutput(name, *lines)
	}
	if err != nil {
		fmt.Fprintf(e.Stderr, "crux: logs %s: %v\n", name, err)
		return ExitError
	}
	if *asJSON {
		return e.writeJSON(out)
	}
	e.printLogs(out, *full)
	return ExitOK
}

// printLogs writes raw log output: header and content for a full capture,
// otherwise one line per entry. Shared with remote mode.
func (e *Env) printLogs(out logsOutput, full bool) {
	if full {
		if out.Header != "" {
			fmt.Fprint(e.Stdout, out.Header)
			if !strings.HasSuffix(out.Header, "\n") {
				fmt.Fprintln(e.Stdout)
			}
		}
		fmt.Fprint(e.Stdout, out.Content)
		if out.Content != "" && !strings.HasSuffix(out.Content, "\n") {
			fmt.Fprintln(e.Stdout)
		}
		return
	}
	for _, line := range out.Lines {
		fmt.Fprintln(e.Stdout, line)
	}
}

// cmdKick is `crux kick`: Kicker.SendKick. The message is the remaining
// arguments joined by spaces, or stdin when the single argument is "-".
func (e *Env) cmdKick(rt crux.Runtime, args []string) int {
	fs := e.subFlags("kick")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(e.Stderr, "usage: crux kick <agent> <message...|->")
		return ExitUsage
	}
	name := rt.ResolveAgent(fs.Arg(0))
	message := strings.Join(fs.Args()[1:], " ")
	if message == "-" {
		data, err := io.ReadAll(e.Stdin)
		if err != nil {
			fmt.Fprintln(e.Stderr, "crux: reading kick from stdin:", err)
			return ExitError
		}
		message = strings.TrimRight(string(data), "\n")
	}
	if strings.TrimSpace(message) == "" {
		fmt.Fprintln(e.Stderr, "crux: kick message is empty")
		return ExitUsage
	}
	if err := rt.SendKick(name, message); err != nil {
		fmt.Fprintf(e.Stderr, "crux: kick %s: %v\n", name, err)
		return ExitError
	}
	fmt.Fprintf(e.Stdout, "kicked %s\n", name)
	return ExitOK
}

// cmdStop is `crux stop`: Lifecycle.Stop.
func (e *Env) cmdStop(rt crux.Runtime, args []string) int {
	fs := e.subFlags("stop")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux stop <agent>")
		return ExitUsage
	}
	name := rt.ResolveAgent(fs.Arg(0))
	if err := rt.Stop(name); err != nil {
		fmt.Fprintf(e.Stderr, "crux: stop %s: %v\n", name, err)
		return ExitError
	}
	fmt.Fprintf(e.Stdout, "stopped %s\n", name)
	return ExitOK
}

func (e *Env) writeJSON(v any) int {
	enc := json.NewEncoder(e.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	return ExitOK
}
