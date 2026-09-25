package agent

// Tests for `hive agy-turn` (agy_turn.go), the per-kick runner behind the
// headless agy shim. A fake agy script stands in for the real CLI: it records
// its argv and stdin per call and prints canned stream-json shaped like agy
// 1.2.9's (captured live on a hive pod).

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const fakeAgyScript = `#!/bin/sh
d="$FAKE_AGY_DIR"
n=$(ls "$d" | grep -c '^args\.')
n=$((n+1))
printf '%s\n' "$@" > "$d/args.$n"
cat > "$d/stdin.$n"
id="${FAKE_AGY_ID:-conv-1}"
status="${FAKE_AGY_STATUS:-SUCCESS}"
cat <<EOF
{"event":"init","conversation_id":"$id","init":{"model":"gemini-3.6-flash-low","cwd":"/tmp"}}
{"event":"step_update","step_update":{"conversation_id":"$id","step_index":2,"state":"DONE","step_type":"user_input"}}
{"event":"step_update","step_update":{"conversation_id":"$id","step_index":5,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"gh pr list\n  --limit 5"}}}}
{"event":"step_update","step_update":{"conversation_id":"$id","step_index":5,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"gh pr list"},"output":"one\r\ntwo\r\nthree\r\nfour\r\nfive\r\n"}}}
{"event":"step_update","step_update":{"conversation_id":"$id","step_index":6,"state":"ACTIVE","step_type":"agent_response","text_delta":"You asked me"}}
{"event":"step_update","step_update":{"conversation_id":"$id","step_index":6,"state":"ACTIVE","step_type":"agent_response","text_delta":" to remember **PELICAN**.\nDone"}}
{"event":"step_update","step_update":{"conversation_id":"$id","step_index":6,"state":"DONE","step_type":"agent_response","text_delta":" now."}}
not json from agy
{"event":"result","result":{"conversation_id":"$id","status":"$status","error":"${FAKE_AGY_ERROR:-}","response":"x","duration_seconds":40.8,"num_turns":2,"usage":{"total_tokens":15703}}}
EOF
exit "${FAKE_AGY_RC:-0}"
`

type fakeAgy struct {
	t      *testing.T
	binary string
	dir    string
}

func newFakeAgy(t *testing.T) *fakeAgy {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	if err := os.Mkdir(calls, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "agy")
	if err := os.WriteFile(bin, []byte(fakeAgyScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_AGY_DIR", calls)
	return &fakeAgy{t: t, binary: bin, dir: calls}
}

func (f *fakeAgy) args(n int) []string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "args."+strconv.Itoa(n)))
	if err != nil {
		f.t.Fatalf("agy call %d not recorded: %v", n, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func (f *fakeAgy) stdin(n int) []byte {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "stdin."+strconv.Itoa(n)))
	if err != nil {
		f.t.Fatalf("agy call %d stdin not recorded: %v", n, err)
	}
	return data
}

