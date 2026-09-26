package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Environment the agent manager exports alongside HIVE_CAVEMAN_MODE.
const (
	// ModeEnvVar carries the agent's jev_mode ("assist" when the tool is on).
	ModeEnvVar = "HIVE_JEV_MODE"
	// EndpointEnvVar carries the decision endpoint base URL.
	EndpointEnvVar = "HIVE_JEV_ENDPOINT"
	Subcommand     = "jev"
)

const cliUsage = `usage: hive jev decide --type <choice|score|probability> --question <text>
                       [--option <name[=description]>]... [--level <description>]...
                       [--state <json> | --state-file <path> | --state-stdin] [--criteria <json>]

Ask Jev, the hive's typed-decision model, one quick question and print
{"answer","confidence","probabilities","model","input_tokens"} as JSON.
  choice       pick one of the --option values (2-32)
  score        position on the ordered --level rubric, low to high (2-10)
  probability  P(yes) for a yes/no question, 0-1 (TypeSafe "noul")
Only available to agents whose jev_mode is "assist"; the hive proxies,
budgets and audits every call. Never use it to generate code or prose.
`

type decideOptions struct {
	req        Request
	stateFile  string
	stateStdin bool
	endpoint   string
	timeout    time.Duration
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// Run implements `hive jev …`. It returns the process exit code: 0 on an
// answer, 1 on a refused/failed call, 2 on usage errors.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stderr, cliUsage)
		return 2
	}
	if args[0] != "decide" {
		fmt.Fprintf(stderr, "hive %s: unknown subcommand %q\n%s", Subcommand, args[0], cliUsage)
		return 2
	}
	opts, err := parseDecideArgs(args[1:], stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "hive %s decide: %v\n", Subcommand, err)
		return 2
	}
	return runDecide(opts, stdin, stdout, stderr)
}

func parseDecideArgs(args []string, stderr io.Writer) (decideOptions, error) {
	var opts decideOptions
	var options, levels stringList
	var state, criteria string
	fs := flag.NewFlagSet(Subcommand+" decide", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, cliUsage) }
	fs.StringVar(&opts.req.Type, "type", TypeChoice, "decision type: choice, score or probability")
	fs.StringVar(&opts.req.Question, "question", "", "what to decide (required)")
	fs.Var(&options, "option", "a candidate for a choice, as name or name=description; repeat per option")
	fs.Var(&levels, "level", "a rubric level for a score, lowest first; repeat per level")
	fs.StringVar(&state, "state", "", "JSON context for the decision")
	fs.StringVar(&opts.stateFile, "state-file", "", "file holding JSON context")
	fs.BoolVar(&opts.stateStdin, "state-stdin", false, "read JSON context from stdin")
	fs.StringVar(&criteria, "criteria", "", "JSON criteria to send instead of the derived one (probability: {\"true\":…,\"false\":…})")
	fs.StringVar(&opts.endpoint, "endpoint", "", "decision endpoint (default $"+EndpointEnvVar+" or "+DefaultEndpoint+")")
	fs.DurationVar(&opts.timeout, "timeout", 15*time.Second, "how long to wait for an answer")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	for _, o := range options {
		name, desc, hasDesc := strings.Cut(o, "=")
		name = strings.TrimSpace(name)
		opts.req.Options = append(opts.req.Options, name)
		if hasDesc && strings.TrimSpace(desc) != "" {
			if opts.req.Descriptions == nil {
				opts.req.Descriptions = map[string]string{}
			}
			opts.req.Descriptions[name] = strings.TrimSpace(desc)
		}
	}
	opts.req.Levels = levels
	sources := 0
	for _, on := range []bool{state != "", opts.stateFile != "", opts.stateStdin} {
		if on {
			sources++
		}
	}
	if sources > 1 {
		return opts, errors.New("use only one of --state, --state-file, --state-stdin")
	}
	if state != "" {
		opts.req.State = json.RawMessage(state)
	}
	if criteria != "" {
		opts.req.Criteria = json.RawMessage(criteria)
	}
	if opts.req.Question == "" {
		return opts, errors.New("--question is required")
	}
	return opts, nil
}

func runDecide(opts decideOptions, stdin io.Reader, stdout, stderr io.Writer) int {
	if os.Getenv(ModeEnvVar) != config.JevModeAssist {
		fmt.Fprintf(stderr, "hive %s: Jev is not enabled for this agent (%s is not %q); set jev_mode: assist in the agent's config\n", Subcommand, ModeEnvVar, config.JevModeAssist)
		return 1
	}
	if opts.stateFile != "" {
		data, err := os.ReadFile(opts.stateFile)
		if err != nil {
			fmt.Fprintf(stderr, "hive %s: reading --state-file: %v\n", Subcommand, err)
			return 2
		}
		opts.req.State = data
	} else if opts.stateStdin {
		data, err := io.ReadAll(io.LimitReader(stdin, MaxStateBytes+1))
		if err != nil {
			fmt.Fprintf(stderr, "hive %s: reading stdin: %v\n", Subcommand, err)
			return 2
		}
		opts.req.State = data
	}
	if err := opts.req.Validate(); err != nil {
		fmt.Fprintf(stderr, "hive %s: %v\n", Subcommand, err)
		return 2
	}
	endpoint := opts.endpoint
	if endpoint == "" {
		endpoint = os.Getenv(EndpointEnvVar)
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	res, err := callEndpoint(endpoint, opts.req, opts.timeout)
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: %v\n", Subcommand, err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	_ = enc.Encode(res)
	return 0
}

// callEndpoint posts req to the hive's decision endpoint. The transport
// ignores HTTP(S)_PROXY on purpose: agents run with the MITM egress proxy in
// those variables, and this is a loopback call to the hive itself.
func callEndpoint(endpoint string, req Request, timeout time.Duration) (Result, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+DecidePath, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	// No identity header on purpose: the hive names the caller from the
	// socket UID and nothing else, so a claimed name would be ignored at best.
	hreq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(hreq)
	if err != nil {
		return Result{}, fmt.Errorf("decision endpoint unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var eb errorBody
		if json.Unmarshal(data, &eb) == nil && eb.Error != "" {
			return Result{}, fmt.Errorf("decision refused (HTTP %d): %s", resp.StatusCode, eb.Error)
		}
		return Result{}, fmt.Errorf("decision refused (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return Result{}, fmt.Errorf("decision endpoint returned malformed JSON: %w", err)
	}
	return res, nil
}
