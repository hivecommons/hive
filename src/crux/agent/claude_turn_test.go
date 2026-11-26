package agent

// Tests for `hive claude-turn` (claude_turn.go) and the headless_turns launch
// and kick wiring (claude_headless.go, #19902). A fake claude script stands in
// for the real CLI: it records its argv and stdin per call and prints
// stream-json shaped like Claude Code's print-mode output.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent/panesignal"
	"github.com/hivecommons/hive/crux/config"
)

const fakeClaudeScript = `#!/bin/sh
d="$FAKE_CLAUDE_DIR"
n=$(ls "$d" | grep -c '^args\.')
n=$((n+1))
printf '%s\n' "$@" > "$d/args.$n"
cat > "$d/stdin.$n"
prev=""
for a in "$@"; do
  if [ "$prev" = "--resume" ] && [ -n "$FAKE_CLAUDE_BAD_RESUME" ]; then
    echo "No conversation found with session ID: $a" >&2
    exit 1
  fi
  prev="$a"
done
id="${FAKE_CLAUDE_ID:-sess-1}"
iserr="${FAKE_CLAUDE_IS_ERROR:-false}"
cat <<EOF
{"type":"system","subtype":"init","session_id":"$id","model":"claude-sonnet-5-5","tools":["Bash"]}
{"type":"assistant","message":{"content":[{"type":"text","text":"Looking at open PRs."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"gh pr list\n  --limit 5"}}]},"session_id":"$id"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"one\ntwo\nthree\nfour\nfive"}]},"session_id":"$id"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"block text"}]}]},"session_id":"$id"}
not json from claude
{"type":"assistant","message":{"content":[{"type":"text","text":"Done: remembered PELICAN."}]},"session_id":"$id"}
{"type":"result","subtype":"${FAKE_CLAUDE_SUBTYPE:-success}","is_error":$iserr,"duration_ms":40800,"num_turns":2,"result":"${FAKE_CLAUDE_RESULT:-ok}","session_id":"$id","total_cost_usd":0.01,"usage":{"input_tokens":1200,"output_tokens":34}}
EOF
exit "${FAKE_CLAUDE_RC:-0}"
`

type fakeClaude struct {
	t      *testing.T
	binary string
	dir    string
}

func newFakeClaude(t *testing.T) *fakeClaude {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	if err := os.Mkdir(calls, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_DIR", calls)
	return &fakeClaude{t: t, binary: bin, dir: calls}
}

func (f *fakeClaude) calls() int {
	entries, _ := os.ReadDir(f.dir)
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "args.") {
			n++
		}
	}
	return n
}

func (f *fakeClaude) args(n int) []string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "args."+strconv.Itoa(n)))
	if err != nil {
		f.t.Fatalf("claude call %d not recorded: %v", n, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func (f *fakeClaude) stdin(n int) []byte {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "stdin."+strconv.Itoa(n)))
	if err != nil {
		f.t.Fatalf("claude call %d stdin not recorded: %v", n, err)
	}
	return data
}

func runClaudeTurnForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunClaudeTurn(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestRunClaudeTurn_PrintModePromptOnStdinAndSessionContinuity pins what the
// runner exists for: claude runs in print mode (-p, the Agent SDK billing
// path) with the agent's own launch flags kept, the prompt reaches it on
// stdin rather than argv, and the second kick resumes the session the first
// one started by explicit id.
func TestRunClaudeTurn_PrintModePromptOnStdinAndSessionContinuity(t *testing.T) {
	fake := newFakeClaude(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	prompt := strings.Repeat("filler ", 30000) + "\nquote ' and \"double\" and $(not a subshell)\n"
	if err := os.WriteFile(promptFile, []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	sessFile := filepath.Join(dir, "session")
	launch := []string{fake.binary, "--model", "anthropic/claude-sonnet-5-5", "--dangerously-skip-permissions",
		"--bare", "--settings", "/etc/hive/claude-inference.json", "--disallowed-tools", "mcp__github__create_issue"}

	turn := func(extra ...string) int {
		args := append([]string{"--prompt-file", promptFile, "--session-file", sessFile}, extra...)
		args = append(append(args, "--"), launch...)
		code, _, stderr := runClaudeTurnForTest(t, args...)
		if stderr != "" {
			t.Logf("stderr: %s", stderr)
		}
		return code
	}

	if code := turn(); code != 0 {
		t.Fatalf("first turn exit = %d, want 0", code)
	}
	args := fake.args(1)
	if hasArg(args, "--resume") {
		t.Fatalf("first turn after launch must start a new session; args: %q", args)
	}
	if args[0] != "-p" || !hasArg(args, "--verbose") {
		t.Fatalf("claude must run in print mode with --verbose; args: %q", args)
	}
	for _, want := range [][2]string{
		{"--input-format", "stream-json"}, {"--output-format", "stream-json"},
		{"--model", "anthropic/claude-sonnet-5-5"}, {"--settings", "/etc/hive/claude-inference.json"},
		{"--disallowed-tools", "mcp__github__create_issue"},
	} {
		if v, _ := argValue(args, want[0]); v != want[1] {
			t.Errorf("claude %s = %q, want %q; args: %q", want[0], v, want[1], args)
		}
	}
	if !hasArg(args, "--dangerously-skip-permissions") || !hasArg(args, "--bare") {
		t.Errorf("the agent's launch flags must be kept; args: %q", args)
	}
	for _, a := range args {
		if strings.Contains(a, "filler") {
			t.Fatalf("prompt text leaked into claude argv")
		}
	}

	raw := fake.stdin(1)
	if bytes.Count(raw, []byte("\n")) != 1 || !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("stdin must be exactly one NDJSON line")
	}
	var in claudeStreamInput
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("stdin is not a stream-json message: %v", err)
	}
	if in.Type != "user" || in.Message.Role != "user" || in.Message.Content != prompt {
		t.Fatalf("stdin message = type %q role %q content-equal %v; want the user prompt verbatim",
			in.Type, in.Message.Role, in.Message.Content == prompt)
	}
	if got := readAgyConversationID(sessFile); got != "sess-1" {
		t.Fatalf("session file = %q, want sess-1", got)
	}

	if code := turn(); code != 0 {
		t.Fatalf("second turn exit = %d, want 0", code)
	}
	if v, _ := argValue(fake.args(2), "--resume"); v != "sess-1" {
		t.Fatalf("second turn must resume sess-1; args: %q", fake.args(2))
	}

	t.Setenv("FAKE_CLAUDE_ID", "sess-2")
	if code := turn("--new-session"); code != 0 {
		t.Fatalf("new-session turn exit = %d, want 0", code)
	}
	if hasArg(fake.args(3), "--resume") {
		t.Fatalf("--new-session must not resume; args: %q", fake.args(3))
	}
	if got := readAgyConversationID(sessFile); got != "sess-2" {
		t.Fatalf("session file after a new session = %q, want sess-2", got)
	}
}

// TestRunClaudeTurn_UnresumableSessionStartsOver: claude refuses an unknown
// --resume id before it starts. The kick must still run, on a new session.
func TestRunClaudeTurn_UnresumableSessionStartsOver(t *testing.T) {
	fake := newFakeClaude(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptFile, []byte("hi"), 0o644)
	sessFile := filepath.Join(dir, "session")
	if err := writeAgyConversationID(sessFile, "gone-1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_BAD_RESUME", "1")
	t.Setenv("FAKE_CLAUDE_ID", "sess-new")

	code, out, _ := runClaudeTurnForTest(t, "--prompt-file", promptFile, "--session-file", sessFile, "--", fake.binary)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 after the fresh retry; pane:\n%s", code, out)
	}
	if fake.calls() != 2 {
		t.Fatalf("want 2 claude calls (resume, then fresh), got %d", fake.calls())
	}
	if v, _ := argValue(fake.args(1), "--resume"); v != "gone-1" {
		t.Fatalf("first call must try the recorded session; args: %q", fake.args(1))
	}
	if hasArg(fake.args(2), "--resume") {
		t.Fatalf("retry must start a new session; args: %q", fake.args(2))
	}
	if !strings.Contains(out, "session gone-1 could not be resumed") {
		t.Errorf("pane must say the session was replaced:\n%s", out)
	}
	if got := readAgyConversationID(sessFile); got != "sess-new" {
		t.Fatalf("session file = %q, want sess-new", got)
	}
}

