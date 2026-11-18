package meetllm

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// fakeCLIBehaviours are the fake claude's turn behaviours.
const (
	fakeOK        = "ok"
	fakeHang      = "hang"
	fakeCrash     = "crash"
	fakeCrashOnce = "crashonce"
	fakeError     = "error"
	// fakeSlow answers after half a second, so turns overlap.
	fakeSlow = "slow"
)

// fakeTokensPerTurn is what the fake charges per turn: 10+5+3 plus 20 cache
// reads at 1/cacheReadWeightDivisor.
const fakeTokensPerTurn = 20

// fakeCLI writes a fake streaming claude into dir: it records its argv
// (one per line) and environment, counts starts and turns, and answers
// every stdin line with a result event per its behaviour.
func fakeCLI(t *testing.T, dir, behaviour string) string {
	t.Helper()
	script := `#!/bin/sh
D='` + dir + `'
printf '%s\n' "$@" > "$D/argv"
env > "$D/env"
echo start >> "$D/starts"
pwd > "$D/cwd"
echo '{"type":"system","subtype":"init","tools":[],"mcp_servers":[]}'
n=0
while IFS= read -r line; do
  n=$((n+1))
  echo "$line" >> "$D/turns"
  case "` + behaviour + `" in
    hang) sleep 30 ;;
    slow) sleep 0.5 ;;
    crash) exit 1 ;;
    crashonce) if [ ! -f "$D/crashed" ]; then : > "$D/crashed"; exit 1; fi ;;
    error) echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":3,"cache_read_input_tokens":20}}'; continue ;;
  esac
  instr=no
  case "$line" in *"<instructions>"*) instr=yes ;; esac
  echo '{"type":"assistant","message":{"content":[]}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":"reply '$n' pid '$$' instr '$instr'","usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":3,"cache_read_input_tokens":20}}'
done
`
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// waitFor polls cond, for state the fake writes asynchronously.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testEnv(extra map[string]string) func(string) string {
	m := map[string]string{"PATH": os.Getenv("PATH"), "HOME": "/hive-home"}
	for k, v := range extra {
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

func cliConfig(t *testing.T, dir, behaviour string) Config {
	return Config{
		Provider: ProviderAgentBackend, Backend: BackendClaude, Model: config.DefaultMeetingsClaudeCLIModel,
		CLIBinary: fakeCLI(t, dir, behaviour), CLIHome: filepath.Join(dir, "home"),
		Timeout: 5 * time.Second, MaxInFlight: 1,
		Getenv: testEnv(map[string]string{"GH_TOKEN": "ghp_secret", "HIVE_GITHUB_TOKEN": "x", "CLAUDE_CONFIG_DIR": "/cfg"}),
	}
}

func newSession(t *testing.T, c Config) *CLISession {
	t.Helper()
	s, err := NewCLISession(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func replyPID(t *testing.T, reply string) int {
	t.Helper()
	f := strings.Fields(reply)
	if len(f) < 4 {
		t.Fatalf("reply %q", reply)
	}
	pid, err := strconv.Atoi(f[3])
	if err != nil {
		t.Fatalf("reply %q: %v", reply, err)
	}
	return pid
}

// Two turns go to one warm process, each with its instructions, and the
// launch carries the no-tools profile and a scrubbed environment.
func TestCLISessionStreamingTurns(t *testing.T) {
	dir := t.TempDir()
	s := newSession(t, cliConfig(t, dir, fakeOK))
	if err := s.Warm(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r1, tok1, err := s.Turn(ctx, "answer briefly", "what is hive?")
	if err != nil || tok1 != fakeTokensPerTurn || !strings.HasPrefix(r1, "reply 1 ") || !strings.HasSuffix(r1, "instr yes") {
		t.Fatalf("turn 1 = %q %d %v", r1, tok1, err)
	}
	r2, _, err := s.Turn(ctx, "", "second")
	if err != nil || !strings.HasPrefix(r2, "reply 2 ") || !strings.HasSuffix(r2, "instr no") || replyPID(t, r1) != replyPID(t, r2) {
		t.Fatalf("turn 2 = %q %v (turn 1 %q)", r2, err, r1)
	}
	if n := len(readLines(t, filepath.Join(dir, "starts"))); n != 1 {
		t.Fatalf("starts = %d, want one warm process", n)
	}
	argv := readLines(t, filepath.Join(dir, "argv"))
	for _, want := range [][]string{
		{"-p"}, {"--tools", ""}, {"--strict-mcp-config"}, {"--disable-slash-commands"}, {"--setting-sources", ""},
		{"--settings", claudeNoHooksSettings}, {"--no-session-persistence"}, {"--system-prompt", cliSystemPrompt},
		{"--input-format", "stream-json"}, {"--output-format", "stream-json"}, {"--verbose"},
		{"--model", config.DefaultMeetingsClaudeCLIModel},
	} {
		if !containsSeq(argv, want) {
			t.Errorf("argv %q lacks %q", argv, want)
		}
	}
	for _, bad := range []string{"--dangerously-skip-permissions", "--mcp-config", "--allowedTools", "--allowed-tools"} {
		if containsSeq(argv, []string{bad}) {
			t.Errorf("argv carries %s", bad)
		}
	}
	env := strings.Join(readLines(t, filepath.Join(dir, "env")), "\n")
	for _, want := range []string{"HOME=" + filepath.Join(dir, "home"), "CLAUDE_CONFIG_DIR=/cfg", "DISABLE_AUTOUPDATER=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	for _, bad := range []string{"GH_TOKEN", "HIVE_GITHUB_TOKEN"} {
		if strings.Contains(env, bad+"=") {
			t.Errorf("env leaks %s", bad)
		}
	}
	cwd := strings.Join(readLines(t, filepath.Join(dir, "cwd")), "")
	if !strings.Contains(filepath.Base(cwd), cliTempPrefix) {
		t.Errorf("cwd %q is not the empty temp dir", cwd)
	}
}

func containsSeq(list, seq []string) bool {
	for i := 0; i+len(seq) <= len(list); i++ {
		if reflect.DeepEqual(list[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

// After RecycleTurns turns the process is replaced.
func TestCLISessionRecycle(t *testing.T) {
	dir := t.TempDir()
	c := cliConfig(t, dir, fakeOK)
	c.RecycleTurns = 2
	s := newSession(t, c)
	ctx := context.Background()
	var pids []int
	for i := 0; i < 3; i++ {
		r, _, err := s.Turn(ctx, "", "q")
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, replyPID(t, r))
	}
	if pids[0] != pids[1] || pids[1] == pids[2] {
		t.Fatalf("pids %v: want the third turn on a new process", pids)
	}
}

// A turn that times out kills the process and starts a fresh one, so a
// late reply cannot answer the next request.
func TestCLISessionTimeout(t *testing.T) {
	dir := t.TempDir()
	s := newSession(t, cliConfig(t, dir, fakeHang))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := s.Turn(ctx, "", "q"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	waitFor(t, "a replacement process", func() bool { return len(readLines(t, filepath.Join(dir, "starts"))) == 2 })
}

// A crashed CLI fails the turn and the next turn runs on a new process.
func TestCLISessionCrashAndRestart(t *testing.T) {
	dir := t.TempDir()
	s := newSession(t, cliConfig(t, dir, fakeCrashOnce))
	ctx := context.Background()
	if _, _, err := s.Turn(ctx, "", "q"); err == nil {
		t.Fatal("want an error from the crashed CLI")
	}
	r, tok, err := s.Turn(ctx, "", "q")
	if err != nil || tok != fakeTokensPerTurn || !strings.HasPrefix(r, "reply 1 ") {
		t.Fatalf("after restart = %q %d %v", r, tok, err)
	}
}

// A CLI that keeps crashing keeps failing; a failed result still counts.
func TestCLISessionCrashAndErrorResult(t *testing.T) {
	s := newSession(t, cliConfig(t, t.TempDir(), fakeCrash))
	for i := 0; i < 2; i++ {
		if _, _, err := s.Turn(context.Background(), "", "q"); err == nil {
			t.Fatal("want an error")
		}
	}
	s2 := newSession(t, cliConfig(t, t.TempDir(), fakeError))
	r, tok, err := s2.Turn(context.Background(), "", "q")
	if err == nil || r != "" || tok != fakeTokensPerTurn {
		t.Fatalf("error result = %q %d %v", r, tok, err)
	}
}

// Complete charges the usage ledger and refuses once the budget is spent,
// without starting a turn.
func TestCompleteSessionUsageAndBudget(t *testing.T) {
	dir := t.TempDir()
	c := cliConfig(t, dir, fakeOK)
	c.UsageFile = filepath.Join(dir, "usage.jsonl")
	c.SlotsDir = filepath.Join(dir, "slots")
	c.MaxTokens = fakeTokensPerTurn + 1
	c.System = "be brief"
	c.Session = newSession(t, c)
	reply, err := Complete(context.Background(), c, http.DefaultClient, "hello")
	if err != nil || !strings.HasSuffix(reply, "instr yes") {
		t.Fatalf("Complete = %q %v", reply, err)
	}
	if n, _ := UsageTotal(c.UsageFile); n != fakeTokensPerTurn {
		t.Fatalf("usage = %d", n)
	}
	if _, err := Complete(context.Background(), c, http.DefaultClient, "again"); err != nil {
		t.Fatalf("second call under budget: %v", err)
	}
	turns := len(readLines(t, filepath.Join(dir, "turns")))
	if _, err := Complete(context.Background(), c, http.DefaultClient, "third"); !errors.Is(err, ErrBudget) {
		t.Fatalf("over budget err = %v", err)
	}
	if n := len(readLines(t, filepath.Join(dir, "turns"))); n != turns {
		t.Fatalf("a refused call reached the CLI (%d turns, was %d)", n, turns)
	}
}

// A call waiting behind its meeting's turn gives up at its deadline; a
// closed session refuses turns.
func TestCLISessionBusyAndClosed(t *testing.T) {
	dir := t.TempDir()
	c := cliConfig(t, dir, fakeOK)
	s := newSession(t, c)
	s.sem <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := s.Turn(ctx, "", "q"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy err = %v", err)
	}
	c.Session = s
	if _, err := completeSession(ctx, c, "q"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy completeSession err = %v", err)
	}
	<-s.sem
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Turn(context.Background(), "", "q"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed err = %v", err)
	}
	if err := s.Warm(); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed Warm err = %v", err)
	}
}

func TestCLISessionStartFailures(t *testing.T) {
	if _, err := NewCLISession(Config{Backend: "codex"}); err == nil {
		t.Fatal("want codex refused")
	}
	s := newSession(t, Config{Backend: BackendClaude, CLIBinary: filepath.Join(t.TempDir(), "missing")})
	if err := s.Warm(); err == nil {
		t.Fatal("want a start error for a missing binary")
	}
	if _, _, err := s.Turn(context.Background(), "", "q"); err == nil {
		t.Fatal("want a turn error for a missing binary")
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))
	if _, err := startCLI(Config{Backend: BackendClaude}, completionProfiles[BackendClaude]); err == nil {
		t.Fatal("want a workdir error")
	}
	if _, err := completeCLIOnce(context.Background(), Config{Backend: BackendClaude, Model: "m"}, "q"); err == nil {
		t.Fatal("want a one-shot workdir error")
	}
}

// fakeOneShot writes a fake `claude -p --output-format json`.
func fakeOneShot(t *testing.T, dir, output string, code int) string {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + dir + "/argv'\ncat > '" + dir + "/stdin'\nprintf '%s' '" + output + "'\nexit " + strconv.Itoa(code) + "\n"
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// `hive meeting-llm` with a backend runs the CLI once, charges the usage
// and prints the reply.
func TestRunAgentBackendOneShot(t *testing.T) {
	dir := t.TempDir()
	bin := fakeOneShot(t, dir, `{"type":"result","subtype":"success","is_error":false,"result":"spek reply","usage":{"input_tokens":7,"output_tokens":3}}`, 0)
	usage := filepath.Join(dir, "usage.jsonl")
	env := testEnv(map[string]string{
		EnvBackend: "claude", EnvModel: "claude-sonnet-5-5", EnvCLIBinary: bin, EnvCLIHome: dir,
		EnvUsageFile: usage, EnvTimeoutMS: "5000", EnvSlotsDir: filepath.Join(dir, "slots"),
	})
	var out, errb bytes.Buffer
	if code := Run(nil, strings.NewReader("the prompt"), &out, &errb, env); code != 0 || out.String() != "spek reply" {
		t.Fatalf("Run = %d %q %q", code, out.String(), errb.String())
	}
	if n, _ := UsageTotal(usage); n != 10 {
		t.Fatalf("usage = %d", n)
	}
	argv := readLines(t, filepath.Join(dir, "argv"))
	if !containsSeq(argv, []string{"--output-format", "json"}) || !containsSeq(argv, []string{"--tools", ""}) || containsSeq(argv, []string{"--input-format", "stream-json"}) {
		t.Fatalf("argv %q", argv)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stdin")); !strings.Contains(string(b), "<request>\nthe prompt\n</request>") {
		t.Fatalf("stdin %q", b)
	}
}

func TestCompleteCLIOnceFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := completeCLIOnce(ctx, Config{Backend: "gemini", Model: "m"}, "q"); err == nil {
		t.Fatal("want gemini refused")
	}
	if _, err := completeCLIOnce(ctx, Config{Backend: BackendClaude}, "q"); err == nil {
		t.Fatal("want a model required")
	}
	dir := t.TempDir()
	c := Config{Backend: BackendClaude, Model: "m", CLIBinary: fakeOneShot(t, dir, "not json", 1), Getenv: testEnv(nil)}
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "CLI failed") {
		t.Fatalf("exit 1 err = %v", err)
	}
	c.CLIBinary = fakeOneShot(t, t.TempDir(), "not json", 0)
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "no result") {
		t.Fatalf("no result err = %v", err)
	}
	c.CLIBinary = fakeOneShot(t, t.TempDir(), `{"type":"result","subtype":"success","result":"x","usage":{"output_tokens":1}}`, 0)
	c.UsageFile = filepath.Join(t.TempDir(), "missing-dir", "u.jsonl")
	if _, err := completeCLIOnce(ctx, c, "q"); err == nil || !strings.Contains(err.Error(), "recording usage") {
		t.Fatalf("usage err = %v", err)
	}
	hang := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c = Config{Backend: BackendClaude, Model: "m", CLIBinary: hang, Getenv: testEnv(nil)}
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := completeCLIOnce(tctx, c, "q"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err = %v", err)
	}
}

func TestFromEnvAgentBackend(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := FromEnv(env(map[string]string{EnvBackend: "claude"})); err == nil {
		t.Fatal("want a model required")
	}
	if _, err := FromEnv(env(map[string]string{EnvBackend: "codex", EnvModel: "m"})); err == nil {
		t.Fatal("want codex refused")
	}
	c, err := FromEnv(env(map[string]string{EnvBackend: "Claude", EnvModel: "m", EnvCLIHome: "/h", EnvCLIBinary: "/bin/claude"}))
	if err != nil || c.Provider != ProviderAgentBackend || c.Backend != "claude" || c.CLIHome != "/h" || c.CLIBinary != "/bin/claude" {
		t.Fatalf("FromEnv = %+v %v", c, err)
	}
}

func TestCLIHelpers(t *testing.T) {
	ev, err := parseCLIResult([]byte(`[{"type":"system"},{"type":"result","subtype":"success","result":"a"}]`))
	if err != nil || ev.Result != "a" {
		t.Fatalf("array = %+v %v", ev, err)
	}
	if _, err := parseCLIResult([]byte(`[{"type":"system"}]`)); err == nil {
		t.Fatal("want no result")
	}
	if _, err := (cliEvent{Subtype: cliResultSuccess, Result: "  "}).reply(); err == nil {
		t.Fatal("want empty text refused")
	}
	var b bytes.Buffer
	w := &limitedWriter{w: &b, n: 3}
	if n, _ := w.Write([]byte("abcdef")); n != 6 || b.String() != "abc" {
		t.Fatalf("limited %d %q", n, b.String())
	}
	if n, _ := w.Write([]byte("g")); n != 1 || b.String() != "abc" {
		t.Fatalf("limited after cap %q", b.String())
	}
	if got := cliTurnText("", "p"); got != "<request>\np\n</request>" {
		t.Fatalf("turn text %q", got)
	}
	if got := cliBinary(Config{Backend: "Claude"}); got != "claude" {
		t.Fatalf("binary %q", got)
	}
	p := completionProfiles[BackendClaude]
	if argv := p.argv("", false); containsSeq(argv, []string{"--model"}) {
		t.Fatalf("empty model passed: %q", argv)
	}
	t.Setenv("HOME", "/inherited")
	env := strings.Join(p.environ(nil, ""), "\n")
	if !strings.Contains(env, "HOME=/inherited") {
		t.Fatalf("env %q", env)
	}
}

// Every backend the config accepts as a CLI completion backend has a
// profile here.
func TestCompletionBackendsMatchConfig(t *testing.T) {
	if got, want := CompletionBackends(), config.MeetingsCompletionBackends; !reflect.DeepEqual(got, want) {
		t.Fatalf("profiles %v, config %v", got, want)
	}
}

// TestCLIUsageTotalDiscountsCacheReads pins the weighting: cache reads count
// at 1/cacheReadWeightDivisor, everything else in full.
func TestCLIUsageTotalDiscountsCacheReads(t *testing.T) {
	u := cliUsage{InputTokens: 100, OutputTokens: 50, CacheCreationInputTokens: 30, CacheReadInputTokens: 1000}
	if got, want := u.total(), 100+50+30+1000/cacheReadWeightDivisor; got != want {
		t.Fatalf("total = %d, want %d", got, want)
	}
}

// TestWarmSessionBudgetCoversALongMeeting is the invariant behind the Betty
// suite going silent: a warm session's history grows every turn and is
// re-read from the cache each time, and thirty questions at that cost must
// still fit the default per-meeting budget instead of exhausting it.
func TestWarmSessionBudgetCoversALongMeeting(t *testing.T) {
	const (
		meetingBudget = 200000 // config.DefaultMeetingsMaxTokensPerMeeting
		questions     = 30
		freshPerTurn  = 900  // new input, output and cache writes of one turn
		historyGrowth = 3300 // history added to the cache by each turn
	)
	total := 0
	for q := 0; q < questions; q++ {
		turn := q % DefaultRecycleTurns // the warm session restarts every DefaultRecycleTurns
		u := cliUsage{InputTokens: freshPerTurn, CacheReadInputTokens: historyGrowth * turn}
		total += u.total()
		if total > meetingBudget {
			t.Fatalf("budget exhausted after %d of %d questions at %d tokens", q+1, questions, total)
		}
	}
}
