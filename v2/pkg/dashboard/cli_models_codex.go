package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Live model discovery for the OpenAI Codex CLI.
//
// `codex app-server` speaks a newline-delimited JSON-RPC-style protocol over
// stdio (the "v2 app-server protocol", documented in openai/codex
// codex-rs/app-server/README.md) and exposes a `model/list` request. The
// returned catalog is BAKED INTO the installed CLI binary — it is
// per-CLI-VERSION, not per-account: the per-account `remote_models` fetch was
// a feature flag that has been removed upstream, and the probe works with a
// completely empty, unauthenticated CODEX_HOME (verified live against codex
// 0.146.0; round-trip ~0.5s).
//
// CODEX_HOME rule: the app-server WRITES into CODEX_HOME (sqlite, lock files,
// installation_id) and requires ownership of it, so the probe always runs
// with CODEX_HOME overridden to a throwaway temp dir the dashboard owns —
// NEVER an agent's own `.codex-<id>` home, whose locks/state must not be
// touched by a probe.
//
// Failure semantics mirror the copilot SDK probe: any failure (binary
// missing, spawn error, handshake timeout, parse error, empty data) yields an
// empty result so queryCLIModels serves the static list with fallback=true.
// On hosted spokes the main image does not install codex at all, so the probe
// degrades instantly to the fallback there — expected and fine.

const (
	// codexBinaryName is the Codex CLI binary probed on PATH. When absent the
	// probe is skipped instantly and the static fallback is served.
	codexBinaryName = "codex"

	// codexAppServerArg is the subcommand that starts the stdio JSON-RPC
	// app-server the probe speaks to.
	codexAppServerArg = "app-server"

	// codexProbeTimeout bounds the whole spawn+handshake+model/list exchange.
	// The measured live round-trip is ~0.5s; 10s leaves generous headroom for
	// cold page cache without letting a wedged binary stall the caller.
	codexProbeTimeout = 10 * time.Second

	// codexRPCClientName / codexRPCClientVersion identify this probe in the
	// mandatory initialize clientInfo. Values are informational to the server
	// but required by the protocol.
	codexRPCClientName    = "hive-dashboard"
	codexRPCClientVersion = "1.0.0"

	// codexProbeHomePrefix names the throwaway per-probe CODEX_HOME temp dir
	// (see the CODEX_HOME rule above). Removed after every probe.
	codexProbeHomePrefix = "hive-codex-probe-"

	// codexRPCInitializeID / codexRPCModelListID are the JSON-RPC request ids
	// for the two requests the probe sends. Arbitrary but fixed so responses
	// can be matched among interleaved server notifications.
	codexRPCInitializeID = 1
	codexRPCModelListID  = 2

	// codexRPCInitializedMethod is the notification the app-server emits once
	// the initialize handshake is accepted.
	codexRPCInitializedMethod = "initialized"

	// codexRPCModelListMethod is the request returning the model catalog.
	codexRPCModelListMethod = "model/list"

	// codexRPCMaxLineBytes caps a single scanned protocol line. The model/list
	// response is small (a few KB), but the default bufio.Scanner limit (64KB)
	// is raised defensively so a verbose future response cannot break parsing.
	codexRPCMaxLineBytes = 1 << 20

	// codexRPCErrorLogLimit caps how much of a JSON-RPC error object is folded
	// into a returned error (and thus a log line) — enough to diagnose, never
	// a full response-body dump.
	codexRPCErrorLogLimit = 300
)

// codexModelEntry mirrors one element of the model/list response data array.
// The server also emits displayName/defaultReasoningEffort/
// supportedReasoningEfforts; discovery only needs the id and the
// hidden/default flags today.
type codexModelEntry struct {
	ID        string `json:"id"`
	Model     string `json:"model"`
	IsDefault bool   `json:"isDefault"`
	Hidden    bool   `json:"hidden"`
}

// codexRPCMessage is the envelope of one newline-delimited protocol message.
// The app-server protocol is JSON-RPC-shaped but omits the "jsonrpc":"2.0"
// field, so none is sent or required here.
type codexRPCMessage struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// runCodexModelProbe executes the app-server probe and returns the served id
// order. A package-level seam (mirroring runCopilotSDKHelper) so unit tests
// can substitute canned results without a real codex binary.
var runCodexModelProbe = execCodexModelProbe

// discoverCodexModels lists the model catalog of the installed Codex CLI via
// `codex app-server` model/list. Best-effort: any probe failure returns
// fallback=true with an empty list so the caller substitutes the static list,
// exactly like the copilot probe's failure semantics.
func (s *Server) discoverCodexModels() cliModelResult {
	models, err := runCodexModelProbe()
	if err != nil {
		// Expected on hosted spokes (main image ships no codex binary); the
		// static fallback keeps the dropdown populated. Never logs bodies.
		s.logger.Info("codex model discovery unavailable, serving static fallback", "err", err.Error())
		return cliModelResult{fallback: true}
	}
	if len(models) == 0 {
		return cliModelResult{fallback: true}
	}
	return cliModelResult{models: dedupeModels(models), fallback: false}
}