// TestRunClaudeTurn_RendersReadablePane: the pane shows assistant text, one
// line per tool call, a few lines of tool output, and a summary, and the
// running marker never survives the end of the turn.
func TestRunClaudeTurn_RendersReadablePane(t *testing.T) {
	fake := newFakeClaude(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptFile, []byte("hi"), 0o644)

	code, out, _ := runClaudeTurnForTest(t, "--prompt-file", promptFile, "--", fake.binary)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"Looking at open PRs.\n",
		"▸ Bash gh pr list --limit 5\n",
		"  one\n  two\n  three\n  … 2 more lines\n",
		"  block text\n",
		"not json from claude\n",
		"Done: remembered PELICAN.\n",
		"HIVE claude turn: status=success turns=2 duration=40.8s input_tokens=1200 output_tokens=34 session=sess-1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pane missing %q:\n%s", want, out)
		}
	}
	// The final line must not be a running status line: the shell's ready
	// marker follows it.
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("pane output must end on a fresh line:\n%q", out)
	}
	tail := out[strings.LastIndex(out, "\r\x1b[K")+len("\r\x1b[K"):]
	if strings.Contains(tail, claudeHeadlessRunningMarker) {
		t.Errorf("running marker left after the turn ended: %q", tail)
	}
}

func TestRunClaudeTurn_ExitCodes(t *testing.T) {
	fake := newFakeClaude(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptFile, []byte("hi"), 0o644)
	run := func() (int, string) {
		code, out, _ := runClaudeTurnForTest(t, "--prompt-file", promptFile, "--", fake.binary)
		return code, out
	}

	t.Setenv("FAKE_CLAUDE_IS_ERROR", "true")
	t.Setenv("FAKE_CLAUDE_SUBTYPE", "error_during_execution")
	t.Setenv("FAKE_CLAUDE_RESULT", "Your credit balance is too low")
	code, out := run()
	if code == 0 {
		t.Fatalf("an is_error result must fail the turn")
	}
	if !strings.Contains(out, "HIVE claude turn error: Your credit balance is too low") {
		t.Errorf("pane must carry the error text:\n%s", out)
	}

	t.Setenv("FAKE_CLAUDE_IS_ERROR", "false")
	t.Setenv("FAKE_CLAUDE_SUBTYPE", "success")
	t.Setenv("FAKE_CLAUDE_RC", "3")
	if code, _ := run(); code != 3 {
		t.Fatalf("claude's own exit code must pass through, got %d", code)
	}

	code, _, _ = runClaudeTurnForTest(t, "--prompt-file", filepath.Join(dir, "missing"), "--", fake.binary)
	if code != 1 {
		t.Fatalf("missing prompt file exit = %d, want 1", code)
	}
}