func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func runAgyTurnForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunAgyTurn(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAgyTurnArgsDerivesEffortFromModelSuffix(t *testing.T) {
	args := agyTurnArgs("gemini-3.8-flash-high", "", "")
	if v, _ := argValue(args, "--effort"); v != "high" {
		t.Fatalf("agy turn effort = %q, want high; args: %q", v, args)
	}

	args = agyTurnArgs("gemini-3.8-flash-high", "medium", "")
	if v, _ := argValue(args, "--effort"); v != "medium" {
		t.Fatalf("explicit agy turn effort = %q, want medium; args: %q", v, args)
	}
}

// TestRunAgyTurn_PromptOnStdinAndConversationContinuity pins the two gaps the
// runner exists for: the prompt reaches agy on stdin (a >128 KiB argv would
// fail with E2BIG), and the second kick resumes the conversation the first one
// started — by explicit id, never by agy's shared-HOME "most recent".
func TestRunAgyTurn_PromptOnStdinAndConversationContinuity(t *testing.T) {
	fake := newFakeAgy(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	prompt := strings.Repeat("filler ", 30000) + "\nquote ' and \"double\" and $(not a subshell)\n"
	if err := os.WriteFile(promptFile, []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	convFile := filepath.Join(dir, "conversation")

	turn := func(extra ...string) (int, string) {
		args := append([]string{"--agy", fake.binary, "--model", "gemini-3.6-flash-low", "--effort", "low",
			"--prompt-file", promptFile, "--conversation-file", convFile}, extra...)
		code, out, stderr := runAgyTurnForTest(t, args...)
		if stderr != "" {
			t.Logf("stderr: %s", stderr)
		}
		return code, out
	}

	if code, _ := turn(); code != 0 {
		t.Fatalf("first turn exit = %d, want 0", code)
	}
	args := fake.args(1)
	if _, ok := argValue(args, "--conversation"); ok {
		t.Fatalf("first turn after launch must start a new conversation; args: %q", args)
	}
	for _, want := range [][2]string{{"--model", "gemini-3.6-flash-low"}, {"--effort", "low"}, {"--input-format", "stream-json"}, {"--output-format", "stream-json"}} {
		if v, _ := argValue(args, want[0]); v != want[1] {
			t.Errorf("agy %s = %q, want %q; args: %q", want[0], v, want[1], args)
		}
	}
	if args[0] != "--dangerously-skip-permissions" {
		t.Errorf("agy must run with --dangerously-skip-permissions first; args: %q", args)
	}
	for _, a := range args {
		if strings.Contains(a, "filler") {
			t.Fatalf("prompt text leaked into agy argv")
		}
	}

	var in agyStreamInput
	raw := fake.stdin(1)
	if bytes.Count(raw, []byte("\n")) != 1 || !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("stdin must be exactly one NDJSON line (agy runs one turn per line)")
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("stdin is not a stream-json message: %v", err)
	}
	if in.Event != "user" || in.Message.Role != "user" || in.Message.Content != prompt {
		t.Fatalf("stdin message = event %q role %q content-equal %v; want the user prompt verbatim",
			in.Event, in.Message.Role, in.Message.Content == prompt)
	}

	if got := readAgyConversationID(convFile); got != "conv-1" {
		t.Fatalf("conversation file = %q, want conv-1", got)
	}

	if code, _ := turn(); code != 0 {
		t.Fatalf("second turn exit = %d, want 0", code)
	}
	if v, _ := argValue(fake.args(2), "--conversation"); v != "conv-1" {
		t.Fatalf("second turn must resume conv-1; args: %q", fake.args(2))
	}

	t.Setenv("FAKE_AGY_ID", "conv-2")
	if code, _ := turn("--new-conversation"); code != 0 {
		t.Fatalf("new-conversation turn exit = %d, want 0", code)
	}
	if _, ok := argValue(fake.args(3), "--conversation"); ok {
		t.Fatalf("--new-conversation must not resume; args: %q", fake.args(3))
	}
	if got := readAgyConversationID(convFile); got != "conv-2" {
		t.Fatalf("conversation file after a new conversation = %q, want conv-2", got)
	}
}

// TestRunAgyTurn_UnresumableConversationIsReplaced: agy silently starts a new
// conversation when --conversation names one it cannot load (verified live
// with a made-up id). The runner must follow it to the new id and say so.
func TestRunAgyTurn_UnresumableConversationIsReplaced(t *testing.T) {
	fake := newFakeAgy(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptFile, []byte("hi"), 0o644)
	convFile := filepath.Join(dir, "conversation")
	_ = os.WriteFile(convFile, []byte("gone-id\n"), 0o600)
	t.Setenv("FAKE_AGY_ID", "fresh-id")

	code, out, _ := runAgyTurnForTest(t, "--agy", fake.binary, "--prompt-file", promptFile, "--conversation-file", convFile)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if v, _ := argValue(fake.args(1), "--conversation"); v != "gone-id" {
		t.Fatalf("runner must try the recorded conversation first; args: %q", fake.args(1))
	}
	if got := readAgyConversationID(convFile); got != "fresh-id" {
		t.Fatalf("conversation file = %q, want fresh-id", got)
	}
	if !strings.Contains(out, "gone-id could not be resumed") {
		t.Errorf("a lost conversation must be visible in the pane; out: %q", out)
	}
}

// TestRunAgyTurn_RendersReadablePane pins what the pane shows: assistant text
// and tool calls as plain lines, the running marker as the last line while the
// turn runs, and a summary with no running marker after it once it ends.
func TestRunAgyTurn_RendersReadablePane(t *testing.T) {
	fake := newFakeAgy(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptFile, []byte("hi"), 0o644)

	code, out, _ := runAgyTurnForTest(t, "--agy", fake.binary, "--prompt-file", promptFile)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"▸ run_command gh pr list --limit 5\n",
		"  one\n  two\n  three\n  … 2 more lines\n",
		"You asked me to remember **PELICAN**.\n",
		"Done now.\n",
		"not json from agy\n",
		"HIVE agy turn: status=SUCCESS turns=2 duration=40.8s tokens=15703 conversation=conv-1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pane output missing %q; out:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"event"`) {
		t.Errorf("raw stream-json must not reach the pane; out:\n%s", out)
	}
	if !strings.Contains(out, agyHeadlessRunningMarker) {
		t.Fatalf("running marker never shown; out:\n%s", out)
	}
	final := out[strings.LastIndex(out, "\r\x1b[K")+len("\r\x1b[K"):]
	if strings.Contains(final, agyHeadlessRunningMarker) {
		t.Errorf("after the turn ends the running marker must be cleared so the pane reads idle; tail: %q", final)
	}
	if !paneShowsAgentWorking("HIVE agy ...\n" + agyHeadlessRunningMarker + " 5s · run_command") {
		t.Errorf("the runner's status line must satisfy paneShowsAgentWorking")
	}
}

func TestRunAgyTurn_ExitCodes(t *testing.T) {
	fake := newFakeAgy(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptFile, []byte("hi"), 0o644)
	run := func() (int, string) {
		code, out, _ := runAgyTurnForTest(t, "--agy", fake.binary, "--prompt-file", promptFile)
		return code, out
	}

	t.Setenv("FAKE_AGY_STATUS", "ERROR")
	t.Setenv("FAKE_AGY_ERROR", "quota exhausted")
	if code, out := run(); code != 1 || !strings.Contains(out, "HIVE agy turn error: quota exhausted") {
		t.Errorf("an ERROR result must fail the turn and show the error; code=%d out=%q", code, out)
	}
	t.Setenv("FAKE_AGY_STATUS", "SUCCESS")
	t.Setenv("FAKE_AGY_RC", "3")
	if code, _ := run(); code != 3 {
		t.Errorf("agy's own exit code must pass through; got %d", code)
	}

	if code, _, stderr := runAgyTurnForTest(t, "--agy", filepath.Join(dir, "missing-agy"), "--prompt-file", promptFile); code != 1 || !strings.Contains(stderr, "starting") {
		t.Errorf("a missing agy binary must fail loudly; code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := runAgyTurnForTest(t, "--agy", fake.binary); code != 2 {
		t.Errorf("missing --prompt-file must be a usage error; got %d", code)
	}
}

func TestValidAgyConversationID(t *testing.T) {
	for _, ok := range []string{"5eb55e60-385e-43ed-9bde-ab004281227c", "conv_1"} {
		if !validAgyConversationID(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "-p", "--continue", "a b", "a\nb", "a;rm", strings.Repeat("a", 129)} {
		if validAgyConversationID(bad) {
			t.Errorf("%q must be refused: it is passed to agy as an argument", bad)
		}
	}
}

// TestAgyHeadlessLaunchShim_ExportsEnvAndConversationFile runs the real launch
// line and two kick lines in bash, with a stand-in for `hive agy-turn`. It pins
// (a) the per-agent env prefix is exported into the pane shell — bash drops
// assignments that prefix a builtin, so `KEY=v export PS1=...` lost them — and
// (b) every kick in one launch gets the same agent-private conversation file,
// while a relaunch gets a new one.
func TestAgyHeadlessLaunchShim_ExportsEnvAndConversationFile(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	runner := filepath.Join(dir, "hive")
	script := "#!/bin/sh\necho \"RUNNER mode=$HIVE_AGENT_MODE proxy=$HTTPS_PROXY args=$*\"\n"
	if err := os.WriteFile(runner, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	t.Setenv("HIVE_AGENT_MODE", "stale-from-session")

	launch := agyHeadlessFullLaunchCmd(
		shellEnvVar("HIVE_AGENT_MODE", "issues-only")+" "+shellEnvVar("HTTPS_PROXY", "http://127.0.0.1:18443")+" ",
		agyHeadlessLaunchCmd())
	kick := agyHeadlessTurnShellCommand(runner, "agy", "", "", "/tmp/p", false)
	line := launch + "\n" + kick + "\n" + kick + "\n" + launch + "\n" + kick + "\n"

	out, err := exec.Command(bash, "--noprofile", "--norc", "-c", line).CombinedOutput()
	if err != nil {
		t.Fatalf("shim failed: %v\n%s", err, out)
	}
	var files []string
	for _, l := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(l, "RUNNER ") {
			continue
		}
		if !strings.Contains(l, "mode=issues-only proxy=http://127.0.0.1:18443") {
			t.Errorf("kick did not see the launch's env prefix: %q", l)
		}
		f, ok := argValue(strings.Fields(l), "--conversation-file")
		if !ok || !strings.HasPrefix(f, filepath.Join(dir, "hive-agy-conversation.")) {
			t.Fatalf("kick got no per-launch conversation file: %q", l)
		}
		files = append(files, f)
	}
	if len(files) != 3 {
		t.Fatalf("want 3 runner calls, got %d:\n%s", len(files), out)
	}
	if files[0] != files[1] {
		t.Errorf("kicks within one launch must share a conversation file: %q vs %q", files[0], files[1])
	}
	if files[2] == files[0] {
		t.Errorf("a relaunch must start a new conversation file, got the old one %q", files[0])
	}
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Errorf("a relaunch must remove the previous launch's conversation file %q (err=%v)", files[0], err)
	}
	if _, err := os.Stat(files[2]); !os.IsNotExist(err) {
		t.Errorf("the shell's EXIT trap must remove the conversation file %q (err=%v)", files[2], err)
	}
}
