package advisor

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// HookSubcommand is the `hive advisor-hook` subcommand every supported
// backend invokes at its turn boundary. It reads the transcript reference the
// backend provides on stdin, asks the hub's loopback advise endpoint for the
// verdict, and prints the answer in the calling backend's dialect. Keeping
// the backend-specific part to formatting is what makes each adapter thin.
const HookSubcommand = "advisor-hook"

// hookFormatClaude renders the Claude Code `Stop` hook dialect: `{}` to let
// the turn end, `{"decision":"block","reason":...}` to hold it with a reason
// the model sees.
const hookFormatClaude = "claude"

// HookFormatCopilot renders the Copilot CLI `agentStop` dialect (camelCase
// payload, `decision: block` with a reason that becomes the forced next
// prompt). HookFormatCodex is the Codex CLI `Stop` dialect, which matches
// Claude Code's payload and answer.
const (
	HookFormatCopilot = "copilot"
	HookFormatCodex   = "codex"
)

const hookUsage = `usage: hive advisor-hook [--format claude|copilot|codex] [--endpoint <url>] [--timeout <dur>]

Turn-end advisor hook. Reads the backend's hook payload on stdin (Claude
Code / Codex CLI Stop hook JSON, or the Copilot CLI agentStop payload), sends the finished turn's transcript tail to
the hive's loopback advise endpoint, and prints the verdict in the calling
backend's hook dialect. Registered by hive at agent launch — never by editing
a backend's own configuration. On any failure it prints a non-blocking answer
and exits 0: an advisor outage must never stall the agent.
`

// claudeStopInput is the subset of the Claude Code Stop-hook stdin payload
// the hook consumes.
type claudeStopInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// copilotStopInput is the subset of the Copilot CLI agentStop stdin payload
// the hook consumes.
type copilotStopInput struct {
	SessionID      string `json:"sessionId"`
	TranscriptPath string `json:"transcriptPath"`
}

// claudeStopOutput is the Stop-hook answer dialect: decision "block" holds
// the turn and shows the model the reason; an empty object lets it end.
type claudeStopOutput struct {
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// RunHook implements `hive advisor-hook`. It returns the process exit code —
// 0 in every advisory outcome including advisor failures (fail open), 2 only
// on usage errors an operator would hit wiring it by hand.
func RunHook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(HookSubcommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, hookUsage) }
	format := fs.String("format", hookFormatClaude, "hook answer dialect (claude, copilot, codex)")
	endpoint := fs.String("endpoint", "", "advise endpoint (default $"+EndpointEnvVar+" or "+DefaultEndpoint+")")
	timeout := fs.Duration("timeout", 60*time.Second, "how long to wait for the verdict, hub timeout included")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	decode := decodeClaudeStop
	switch *format {
	case hookFormatClaude, HookFormatCodex:
	case HookFormatCopilot:
		decode = decodeCopilotStop
	default:
		fmt.Fprintf(stderr, "hive %s: unsupported --format %q (supported: %s, %s, %s)\n", HookSubcommand, *format, hookFormatClaude, HookFormatCopilot, HookFormatCodex)
		return 2
	}
	ep := strings.TrimSpace(*endpoint)
	if ep == "" {
		ep = os.Getenv(EndpointEnvVar)
	}
	if ep == "" {
		ep = DefaultEndpoint
	}
	return runStopHook(ep, *timeout, decode, stdin, stdout, stderr)
}

// decodeClaudeStop extracts the session and transcript path from a Claude Code
// or Codex CLI Stop-hook payload.
func decodeClaudeStop(payload []byte) (session, transcriptPath string, err error) {
	var in claudeStopInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return "", "", err
	}
	return in.SessionID, in.TranscriptPath, nil
}

// decodeCopilotStop extracts the same from a Copilot CLI agentStop payload.
func decodeCopilotStop(payload []byte) (session, transcriptPath string, err error) {
	var in copilotStopInput
	if err := json.Unmarshal(payload, &in); err != nil {
		return "", "", err
	}
	return in.SessionID, in.TranscriptPath, nil
}

// runStopHook handles one turn-end event (Claude Code / Codex Stop, Copilot
// agentStop — all answer with the same block-decision JSON). Failures never
// block: the worst an advisor outage can do is print `{}`.
func runStopHook(endpoint string, timeout time.Duration, decode func([]byte) (string, string, error), stdin io.Reader, stdout, stderr io.Writer) int {
	allow := func() int {
		fmt.Fprintln(stdout, "{}")
		return 0
	}
	payload, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: reading hook input: %v\n", HookSubcommand, err)
		return allow()
	}
	session, transcriptPath, err := decode(payload)
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: hook input is not Stop-hook JSON: %v\n", HookSubcommand, err)
		return allow()
	}
	transcript := readTranscriptTail(transcriptPath)
	if strings.TrimSpace(transcript) == "" {
		return allow() // nothing to review
	}
	resp, err := callAdvise(endpoint, Request{Turn: session, Transcript: transcript}, timeout)
	if err != nil {
		fmt.Fprintf(stderr, "hive %s: %v\n", HookSubcommand, err)
		return allow()
	}
	if resp.Skipped != "" || !resp.Interjected || strings.TrimSpace(resp.Text) == "" {
		// Asides are recorded, never delivered; skipped reviews fail open.
		return allow()
	}
	// Concerns and blockers are both interrupting deliveries: the Stop hook's
	// block decision is the one channel that puts text in front of the model
	// before its next step. The consecutive-block limit is enforced hub-side,
	// so a chain of blockers is bounded there — well under Claude's own
	// stop_hook_active loop guard.
	out := claudeStopOutput{Decision: "block", Reason: hookReason(resp)}
	data, err := json.Marshal(out)
	if err != nil {
		return allow()
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// hookReason phrases the advisor's text for the model, keeping the severity
// contract visible: a concern lets the agent finish its turn once the
// objection is in front of it, a blocker demands a response.
func hookReason(resp Response) string {
	switch resp.Severity {
	case SeverityBlocker:
		return "Advisor blocker — respond to this objection before ending your turn: " + resp.Text
	default:
		return "Advisor concern — take your next step with this in mind (you may proceed): " + resp.Text
	}
}

// readTranscriptTail reads the trailing lines of the backend's transcript
// file, bounded the same way the evaluator bounds its prompt. A missing or
// unreadable transcript returns "" and the hook allows the turn to end.
func readTranscriptTail(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	// Read at most the trailing window the evaluator would keep anyway.
	const window = int64(maxTranscriptChars)
	offset := int64(0)
	if info.Size() > window {
		offset = info.Size() - window
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, window))
	if err != nil {
		return ""
	}
	return tailLines(string(data), DefaultTranscriptLines)
}

// callAdvise posts one review request to the hive's loopback advise endpoint.
// The transport ignores HTTP(S)_PROXY on purpose: agents run with the MITM
// egress proxy in those variables, and this is a loopback call to the hive
// itself. No identity header on purpose either — the hive names the caller
// from the socket UID and nothing else.
func callAdvise(endpoint string, req Request, timeout time.Duration) (Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(endpoint, "/")+AdvisePath, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("advise endpoint unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var eb errorBody
		if json.Unmarshal(data, &eb) == nil && eb.Error != "" {
			return Response{}, fmt.Errorf("advise refused (HTTP %d): %s", resp.StatusCode, eb.Error)
		}
		return Response{}, fmt.Errorf("advise refused (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return Response{}, fmt.Errorf("advise endpoint returned malformed JSON: %w", err)
	}
	return out, nil
}