func TestParseClaudeTurnArgs(t *testing.T) {
	var stderr bytes.Buffer
	if _, err := parseClaudeTurnArgs([]string{"--prompt-file", "p"}, &stderr); err == nil {
		t.Fatal("a missing claude command must be refused")
	}
	if _, err := parseClaudeTurnArgs([]string{"--", "claude"}, &stderr); err == nil {
		t.Fatal("a missing --prompt-file must be refused")
	}
	for _, owned := range []string{"-p", "--resume", "--continue", "--output-format"} {
		if _, err := parseClaudeTurnArgs([]string{"--prompt-file", "p", "--", "claude", owned}, &stderr); err == nil {
			t.Errorf("a launch line setting %s must be refused: the runner owns it", owned)
		}
	}
	opts, err := parseClaudeTurnArgs([]string{"--prompt-file", "p", "--new-session", "--", "claude", "--model", "m"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.newSession || strings.Join(opts.command, " ") != "claude --model m" {
		t.Fatalf("parsed %+v", opts)
	}
}

// TestAgentUsesClaudeHeadless pins the opt-in: only translator-routed
// backends with headless_turns set and no operator launch_cmd qualify.
func TestAgentUsesClaudeHeadless(t *testing.T) {
	m := &Manager{}
	mk := func(headless bool, launchCmd string) *AgentProcess {
		return &AgentProcess{Config: config.AgentConfig{HeadlessTurns: headless, LaunchCmd: launchCmd}}
	}
	cases := []struct {
		name    string
		backend string
		agent   *AgentProcess
		want    bool
	}{
		{"litellm opted in", "litellm", mk(true, ""), true},
		{"vllm opted in", "vllm", mk(true, ""), true},
		{"default off", "litellm", mk(false, ""), false},
		{"claude subscription backend", "claude", mk(true, ""), false},
		{"codex", "codex", mk(true, ""), false},
		{"launch_cmd override", "litellm", mk(true, "claude --model x"), false},
		{"nil agent", "litellm", nil, false},
	}
	for _, tc := range cases {
		if got := m.agentUsesClaudeHeadless(tc.backend, tc.agent); got != tc.want {
			t.Errorf("%s: agentUsesClaudeHeadless = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestClaudeHeadlessLaunchShim_ExportsEnvAndSessionFile runs the real launch
// line and kick lines in bash, with a stand-in for `hive claude-turn`. It pins
// that the env prefix (ANTHROPIC_BASE_URL and the placeholder key among it)
// reaches every kick, that kicks in one launch share a session file while a
// relaunch gets a new one, and that the quoted claude launch line arrives as
// separate argv words after --.
func TestClaudeHeadlessLaunchShim_ExportsEnvAndSessionFile(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	runner := filepath.Join(dir, "hive")
	script := "#!/bin/sh\necho \"RUNNER base=$ANTHROPIC_BASE_URL args=$*\"\nfor a in \"$@\"; do echo \"ARG $a\"; done\n"
	if err := os.WriteFile(runner, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)

	launch := agyHeadlessFullLaunchCmd(shellEnvVar("ANTHROPIC_BASE_URL", "http://127.0.0.1:18080")+" ", claudeHeadlessLaunchCmd())
	claudeCmd := "claude --model 'anthropic/claude-sonnet-5-5' --dangerously-skip-permissions --disallowed-tools 'Bash(rm -rf /:*)'"
	kick := claudeHeadlessTurnShellCommand(runner, claudeCmd, "/tmp/p", false)
	line := launch + "\n" + kick + "\n" + kick + "\n" + launch + "\n" + kick + "\n"

	out, err := exec.Command(bash, "--noprofile", "--norc", "-c", line).CombinedOutput()
	if err != nil {
		t.Fatalf("shim failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ARG Bash(rm -rf /:*)\n") || !strings.Contains(string(out), "ARG --\n") {
		t.Fatalf("the quoted launch line must reach the runner as argv words after --:\n%s", out)
	}
	if !strings.Contains(string(out), claudeHeadlessReadyMarker) || !strings.Contains(string(out), claudeHeadlessRunningMarker) {
		t.Fatalf("shim must print the running and ready markers:\n%s", out)
	}
	var files []string
	for _, l := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(l, "RUNNER ") {
			continue
		}
		if !strings.Contains(l, "base=http://127.0.0.1:18080") {
			t.Errorf("kick did not see the launch's env prefix: %q", l)
		}
		f, ok := argValue(strings.Fields(l), "--session-file")
		if !ok || !strings.HasPrefix(f, filepath.Join(dir, "hive-claude-session.")) {
			t.Fatalf("kick got no per-launch session file: %q", l)
		}
		files = append(files, f)
	}
	if len(files) != 3 {
		t.Fatalf("want 3 runner calls, got %d:\n%s", len(files), out)
	}
	if files[0] != files[1] {
		t.Errorf("kicks within one launch must share a session file: %q vs %q", files[0], files[1])
	}
	if files[2] == files[0] {
		t.Errorf("a relaunch must start a new session file, got the old one %q", files[0])
	}

	if fresh := claudeHeadlessTurnShellCommand(runner, claudeCmd, "/tmp/p", true); !strings.Contains(fresh, " --new-session -- ") {
		t.Errorf("ClearOnKick must map to --new-session before --: %q", fresh)
	}
}

// TestClaudeHeadlessPaneSignals: the shim's markers drive the same gates as
// agy's — a pane at the ready prompt is a live backend accepting input, and a
// running turn is busy until the ready marker follows it.
func TestClaudeHeadlessPaneSignals(t *testing.T) {
	idle := "HIVE_CLAUDE_HEADLESS_READY\nHIVE_CLAUDE_HEADLESS_READY> "
	if !panesignal.HasCLIMarker(idle) || !panesignal.ShowsInputPrompt(idle) {
		t.Fatalf("an idle headless claude pane must read as a ready CLI")
	}
	if panesignal.ShowsAgentWorking(idle) {
		t.Fatalf("an idle headless claude pane must not read as working")
	}
	running := idle + "printf ...\n\n" + claudeHeadlessRunningMarker + "\nLooking at PRs.\n" + claudeHeadlessRunningMarker + " 12s · Bash"
	if !panesignal.ShowsAgentWorking(running) {
		t.Fatalf("a running headless claude turn must read as working")
	}
	done := running + "\r\x1b[KHIVE claude turn: status=success\n" + claudeHeadlessReadyMarker + "\n" + claudeHeadlessReadyMarker + "> "
	if panesignal.ShowsAgentWorking(done) {
		t.Fatalf("a finished headless claude turn must not read as working")
	}
}

// TestDeliverKickLocked_ClaudeHeadlessRoutesToTurnRunner pins the manager
// wiring for a headless_turns agent: a kick types one `hive claude-turn` line
// (not the raw prompt) into the shell pane, ClearOnKick maps to
// --new-session, and the inference stall watchdog stays disarmed so its
// "continue" nudges are never typed into bash between turns.
func TestDeliverKickLocked_ClaudeHeadlessRoutesToTurnRunner(t *testing.T) {
	for _, clear := range []bool{false, true} {
		m, agent, _ := kickLogTestManager(t, "")
		agent.Config.Backend = "litellm"
		agent.Config.HeadlessTurns = true
		agent.Config.ClearOnKick = clear
		agent.claudeHeadlessTurnCmd = "'claude' '--model' 'qwen'"
		t.Cleanup(func() { _ = os.Remove(filepath.Join(agentStateDir, ".hive-claude-prompt-"+agent.Name+".txt")) })
		termSeams(m).captureVisiblePane = func(*AgentProcess) string {
			return claudeHeadlessReadyMarker + "\n" + claudeHeadlessReadyMarker + "> "
		}
		termSeams(m).sendKeys = func(*AgentProcess, ...string) {}
		var typed []string
		termSeams(m).sendLiteral = func(_ *AgentProcess, text string) { typed = append(typed, text) }

		if err := m.deliverKickLocked(agent, "analyze coverage", "send-kick"); err != nil {
			t.Fatalf("clear=%v: deliverKickLocked: %v", clear, err)
		}
		if len(typed) != 1 {
			t.Fatalf("clear=%v: typed %d lines, want one turn-runner line: %#v", clear, len(typed), typed)
		}
		line := typed[0]
		if !strings.Contains(line, " "+ClaudeTurnSubcommand+" ") || !strings.Contains(line, "-- 'claude' '--model' 'qwen'") {
			t.Fatalf("clear=%v: kick did not type the claude-turn runner line: %q", clear, line)
		}
		if strings.Contains(line, "analyze coverage") {
			t.Fatalf("clear=%v: prompt text typed into the shell instead of the prompt file: %q", clear, line)
		}
		if got := strings.Contains(line, "--new-session"); got != clear {
			t.Fatalf("clear=%v: --new-session present = %v", clear, got)
		}
		if agent.LastKick == nil {
			t.Fatalf("clear=%v: delivered kick not recorded", clear)
		}
		if !agent.lastInferKickAt.IsZero() {
			t.Fatalf("clear=%v: inference stall watchdog armed for a headless_turns pane", clear)
		}
	}
}

// TestDeliverKickLocked_ClaudeHeadlessRefusesUnbuiltCommand pins that a kick
// racing the first launch, before the per-kick claude line exists, is refused
// rather than typed into the shell.
func TestDeliverKickLocked_ClaudeHeadlessRefusesUnbuiltCommand(t *testing.T) {
	m, agent, _ := kickLogTestManager(t, "")
	agent.Config.Backend = "litellm"
	agent.Config.HeadlessTurns = true
	termSeams(m).captureVisiblePane = func(*AgentProcess) string { return claudeHeadlessReadyMarker + "> " }
	var typed []string
	termSeams(m).sendKeys = func(_ *AgentProcess, keys ...string) { typed = append(typed, keys...) }
	termSeams(m).sendLiteral = func(_ *AgentProcess, text string) { typed = append(typed, text) }

	err := m.deliverKickLocked(agent, "analyze coverage", "send-kick")
	if !errors.Is(err, errClaudeHeadlessNotLaunched) {
		t.Fatalf("deliverKickLocked err = %v, want errClaudeHeadlessNotLaunched", err)
	}
	if len(typed) != 0 {
		t.Fatalf("typed into the pane with no claude command built: %#v", typed)
	}
}