// execCodexModelProbe is the real prober: spawn `codex app-server` with a
// throwaway CODEX_HOME, run the initialize handshake and model/list exchange
// over stdio, then kill and reap the server.
func execCodexModelProbe() ([]string, error) {
	if _, err := exec.LookPath(codexBinaryName); err != nil {
		return nil, fmt.Errorf("codex binary not on PATH: %w", err)
	}

	// Throwaway dashboard-owned CODEX_HOME (see the CODEX_HOME rule in the
	// file header); removed after the probe.
	home, err := os.MkdirTemp("", codexProbeHomePrefix)
	if err != nil {
		return nil, fmt.Errorf("create probe CODEX_HOME: %w", err)
	}
	defer os.RemoveAll(home)

	ctx, cancel := context.WithTimeout(context.Background(), codexProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, codexBinaryName, codexAppServerArg)
	// os/exec documents that for duplicate keys the LAST value wins, so
	// appending overrides any inherited CODEX_HOME.
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("app-server stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn app-server: %w", err)
	}
	// The server runs until killed; always reap it so no zombie is left. On
	// timeout the CommandContext kill closes the pipes, which surfaces in the
	// exchange as a read error.
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	entries, err := codexModelListExchange(stdin, stdout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("app-server probe timed out: %w", ctx.Err())
		}
		return nil, err
	}
	return orderCodexServedModels(entries), nil
}

// codexModelListExchange drives the newline-delimited protocol over the given
// transport: initialize (with mandatory clientInfo), wait for the handshake
// acknowledgement, then model/list with includeHidden:true. Factored over
// io.Writer/io.Reader so tests can feed canned stdout without a real binary.
func codexModelListExchange(in io.Writer, out io.Reader) ([]codexModelEntry, error) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 4096), codexRPCMaxLineBytes)

	if err := writeCodexRPC(in, map[string]any{
		"id":     codexRPCInitializeID,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]any{
				"name":    codexRPCClientName,
				"version": codexRPCClientVersion,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send initialize: %w", err)
	}

	// The handshake is complete once the server answers the initialize
	// request or emits the `initialized` notification — accept whichever
	// arrives first (any duplicate is skipped by the model/list loop below).
	for {
		msg, err := readCodexRPC(sc)
		if err != nil {
			return nil, fmt.Errorf("initialize handshake: %w", err)
		}
		if msg.ID != nil && *msg.ID == codexRPCInitializeID {
			if len(msg.Error) != 0 {
				return nil, fmt.Errorf("initialize rejected: %s", truncateForLog(msg.Error, codexRPCErrorLogLimit))
			}
			break
		}
		if msg.Method == codexRPCInitializedMethod {
			break
		}
	}

	// includeHidden:true — hidden ids are deliberately requested and served
	// (see orderCodexServedModels for why).
	if err := writeCodexRPC(in, map[string]any{
		"id":     codexRPCModelListID,
		"method": codexRPCModelListMethod,
		"params": map[string]any{"includeHidden": true},
	}); err != nil {
		return nil, fmt.Errorf("send model/list: %w", err)
	}

	for {
		msg, err := readCodexRPC(sc)
		if err != nil {
			return nil, fmt.Errorf("model/list: %w", err)
		}
		if msg.ID == nil || *msg.ID != codexRPCModelListID {
			continue // server notification or stray message — skip
		}
		if len(msg.Error) != 0 {
			return nil, fmt.Errorf("model/list rejected: %s", truncateForLog(msg.Error, codexRPCErrorLogLimit))
		}
		var result struct {
			Data []codexModelEntry `json:"data"`
		}
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return nil, fmt.Errorf("parse model/list result: %w", err)
		}
		if len(result.Data) == 0 {
			return nil, errors.New("model/list returned no models")
		}
		return result.Data, nil
	}
}

// orderCodexServedModels builds the served id order from a model/list result:
// visible models first in the server's returned (picker) order with the
// isDefault entry promoted to the front, then hidden ids appended at the end.
//
// Hidden ids are included on purpose: they are still valid `--model` values
// (e.g. gpt-5.4 at codex 0.146.0), and excluding them would let the
// stabilize/auto-heal machinery migrate agents off a working pinned model the
// moment a CLI upgrade hides it from the picker.
func orderCodexServedModels(entries []codexModelEntry) []string {
	var def, visible, hidden []string
	for _, e := range entries {
		id := e.ID
		if id == "" {
			// The catalog id and underlying model slug are usually identical;
			// tolerate an entry that only carries the latter.
			id = e.Model
		}
		if id == "" {
			continue
		}
		switch {
		case e.Hidden:
			hidden = append(hidden, id)
		case e.IsDefault:
			def = append(def, id)
		default:
			visible = append(visible, id)
		}
	}
	return append(append(def, visible...), hidden...)
}

// writeCodexRPC marshals one protocol message and writes it newline-delimited.
func writeCodexRPC(in io.Writer, msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = in.Write(append(b, '\n'))
	return err
}

// readCodexRPC scans the next non-empty protocol line and decodes its
// envelope. A non-JSON line is a protocol violation, not something to skip.
func readCodexRPC(sc *bufio.Scanner) (codexRPCMessage, error) {
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg codexRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return codexRPCMessage{}, fmt.Errorf("malformed protocol line: %w", err)
		}
		return msg, nil
	}
	if err := sc.Err(); err != nil {
		return codexRPCMessage{}, err
	}
	return codexRPCMessage{}, io.ErrUnexpectedEOF
}

// truncateForLog bounds raw upstream bytes destined for an error/log line.
func truncateForLog(b []byte, limit int) string {
	s := string(b)
	if len(s) > limit {
		return s[:limit]
	}
	return s
}
